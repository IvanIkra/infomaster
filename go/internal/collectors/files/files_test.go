package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/links"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = -1001234567890

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ)

// makePDF is a PDF with one page per text (ASCII, one line each, Helvetica).
func makePDF(pages []string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	for _, text := range pages {
		var stream strings.Builder
		for i, line := range strings.Split(text, "\n") {
			if text == "" {
				break
			}
			fmt.Fprintf(&stream, "BT /F1 12 Tf 50 %d Td (%s) Tj ET\n", 750-16*i, line)
		}
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", stream.Len(), stream.String()))
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << "+
			"/Font << /F1 3 0 R >> >> /Contents %d 0 R >>", len(objects)))
		kids = append(kids, fmt.Sprintf("%d 0 R", len(objects)))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	out := []byte("%PDF-1.4\n")
	var offsets []int
	for n, body := range objects {
		offsets = append(offsets, len(out))
		out = append(out, fmt.Sprintf("%d 0 obj\n%s\nendobj\n", n+1, body)...)
	}
	xref := len(out)
	out = append(out, fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)...)
	for _, offset := range offsets {
		out = append(out, fmt.Sprintf("%010d 00000 n \n", offset)...)
	}
	return append(out, fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1,
		xref)...)
}

var (
	sheet3      = makePDF([]string{"Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS", "Task 1. Prove that the series"})
	sheet3Fixed = makePDF([]string{"Calculus. Sheet 3. Series\nHand in by 30.10 23:59 in the LMS",
		"Task 1. Prove that the series converges"})
	jpeg  = []byte("\xff\xd8\xff\xe0\x00\x10JFIF" + strings.Repeat("timetable", 20))
	png   = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("notice", 20))
	heic  = []byte("\x00\x00\x00\x18ftypheic" + strings.Repeat("x", 40))
	photo = inbox.File{"kind": "photo", "mime": "image/jpeg"}
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

type key struct{ chat, msg int64 }

type fixture struct {
	files     *Files
	inbox     *inbox.Inbox
	remote    map[key][]byte
	downloads []int64
	offline   bool
	links     map[string]any // url: PDF bytes, or an error
	asked     []string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	s := must(store.Open(filepath.Join(dir, "x.db")))
	t.Cleanup(func() { s.Close() })
	f := &fixture{files: New(s, dir), inbox: inbox.Open(s), remote: map[key][]byte{}, links: map[string]any{}}
	f.files.Download = f.download
	f.files.FetchURL = func(ctx context.Context, url, path string, limit int64) (string, error) {
		f.asked = append(f.asked, url)
		switch found := f.links[url].(type) {
		case error:
			return "", found
		case []byte:
			return "Sheet-3.pdf", os.WriteFile(path, found, 0o600)
		}
		return "", nil
	}
	return f
}

func (f *fixture) download(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
	f.downloads = append(f.downloads, msgID)
	if f.offline {
		return false, &NotNow{"the twin is not connected to Telegram"}
	}
	data, ok := f.remote[key{chatID, msgID}]
	if !ok {
		return false, nil
	}
	return true, os.WriteFile(path, data, 0o600)
}

func (f *fixture) add(msgID int64, text string, files []inbox.File, at time.Time) {
	name := "Ассистент"
	sender := int64(7)
	must(f.inbox.Add(chat, msgID, inbox.Fields{SenderID: &sender, SenderName: &name, At: at, Text: text,
		Files: files}, "tg", time.Time{}))
}

// send is a message with a PDF the twin can download.
func (f *fixture) send(msgID int64, data []byte, name string) {
	var files []inbox.File
	if data != nil {
		f.remote[key{chat, msgID}] = data
		files = []inbox.File{{"kind": "pdf", "name": name, "mime": "application/pdf", "size": len(data)}}
	}
	f.add(msgID, "", files, now.Add(-30*time.Minute))
}

// sendImage is a message with an image the twin can download.
func (f *fixture) sendImage(msgID int64, data []byte, meta inbox.File) {
	f.remote[key{chat, msgID}] = data
	file := inbox.File{}
	for k, v := range meta {
		file[k] = v
	}
	file["size"] = len(data)
	f.add(msgID, "", []inbox.File{file}, now.Add(-30*time.Minute))
}

