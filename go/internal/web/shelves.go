package web

// The shelves of the web app (§8.6, stage 16.8, D67): the problem sheets with their PDFs and versions, the library's
// raw material by course, and the sorting of the library's inbox. The port of web/shelves.py.
//
// The screens read through the sheets' store and the library; every change is the connector's write tool
// (Connector.Act), so it is checked once, journaled with `via` web and undone from the journal like the others:
// edit_sheet, join_sheet, delete_sheet, sort_material, update_material, remove_material. An own sheet's PDF is a
// multipart upload that no tool can take; it is kept as the sheets keep a chat's and journaled by hand, with the undo
// that deletes it again.
//
// Two names of a course meet here. A sheet keeps the course as the model named it and is matched to a lens by the
// course's key (Data.Key); the library keeps the timetable's name (Library.Course). The app speaks keys: CourseName
// turns a key into a name before a tool is called, and Data.Key turns a name into a key on the way out.
//
// A sheet's PDF has three homes, tried in this order: the library's copy of it (made when the sheet was read, kept
// for good, opened by a signed link), the file the sheets keep for 14 days (streamed by GET /shelf/sheets/{id}/file
// behind the session, or by a signed link when the links have Kept), and none, when both are gone.

import (
	"bytes"
	"context"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/links"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

const (
	SheetCourseLimit = 60
	SheetNumberLimit = 60  // the number, or a short title instead
	ShelfItems       = 500 // of a course's page or of the inbox
	ShelfClasses     = 40  // the classes a course's item can be moved to
	Preview          = 600 // characters of a text on a list
	Candidates       = 30  // the sheets «It is a newer version of…» offers
	FieldBytes       = 400 // bytes of a form field before the files
	chunk            = 1 << 16
)

// SheetListed is a line of the sheets' listing (processing/sheets Listed).
type SheetListed struct {
	ID         int64
	First      int64
	Versions   int
	Status     string
	Error      *string
	Course     *string // as the model named it
	Number     string
	Name       string
	Due        *string
	DueAssumed bool
	Chat       *string
	Own        bool
	CreatedAt  string
	UpdatedAt  string
	Summary    *string
	Caption    *string
	File       bool
}

// SheetVersion is a row of the sheets (one version of a sheet).
type SheetVersion struct {
	ID        int64
	ChatID    int64
	MsgID     int64
	Name      string
	Sha256    string
	Version   int64
	Status    string
	CreatedAt string
	Changes   *string
}

// KeptFile is a PDF the owner uploaded, as the sheets' files keep it: its name and the row the sheets take.
type KeptFile struct {
	Name string
	Row  any
}

// SheetTopic is the sheets' publisher as the shelves read it (Python's SheetPublisher: its sheets, its files and
// its publish). The wiring adapts processing/sheets and outputs/sheets to it.
type SheetTopic interface {
	Listing() ([]SheetListed, error)
	Get(id int64) (*SheetVersion, error) // nil for none
	First(sheet *SheetVersion) (*SheetVersion, error)
	Versions(first *SheetVersion) ([]*SheetVersion, error)
	// ByHash is the kept file of a PDF, "" when it is gone (14 days).
	ByHash(sha256 string) string
	// Keep keeps an uploaded PDF; an error is a refusal whose i18n.Reason the owner reads (too big, no PDF).
	Keep(ctx context.Context, data []byte, name string, at time.Time) (KeptFile, error)
	// AddOwn is an own sheet of a kept PDF, posted at the next publish; its id.
	AddOwn(file KeptFile, course, number string, due *string, at time.Time) (int64, error)
	Publish(ctx context.Context) error
	// CanRemoveOwn reports whether the undo op _remove_own_sheet is registered on the connector.
	CanRemoveOwn() bool
}

func (w *WebApp) shelfRoutes() []route {
	return []route{
		{"GET", "", false, shelfHome},
		{"GET", "/sheets", false, sheetList},
		{"POST", "/sheets", false, addSheet},
		{"GET", "/sheets/{sheet_id}", false, sheetCard},
		{"PATCH", "/sheets/{sheet_id}", false, editSheet},
		{"DELETE", "/sheets/{sheet_id}", false, deleteSheet},
		{"POST", "/sheets/{sheet_id}/join", false, joinSheet},
		{"GET", "/sheets/{sheet_id}/file", false, sheetFile},
		{"GET", "/raw", false, rawShelf},
		{"GET", "/inbox", false, libraryInbox},
		{"GET", "/classes", false, shelfClasses},
		{"GET", "/items/{item_id}", false, itemCard},
		{"PATCH", "/items/{item_id}", false, updateItem},
		{"DELETE", "/items/{item_id}", false, removeItem},
		{"POST", "/sort", false, sortItems},
		{"POST", "/remove", false, removeItems},
	}
}

// --- what the handlers lean on ----------------------------------------------------------------------------------

func (c *call) library() (*library.Library, error) {
	data, err := c.data()
	if err != nil {
		return nil, err
	}
	if data.Library == nil {
		return nil, httpx.NotFound("the library is off")
	}
	return data.Library, nil
}

func (c *call) topic() (SheetTopic, error) {
	if c.app.Parts.Sheets == nil {
		return nil, httpx.NotFound("the sheets are off")
	}
	return c.app.Parts.Sheets, nil
}

// name is the course's name for a key of the lenses ("" for none); 400 for a key that is none.
func (c *call) name(key any) (string, error) {
	if key == nil || key == "" {
		return "", nil
	}
	text, ok := key.(string)
	if !ok {
		return "", httpx.BadRequest("course is a key of the lenses")
	}
	data, err := c.data()
	if err != nil {
		return "", err
	}
	name, err := data.CourseName(text)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", httpx.BadRequest("no such course")
	}
	return name, nil
}

