// Package library is the library (stage 14, D58): the raw material of the courses and the knowledge base made of it.
//
// Two stores, kept for good under data/library/:
//   - materials: what arrives raw. Photos of the board, audio, notes of the owner and classmates, slides, sheets, links
//     (remembered, never fetched) and texts. Each has a course (a name of the timetable, `course`) and a class of RUZ
//     (`class_uid`), or neither: then it waits in the inbox. Each is `new` until Claude has made it into the knowledge
//     base (`processed`). An item may hang on another one (`parent`): a transcript of a recording.
//   - knowledge: what Claude made of it, per course. A `book` (the whole course), a `lecture` or a `seminar` (one
//     class), and a `cheatsheet` (notation, formatting, the topics covered: for Claude itself). Over all courses, one
//     `guide`: the rules every document is written by (what goes where, the layout, the style; for Claude too). Every
//     version is kept: a version is a source (`.tex`, or Markdown for a cheat sheet and the guide) and a PDF. The server
//     never runs TeX: the PDF comes built.
//
// A file arrives in parts (an upload, a Telegram download) at a `.part` path and is moved into place once complete, so
// a half file is never listed. Files that arrive by an upload link (library/links) wait as a draft row until their
// bytes are in.
package library

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Schema is the library's tables (§1.3), run at every start.
const Schema = `
create table if not exists materials (  -- raw material of the courses (D58): kept for good
    id integer primary key,
    course text,                -- a name of the timetable (classes.subject) or as a source names it; null: the inbox
    class_uid text,             -- its RUZ class (classes.uid); null: the course as a whole, or not known
    day text,                   -- YYYY-MM-DD: of its class, else of its arrival
    kind text not null,         -- photo, audio, video, notes, slides, sheet, link, text, transcript, file
    title text not null default '',
    author text not null default '',  -- who wrote or sent it: "owner", a classmate's name, a chat's
    text text,                  -- a text's or a link's words; a transcript
    url text,                   -- a link: remembered, never fetched
    path text,                  -- its file under the data directory; null for a link or a text
    name text,                  -- the file's name, as it came
    media_type text,
    size integer,
    sha256 text,
    parent integer,             -- the item it belongs to: a transcript's recording
    source text not null,       -- chat, bot, miniapp, connector, sheet, web
    chat_id integer,            -- where it came from
    msg_id integer,
    status text not null,       -- uploading (a draft: no bytes yet), new, processed
    note text not null default '',  -- why it is in the inbox; what Claude made of it
    created_at text not null,
    processed_at text,
    removed_at text             -- removed by the owner or Claude: listed nowhere, the file goes after REMOVED_KEEP
);
create index if not exists materials_course on materials(course, day);
create index if not exists materials_origin on materials(source, chat_id, msg_id);
create index if not exists materials_sha256 on materials(sha256);
create table if not exists knowledge (  -- the knowledge base (D58): one document per course and class
    id integer primary key,
    course text not null,       -- '' for the guide, which is of no course
    kind text not null,         -- book, lecture, seminar, cheatsheet, guide
    class_uid text,             -- a lecture's or a seminar's RUZ class
    day text,                   -- YYYY-MM-DD of that class
    title text not null,
    created_at text not null
);
create index if not exists knowledge_course on knowledge(course, kind);
create table if not exists knowledge_versions (
    id integer primary key,
    doc_id integer not null,
    version integer,            -- 1, 2…; null for a draft whose files are still coming
    source_path text,           -- the .tex (Markdown of a cheat sheet)
    pdf_path text,
    note text not null default '',      -- what changed
    materials text not null default '[]',  -- JSON [material id] it was made from
    created_at text not null,
    published_at text
);
create index if not exists knowledge_doc on knowledge_versions(doc_id, version);
`

// Kinds of materials, their sources, the kinds of documents.
var (
	Kinds     = []string{"photo", "audio", "video", "notes", "slides", "sheet", "link", "text", "transcript", "file"}
	Sources   = []string{"chat", "bot", "miniapp", "connector", "sheet", "web"}
	DocKinds  = []string{"book", "lecture", "seminar", "cheatsheet", "guide"}
	ClassDocs = []string{"lecture", "seminar"} // a document of one class
)

// Guide is the kind of the one document over all courses: the rules the knowledge base is written by.
const Guide = "guide"

// TextOnly says a document of this kind is Markdown for Claude with no PDF: a cheat sheet, the guide.
func TextOnly(kind string) bool { return kind == "cheatsheet" || kind == Guide }

// Parts are a version's files and their suffixes; the source of a cheat sheet and of the guide is .md.
var Parts = map[string]string{"source": ".tex", "pdf": ".pdf"}

var partNames = []string{"source", "pdf"}

const (
	MaxFile     = 2 << 30   // bytes of one raw file (a lecture's video)
	MaxDocFile  = 64 << 20  // bytes of a version's file
	MaxText     = 1_000_000 // characters of a text item
	ClassBefore = 20 * time.Minute
	ClassAfter  = 7 * 24 * time.Hour  // an item belongs to the last class of its course this long ago
	DraftKeep   = 24 * time.Hour      // a draft whose files never came
	RemovedKeep = 30 * 24 * time.Hour // a removed item: undo brings it back until then
	Chunk       = 1 << 16
)

var mediaKinds = [][2]string{{"image/", "photo"}, {"audio/", "audio"}, {"video/", "video"}}

var slideTypes = []string{"application/vnd.ms-powerpoint", "application/vnd.openxmlformats-officedocument.presentationml"}

// Error is a request the library refuses: the text says why (Python's LibraryError, a ValueError).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func refuse(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// GuessKind is the kind of a file by its type and name: photo, audio, video, slides, or file.
func GuessKind(mediaType, name string) string {
	mediaType, name = strings.ToLower(mediaType), strings.ToLower(name)
	for _, mk := range mediaKinds {
		if strings.HasPrefix(mediaType, mk[0]) {
			return mk[1]
		}
	}
	for _, prefix := range slideTypes {
		if strings.HasPrefix(mediaType, prefix) {
			return "slides"
		}
	}
	for _, ext := range []string{".ppt", ".pptx", ".key", ".odp"} {
		if strings.HasSuffix(name, ext) {
			return "slides"
		}
	}
	return "file"
}

var unsafeChars = regexp.MustCompile(`[\x00-\x1f/\\]`)

