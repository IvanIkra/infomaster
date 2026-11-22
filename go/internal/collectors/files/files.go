// Package files is the files of chat messages (§5.1, §6.3, §9): PDFs and images, fetched by the twin when a batch
// needs them, never ahead.
//
// A batch asks for the PDF of a message it is about to parse, or for its image in a chat with «Картинки» on. The first
// time the twin downloads it into `<data>/files/<sha256>.<ext>`, a PDF's first page is read locally, an image's type
// is checked, and a row of `files` keeps the result, also a failure of the file itself (it is gone, broken, no image),
// so a message edited later does not download the same file again; a message whose file was replaced gets the new
// one. A download that fails on the way (the twin is offline, the network, a flood wait) is not kept: the batch leaves
// the message in the queue for its next run (NotNow). What Haiku read on an image (processing/images) lives in the
// same row. Files and rows go with their messages after 14 days (D29); a sheet made of a file keeps its text
// (processing/sheets), and the file lives on in the library.
//
// A letter's PDF attachment is kept when the letter arrives (collectors/mail), in a row of source `mail`: the mailbox
// is not asked again. A message of a study chat or a letter without a PDF of its own may link to one (D59): the batch
// asks for it (FetchLink), and the row keeps the link it came from. A download that fails for another reason than a
// gone or broken file is tried again at the next runs, MaxTries times in all. And the PDFs the owner uploads in the
// web app as sheets of their own (D44): kept the same way, in rows of source `own` of the chat OwnChat.
package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/links"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/processing/pdf"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	MaxSize        = 20 * 1024 * 1024 // bytes: a bigger PDF is a book, not a sheet, and is not downloaded (256 MB of memory)
	UploadMaxSize  = 20 * 1024 * 1024 // bytes of a PDF the owner uploads
	OwnChat        = -1               // the chat of the owner's own sheets: no chat has this id (the mailbox is 0)
	ImageMaxSize   = 3_750_000        // bytes: in base64 it stays within the API's 5 MB for an image
	FirstPageChars = 4000             // kept of the first page
	MaxTries       = 3                // failed downloads of one file, at runs of the batches, before it is given up
)

// PDFTypes are the media types of a PDF.
var PDFTypes = []string{"application/pdf", "application/x-pdf", "application/acrobat"}

// ImageTypes are what the model reads, by the first bytes of the file: the extension it is kept under, by media type.
var ImageTypes = map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/gif": "gif", "image/webp": "webp"}

// NotNow is a file that cannot be fetched now, but may be later: nothing is kept, the message waits. The twin's
// download returns it when it is offline or must wait.
type NotNow struct{ Msg string }

func (e *NotNow) Error() string { return e.Msg }

// IsNotNow reports whether err is a NotNow.
func IsNotNow(err error) bool {
	var n *NotNow
	return errors.As(err, &n)
}

// finalError is a failure of the file itself (Python's FileNotFoundError and ValueError): it is not tried again.
type finalError struct{ msg string }

func (e *finalError) Error() string { return e.msg }

func final(err error) bool {
	var f *finalError
	var p *pdf.PdfError
	return errors.As(err, &f) || errors.As(err, &p) || errors.Is(err, fs.ErrNotExist)
}