// libraryCourse is the library's course of a key: its timetable name; "" when the key names no course.
func (c *call) libraryCourse(key any) (string, error) {
	name, err := c.name(key)
	if err != nil || name == "" {
		return "", err
	}
	lib, err := c.library()
	if err != nil {
		return "", err
	}
	return lib.Course(name)
}

// --- the sheets -------------------------------------------------------------------------------------------------

// entry is a line of the sheets' listing as the screen has it: the course by its key.
func (c *call) entry(row SheetListed) (httpx.Object, string, error) {
	data, err := c.data()
	if err != nil {
		return nil, "", err
	}
	key, err := data.Key(deref(row.Course))
	if err != nil {
		return nil, "", err
	}
	title, err := data.Title(key)
	if err != nil {
		return nil, "", err
	}
	return httpx.Object{{Key: "id", Value: row.ID}, {Key: "first", Value: row.First},
			{Key: "versions", Value: row.Versions}, {Key: "status", Value: row.Status}, {Key: "error", Value: row.Error},
			{Key: "course", Value: nullable(key)}, {Key: "course_title", Value: nullable(title)},
			{Key: "number", Value: row.Number}, {Key: "name", Value: row.Name}, {Key: "due", Value: row.Due},
			{Key: "due_assumed", Value: row.DueAssumed}, {Key: "chat", Value: row.Chat}, {Key: "own", Value: row.Own},
			{Key: "created_at", Value: row.CreatedAt}, {Key: "updated_at", Value: row.UpdatedAt},
			{Key: "summary", Value: row.Summary}, {Key: "caption", Value: row.Caption}, {Key: "file", Value: row.File}},
		key, nil
}

// fileURL is where a version's PDF opens, nil when it is gone: the library's copy first, then the kept file.
func (c *call) fileURL(version *SheetVersion) (any, error) {
	data, err := c.data()
	if err != nil {
		return nil, err
	}
	topic, err := c.topic()
	if err != nil {
		return nil, err
	}
	if data.Library != nil && data.Links != nil {
		found, err := data.Library.FromOrigin("sheet", version.ChatID, version.MsgID, version.Sha256)
		if err != nil {
			return nil, err
		}
		if found != nil && data.Library.Path(found) != "" {
			return data.Links.Material(found, links.GetTTL), nil
		}
	}
	if topic.ByHash(version.Sha256) == "" {
		return nil, nil
	}
	if data.Links != nil && data.Links.Kept != nil { // signed: it opens outside the session too (D69)
		return data.Links.KeptPDF(version.Sha256, version.Name, links.GetTTL), nil
	}
	return "/web/api/shelf/sheets/" + strconv.FormatInt(version.ID, 10) + "/file", nil
}