// SafeName is a file name to keep and to serve: no directories, no control characters, at most 120 characters.
func SafeName(name, fallback string) string {
	name = strings.Trim(unsafeChars.ReplaceAllString(pytext.Strip(name), "_"), ". ")
	if name == "" {
		name = fallback
	}
	if pytext.Len(name) > 120 {
		i := strings.LastIndex(name, ".")
		if i >= 0 && pytext.Len(name[i+1:]) <= 9 {
			name = pytext.Cut(name[:i], 110) + "." + pytext.Cut(name[i+1:], 9)
		} else {
			name = pytext.Cut(name, 120)
		}
	}
	return name
}

// ClassKind is a class's kind of the timetable as a document's: lecture, seminar, or "" (an exam, a midterm).
func ClassKind(kind string) string {
	switch schedule.TypeOf(kind, true) {
	case "Lecture":
		return "lecture"
	case "Seminar", "Lab", "Class":
		return "seminar"
	}
	return ""
}

// suffix is Python's Path(name).suffix: ".gz" of "a.tar.gz", "" of ".bashrc" and of "a.".
func suffix(name string) string {
	name = strings.TrimRight(name, "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 || strings.Trim(name, ".") == "" {
		return ""
	}
	return name[i:]
}

// WriteParts writes an incoming stream to `target`: its size and sha256. An *Error past `limit` bytes, the part
// removed.
func WriteParts(chunks io.Reader, target string, limit int64) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, "", err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	digest, size := sha256.New(), int64(0)
	buf := make([]byte, Chunk)
	fail := func(err error) (int64, string, error) {
		out.Close()
		os.Remove(target)
		return 0, "", err
	}
	for {
		n, readErr := chunks.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > limit {
				return fail(refuse("the file is over %d MB", limit>>20))
			}
			digest.Write(buf[:n])
			if _, err := out.Write(buf[:n]); err != nil {
				return fail(err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fail(readErr)
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(target)
		return 0, "", err
	}
	os.Chmod(target, 0o600)
	return size, hex.EncodeToString(digest.Sum(nil)), nil
}

// FileHash is the sha256 of a file.
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.CopyBuffer(digest, f, make([]byte, Chunk*16)); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Material is a row of `materials`.
type Material struct {
	ID          int64   `json:"id"`
	Course      *string `json:"course"`
	ClassUID    *string `json:"class_uid"`
	Day         *string `json:"day"`
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Author      string  `json:"author"`
	Text        *string `json:"text"`
	URL         *string `json:"url"`
	Path        *string `json:"path"`
	Name        *string `json:"name"`
	MediaType   *string `json:"media_type"`
	Size        *int64  `json:"size"`
	Sha256      *string `json:"sha256"`
	Parent      *int64  `json:"parent"`
	Source      string  `json:"source"`
	ChatID      *int64  `json:"chat_id"`
	MsgID       *int64  `json:"msg_id"`
	Status      string  `json:"status"`
	Note        string  `json:"note"`
	CreatedAt   string  `json:"created_at"`
	ProcessedAt *string `json:"processed_at"`
	RemovedAt   *string `json:"removed_at"`
}

const materialColumns = "id, course, class_uid, day, kind, title, author, text, url, path, name, media_type, size, " +
	"sha256, parent, source, chat_id, msg_id, status, note, created_at, processed_at, removed_at"

func scanMaterial(scan func(...any) error) (*Material, error) {
	var m Material
	err := scan(&m.ID, &m.Course, &m.ClassUID, &m.Day, &m.Kind, &m.Title, &m.Author, &m.Text, &m.URL, &m.Path, &m.Name,
		&m.MediaType, &m.Size, &m.Sha256, &m.Parent, &m.Source, &m.ChatID, &m.MsgID, &m.Status, &m.Note, &m.CreatedAt,
		&m.ProcessedAt, &m.RemovedAt)
	return &m, err
}

// Library is the two stores.
type Library struct {
	Store   *store.Store
	db      *sql.DB
	DataDir string
	Root    string
	Now     func() time.Time
	// SameCourse tells two names of one course (a chat's «Матан» and RUZ's «Математический анализ»):
	// Schedule.SameCourse when the service runs; the plain name otherwise.
	SameCourse func(a, b string) bool
}

// New opens the library under dataDir and makes its tables.
func New(s *store.Store, dataDir string) (*Library, error) {
	if _, err := s.DB.Exec(Schema); err != nil {
		return nil, fmt.Errorf("library schema: %w", err)
	}
	return &Library{Store: s, db: s.DB, DataDir: dataDir, Root: filepath.Join(dataDir, "library"), Now: clock.NowUTC,
		SameCourse: func(a, b string) bool {
			return a != "" && b != "" && pytext.Casefold(a) == pytext.Casefold(b)
		}}, nil
}

func strs(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, rows.Err()
}

// --- courses and classes -------------------------------------------------------------------

// Course is the timetable's name of a course, if it has one; the name as given otherwise; "" for none. A course RUZ
// renamed (its feed went from Russian to English) has several names in `classes`: the newest one wins.
func (l *Library) Course(name string) (string, error) {
	name = pytext.Strip(name)
	if name == "" {
		return "", nil
	}
	subjects, err := strs(l.db, "select subject from classes group by subject order by max(start) desc")
	if err != nil {
		return "", err
	}
	for _, subject := range subjects {
		if subject == name || l.SameCourse(name, subject) {
			return subject, nil
		}
	}
	return name, nil
}

// Names are every name of the course in the timetable: its classes under an old name are its classes too.
func (l *Library) Names(course string) ([]string, error) {
	course, err := l.Course(course)
	if err != nil || course == "" {
		return nil, err
	}
	subjects, err := strs(l.db, "select distinct subject from classes")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, subject := range subjects {
		of, err := l.Course(subject)
		if err != nil {
			return nil, err
		}
		if of == course {
			names = append(names, subject)
		}
	}
	if len(names) == 0 {
		names = []string{course}
	}
	return names, nil
}

// CourseInfo is a course with the counts of its material and documents.
type CourseInfo struct {
	Course    string `json:"course"`
	Items     int64  `json:"items"`
	New       int64  `json:"new"`
	Documents int64  `json:"documents"`
}

type counts struct{ items, fresh int64 }

func countsBy(q *sql.DB, query string, args ...any) (map[string]counts, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]counts{}
	for rows.Next() {
		var key *string
		var items int64
		var fresh *int64
		if err := rows.Scan(&key, &items, &fresh); err != nil {
			return nil, err
		}
		c := counts{items: items}
		if fresh != nil {
			c.fresh = *fresh
		}
		k := ""
		if key != nil {
			k = *key
		}
		out[k] = c
	}
	return out, rows.Err()
}