func (f *fixture) message(msgID int64) *inbox.Message {
	return must(f.inbox.Message(chat, msgID, "tg"))
}

func listDir(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFilesAreFetchedOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.send(1, sheet3, "list3.pdf")
	row := must(f.files.Fetch(ctx, f.message(1)))
	if !reflect.DeepEqual(f.downloads, []int64{1}) || *row.Pages != 2 ||
		!strings.HasPrefix(*row.FirstPage, "Calculus. Sheet 3") {
		t.Fatal(row, f.downloads)
	}
	if data := must(os.ReadFile(f.files.Path(row))); string(data) != string(sheet3) ||
		*row.Path != "files/"+*row.Sha256+".pdf" {
		t.Fatal(*row.Path)
	}
	if again := must(f.files.Fetch(ctx, f.message(1))); again.ID != row.ID || len(f.downloads) != 1 {
		t.Fatal("downloaded twice")
	}

	// The message's file was replaced: its new one is fetched.
	f.remote[key{chat, 1}] = sheet3Fixed
	must(f.inbox.Edit(chat, 1, "", []inbox.File{{"kind": "pdf", "name": "list3.pdf", "size": len(sheet3Fixed)}}, now))
	fixed := must(f.files.Fetch(ctx, f.message(1)))
	if !reflect.DeepEqual(f.downloads, []int64{1, 1}) || *fixed.Sha256 == *row.Sha256 {
		t.Fatal("not fetched again")
	}

	f.send(2, nil, "")
	if r := must(f.files.Fetch(ctx, f.message(2))); r != nil {
		t.Fatal("no PDF")
	}
	f.add(3, "", []inbox.File{{"kind": "pdf", "name": "gone.pdf", "size": 5}}, now)
	failed := must(f.files.Fetch(ctx, f.message(3))) // deleted before the batch came
	if failed.Sha256 != nil || *failed.Error != "the message or its file is gone" {
		t.Fatal(failed)
	}
	if again := must(f.files.Fetch(ctx, f.message(3))); !reflect.DeepEqual(again, failed) ||
		!reflect.DeepEqual(f.downloads, []int64{1, 1, 3}) {
		t.Fatal("a gone file is asked again")
	}
	f.add(4, "", []inbox.File{{"kind": "pdf", "name": "book.pdf", "size": 80 * 1024 * 1024}}, now)
	if r := must(f.files.Fetch(ctx, f.message(4))); r != nil {
		t.Fatal("a book is downloaded")
	}
	f.files.Download = nil // the twin is not logged in
	f.send(5, sheet3, "other.pdf")
	if r := must(f.files.Fetch(ctx, f.message(5))); r != nil {
		t.Fatal("fetched without a twin")
	}

	if n := must(f.files.Prune(now.Add(15 * 24 * time.Hour))); n != 2 {
		t.Fatal(n)
	}
	if f.files.Path(fixed) != "" || len(listDir(t, f.files.Directory)) != 0 {
		t.Fatal(listDir(t, f.files.Directory))
	}
}

func TestWhatIsAnImage(t *testing.T) {
	if MediaType(jpeg) != "image/jpeg" || MediaType(png) != "image/png" || MediaType([]byte("GIF89a....")) != "image/gif" ||
		MediaType([]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")) != "image/webp" {
		t.Fatal("types")
	}
	if MediaType(heic) != "" || MediaType([]byte("%PDF-1.4")) != "" {
		t.Fatal("not images")
	}
	if !reflect.DeepEqual(ImageFile([]inbox.File{photo}), photo) {
		t.Fatal("photo")
	}
	screenshot := inbox.File{"kind": "document", "name": "s.png", "mime": "image/png", "size": 10.0}
	if !reflect.DeepEqual(ImageFile([]inbox.File{screenshot}), screenshot) {
		t.Fatal("screenshot")
	}
	if ImageFile([]inbox.File{{"kind": "document", "name": "a.heic", "mime": "image/heic"}}) != nil {
		t.Fatal("heic")
	}
	if ImageFile([]inbox.File{{"kind": "sticker", "mime": "image/webp"}, {"kind": "gif", "mime": "video/mp4"}}) != nil {
		t.Fatal("sticker")
	}
}

func TestImagesAreFetchedOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.sendImage(1, jpeg, photo)
	row := must(f.files.FetchImage(ctx, f.message(1)))
	if !reflect.DeepEqual(f.downloads, []int64{1}) || row.Kind != "image" || row.Reading != nil {
		t.Fatal(row)
	}
	if *row.Path != "files/"+*row.Sha256+".jpg" || string(must(os.ReadFile(f.files.Path(row)))) != string(jpeg) {
		t.Fatal(*row.Path)
	}
	if again := must(f.files.FetchImage(ctx, f.message(1))); again.ID != row.ID || len(f.downloads) != 1 {
		t.Fatal("twice")
	}
	if r := must(f.files.Fetch(ctx, f.message(1))); r != nil { // no PDF in it
		t.Fatal("a PDF")
	}

	f.sendImage(2, heic, inbox.File{"kind": "document", "name": "a.png", "mime": "image/png"}) // a lie in its name
	failed := must(f.files.FetchImage(ctx, f.message(2)))
	if failed.Sha256 != nil || !strings.Contains(*failed.Error, "not a JPEG") {
		t.Fatal(failed)
	}
	f.add(3, "", []inbox.File{{"kind": "photo", "mime": "image/jpeg", "size": ImageMaxSize + 1}}, now)
	if r := must(f.files.FetchImage(ctx, f.message(3))); r != nil { // too big for the model: not downloaded
		t.Fatal("too big")
	}

	f.sendImage(4, png, inbox.File{"kind": "document", "name": "notice.png", "mime": "image/png"})
	if r := must(f.files.FetchImage(ctx, f.message(4))); !strings.HasSuffix(*r.Path, ".png") {
		t.Fatal(*r.Path)
	}
	if n := must(f.files.Prune(now.Add(15 * 24 * time.Hour))); n != 3 {
		t.Fatal(n)
	}
	if names := listDir(t, f.files.Directory); len(names) != 0 {
		t.Fatal(names)
	}
}

func TestAFileTheTwinCannotDownloadNowIsNotKept(t *testing.T) {
	f := setup(t)
	f.offline = true
	f.sendImage(1, jpeg, photo)
	if _, err := f.files.FetchImage(context.Background(), f.message(1)); !IsNotNow(err) {
		t.Fatal(err)
	}
	if r := must(f.files.Row("tg", chat, 1)); r != nil {
		t.Fatal("kept")
	}
}

const dropbox = "https://www.dropbox.com/scl/fi/abc123/Sheet-3.pdf?rlkey=k9&dl=0"

func TestASheetBehindALink(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.links[dropbox] = sheet3
	f.add(1, "Новый листок: "+dropbox, nil, now.Add(-30*time.Minute))
	row := must(f.files.FetchLink(ctx, f.message(1)))
	if *row.URL != dropbox || *row.Name != "Sheet-3.pdf" || *row.Pages != 2 || row.Tries != MaxTries {
		t.Fatal(row)
	}
	row = must(f.files.FetchLink(ctx, f.message(1)))
	if *row.URL != dropbox || !reflect.DeepEqual(f.asked, []string{dropbox}) { // kept: not downloaded again
		t.Fatal(f.asked)
	}
	if again := must(f.files.Fetch(ctx, f.message(1))); again != nil {
		t.Fatal("a link's PDF is no file of the message")
	}
}

func TestALinkIsTriedAgainThenGivenUp(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.links[dropbox] = &links.LinkError{Msg: "HTTP 503"}
	f.add(1, dropbox, nil, now.Add(-30*time.Minute))
	message := f.message(1)
	for tries := int64(1); tries <= MaxTries; tries++ {
		row := must(f.files.FetchLink(ctx, message))
		if row.Tries != tries || row.Sha256 != nil || !strings.Contains(*row.Error, "HTTP 503") {
			t.Fatal(row)
		}
	}
	must(f.files.FetchLink(ctx, message))
	if len(f.asked) != MaxTries { // given up
		t.Fatal(f.asked)
	}

	f.add(2, "https://disk.yandex.ru/d/folder", nil, now) // a folder: no PDF, not asked again
	row := must(f.files.FetchLink(ctx, f.message(2)))
	if *row.Error != "no PDF behind the links" || row.Tries != MaxTries {
		t.Fatal(row)
	}
	f.add(3, "просто текст", nil, now)
	if r := must(f.files.FetchLink(ctx, f.message(3))); r != nil {
		t.Fatal("no link")
	}
}