// shelfHome is the shelves' entry: how many sheets, what waits in the inbox, and the courses with raw material.
func shelfHome(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	courses := []httpx.Object{}
	inbox := int64(0)
	if lib := data.Library; lib != nil {
		listed, err := lib.Courses()
		if err != nil {
			return err
		}
		for _, course := range listed {
			key, err := data.Key(course.Course)
			if err != nil {
				return err
			}
			if key != "" && (course.Items > 0 || course.Documents > 0) {
				title, err := data.Title(key)
				if err != nil {
					return err
				}
				courses = append(courses, httpx.Object{{Key: "key", Value: key}, {Key: "title", Value: title},
					{Key: "items", Value: course.Items}, {Key: "new", Value: course.New},
					{Key: "documents", Value: course.Documents}})
			}
		}
		if inbox, err = lib.InboxCount(); err != nil {
			return err
		}
	}
	var sheets any
	if topic := c.app.Parts.Sheets; topic != nil {
		listed, err := topic.Listing()
		if err != nil {
			return err
		}
		sheets = len(listed)
	}
	return c.jsonLang(httpx.Object{{Key: "library", Value: data.Library != nil}, {Key: "inbox", Value: inbox},
		{Key: "sheets", Value: sheets}, {Key: "courses", Value: courses}})
}

func (c *call) entries() ([]httpx.Object, []string, error) {
	topic, err := c.topic()
	if err != nil {
		return nil, nil, err
	}
	rows, err := topic.Listing()
	if err != nil {
		return nil, nil, err
	}
	listed, keys := []httpx.Object{}, []string{}
	for _, row := range rows {
		entry, key, err := c.entry(row)
		if err != nil {
			return nil, nil, err
		}
		listed, keys = append(listed, entry), append(keys, key)
	}
	return listed, keys, nil
}

// sheetList is ?course=<key>: the sheets of a course, or all of them; the newest first. Those of no known course
// come with all of them.
func sheetList(c *call) error {
	if _, err := c.topic(); err != nil {
		return err
	}
	key := c.course()
	listed, keys, err := c.entries()
	if err != nil {
		return err
	}
	if key != "" {
		kept := []httpx.Object{}
		for i, entry := range listed {
			if keys[i] == key {
				kept = append(kept, entry)
			}
		}
		listed = kept
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	title, err := data.Title(key)
	if err != nil {
		return err
	}
	courses, err := data.Courses()
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "course", Value: nullable(key)}, {Key: "title", Value: nullable(title)},
		{Key: "sheets", Value: listed}, {Key: "courses", Value: courses}})
}