// Courses are every course of the timetable and of the library, with the counts of its material and documents.
func (l *Library) Courses() ([]CourseInfo, error) {
	names := map[string]bool{}
	subjects, err := strs(l.db, "select distinct subject from classes where gone_at is null")
	if err != nil {
		return nil, err
	}
	for _, subject := range subjects {
		course, err := l.Course(subject)
		if err != nil {
			return nil, err
		}
		if course != "" {
			names[course] = true
		}
	}
	for _, query := range []string{
		"select distinct course from materials where course is not null and removed_at is null",
		"select distinct course from knowledge where course != ''"} {
		more, err := strs(l.db, query)
		if err != nil {
			return nil, err
		}
		for _, name := range more {
			names[name] = true
		}
	}
	items, err := countsBy(l.db, "select course, count(*) items, sum(status = 'new') new from materials "+
		"where removed_at is null and status != 'uploading' group by course")
	if err != nil {
		return nil, err
	}
	docs, err := countsBy(l.db, "select k.course, count(*) n, 0 from knowledge k where exists "+
		"(select 1 from knowledge_versions v where v.doc_id = k.id and v.version is not null) group by k.course")
	if err != nil {
		return nil, err
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Slice(sorted, func(i, j int) bool {
		a, b := pytext.Casefold(sorted[i]), pytext.Casefold(sorted[j])
		if a != b {
			return a < b
		}
		return sorted[i] < sorted[j]
	})
	out := make([]CourseInfo, 0, len(sorted))
	for _, name := range sorted {
		out = append(out, CourseInfo{Course: name, Items: items[name].items, New: items[name].fresh,
			Documents: docs[name].items})
	}
	return out, nil
}

// InboxCount is the items waiting in the inbox.
func (l *Library) InboxCount() (int64, error) {
	var n int64
	err := l.db.QueryRow("select count(*) n from materials where course is null and removed_at is null " +
		"and status != 'uploading'").Scan(&n)
	return n, err
}

// ClassInfo is a class of a course with its kind and its counts of material.
type ClassInfo struct {
	UID       string `json:"uid"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Kind      string `json:"kind"`
	RuzKind   string `json:"ruz_kind"`
	Cancelled bool   `json:"cancelled"`
	Items     int64  `json:"items"`
	New       int64  `json:"new"`
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

func anys(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Classes are the course's classes of RUZ that have started (by `until`, now when zero), the last first, each with
// its kind (lecture, seminar or "") and its counts of material.
func (l *Library) Classes(course string, until time.Time) ([]ClassInfo, error) {
	if until.IsZero() {
		until = l.Now()
	}
	names, err := l.Names(course)
	if err != nil {
		return nil, err
	}
	if course, err = l.Course(course); err != nil {
		return nil, err
	}
	rows, err := l.db.Query("select uid, start, end, kind, cancelled from classes where subject in ("+
		marks(len(names))+") and gone_at is null and start <= ? order by start desc",
		append(anys(names), clock.LocalISO(until))...)
	if err != nil {
		return nil, err
	}
	var found []ClassInfo
	for rows.Next() {
		var c ClassInfo
		if err := rows.Scan(&c.UID, &c.Start, &c.End, &c.RuzKind, &c.Cancelled); err != nil {
			rows.Close()
			return nil, err
		}
		c.Kind = ClassKind(c.RuzKind)
		found = append(found, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byClass, err := countsBy(l.db, "select class_uid, count(*) items, sum(status = 'new') new from materials "+
		"where course = ? and removed_at is null and status != 'uploading' and class_uid is not null "+
		"group by class_uid", course)
	if err != nil {
		return nil, err
	}
	for i := range found {
		found[i].Items, found[i].New = byClass[found[i].UID].items, byClass[found[i].UID].fresh
	}
	if found == nil {
		found = []ClassInfo{}
	}
	return found, nil
}

// ClassRef is a class as the library names it: its subject is the course's timetable name.
type ClassRef struct {
	UID     string `json:"uid"`
	Subject string `json:"subject"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Kind    string `json:"kind"`
	RuzKind string `json:"ruz_kind"`
}

// ClassOf is a class by its uid; nil for none.
func (l *Library) ClassOf(uid string) (*ClassRef, error) {
	if uid == "" {
		return nil, nil
	}
	var c ClassRef
	err := l.db.QueryRow("select uid, subject, start, end, kind from classes where uid = ?", uid).
		Scan(&c.UID, &c.Subject, &c.Start, &c.End, &c.RuzKind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.Subject, err = l.Course(c.Subject); err != nil {
		return nil, err
	}
	c.Kind = ClassKind(c.RuzKind)
	return &c, nil
}

// FindClass is the class an item sent at `at` belongs to: the course's last class that started by a little after
// `at`, within a week. With `seminar`, the course's seminar nearest to `at` either way, within a week: a sheet comes
// the night before its seminar or a day after it. Nil for an unknown course or a course without classes then.
func (l *Library) FindClass(course string, at time.Time, seminar bool) (*ClassRef, error) {
	names, err := l.Names(course)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	if seminar {
		rows, err := l.db.Query("select uid, start, kind from classes where subject in ("+marks(len(names))+") and "+
			"gone_at is null and cancelled = 0 and start <= ? and start >= ? and length(start) > 10",
			append(anys(names), clock.LocalISO(at.Add(ClassAfter)), clock.LocalISO(at.Add(-ClassAfter)))...)
		if err != nil {
			return nil, err
		}
		nearest, best := "", time.Duration(-1)
		for rows.Next() {
			var uid, start, kind string
			if err := rows.Scan(&uid, &start, &kind); err != nil {
				rows.Close()
				return nil, err
			}
			if ClassKind(kind) != "seminar" {
				continue
			}
			moment, err := clock.ParseLocal(start)
			if err != nil {
				continue
			}
			distance := moment.Sub(at)
			if distance < 0 {
				distance = -distance
			}
			if best < 0 || distance < best {
				nearest, best = uid, distance
			}
		}
		rows.Close()
		if nearest != "" {
			return l.ClassOf(nearest)
		}
	}
	var uid string
	err = l.db.QueryRow("select uid from classes where subject in ("+marks(len(names))+") and gone_at is null and "+
		"cancelled = 0 and start <= ? and start >= ? and length(start) > 10 order by start desc limit 1",
		append(anys(names), clock.LocalISO(at.Add(ClassBefore)), clock.LocalISO(at.Add(-ClassAfter)))...).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l.ClassOf(uid)
}

// --- materials -----------------------------------------------------------------------------

// place is (course, class_uid, day) of an item: the class must be of the course; a class without a course gives it.
func (l *Library) place(course, classUID string, at time.Time) (*string, *string, string, error) {
	course, err := l.Course(course)
	if err != nil {
		return nil, nil, "", err
	}
	found, err := l.ClassOf(classUID)
	if err != nil {
		return nil, nil, "", err
	}
	if classUID != "" && found == nil {
		return nil, nil, "", refuse("no class %s in the timetable", classUID)
	}
	if found != nil {
		if course != "" && course != found.Subject {
			return nil, nil, "", refuse("class %s is of %s, not %s", classUID, found.Subject, course)
		}
		return &found.Subject, &found.UID, pytext.Cut(found.Start, 10), nil
	}
	var coursePtr *string
	if course != "" {
		coursePtr = &course
	}
	return coursePtr, nil, clock.LocalDay(at).Format(clock.DateLayout), nil
}

// File is a file already in place, as an item keeps it; "" and nil are null.
type File struct {
	Path      string
	Name      string
	MediaType string
	Size      *int64
	Sha256    string
}

// Item is a new item of Add: Status "" is "new", At zero is now.
type Item struct {
	Source   string
	Course   string
	ClassUID string
	Title    string
	Author   string
	Text     *string
	URL      *string
	ChatID   *int64
	MsgID    *int64
	Parent   *int64
	Note     string
	At       time.Time
	Status   string
	File     File
}

func null(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// Add makes a new item; its id.
func (l *Library) Add(kind string, item Item) (int64, error) {
	if !slices.Contains(Kinds, kind) {
		return 0, refuse("kind is one of %s", strings.Join(Kinds, ", "))
	}
	if !slices.Contains(Sources, item.Source) {
		return 0, refuse("source is one of %s", strings.Join(Sources, ", "))
	}
	if item.Text != nil && pytext.Len(*item.Text) > MaxText {
		return 0, refuse("the text is over %d characters", MaxText)
	}
	if kind == "link" && (item.URL == nil || *item.URL == "") {
		return 0, refuse("a link needs its url")
	}
	if item.Parent != nil {
		parent, err := l.Get(*item.Parent, false, false)
		if err != nil {
			return 0, err
		}
		if parent == nil {
			return 0, refuse("no item %d", *item.Parent)
		}
	}
	at := item.At
	if at.IsZero() {
		at = l.Now()
	}
	status := item.Status
	if status == "" {
		status = "new"
	}
	course, classUID, day, err := l.place(item.Course, item.ClassUID, at)
	if err != nil {
		return 0, err
	}
	result, err := l.db.Exec("insert into materials(course, class_uid, day, kind, title, author, text, url, path, name, "+
		"media_type, size, sha256, parent, source, chat_id, msg_id, status, note, created_at) "+
		"values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		course, classUID, day, kind, pytext.Strip(item.Title), pytext.Strip(item.Author), item.Text, item.URL,
		null(item.File.Path), null(item.File.Name), null(item.File.MediaType), item.File.Size, null(item.File.Sha256),
		item.Parent, item.Source, item.ChatID, item.MsgID, status, item.Note, clock.LocalISO(at))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// FromOrigin is the item that came from this message already, if any: a message is taken once. With `sha256`
// (non-empty), only the item of that file: a message whose file was replaced (a sheet's new version) is taken again.
func (l *Library) FromOrigin(source string, chatID, msgID int64, sha string) (*Material, error) {
	query, args := "select "+materialColumns+" from materials where source = ? and chat_id = ? and msg_id = ? "+
		"and removed_at is null", []any{source, chatID, msgID}
	if sha != "" {
		query += " and sha256 = ?"
		args = append(args, sha)
	}
	m, err := scanMaterial(l.db.QueryRow(query, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (l *Library) newPart() (string, error) {
	directory := filepath.Join(l.Root, "raw")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(directory, hex.EncodeToString(b[:])+".part"), nil
}

func (l *Library) relative(path string) (string, error) {
	rel, err := filepath.Rel(l.DataDir, path)
	return filepath.ToSlash(rel), err
}

// keepFile moves a complete part under data/library/raw/<sha256[:2]>/<sha256>.<ext>; the same bytes are kept once.
func (l *Library) keepFile(part, name string, size int64, sha string) (File, error) {
	ext := pytext.Cut(strings.ToLower(suffix(name)), 10)
	target := filepath.Join(l.Root, "raw", sha[:2], sha+ext)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return File{}, err
	}
	if _, err := os.Stat(target); err == nil {
		os.Remove(part)
	} else {
		if err := os.Rename(part, target); err != nil {
			return File{}, err
		}
		os.Chmod(target, 0o600)
	}
	rel, err := l.relative(target)
	return File{Path: rel, Size: &size, Sha256: sha}, err
}

func orDefault(text, fallback string) string {
	if text == "" {
		return fallback
	}
	return text
}

// AddStream makes a new item from a stream of bytes (an upload); its id. Kind "" is guessed.
func (l *Library) AddStream(chunks io.Reader, name, mediaType, kind string, item Item) (int64, error) {
	part, err := l.newPart()
	if err != nil {
		return 0, err
	}
	size, sha, err := WriteParts(chunks, part, MaxFile)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		os.Remove(part)
		return 0, refuse("the file is empty")
	}
	file, err := l.keepFile(part, name, size, sha)
	if err != nil {
		return 0, err
	}
	file.Name, file.MediaType = SafeName(name, "file-"+sha[:8]), orDefault(mediaType, "application/octet-stream")
	item.File = file
	return l.Add(orDefault(kind, GuessKind(mediaType, name)), item)
}

// AddDownload makes a new item from a download into a path (Telegram): `download(path)` reports false when the file
// is gone, and the item is not made then (nil). Errors of the download pass through.
func (l *Library) AddDownload(download func(path string) (bool, error), name, mediaType, kind string, item Item) (
	*int64, error) {
	part, err := l.newPart()
	if err != nil {
		return nil, err
	}
	defer os.Remove(part)
	ok, err := download(part)
	if err != nil {
		return nil, err
	}
	info, statErr := os.Stat(part)
	if !ok || statErr != nil {
		return nil, nil
	}
	if info.Size() > MaxFile {
		return nil, refuse("the file is over %d MB", MaxFile>>20)
	}
	sha, err := FileHash(part)
	if err != nil {
		return nil, err
	}
	file, err := l.keepFile(part, name, info.Size(), sha)
	if err != nil {
		return nil, err
	}
	file.Name, file.MediaType = SafeName(name, "file-"+sha[:8]), orDefault(mediaType, "application/octet-stream")
	item.File = file
	id, err := l.Add(orDefault(kind, GuessKind(mediaType, name)), item)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// Fill puts in the bytes of a draft item (made with status "uploading"): it becomes new. An *Error for no draft.
func (l *Library) Fill(itemID int64, chunks io.Reader) (*Material, error) {
	row, err := l.Get(itemID, true, false)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Status != "uploading" {
		return nil, refuse("this upload is used already or gone")
	}
	part, err := l.newPart()
	if err != nil {
		return nil, err
	}
	size, sha, err := WriteParts(chunks, part, MaxFile)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		os.Remove(part)
		return nil, refuse("the file is empty")
	}
	name := ""
	if row.Name != nil {
		name = *row.Name
	}
	file, err := l.keepFile(part, name, size, sha)
	if err != nil {
		return nil, err
	}
	if _, err := l.db.Exec("update materials set path = ?, size = ?, sha256 = ?, status = 'new' where id = ?",
		file.Path, size, sha, itemID); err != nil {
		return nil, err
	}
	return l.Get(itemID, false, false)
}

// Get is one item: nil for none, for a removed one (unless `removed`) and for a draft (unless `drafts`).
func (l *Library) Get(itemID int64, drafts, removed bool) (*Material, error) {
	m, err := scanMaterial(l.db.QueryRow("select "+materialColumns+" from materials where id = ?", itemID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.RemovedAt != nil && !removed || m.Status == "uploading" && !drafts {
		return nil, nil
	}
	return m, nil
}

// Path is where an item's file is; "" for a link, a text, a draft or a lost file.
func (l *Library) Path(row *Material) string {
	if row == nil || row.Path == nil || *row.Path == "" {
		return ""
	}
	path := filepath.Join(l.DataDir, filepath.FromSlash(*row.Path))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// Query picks items: of the inbox, of a course (and a class), or all; Status new or processed; "" and nil for any.
type Query struct {
	Course   *string
	Inbox    bool
	ClassUID *string
	Status   string
	Kind     string
	Limit    int // 0: 200
}

// Items are the items, the newest first.
func (l *Library) Items(q Query) ([]*Material, error) {
	where, values := []string{"removed_at is null", "status != 'uploading'"}, []any{}
	if q.Inbox {
		where = append(where, "course is null")
	} else if q.Course != nil {
		course, err := l.Course(*q.Course)
		if err != nil {
			return nil, err
		}
		where = append(where, "course = ?")
		values = append(values, null(course))
	}
	if q.ClassUID != nil {
		where = append(where, "class_uid = ?")
		values = append(values, *q.ClassUID)
	}
	if q.Status != "" {
		where = append(where, "status = ?")
		values = append(values, q.Status)
	}
	if q.Kind != "" {
		where = append(where, "kind = ?")
		values = append(values, q.Kind)
	}
	limit := q.Limit
	if limit == 0 {
		limit = 200
	}
	return l.materials("select "+materialColumns+" from materials where "+strings.Join(where, " and ")+
		" order by day desc, id desc limit ?", append(values, limit)...)
}

func (l *Library) materials(query string, args ...any) ([]*Material, error) {
	rows, err := l.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Material{}
	for rows.Next() {
		m, err := scanMaterial(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Children are the items that hang on one: its transcripts.
func (l *Library) Children(itemID int64) ([]*Material, error) {
	return l.materials("select "+materialColumns+" from materials where parent = ? and removed_at is null and "+
		"status != 'uploading' order by id", itemID)
}

func (m *Material) field(name string) (any, bool) {
	ptr := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	switch name {
	case "course":
		return ptr(m.Course), true
	case "class_uid":
		return ptr(m.ClassUID), true
	case "day":
		return ptr(m.Day), true
	case "kind":
		return m.Kind, true
	case "title":
		return m.Title, true
	case "author":
		return m.Author, true
	case "text":
		return ptr(m.Text), true
	case "note":
		return m.Note, true
	case "status":
		return m.Status, true
	case "processed_at":
		return ptr(m.ProcessedAt), true
	case "url":
		return ptr(m.URL), true
	case "name":
		return ptr(m.Name), true
	case "source":
		return m.Source, true
	}
	return nil, false
}

func asText(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// Update changes an item: course and class_uid (sorting the inbox: a course of nil puts it back), title, author,
// kind, text, note, status (new or processed). The fields are as the caller names them (values: a string or nil). It
// returns the old values, for an undo.
func (l *Library) Update(itemID int64, fields map[string]any) (map[string]any, error) {
	row, err := l.Get(itemID, false, false)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse("no item %d", itemID)
	}
	old := map[string]any{}
	for name := range fields {
		if value, ok := row.field(name); ok {
			old[name] = value
		}
	}
	values := map[string]any{}
	for name, value := range fields {
		values[name] = value
	}
	_, hasCourse := fields["course"]
	_, hasClass := fields["class_uid"]
	if hasCourse || hasClass {
		course := ""
		if hasCourse {
			course = asText(fields["course"])
		} else if row.Course != nil {
			course = *row.Course
		}
		classUID := ""
		if hasClass {
			classUID = asText(fields["class_uid"])
		} else if !hasCourse && row.ClassUID != nil {
			classUID = *row.ClassUID
		}
		at, err := time.ParseInLocation(clock.LocalLayout, row.CreatedAt, clock.TZ)
		if err != nil {
			if at, err = clock.ParseLocal(row.CreatedAt); err != nil {
				return nil, err
			}
		}
		placedCourse, placedClass, day, err := l.place(course, classUID, at)
		if err != nil {
			return nil, err
		}
		values["course"], values["class_uid"], values["day"] = deref(placedCourse), deref(placedClass), day
		if _, ok := old["course"]; !ok {
			old["course"], _ = row.field("course")
		}
		if _, ok := old["class_uid"]; !ok {
			old["class_uid"], _ = row.field("class_uid")
		}
	}
	if kind, ok := values["kind"]; ok && !slices.Contains(Kinds, asText(kind)) {
		return nil, refuse("kind is one of %s", strings.Join(Kinds, ", "))
	}
	if status, ok := values["status"]; ok {
		switch asText(status) {
		case "new":
			values["processed_at"] = nil
		case "processed":
			values["processed_at"] = clock.LocalISO(l.Now())
		default:
			return nil, refuse("status is new or processed")
		}
	}
	allowed := []string{"course", "class_uid", "day", "title", "author", "kind", "text", "note", "status",
		"processed_at"}
	var unknown, names []string
	for name := range values {
		if !slices.Contains(allowed, name) {
			unknown = append(unknown, name)
		}
		names = append(names, name)
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, refuse("cannot change %s", strings.Join(unknown, ", "))
	}
	slices.Sort(names)
	var sets []string
	var args []any
	for _, name := range names {
		sets = append(sets, name+" = ?")
		args = append(args, values[name])
	}
	if _, err := l.db.Exec("update materials set "+strings.Join(sets, ", ")+" where id = ?",
		append(args, itemID)...); err != nil {
		return nil, err
	}
	return old, nil
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Remove takes an item out of every list; the file stays RemovedKeep for an undo (Restore). Its transcripts go with
// it. A draft is removed too: its upload link then takes nothing.
func (l *Library) Remove(itemID int64) error {
	row, err := l.Get(itemID, true, false)
	if err != nil {
		return err
	}
	if row == nil {
		return refuse("no item %d", itemID)
	}
	_, err = l.db.Exec("update materials set removed_at = ? where (id = ? or parent = ?) and removed_at is null",
		clock.LocalISO(l.Now()), itemID, itemID)
	return err
}

// Restore brings back a removed item and the transcripts removed with it.
func (l *Library) Restore(itemID int64) error {
	row, err := l.Get(itemID, false, true)
	if err != nil {
		return err
	}
	if row == nil || row.RemovedAt == nil {
		return refuse("item %d is not removed", itemID)
	}
	if row.Path != nil && *row.Path != "" && l.Path(row) == "" {
		return refuse("its file is gone already")
	}
	_, err = l.db.Exec("update materials set removed_at = null where (id = ? or parent = ?) and removed_at = ?",
		itemID, itemID, *row.RemovedAt)
	return err
}

// --- the knowledge base --------------------------------------------------------------------

// Document is a row of `knowledge`.
type Document struct {
	ID        int64   `json:"id"`
	Course    string  `json:"course"`
	Kind      string  `json:"kind"`
	ClassUID  *string `json:"class_uid"`
	Day       *string `json:"day"`
	Title     string  `json:"title"`
	CreatedAt string  `json:"created_at"`
}

// DocumentSummary is a document with the number of its newest version (nil while only a draft exists).
type DocumentSummary struct {
	Document
	Version *int64 `json:"version"`
}

// VersionRow is a row of `knowledge_versions`.
type VersionRow struct {
	ID          int64   `json:"id"`
	DocID       int64   `json:"doc_id"`
	Version     *int64  `json:"version"`
	SourcePath  *string `json:"source_path"`
	PdfPath     *string `json:"pdf_path"`
	Note        string  `json:"note"`
	Materials   string  `json:"materials"`
	CreatedAt   string  `json:"created_at"`
	PublishedAt *string `json:"published_at"`
}

// Version is a version with its document's course, kind, title and class.
type Version struct {
	VersionRow
	Course   string  `json:"course"`
	Kind     string  `json:"kind"`
	Title    string  `json:"title"`
	ClassUID *string `json:"class_uid"`
}

// DocumentVersions is a document with its published versions, the newest first.
type DocumentVersions struct {
	Document
	Versions []VersionRow `json:"versions"`
}

const documentColumns = "k.id, k.course, k.kind, k.class_uid, k.day, k.title, k.created_at"
const versionColumns = "v.id, v.doc_id, v.version, v.source_path, v.pdf_path, v.note, v.materials, v.created_at, " +
	"v.published_at"

func scanVersionRow(r *VersionRow) []any {
	return []any{&r.ID, &r.DocID, &r.Version, &r.SourcePath, &r.PdfPath, &r.Note, &r.Materials, &r.CreatedAt,
		&r.PublishedAt}
}

// Documents are the documents with their newest version, by course, kind and day; course nil for all.
func (l *Library) Documents(course *string) ([]DocumentSummary, error) {
	where, values := "", []any{}
	if course != nil {
		name, err := l.Course(*course)
		if err != nil {
			return nil, err
		}
		where, values = "where k.course = ?", []any{null(name)}
	}
	rows, err := l.db.Query("select "+documentColumns+", (select max(version) from knowledge_versions v where "+
		"v.doc_id = k.id) version from knowledge k "+where+" order by k.course, k.kind, k.day desc, k.id", values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocumentSummary{}
	for rows.Next() {
		var d DocumentSummary
		if err := rows.Scan(&d.ID, &d.Course, &d.Kind, &d.ClassUID, &d.Day, &d.Title, &d.CreatedAt,
			&d.Version); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDocument is a document with its published versions; nil for none.
func (l *Library) GetDocument(docID int64) (*DocumentVersions, error) {
	var d DocumentVersions
	err := l.db.QueryRow("select "+documentColumns+" from knowledge k where k.id = ?", docID).
		Scan(&d.ID, &d.Course, &d.Kind, &d.ClassUID, &d.Day, &d.Title, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := l.db.Query("select "+versionColumns+" from knowledge_versions v where v.doc_id = ? and "+
		"v.version is not null order by v.version desc", docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Versions = []VersionRow{}
	for rows.Next() {
		var v VersionRow
		if err := rows.Scan(scanVersionRow(&v)...); err != nil {
			return nil, err
		}
		d.Versions = append(d.Versions, v)
	}
	return &d, rows.Err()
}

// FindDocument is a course's book or cheat sheet, a class's lecture or seminar, or the guide (its course is
// ignored); nil for none.
func (l *Library) FindDocument(course, kind, classUID string) (*DocumentVersions, error) {
	name, err := l.Course(course)
	if err != nil {
		return nil, err
	}
	of := null(name)
	if kind == Guide {
		of = ""
	}
	var id int64
	err = l.db.QueryRow("select id from knowledge where course = ? and kind = ? and coalesce(class_uid, '') = ?",
		of, kind, classUID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l.GetDocument(id)
}

// GetVersion is a version with its document's fields; nil for none.
func (l *Library) GetVersion(versionID int64) (*Version, error) {
	var v Version
	err := l.db.QueryRow("select "+versionColumns+", k.course, k.kind, k.title, k.class_uid from knowledge_versions v "+
		"join knowledge k on k.id = v.doc_id where v.id = ?", versionID).
		Scan(append(scanVersionRow(&v.VersionRow), &v.Course, &v.Kind, &v.Title, &v.ClassUID)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Draft makes a new version of a document as a draft: its files come next (Put), then Publish. The document is made
// with it if it is new. The draft's row.
func (l *Library) Draft(course, kind, classUID, title, note string, materials []int64) (*Version, error) {
	if !slices.Contains(DocKinds, kind) {
		return nil, refuse("kind is one of %s", strings.Join(DocKinds, ", "))
	}
	course, err := l.Course(course)
	if err != nil {
		return nil, err
	}
	if kind == Guide && course != "" {
		return nil, refuse("the guide is of the whole knowledge base: no course")
	}
	if course == "" && kind != Guide {
		return nil, refuse("a document needs its course")
	}
	var day *string
	if slices.Contains(ClassDocs, kind) {
		found, err := l.ClassOf(classUID)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, refuse("a %s needs class_uid, a class of the timetable", kind)
		}
		if found.Subject != course {
			return nil, refuse("class %s is of %s, not %s", classUID, found.Subject, course)
		}
		d := pytext.Cut(found.Start, 10)
		day = &d
	} else if classUID != "" {
		return nil, refuse("a %s is of the whole course: no class_uid", kind)
	}
	for _, id := range materials {
		item, err := l.Get(id, false, false)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, refuse("no item %d", id)
		}
	}
	now := clock.LocalISO(l.Now())
	found, err := l.FindDocument(course, kind, classUID)
	if err != nil {
		return nil, err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var docID int64
	title = pytext.Strip(title)
	if found == nil {
		fallback := map[string]string{"book": course, "cheatsheet": course + ": notes for Claude",
			Guide: "Knowledge base guide"}[kind]
		if fallback == "" {
			fallback = fmt.Sprintf("%s: %s %s", course, kind, deref(day))
		}
		result, err := tx.Exec("insert into knowledge(course, kind, class_uid, day, title, created_at) "+
			"values(?, ?, ?, ?, ?, ?)", course, kind, null(classUID), day, orDefault(title, fallback), now)
		if err != nil {
			return nil, err
		}
		if docID, err = result.LastInsertId(); err != nil {
			return nil, err
		}
	} else {
		docID = found.ID
		if title != "" {
			if _, err := tx.Exec("update knowledge set title = ? where id = ?", title, docID); err != nil {
				return nil, err
			}
		}
	}
	result, err := tx.Exec("insert into knowledge_versions(doc_id, note, materials, created_at) values(?, ?, ?, ?)",
		docID, pytext.Strip(note), jsonIDs(materials), now)
	if err != nil {
		return nil, err
	}
	versionID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.GetVersion(versionID)
}

func (l *Library) partTarget(v *Version, part string) string {
	ext := Parts[part]
	if part == "source" && TextOnly(v.Kind) {
		ext = ".md"
	}
	return filepath.Join(l.Root, "knowledge", fmt.Sprint(v.DocID), fmt.Sprintf("%d%s", v.ID, ext))
}

// Put writes a draft's source or PDF; again replaces it. An *Error for a published version or an unknown part.
func (l *Library) Put(versionID int64, part string, chunks io.Reader) (*Version, error) {
	if _, ok := Parts[part]; !ok {
		return nil, refuse("the part is source or pdf")
	}
	v, err := l.GetVersion(versionID)
	if err != nil {
		return nil, err
	}
	if v == nil || v.Version != nil {
		return nil, refuse("this draft is published already or gone")
	}
	target := l.partTarget(v, part)
	partPath := target + ".part"
	size, _, err := WriteParts(chunks, partPath, MaxDocFile)
	if err != nil {
		return nil, err
	}
	if part == "pdf" {
		head := make([]byte, 5)
		f, err := os.Open(partPath)
		if err != nil {
			return nil, err
		}
		n, _ := io.ReadFull(f, head)
		f.Close()
		if string(head[:n]) != "%PDF-" {
			os.Remove(partPath)
			return nil, refuse("this is no PDF")
		}
	} else if size == 0 {
		os.Remove(partPath)
		return nil, refuse("the source is empty")
	}
	if err := os.Rename(partPath, target); err != nil {
		return nil, err
	}
	rel, err := l.relative(target)
	if err != nil {
		return nil, err
	}
	if _, err := l.db.Exec("update knowledge_versions set "+part+"_path = ? where id = ?", rel, versionID); err != nil {
		return nil, err
	}
	return l.GetVersion(versionID)
}

// PutText is Put of a text.
func (l *Library) PutText(versionID int64, part, text string) (*Version, error) {
	return l.Put(versionID, part, strings.NewReader(text))
}

// Publish makes a draft the document's newest version. A lecture, a seminar and a book need the source and the PDF;
// a cheat sheet and the guide their source. The materials it was made from become processed.
func (l *Library) Publish(versionID int64) (*Version, error) {
	v, err := l.GetVersion(versionID)
	if err != nil {
		return nil, err
	}
	if v == nil || v.Version != nil {
		return nil, refuse("this draft is published already or gone")
	}
	if v.SourcePath == nil || *v.SourcePath == "" {
		return nil, refuse("the source is not uploaded yet")
	}
	if !TextOnly(v.Kind) && (v.PdfPath == nil || *v.PdfPath == "") {
		return nil, refuse("the PDF is not uploaded yet")
	}
	now := clock.LocalISO(l.Now())
	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var number int64
	if err := tx.QueryRow("select coalesce(max(version), 0) + 1 n from knowledge_versions where doc_id = ?",
		v.DocID).Scan(&number); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("update knowledge_versions set version = ?, published_at = ? where id = ?", number, now,
		versionID); err != nil {
		return nil, err
	}
	if ids := parseIDs(v.Materials); len(ids) > 0 {
		args := []any{now}
		for _, id := range ids {
			args = append(args, id)
		}
		if _, err := tx.Exec("update materials set status = 'processed', processed_at = ? where status = 'new' "+
			"and id in ("+marks(len(ids))+")", args...); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.GetVersion(versionID)
}

// Unpublish takes back the newest version of a document (an undo): its files go, the one before is the newest again.
func (l *Library) Unpublish(versionID int64) error {
	v, err := l.GetVersion(versionID)
	if err != nil {
		return err
	}
	if v == nil || v.Version == nil {
		return refuse("no such version")
	}
	var newest *int64
	if err := l.db.QueryRow("select max(version) n from knowledge_versions where doc_id = ?", v.DocID).
		Scan(&newest); err != nil {
		return err
	}
	if newest == nil || *v.Version != *newest {
		return refuse("a newer version came after it")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("delete from knowledge_versions where id = ?", versionID); err != nil {
		return err
	}
	var one int
	err = tx.QueryRow("select 1 from knowledge_versions where doc_id = ?", v.DocID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.Exec("delete from knowledge where id = ?", v.DocID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, path := range []*string{v.SourcePath, v.PdfPath} {
		if path != nil && *path != "" {
			os.Remove(filepath.Join(l.DataDir, filepath.FromSlash(*path)))
		}
	}
	return nil
}

// PartPath is where a version's part is; "" for none.
func (l *Library) PartPath(v *Version, part string) string {
	if v == nil {
		return ""
	}
	var relative *string
	switch part {
	case "source":
		relative = v.SourcePath
	case "pdf":
		relative = v.PdfPath
	}
	if relative == nil || *relative == "" {
		return ""
	}
	path := filepath.Join(l.DataDir, filepath.FromSlash(*relative))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// ReadSource is a version's source text (invalid UTF-8 replaced); nil for none.
func (l *Library) ReadSource(v *Version) (*string, error) {
	path := l.PartPath(v, "source")
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ToValidUTF8(string(data), "�")
	return &text, nil
}

// --- upkeep ---------------------------------------------------------------------------------

// Prune removes drafts whose files never came, removed items past RemovedKeep, and every file no row has. How many
// rows went.
func (l *Library) Prune() (int64, error) {
	now := l.Now()
	stale, gone := clock.LocalISO(now.Add(-DraftKeep)), clock.LocalISO(now.Add(-RemovedKeep))
	tx, err := l.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var removed int64
	for _, step := range []struct {
		query string
		arg   string
	}{
		{"delete from materials where status = 'uploading' and created_at < ?", stale},
		{"delete from materials where removed_at is not null and removed_at < ?", gone},
		{"delete from knowledge_versions where version is null and created_at < ?", stale},
	} {
		result, err := tx.Exec(step.query, step.arg)
		if err != nil {
			return 0, err
		}
		n, _ := result.RowsAffected()
		removed += n
	}
	if _, err := tx.Exec("delete from knowledge where not exists " +
		"(select 1 from knowledge_versions v where v.doc_id = knowledge.id)"); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	kept := map[string]bool{}
	for _, query := range []string{"select path from materials where path is not null",
		"select source_path from knowledge_versions where source_path is not null union " +
			"select pdf_path from knowledge_versions where pdf_path is not null"} {
		paths, err := strs(l.db, query)
		if err != nil {
			return 0, err
		}
		for _, path := range paths {
			kept[path] = true
		}
	}
	dayAgo := now.Add(-DraftKeep)
	err = filepath.WalkDir(l.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := l.relative(path)
		if err != nil || kept[rel] {
			return err
		}
		if filepath.Ext(path) == ".part" {
			if info, err := entry.Info(); err == nil && info.ModTime().After(dayAgo) {
				return nil // still coming
			}
		}
		os.Remove(path)
		return nil
	})
	return removed, err
}

// Usage is items, documents and bytes on disk: «Machine».
type Usage struct {
	Items     int64  `json:"items"`
	New       int64  `json:"new"`
	Inbox     int64  `json:"inbox"`
	Documents int64  `json:"documents"`
	Bytes     int64  `json:"bytes"`
	FreeBytes *int64 `json:"free_bytes"`
}

// Usage counts the library.
func (l *Library) Usage() (Usage, error) {
	var u Usage
	filepath.WalkDir(l.Root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				u.Bytes += info.Size()
			}
		}
		return nil
	})
	var fresh *int64
	if err := l.db.QueryRow("select count(*) n, sum(status = 'new') new from materials where removed_at is null "+
		"and status != 'uploading'").Scan(&u.Items, &fresh); err != nil {
		return u, err
	}
	if fresh != nil {
		u.New = *fresh
	}
	var err error
	if u.Inbox, err = l.InboxCount(); err != nil {
		return u, err
	}
	docs, err := l.Documents(nil)
	if err != nil {
		return u, err
	}
	for _, d := range docs {
		if d.Version != nil && *d.Version != 0 {
			u.Documents++
		}
	}
	var stat syscall.Statfs_t
	if _, err := os.Stat(l.DataDir); err == nil && syscall.Statfs(l.DataDir, &stat) == nil {
		free := int64(stat.Bavail) * int64(stat.Bsize)
		u.FreeBytes = &free
	}
	return u, nil
}

func jsonIDs(ids []int64) string {
	if ids == nil {
		ids = []int64{}
	}
	text, err := pyjson.DumpsASCII(ids)
	if err != nil {
		panic(err)
	}
	return text
}

func parseIDs(raw string) []int64 {
	var values []any
	if err := json.Unmarshal([]byte(orDefault(raw, "[]")), &values); err != nil {
		return nil
	}
	var ids []int64
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			ids = append(ids, int64(v))
		case string:
			var n int64
			if _, err := fmt.Sscan(v, &n); err != nil {
				return nil
			}
			ids = append(ids, n)
		default:
			return nil
		}
	}
	return ids
}