// Size is a `files` entry's size as JSON gave it (a float64), or an int; 0 for none.
func Size(entry inbox.File) int64 {
	switch v := entry["size"].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func text(entry inbox.File, key string) string {
	s, _ := entry[key].(string)
	return s
}

// IsPDF is a `files` entry of a PDF: by its kind, or a document whose type or name says PDF (a teacher's client may
// send one as application/octet-stream).
func IsPDF(entry inbox.File) bool {
	kind := text(entry, "kind")
	return kind == "pdf" || kind == "document" && (slices.Contains(PDFTypes, strings.ToLower(text(entry, "mime"))) ||
		strings.HasSuffix(strings.ToLower(text(entry, "name")), ".pdf"))
}

// PDFFile is the PDF of a message (its `files` entry), if it has one.
func PDFFile(files []inbox.File) inbox.File {
	for _, f := range files {
		if IsPDF(f) {
			return f
		}
	}
	return nil
}

// ImageFile is the image of a message: a photo, or a picture sent as a file. Stickers and GIF animations are not
// images.
func ImageFile(files []inbox.File) inbox.File {
	for _, f := range files {
		kind := text(f, "kind")
		if _, image := ImageTypes[text(f, "mime")]; kind == "photo" || kind == "document" && image {
			return f
		}
	}
	return nil
}

// MediaType is the type of an image the model reads, by its signature; "" for anything else (HEIC, BMP, not an
// image).
func MediaType(data []byte) string {
	switch {
	case bytesHasPrefix(data, "\xff\xd8\xff"):
		return "image/jpeg"
	case bytesHasPrefix(data, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case bytesHasPrefix(data, "GIF87a"), bytesHasPrefix(data, "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func bytesHasPrefix(data []byte, prefix string) bool { return strings.HasPrefix(string(data), prefix) }

// ImageReading is what Haiku read on an image.
type ImageReading struct {
	Shows string `json:"shows"`
	Text  string `json:"text"`
}

// Reading is what Haiku read on an image: nil while unread or when it could not.
func Reading(row *Row) *ImageReading {
	if row == nil || row.Reading == nil || *row.Reading == "" {
		return nil
	}
	var r ImageReading
	if json.Unmarshal([]byte(*row.Reading), &r) != nil {
		return nil
	}
	return &r
}

// Row is a row of `files`.
type Row struct {
	ID        int64   `json:"id"`
	Source    string  `json:"source"`
	ChatID    int64   `json:"chat_id"`
	MsgID     int64   `json:"msg_id"`
	At        string  `json:"at"`
	Name      *string `json:"name"`
	Size      *int64  `json:"size"`
	Sha256    *string `json:"sha256"`
	Path      *string `json:"path"`
	Pages     *int64  `json:"pages"`
	FirstPage *string `json:"first_page"`
	Error     *string `json:"error"`
	Kind      string  `json:"kind"`
	Reading   *string `json:"reading"`
	URL       *string `json:"url"`
	Tries     int64   `json:"tries"`
}

const columns = "id, source, chat_id, msg_id, at, name, size, sha256, path, pages, first_page, error, kind, reading, " +
	"url, tries"

func scanRow(scan func(...any) error) (*Row, error) {
	var r Row
	err := scan(&r.ID, &r.Source, &r.ChatID, &r.MsgID, &r.At, &r.Name, &r.Size, &r.Sha256, &r.Path, &r.Pages,
		&r.FirstPage, &r.Error, &r.Kind, &r.Reading, &r.URL, &r.Tries)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Files are the files of the messages.
type Files struct {
	store     *store.Store
	db        *sql.DB
	DataDir   string
	Directory string
	// Download is the twin's download of a message's file into a path; false when the message or its file is gone,
	// a *NotNow when it cannot be done now. Nil without a twin: nothing is fetched then, and the batch reads the file's
	// name only.
	Download func(ctx context.Context, chatID, msgID int64, path string) (bool, error)
	// FetchURL is the PDF behind a link into a path: its name, "" when there is none, a *links.LinkError when it
	// cannot be read now (links.Fetch). Nil: links are not followed.
	FetchURL func(ctx context.Context, url, path string, limit int64) (string, error)
}

// New gives the files kept under dataDir/files.
func New(s *store.Store, dataDir string) *Files {
	return &Files{store: s, db: s.DB, DataDir: dataDir, Directory: filepath.Join(dataDir, "files")}
}

// Path is where the file is, while it is kept; "" otherwise.
func (f *Files) Path(row *Row) string {
	if row == nil || row.Path == nil || *row.Path == "" {
		return ""
	}
	path := filepath.Join(f.DataDir, filepath.FromSlash(*row.Path))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// ByHash is a PDF by its hash, while it is kept: a sheet finds its file this way even after its message's file was
// replaced. "" when it is gone.
func (f *Files) ByHash(sha string) string {
	path := filepath.Join(f.Directory, sha+".pdf")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// Fetch is the row of the message's PDF: stored, or downloaded and read now. Nil when the message has no PDF, it is
// too big, or there is no twin; a row with Error when the file is gone or unreadable. A *NotNow when the download
// failed on the way.
func (f *Files) Fetch(ctx context.Context, m *inbox.Message) (*Row, error) {
	meta := PDFFile(m.Files)
	if meta == nil || Size(meta) > MaxSize {
		return nil, nil
	}
	return f.fetch(ctx, m, meta, "pdf")
}

// FetchImage is the row of the message's image, as Fetch for a PDF. One too big for the model is not downloaded.
func (f *Files) FetchImage(ctx context.Context, m *inbox.Message) (*Row, error) {
	meta := ImageFile(m.Files)
	if meta == nil || Size(meta) > ImageMaxSize {
		return nil, nil
	}
	return f.fetch(ctx, m, meta, "image")
}

// ReadAs is the reading of the same image in another message (a forward, a repost): it is not read twice. Nil for
// none.
func (f *Files) ReadAs(row *Row) (*string, error) {
	var found string
	err := f.db.QueryRow("select reading from files where sha256 = ? and kind = 'image' and reading != '' "+
		"and id != ? limit 1", row.Sha256, row.ID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &found, nil
}

// SetReading keeps what Haiku read on the image of a row ("" when it could not, with the error; errText "" is
// null).
func (f *Files) SetReading(row *Row, value, errText string) (*Row, error) {
	var e *string
	if errText != "" {
		e = &errText
	}
	if _, err := f.db.Exec("update files set reading = ?, error = ? where id = ?", value, e, row.ID); err != nil {
		return nil, err
	}
	out := *row
	out.Reading, out.Error = &value, e
	return &out, nil
}

func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *Files) mkdir() error { return os.MkdirAll(f.Directory, 0o700) }

// Keep is a PDF the owner uploads: stored as a downloaded one, its first page read; the row of source `own` goes
// after 14 days like the others. An *i18n.Error when it is too big or no PDF that can be read.
func (f *Files) Keep(data []byte, name string, at time.Time) (*Row, error) {
	if len(data) > UploadMaxSize {
		mb := UploadMaxSize >> 20
		return nil, i18n.Err(fmt.Sprintf("the file is over %d MB", mb), fmt.Sprintf("файл больше %d МБ", mb))
	}
	if err := f.mkdir(); err != nil {
		return nil, err
	}
	part := filepath.Join(f.Directory, "own_"+token(6)+".part")
	defer os.Remove(part)
	if err := os.WriteFile(part, data, 0o600); err != nil {
		return nil, err
	}
	values, err := f.store_(part, "pdf")
	if err != nil {
		var p *pdf.PdfError
		if errors.As(err, &p) {
			return nil, i18n.Err("not a PDF that can be read: "+p.Msg, "это не PDF или его не прочитать: "+p.Msg)
		}
		return nil, err
	}
	var msgID int64
	err = f.store.InTx(func(tx *sql.Tx) error {
		if err := tx.QueryRow("select coalesce(max(msg_id), 0) + 1 n from files where source = 'own'").
			Scan(&msgID); err != nil {
			return err
		}
		_, err := tx.Exec("insert into files(source, chat_id, msg_id, at, name, size, sha256, path, pages, "+
			"first_page, error, kind) values('own', ?, ?, ?, ?, ?, ?, ?, ?, ?, null, 'pdf')",
			OwnChat, msgID, clock.LocalISO(at), name, len(data), values.sha256, values.path, values.pages,
			values.firstPage)
		return err
	})
	if err != nil {
		return nil, err
	}
	return scanRow(f.db.QueryRow("select "+columns+" from files where source = 'own' and msg_id = ?", msgID).Scan)
}

// Row is the row of a message's file; nil for none.
func (f *Files) Row(source string, chatID, msgID int64) (*Row, error) {
	return scanRow(f.db.QueryRow("select "+columns+" from files where source = ? and chat_id = ? and msg_id = ?",
		source, chatID, msgID).Scan)
}

// stored is what a kept file gives its row; nil pointers are nulls.
type stored struct {
	sha256, path, firstPage, err *string
	pages                        *int64
}

func ptr[T any](v T) *T { return &v }

func errText(err error) string {
	if s := err.Error(); s != "" {
		return s
	}
	return fmt.Sprintf("%T", err)
}

func sameName(name *string, meta inbox.File) bool {
	v, ok := meta["name"].(string)
	if !ok {
		return name == nil
	}
	return name != nil && *name == v
}

func sameSize(size *int64, meta inbox.File) bool {
	if meta["size"] == nil {
		return size == nil
	}
	return size != nil && *size == Size(meta)
}

func metaName(meta inbox.File) *string {
	if v, ok := meta["name"].(string); ok {
		return &v
	}
	return nil
}

func metaSize(meta inbox.File) *int64 {
	if meta["size"] == nil {
		return nil
	}
	return ptr(Size(meta))
}

func (f *Files) fetch(ctx context.Context, m *inbox.Message, meta inbox.File, kind string) (*Row, error) {
	if m.Source == "mail" { // kept when the letter came, or never
		row, err := f.Row("mail", m.ChatID, m.MsgID)
		if err != nil || row == nil || row.Kind != kind || row.URL != nil {
			return nil, err
		}
		return row, nil
	}
	if m.Source != "tg" {
		return nil, nil
	}
	chatID, msgID := m.ChatID, m.MsgID
	row, err := f.Row("tg", chatID, msgID)
	if err != nil {
		return nil, err
	}
	same := row != nil && row.Kind == kind && sameName(row.Name, meta) && sameSize(row.Size, meta) && row.URL == nil
	if same && (row.Sha256 != nil && *row.Sha256 != "" || row.Tries >= MaxTries) {
		return row, nil
	}
	if f.Download == nil {
		return nil, nil
	}
	var tries int64
	if same {
		tries = row.Tries
	}
	var values stored
	if err := f.mkdir(); err != nil {
		return nil, err
	}
	part := filepath.Join(f.Directory, fmt.Sprintf("%d_%d.part", chatID, msgID))
	defer os.Remove(part)
	err = func() error {
		ok, err := f.Download(ctx, chatID, msgID, part)
		if err != nil {
			return err
		}
		if !ok {
			return &finalError{"the message or its file is gone"}
		}
		values, err = f.store_(part, kind)
		return err
	}()
	if err != nil {
		if IsNotNow(err) {
			return nil, err
		}
		slog.Warn(fmt.Sprintf("%s of message %d in %d not read: %v", kind, msgID, chatID, err))
		values = stored{err: ptr(errText(err))}
		// A gone or broken file stays so; anything else (Telegram refused it once) is tried at the next runs.
		if final(err) {
			tries = MaxTries
		} else {
			tries++
		}
	}
	return f.put("tg", chatID, msgID, inbox.KeptFrom(m), metaName(meta), metaSize(meta), values, kind, nil, tries)
}

func (f *Files) put(source string, chatID, msgID int64, at string, name *string, size *int64, values stored,
	kind string, url *string, tries int64) (*Row, error) {
	_, err := f.db.Exec(
		"insert into files(source, chat_id, msg_id, at, name, size, sha256, path, pages, first_page, error, "+
			"kind, reading, url, tries) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, null, ?, ?) "+
			"on conflict(source, chat_id, msg_id) do update set "+
			"at = excluded.at, name = excluded.name, size = excluded.size, sha256 = excluded.sha256, "+
			"path = excluded.path, pages = excluded.pages, first_page = excluded.first_page, "+
			"error = excluded.error, kind = excluded.kind, reading = null, url = excluded.url, "+
			"tries = excluded.tries",
		source, chatID, msgID, at, name, size, values.sha256, values.path, values.pages, values.firstPage,
		values.err, kind, url, tries)
	if err != nil {
		return nil, err
	}
	return f.Row(source, chatID, msgID)
}

// KeepAttachment keeps a letter's PDF attachment when the letter arrives (Mail.Poll), before the letter itself: the
// batch that reads the letter finds it. Nil for one too big; a row with Error for a PDF that cannot be read.
func (f *Files) KeepAttachment(msgID int64, at time.Time, name string, data []byte) (*Row, error) {
	if len(data) > MaxSize {
		return nil, nil
	}
	if err := f.mkdir(); err != nil {
		return nil, err
	}
	part := filepath.Join(f.Directory, fmt.Sprintf("mail_%d_%s.part", msgID, token(4)))
	defer os.Remove(part)
	values, err := func() (stored, error) {
		if err := os.WriteFile(part, data, 0o600); err != nil {
			return stored{}, err
		}
		return f.store_(part, "pdf")
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("the attachment %s of letter %d not read: %v", name, msgID, err))
		values = stored{err: ptr(errText(err))}
	}
	return f.put("mail", inbox.Mailbox, msgID, clock.LocalISO(at), &name, ptr(int64(len(data))), values, "pdf", nil,
		MaxTries)
}

// FetchLink is the PDF behind the first link of a message that leads to one (collectors/links), for a message
// without a PDF of its own: the row, stored or downloaded now; nil when the message links to none or links are not
// followed. A link that cannot be read now is tried at the next runs, MaxTries times in all.
func (f *Files) FetchLink(ctx context.Context, m *inbox.Message) (*Row, error) {
	found := links.DocumentLinks(m.Text)
	if len(found) == 0 {
		return nil, nil
	}
	source, chatID, msgID := m.Source, m.ChatID, m.MsgID
	row, err := f.Row(source, chatID, msgID)
	if err != nil {
		return nil, err
	}
	same := row != nil && row.URL != nil && slices.Contains(found, *row.URL)
	if same && (row.Sha256 != nil && *row.Sha256 != "" || row.Tries >= MaxTries) {
		return row, nil
	}
	if f.FetchURL == nil {
		return nil, nil
	}
	var tries int64
	if same {
		tries = row.Tries
	}
	if err := f.mkdir(); err != nil {
		return nil, err
	}
	part := filepath.Join(f.Directory, fmt.Sprintf("link_%d_%d.part", chatID, msgID))
	defer os.Remove(part)
	var values stored
	url, name, failed := found[0], "", false
	err = func() error {
		for _, link := range found {
			got, err := f.FetchURL(ctx, link, part, MaxSize)
			var linkErr *links.LinkError
			if errors.As(err, &linkErr) {
				failed, values.err = true, ptr(link+": "+err.Error())
				continue
			}
			if err != nil {
				return err
			}
			if got != "" {
				url, name = link, got
				kept, err := f.store_(part, "pdf")
				if err != nil {
					return err
				}
				values = kept
				return nil
			}
		}
		if !failed {
			values.err = ptr("no PDF behind the links")
		}
		return nil
	}()
	if err != nil { // a PDF that cannot be read
		slog.Warn(fmt.Sprintf("the PDF of %s (message %d in %d) not read: %v", url, msgID, chatID, err))
		values.err = ptr(errText(err))
	}
	if failed && values.sha256 == nil {
		tries++
	} else {
		tries = MaxTries
	}
	if values.sha256 != nil {
		slog.Info(fmt.Sprintf("the PDF of %s (message %d in %d) is read", url, msgID, chatID))
	}
	var namePtr *string
	if name != "" {
		namePtr = &name
	}
	return f.put(source, chatID, msgID, inbox.KeptFrom(m), namePtr, nil, values, "pdf", &url, tries)
}

// store_ keeps the downloaded file under its hash, a PDF with its first page read.
func (f *Files) store_(part, kind string) (stored, error) {
	info, err := os.Stat(part)
	if err != nil {
		return stored{}, err
	}
	if kind == "pdf" && info.Size() > MaxSize {
		return stored{}, &finalError{fmt.Sprintf("the PDF is too big (%d bytes)", info.Size())}
	}
	file, err := os.Open(part)
	if err != nil {
		return stored{}, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil {
		return stored{}, err
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	values := stored{sha256: &sha}
	var extension string
	if kind == "pdf" {
		extract, err := pdf.ExtractText(part, 1)
		if err != nil {
			return stored{}, err
		}
		values.pages = ptr(int64(extract.Pages))
		values.firstPage = ptr(pytext.Cut(extract.First(), FirstPageChars))
		extension = "pdf"
	} else {
		data, err := os.ReadFile(part) // an image: within ImageMaxSize
		if err != nil {
			return stored{}, err
		}
		found := MediaType(data)
		if found == "" {
			return stored{}, &finalError{"not a JPEG, PNG, GIF or WebP image"}
		}
		if len(data) > ImageMaxSize {
			return stored{}, &finalError{"the image is too big for the model"}
		}
		extension = ImageTypes[found]
	}
	name := sha + "." + extension
	target := filepath.Join(f.Directory, name)
	if err := os.Rename(part, target); err != nil {
		return stored{}, err
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return stored{}, err
	}
	values.path = ptr("files/" + name)
	return values, nil
}

// Prune removes the rows of messages past their 14 days, and every file no row has: theirs, and those a replaced
// file left.
func (f *Files) Prune(now time.Time) (int64, error) {
	result, err := f.db.Exec("delete from files where at < ?", clock.LocalISO(now.Add(-inbox.Retention)))
	if err != nil {
		return 0, err
	}
	removed, _ := result.RowsAffected()
	rows, err := f.db.Query("select path from files where path is not null")
	if err != nil {
		return 0, err
	}
	kept := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return 0, err
		}
		kept[path] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(f.Directory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return removed, err
	}
	for _, entry := range entries {
		path := filepath.Join(f.Directory, entry.Name())
		if strings.HasSuffix(entry.Name(), ".part") { // a download under way, or one a crash left: gone after a day
			if info, err := entry.Info(); err == nil && info.ModTime().Before(time.Now().Add(-24*time.Hour)) {
				os.Remove(path)
			}
		} else if !kept["files/"+entry.Name()] {
			os.Remove(path)
		}
	}
	return removed, nil
}