// sheetCard is a sheet with every version and its PDF, and the sheets it may become a newer version of.
func sheetCard(c *call) error {
	topic, err := c.topic()
	if err != nil {
		return err
	}
	id, err := c.id("sheet_id")
	if err != nil {
		return err
	}
	found, err := topic.Get(id)
	if err != nil {
		return err
	}
	if found == nil {
		return httpx.NotFound("no such sheet")
	}
	first, err := topic.First(found)
	if err != nil {
		return err
	}
	listed, keys, err := c.entries()
	if err != nil {
		return err
	}
	var entry httpx.Object
	key := ""
	for i, e := range listed {
		if e.Get("first") == first.ID {
			entry, key = e, keys[i]
			break
		}
	}
	if entry == nil {
		return httpx.NotFound("no such sheet")
	}
	chain := []*SheetVersion{first}
	if first.Status == "posted" {
		if chain, err = topic.Versions(first); err != nil {
			return err
		}
	}
	versions := []httpx.Object{}
	for _, v := range chain {
		url, err := c.fileURL(v)
		if err != nil {
			return err
		}
		versions = append(versions, httpx.Object{{Key: "id", Value: v.ID}, {Key: "version", Value: v.Version},
			{Key: "name", Value: v.Name}, {Key: "created_at", Value: v.CreatedAt}, {Key: "changes", Value: v.Changes},
			{Key: "file_url", Value: url}})
	}
	// «It is a newer version of…» works only for a posted sheet without versions: the others are the candidates
	candidates := []httpx.Object{}
	if entry.Get("status") == "posted" && entry.Get("versions") == 1 {
		var same, other []httpx.Object
		for i, e := range listed {
			if e.Get("status") == "posted" && e.Get("first") != first.ID {
				if keys[i] == key {
					same = append(same, e)
				} else {
					other = append(other, e)
				}
			}
		}
		for _, e := range append(same, other...) {
			if len(candidates) == Candidates {
				break
			}
			candidates = append(candidates, httpx.Object{{Key: "id", Value: e.Get("id")},
				{Key: "course", Value: e.Get("course")}, {Key: "course_title", Value: e.Get("course_title")},
				{Key: "number", Value: e.Get("number")}, {Key: "name", Value: e.Get("name")},
				{Key: "versions", Value: e.Get("versions")}})
		}
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	courses, err := data.Courses()
	if err != nil {
		return err
	}
	return c.jsonLang(append(entry, httpx.Field{Key: "versions_list", Value: versions},
		httpx.Field{Key: "candidates", Value: candidates}, httpx.Field{Key: "courses", Value: courses}))
}

// asciiName is a file name for a header: non-ASCII characters, " and ? replaced by _.
func asciiName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r > 127 || r == '"' || r == '?' {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sheetFile is a version's PDF from the file the sheets keep (14 days), behind the session.
func sheetFile(c *call) error {
	topic, err := c.topic()
	if err != nil {
		return err
	}
	id, err := c.id("sheet_id")
	if err != nil {
		return err
	}
	found, err := topic.Get(id)
	if err != nil {
		return err
	}
	path := ""
	if found != nil {
		path = topic.ByHash(found.Sha256)
	}
	if path == "" {
		return httpx.NotFound("the file is gone")
	}
	name := found.Name
	if name == "" {
		name = "sheet.pdf"
	}
	file, err := openFile(path)
	if err != nil {
		return httpx.NotFound("the file is gone")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/pdf")
	c.w.Header().Set("Content-Disposition", `inline; filename="`+asciiName(name)+`"`)
	c.w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(c.w, c.r, "", info.ModTime(), file)
	return nil
}

// newest is the id the tools and the listing use for a sheet: its newest version's.
func (c *call) newest(sheetID int64) (int64, error) {
	topic, err := c.topic()
	if err != nil {
		return 0, err
	}
	found, err := topic.Get(sheetID)
	if err != nil {
		return 0, err
	}
	if found == nil {
		return 0, httpx.NotFound("no such sheet")
	}
	first, err := topic.First(found)
	if err != nil {
		return 0, err
	}
	chain, err := topic.Versions(first)
	if err != nil {
		return 0, err
	}
	if len(chain) > 0 && first.Status == "posted" {
		return chain[len(chain)-1].ID, nil
	}
	return found.ID, nil
}

// editSheet is some of {course: a key or "", number, due: YYYY-MM-DD or null, summary}: the connector's edit_sheet.
func editSheet(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	id, err := c.id("sheet_id")
	if err != nil {
		return err
	}
	sheetID, err := c.newest(id)
	if err != nil {
		return err
	}
	if len(values) == 0 || others(values, "course", "number", "due", "summary") {
		return httpx.BadRequest("expected some of course, number, due, summary")
	}
	arguments := map[string]any{"sheet_id": sheetID}
	for name, value := range values {
		if name != "course" {
			arguments[name] = value
		}
	}
	if course, ok := values["course"]; ok {
		name, err := c.name(course)
		if err != nil {
			return err
		}
		arguments["course"] = name
	}
	changed, err := c.actChange("edit_sheet", arguments)
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}, {Key: "id", Value: sheetID}})
}

// joinSheet is {previous: a sheet's id}: the sheet is a newer version of it; the connector's join_sheet, which
// cannot be undone.
func joinSheet(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	id, err := c.id("sheet_id")
	if err != nil {
		return err
	}
	previous, ok := values["previous"].(int64)
	if !ok {
		return httpx.BadRequest("expected {previous: id}")
	}
	changed, err := c.actChange("join_sheet", map[string]any{"sheet_id": id, "previous": previous})
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}, {Key: "id", Value: previous}})
}