func TestAFailedDownloadIsTriedAgain(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.send(1, sheet3, "list3.pdf")
	message := f.message(1)
	f.files.Download = func(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
		f.downloads = append(f.downloads, msgID)
		return false, errors.New("CHANNEL_PRIVATE")
	}
	if row := must(f.files.Fetch(ctx, message)); row.Tries != 1 {
		t.Fatal(row.Tries)
	}
	f.files.Download = f.download // the next run: Telegram gives it
	row := must(f.files.Fetch(ctx, message))
	if row.Sha256 == nil || row.Error != nil || !reflect.DeepEqual(f.downloads, []int64{1, 1}) {
		t.Fatal(row)
	}
}

func TestAPDFSentAsADocument(t *testing.T) {
	if PDFFile([]inbox.File{{"kind": "document", "name": "HW 3.PDF", "mime": "application/octet-stream"}}) == nil ||
		PDFFile([]inbox.File{{"kind": "document", "name": "x", "mime": "application/x-pdf"}}) == nil ||
		PDFFile([]inbox.File{{"kind": "document", "name": "a.docx", "mime": "application/msword"}}) != nil {
		t.Fatal("pdf_file")
	}
}

func TestOwnPDFsAndAttachments(t *testing.T) {
	f := setup(t)
	row := must(f.files.Keep(sheet3, "Sheet 3.pdf", now))
	if row.Source != "own" || row.ChatID != OwnChat || row.MsgID != 1 || *row.Pages != 2 || row.At != "2026-10-20T12:00:00" {
		t.Fatal(row)
	}
	if f.files.ByHash(*row.Sha256) == "" || f.files.ByHash("0") != "" {
		t.Fatal("by hash")
	}
	if second := must(f.files.Keep(sheet3Fixed, "x.pdf", now)); second.MsgID != 2 {
		t.Fatal(second.MsgID)
	}
	_, err := f.files.Keep([]byte("<html>"), "page.pdf", now)
	var phrase *i18n.Error
	if !errors.As(err, &phrase) || !strings.HasPrefix(phrase.Phrase.Ru, "это не PDF") {
		t.Fatal(err)
	}
	if _, err := f.files.Keep(make([]byte, UploadMaxSize+1), "big.pdf", now); err == nil ||
		err.Error() != "the file is over 20 MB" {
		t.Fatal(err)
	}

	broken := must(f.files.KeepAttachment(7, now, "task.pdf", []byte("%PDF-1.4")))
	if broken.Source != "mail" || broken.ChatID != inbox.Mailbox || broken.Error == nil || broken.Tries != MaxTries ||
		*broken.Size != 8 {
		t.Fatal(broken)
	}
	if r := must(f.files.KeepAttachment(8, now, "big.pdf", make([]byte, MaxSize+1))); r != nil {
		t.Fatal("too big")
	}
	if names := listDir(t, f.files.Directory); len(names) != 2 { // no .part left
		t.Fatal(names)
	}
}

func TestReadings(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.sendImage(1, jpeg, photo)
	f.sendImage(2, jpeg, photo)
	first := must(f.files.FetchImage(ctx, f.message(1)))
	second := must(f.files.FetchImage(ctx, f.message(2)))
	if Reading(first) != nil || must(f.files.ReadAs(second)) != nil {
		t.Fatal("read")
	}
	first = must(f.files.SetReading(first, `{"shows": "Notice", "text": ""}`, ""))
	if same := must(f.files.ReadAs(second)); same == nil || *same != *first.Reading {
		t.Fatal("the same image")
	}
	if r := Reading(first); r == nil || r.Shows != "Notice" {
		t.Fatal(r)
	}
	failed := must(f.files.SetReading(second, "", "bad image"))
	if Reading(failed) != nil || *failed.Error != "bad image" {
		t.Fatal(failed)
	}
}