// deleteSheet is an own sheet with its versions: the connector's delete_sheet, which cannot be undone.
func deleteSheet(c *call) error {
	id, err := c.id("sheet_id")
	if err != nil {
		return err
	}
	changed, err := c.actChange("delete_sheet", map[string]any{"sheet_id": id})
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}})
}

// formField is a form field before the files: at most limit bytes, stripped.
func formField(part *multipart.Part, limit int) (string, error) {
	data, err := io.ReadAll(io.LimitReader(part, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", httpx.BadRequest(part.FormName() + ": up to " + strconv.Itoa(limit) + " bytes")
	}
	return pytext.Strip(strings.ToValidUTF8(string(data), "�")), nil
}

// addSheet is multipart: course (a key), number, due, then the PDF `file`: an own sheet, posted at once. Journaled
// by hand, with the undo that deletes it.
func addSheet(c *call) error {
	topic, err := c.topic()
	if err != nil {
		return err
	}
	reader, err := c.r.MultipartReader()
	if err != nil {
		return httpx.BadRequest("multipart/form-data expected")
	}
	fields := map[string]string{}
	var content []byte
	name, has := "", false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return httpx.BadRequest("multipart/form-data expected")
		}
		if part.FileName() == "" {
			if formName := part.FormName(); formName == "course" || formName == "number" || formName == "due" {
				if fields[formName], err = formField(part, FieldBytes); err != nil {
					return err
				}
			}
			continue
		}
		if has {
			return httpx.BadRequest("one PDF at a time")
		}
		name, has = part.FileName(), true
		var buffer bytes.Buffer
		n, err := io.Copy(&buffer, io.LimitReader(part, files.UploadMaxSize+1))
		if err != nil {
			return httpx.BadRequest("multipart/form-data expected")
		}
		if n > files.UploadMaxSize {
			return httpx.BadRequest("the file is too big")
		}
		content = buffer.Bytes()
	}
	if len(content) == 0 {
		return httpx.BadRequest("choose a PDF")
	}
	number := spaced(fields["number"])
	if pytext.Len(number) > SheetNumberLimit {
		return httpx.BadRequest("number: up to " + strconv.Itoa(SheetNumberLimit) + " characters")
	}
	var due *string
	if fields["due"] != "" {
		day, err := time.Parse(clock.DateLayout, fields["due"])
		if err != nil {
			return httpx.BadRequest("the due date is a date like 2026-10-22")
		}
		text := day.Format(clock.DateLayout)
		due = &text
	}
	course, err := c.name(fields["course"])
	if err != nil {
		return err
	}
	if name == "" {
		name = "sheet.pdf"
	}
	file, err := topic.Keep(c.ctx(), content, name, clock.NowUTC())
	if err != nil {
		return httpx.BadRequest(i18n.Reason(err, c.app.Lang()))
	}
	sheetID, err := topic.AddOwn(file, course, number, due, clock.NowUTC())
	if err != nil {
		return err
	}
	shown := file.Name
	if shown == "" {
		shown = name
	}
	summary := "Added the own sheet «" + shown + "»"
	connector := c.app.Connector
	var undoSteps []tools.Step
	if connector != nil && topic.CanRemoveOwn() {
		undoSteps = []tools.Step{{Tool: "_remove_own_sheet", Arguments: map[string]any{"sheet_id": sheetID}}}
	}
	var changeID any
	if connector != nil {
		if changeID, err = connector.Journal("add_sheet", map[string]any{"sheet_id": sheetID}, summary, undoSteps, "",
			"web"); err != nil {
			return err
		}
	}
	if err := topic.Publish(c.ctx()); err != nil { // published at the next run
		log.Printf("the own sheet %d is not posted yet: %v", sheetID, err)
	}
	return c.jsonLang(httpx.Object{{Key: "id", Value: sheetID}, {Key: "change", Value: httpx.Object{
		{Key: "id", Value: changeID}, {Key: "summary", Value: summary}, {Key: "undo", Value: undoSteps != nil},
		{Key: "note", Value: ""}}}})
}

// --- the library: raw material and the inbox ------------------------------------------------------------------

// view is an item for the screen: its file by a signed link, a text cut to a preview unless whole, its course by key.
func (c *call) view(row *library.Material, whole bool) (httpx.Object, error) {
	data, err := c.data()
	if err != nil {
		return nil, err
	}
	text := deref(row.Text)
	key, err := data.Key(deref(row.Course))
	if err != nil {
		return nil, err
	}
	title, err := data.Title(key)
	if err != nil {
		return nil, err
	}
	shown := text
	if !whole {
		shown = pytext.Cut(text, Preview)
	}
	var url any
	if data.Links != nil && data.Library.Path(row) != "" {
		url = data.Links.Material(row, links.GetTTL)
	}
	return httpx.Object{{Key: "id", Value: row.ID}, {Key: "kind", Value: row.Kind}, {Key: "title", Value: row.Title},
		{Key: "author", Value: row.Author}, {Key: "url", Value: row.URL}, {Key: "name", Value: row.Name},
		{Key: "media_type", Value: row.MediaType}, {Key: "size", Value: row.Size}, {Key: "status", Value: row.Status},
		{Key: "day", Value: row.Day}, {Key: "created_at", Value: row.CreatedAt}, {Key: "class_uid", Value: row.ClassUID},
		{Key: "note", Value: row.Note}, {Key: "parent", Value: row.Parent}, {Key: "source", Value: row.Source},
		{Key: "course", Value: nullable(key)}, {Key: "course_title", Value: nullable(title)}, {Key: "text", Value: shown},
		{Key: "text_cut", Value: !whole && pytext.Len(text) > Preview}, {Key: "file_url", Value: url}}, nil
}

func classObject(row library.ClassInfo) httpx.Object {
	return httpx.Object{{Key: "uid", Value: row.UID}, {Key: "start", Value: row.Start}, {Key: "end", Value: row.End},
		{Key: "kind", Value: row.Kind}, {Key: "items", Value: row.Items}, {Key: "new", Value: row.New}}
}

func (c *call) queryValue(name string) any {
	if value, ok := c.query(name); ok {
		return value
	}
	return nil
}

// rawShelf is ?course=<key>: the raw material of a course by class, and what has no class.
func rawShelf(c *call) error {
	lib, err := c.library()
	if err != nil {
		return err
	}
	key := c.queryValue("course")
	course, err := c.libraryCourse(key)
	if err != nil {
		return err
	}
	if course == "" {
		return httpx.BadRequest("which course?")
	}
	started, err := lib.Classes(course, time.Time{})
	if err != nil {
		return err
	}
	byClass := map[string][]httpx.Object{}
	for _, class := range started {
		byClass[class.UID] = []httpx.Object{}
	}
	loose := []httpx.Object{}
	rows, err := lib.Items(library.Query{Course: &course, Limit: ShelfItems})
	if err != nil {
		return err
	}
	for _, row := range rows {
		item, err := c.view(row, false)
		if err != nil {
			return err
		}
		if list, ok := byClass[deref(row.ClassUID)]; ok && row.ClassUID != nil {
			byClass[*row.ClassUID] = append(list, item)
		} else {
			loose = append(loose, item)
		}
	}
	classes := []httpx.Object{}
	for _, class := range started {
		classes = append(classes, append(classObject(class), httpx.Field{Key: "items_list", Value: byClass[class.UID]}))
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	title, err := data.Title(str(key))
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "course", Value: key}, {Key: "title", Value: nullable(title)},
		{Key: "classes", Value: classes}, {Key: "items", Value: loose}})
}

// libraryInbox is what came to the library without a course, the newest first, and the courses to sort it into.
func libraryInbox(c *call) error {
	lib, err := c.library()
	if err != nil {
		return err
	}
	rows, err := lib.Items(library.Query{Inbox: true, Limit: ShelfItems})
	if err != nil {
		return err
	}
	items := []httpx.Object{}
	for _, row := range rows {
		item, err := c.view(row, false)
		if err != nil {
			return err
		}
		items = append(items, item)
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	courses, err := data.Courses()
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "items", Value: items}, {Key: "courses", Value: courses}})
}

// shelfClasses is ?course=<key>: the started classes of a course, the last first: where an item can be moved to.
func shelfClasses(c *call) error {
	key := c.queryValue("course")
	course, err := c.libraryCourse(key)
	if err != nil {
		return err
	}
	listed := []httpx.Object{}
	if course != "" {
		lib, err := c.library()
		if err != nil {
			return err
		}
		found, err := lib.Classes(course, time.Time{})
		if err != nil {
			return err
		}
		for i, class := range found {
			if i == ShelfClasses {
				break
			}
			listed = append(listed, classObject(class))
		}
	}
	return c.jsonLang(httpx.Object{{Key: "course", Value: key}, {Key: "classes", Value: listed}})
}

// itemCard is one item with its whole text, the class it is at and the items that hang on it (a recording's
// transcripts).
func itemCard(c *call) error {
	lib, err := c.library()
	if err != nil {
		return err
	}
	id, err := c.id("item_id")
	if err != nil {
		return err
	}
	row, err := lib.Get(id, false, false)
	if err != nil {
		return err
	}
	if row == nil {
		return httpx.NotFound("no such item")
	}
	var placed any
	if row.ClassUID != nil && *row.ClassUID != "" {
		found, err := lib.ClassOf(*row.ClassUID)
		if err != nil {
			return err
		}
		if found != nil {
			placed = httpx.Object{{Key: "uid", Value: found.UID}, {Key: "start", Value: found.Start},
				{Key: "kind", Value: found.Kind}}
		}
	}
	children, err := lib.Children(row.ID)
	if err != nil {
		return err
	}
	listed := []httpx.Object{}
	for _, child := range children {
		item, err := c.view(child, false)
		if err != nil {
			return err
		}
		listed = append(listed, item)
	}
	item, err := c.view(row, true)
	if err != nil {
		return err
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	courses, err := data.Courses()
	if err != nil {
		return err
	}
	return c.jsonLang(append(item, httpx.Field{Key: "class", Value: placed}, httpx.Field{Key: "children", Value: listed},
		httpx.Field{Key: "courses", Value: courses}))
}

// updateItem is some of {title, note, status: new or processed}: the connector's update_material.
func updateItem(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	id, err := c.id("item_id")
	if err != nil {
		return err
	}
	if len(values) == 0 || others(values, "title", "note", "status") {
		return httpx.BadRequest("expected some of title, note, status")
	}
	arguments := map[string]any{"id": id}
	for key, value := range values {
		arguments[key] = value
	}
	changed, err := c.actChange("update_material", arguments)
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}})
}

// removeItem is the connector's remove_material: the item and its transcripts leave every list, and come back with
// the undo.
func removeItem(c *call) error {
	id, err := c.id("item_id")
	if err != nil {
		return err
	}
	changed, err := c.actChange("remove_material", map[string]any{"id": id})
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}})
}

func itemIDs(values map[string]any) ([]any, error) {
	ids, ok := values["ids"].([]any)
	bad := httpx.BadRequest("expected {ids: [item ids]}")
	if !ok || len(ids) == 0 {
		return nil, bad
	}
	for _, id := range ids {
		if _, ok := id.(int64); !ok {
			return nil, bad
		}
	}
	return ids, nil
}

// removeItems is {ids}: the connector's remove_material of several items, one change with one undo (the inbox's
// «Delete»).
func removeItems(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	ids, err := itemIDs(values)
	if err != nil {
		return err
	}
	changed, err := c.actChange("remove_material", map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}})
}

// sortItems is {ids, class_uid} or {ids, course: a key, or null for back to the inbox}: the connector's
// sort_material.
func sortItems(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	ids, err := itemIDs(values)
	if err != nil {
		return err
	}
	arguments := map[string]any{"ids": ids}
	if truthy(values["class_uid"]) {
		arguments["class_uid"] = values["class_uid"]
	} else if course, ok := values["course"]; ok {
		name, err := c.name(course)
		if err != nil {
			return err
		}
		arguments["course"] = nil
		if name != "" {
			arguments["course"] = name
		}
	} else {
		return httpx.BadRequest("give class_uid or course")
	}
	changed, err := c.actChange("sort_material", arguments)
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "change", Value: changed}})
}

func openFile(path string) (*os.File, error) { return os.Open(path) }
