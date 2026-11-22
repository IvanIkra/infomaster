package grades

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	stdnet "net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/links"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	pubnet "github.com/IvanIkra/infomaster/go/internal/net"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

const (
	PollEvery      = 30 * time.Minute // Tablo's poll_minutes
	Timeout        = 120 * time.Second
	StaleAfter     = 12 * time.Hour
	DropboxContent = "dl.dropboxusercontent.com"
)

// The addresses of a Google Sheet's export and HTML view; variables for tests.
var (
	ExportURL = "https://docs.google.com/spreadsheets/d/{id}/export?format=xlsx"
	TitleURL  = "https://docs.google.com/spreadsheets/d/{id}/htmlview"
)

// NoRow is the error of a sheet without the own row on any tab.
var NoRow = i18n.P("the own row is on no tab: check the name in the web app, Machine → Gradebooks",
	"своей строки нет ни на одной вкладке: проверьте ФИО в приложении, Машина → Таблицы оценок")

// SheetID is a Google Sheet's id, or "dropbox-" and the id of a Dropbox link; a settings error for other links.
func SheetID(url string) (string, error) { return settings.GradeSheetID(url) }

// IsDropbox reports a Dropbox sheet's id.
func IsDropbox(sid string) bool { return strings.HasPrefix(sid, settings.DropboxPrefix) }

// DownloadURL is where a sheet's xlsx comes from: Google's export, or the Dropbox link's file on Dropbox's content
// host with dl=1 (rlkey and the other parameters kept). Some resolvers block www.dropbox.com, and dropbox.com
// redirects there: dl.dropboxusercontent.com serves the same shared file directly.
func DownloadURL(url string) (string, error) {
	sid, err := SheetID(url)
	if err != nil {
		return "", err
	}
	if !IsDropbox(sid) {
		return strings.ReplaceAll(ExportURL, "{id}", sid), nil
	}
	parts := links.SplitURL(pytext.Strip(url))
	var query []links.Pair
	dl := false
	for _, p := range links.ParseQSL(parts.Query, true) {
		if p.Key == "dl" {
			p.Value, dl = "1", true
		}
		query = append(query, p)
	}
	if !dl {
		query = append(query, links.Pair{Key: "dl", Value: "1"})
	}
	return links.URL{Scheme: "https", Netloc: DropboxContent, Path: parts.Path, Query: links.URLEncode(query)}.String(), nil
}

// downloadURL is DownloadURL; tests serve Dropbox's content host locally.
var downloadURL = DownloadURL

// FileTitle is a Dropbox sheet's title: its file name without the extension; "" for none.
func FileTitle(name string) string {
	name = strings.TrimRight(name, "/")
	name = name[strings.LastIndex(name, "/")+1:]
	if i := strings.LastIndex(name, "."); i > 0 && i < len(name)-1 {
		name = name[:i]
	}
	return pytext.Strip(name)
}

// Sheet is one sheet read: what OnSheet gets.
type Sheet struct {
	ID      string
	URL     string
	Title   *string // as Google names it, or a Dropbox file's name; nil when the title could not be fetched
	Course  string  // the short name in front of the labels: the setting's label, else the title's start
	Student string
	Views   []TabView
}

// Read is what OnSheet made of a sheet.
type Read struct {
	First  bool     // the baseline: the first read of the sheet records no new grades
	Items  int      // grades in the sheet now
	Unread []string // tabs the model could not lay out: skipped until their header changes
}

// SheetStatus is the state of one configured sheet.
type SheetStatus struct {
	Label        string // the setting's: names the sheet until its title is known
	Title        *string
	OkAt         *string
	Error        any // nil, a string, or an i18n.Phrase
	Tabs         int
	Items        int
	Unread       []string
	failingSince *time.Time
	told         bool
}

// Name is the sheet's title, else its label.
func (st *SheetStatus) Name() string {
	if st.Title != nil && *st.Title != "" {
		return *st.Title
	}
	if st.Label != "" {
		return st.Label
	}
	return "?"
}

// Public is a sheet's status as the API shows it: never the link or the name.
type Public struct {
	Title  *string  `json:"title"`
	OkAt   *string  `json:"ok_at"`
	Error  any      `json:"error"`
	Tabs   int      `json:"tabs"`
	Items  int      `json:"items"`
	Unread []string `json:"unread"`
}

// Status is the source's status.
type Status struct {
	Configured bool              `json:"configured"`
	Sheets     map[string]Public `json:"sheets"`
}

// ShortCourse is the course in front of the labels: the label, else the start of the title, else of the id.
func ShortCourse(label string, title *string, sid string) string {
	if label != "" {
		return label
	}
	name := sid
	if title != nil && *title != "" {
		name = *title
	}
	return pytext.Strip(pytext.Cut(name, CourseFromTitle))
}

// GradeSheets is the source.
type GradeSheets struct {
	Settings *settings.Settings
	Now      func() time.Time
	OnSheet  func(ctx context.Context, sheet Sheet) (Read, error)
	Notify   func(i18n.Phrase) // a line for the owner (Web Push)
	Client   *http.Client      // nil: public addresses only (D79)

	mu     sync.Mutex
	sheets map[string]*SheetStatus // by sheet id, the configured sheets only
	wake   chan struct{}
}

// New makes the source.
func New(s *settings.Settings) *GradeSheets {
	return &GradeSheets{Settings: s, Now: clock.NowUTC, sheets: map[string]*SheetStatus{}, wake: make(chan struct{}, 1)}
}

func (g *GradeSheets) config() (string, []settings.GradeSheet) {
	student, _ := g.Settings.String("grades_student")
	sheets, _ := g.Settings.GradeSheets()
	return student, sheets
}

// Configured reports a name and at least one sheet.
func (g *GradeSheets) Configured() bool {
	student, sheets := g.config()
	return student != "" && len(sheets) > 0
}

// Status is the sheets by id; never the links or the name.
func (g *GradeSheets) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := Status{Configured: g.Configured(), Sheets: map[string]Public{}}
	for sid, st := range g.sheets {
		unread := append([]string{}, st.Unread...)
		out.Sheets[sid] = Public{st.Title, st.OkAt, st.Error, st.Tabs, st.Items, unread}
	}
	return out
}

// Wake polls now, not in 30 minutes: the settings changed.
func (g *GradeSheets) Wake() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Run polls until ctx is done.
func (g *GradeSheets) Run(ctx context.Context) {
	for {
		if err := g.Poll(ctx); err != nil {
			slog.Error("grades poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-g.wake: // a change during a poll gives one more poll
		case <-time.After(PollEvery):
		}
	}
}

// Poll reads every configured sheet once.
func (g *GradeSheets) Poll(ctx context.Context) error {
	student, sheets := g.config()
	ids := make([]string, len(sheets))
	for i, sheet := range sheets {
		sid, err := SheetID(sheet.URL)
		if err != nil {
			return err
		}
		ids[i] = sid
	}
	g.mu.Lock()
	fresh := map[string]*SheetStatus{}
	for _, sid := range ids {
		if st := g.sheets[sid]; st != nil {
			fresh[sid] = st
		} else {
			fresh[sid] = &SheetStatus{}
		}
	}
	g.sheets = fresh
	g.mu.Unlock()
	if student == "" || len(sheets) == 0 {
		return nil
	}
	client := g.Client
	if client == nil {
		client = pubnet.Client(Timeout)
	}
	for i, sheet := range sheets {
		g.pollSheet(ctx, client, ids[i], sheet, student)
	}
	return nil
}

type statusError struct {
	code   int
	reason string
}

func (e *statusError) Error() string {
	return strings.TrimSpace(fmt.Sprintf("HTTP %d %s", e.code, e.reason))
}

func (g *GradeSheets) download(ctx context.Context, client *http.Client, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req) // redirects are followed
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
		return nil, "", &statusError{resp.StatusCode, reason}
	}
	tooBig := &ReadError{fmt.Sprintf("the sheet is over %d MB", XLSXMax>>20)}
	if resp.ContentLength > XLSXMax {
		return nil, "", tooBig
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, XLSXMax+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > XLSXMax {
		return nil, "", tooBig
	}
	return data, resp.Header.Get("Content-Disposition"), nil
}

// dispositionName is the file name of a Content-Disposition, filename* first; "" for none.
func dispositionName(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return params["filename"]
}

func (g *GradeSheets) pollSheet(ctx context.Context, client *http.Client, sid string, sheet settings.GradeSheet,
	student string) {
	g.mu.Lock()
	st := g.sheets[sid]
	st.Label = sheet.Label
	if IsDropbox(sid) && st.Title == nil { // named before the download, so that a failing sheet has its name too
		if _, name, _, _ := settings.DropboxFile(sheet.URL); FileTitle(name) != "" {
			title := FileTitle(name)
			st.Title = &title
		}
	}
	title := st.Title
	g.mu.Unlock()

	views, read, err := func() ([]TabView, Read, error) {
		url, err := downloadURL(sheet.URL)
		if err != nil {
			return nil, Read{}, err
		}
		data, disposition, err := g.download(ctx, client, url)
		if err != nil {
			return nil, Read{}, err
		}
		if IsDropbox(sid) {
			if named := dispositionName(disposition); named != "" { // the file may have been renamed
				if !strings.HasSuffix(strings.ToLower(named), ".xlsx") {
					return nil, Read{}, &i18n.Error{Phrase: settings.NotXLSX(named)}
				}
				if t := FileTitle(named); t != "" {
					title = &t
				}
			}
		} else if title == nil { // after the file: a sheet closed to the link has the title of a login page
			title = FetchTitle(ctx, client, sid)
		}
		g.mu.Lock()
		st.Title = title
		g.mu.Unlock()
		views, err := ReadViews(data, student)
		if err != nil {
			return nil, Read{}, err
		}
		if len(views) == 0 {
			return nil, Read{}, &i18n.Error{Phrase: NoRow}
		}
		if g.OnSheet == nil {
			return views, Read{}, nil
		}
		read, err := g.OnSheet(ctx, Sheet{sid, sheet.URL, title, ShortCourse(sheet.Label, title, sid), student, views})
		return views, read, err
	}()
	if err != nil {
		g.failed(st, describe(err, st))
		return
	}
	g.mu.Lock()
	st.Tabs, st.Items, st.Unread = len(views), read.Items, read.Unread
	name := st.Name()
	g.mu.Unlock()
	g.recovered(st)
	if read.First {
		g.notify(i18n.P(
			fmt.Sprintf("The grade sheet «%s» is connected: tabs with the own row: %d, grades: %d. "+
				"They are in the course lenses of the web app.", name, len(views), read.Items),
			fmt.Sprintf("Таблица оценок «%s» подключена: вкладок со своей строкой — %d, оценок — %d. "+
				"Они в линзах курсов в приложении.", name, len(views), read.Items)))
	}
}

// describe is the status error of a failure, as the Python service words it.
func describe(err error, st *SheetStatus) any {
	var phrase *i18n.Error
	var status *statusError
	var readErr *ReadError
	var private *pubnet.PrivateAddress
	var llmErr *llm.Error
	var netErr stdnet.Error
	switch {
	case errors.As(err, &phrase):
		return phrase.Phrase
	case errors.As(err, &status):
		return status.Error()
	case errors.As(err, &readErr):
		return "ValueError: " + readErr.Msg
	case errors.As(err, &private):
		return "PrivateAddress: " + private.Error()
	case errors.As(err, &llmErr), llm.IsBudget(err): // the layout; asked again at the next poll
		return err.Error()
	case errors.As(err, &netErr) && netErr.Timeout():
		return "TimeoutError: "
	case errors.As(err, &netErr):
		return "ClientConnectorError: " + err.Error()
	}
	slog.Error("grades of a sheet failed", "sheet", st.Name(), "error", err)
	return fmt.Sprintf("%T: %v", err, err)
}

func (g *GradeSheets) failed(st *SheetStatus, reason any) {
	g.mu.Lock()
	st.Error = reason
	slog.Warn("grades sheet failed", "sheet", st.Name(), "error", reason)
	now := g.Now()
	if st.failingSince == nil {
		st.failingSince = &now
	}
	var line *i18n.Phrase
	if !st.told && now.Sub(*st.failingSince) >= StaleAfter {
		st.told = true
		since := st.failingSince.In(clock.TZ).Format("02.01 15:04")
		say := func(lang string) string {
			if p, ok := reason.(i18n.Phrase); ok {
				return p.Get(lang)
			}
			return fmt.Sprint(reason)
		}
		alert := i18n.AlertP(
			fmt.Sprintf("The grade sheet «%s» cannot be read since %s: %s.\nIts grades are not updated.",
				st.Name(), since, say("en")),
			fmt.Sprintf("Таблица оценок «%s» не читается с %s: %s.\nОценки из неё не обновляются.",
				st.Name(), since, say("ru")))
		line = &alert
	}
	g.mu.Unlock()
	if line != nil {
		g.notify(*line)
	}
}

func (g *GradeSheets) recovered(st *SheetStatus) {
	g.mu.Lock()
	told, name := st.told, st.Name()
	okAt := clock.LocalISO(g.Now())
	st.OkAt, st.Error = &okAt, nil
	st.failingSince, st.told = nil, false
	g.mu.Unlock()
	if told {
		g.notify(i18n.P(fmt.Sprintf("The grade sheet «%s» is read again.", name),
			fmt.Sprintf("Таблица оценок «%s» снова читается.", name)))
	}
}

func (g *GradeSheets) notify(line i18n.Phrase) {
	if g.Notify != nil {
		g.Notify(line)
	}
}

var titleRe = regexp.MustCompile(`(?s)<title>(.*?)</title>`)

// FetchTitle is the sheet's title from its HTML view; nil on any failure (asked again at the next poll).
func FetchTitle(ctx context.Context, client *http.Client, sid string) *string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.ReplaceAll(TitleURL, "{id}", sid), nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, XLSXMax))
	if err != nil || !utf8.Valid(page) {
		return nil
	}
	found := titleRe.FindSubmatch(page)
	if found == nil {
		return nil
	}
	title := pytext.Strip(html.UnescapeString(string(found[1])))
	title = strings.TrimSuffix(title, " - Google Диск")
	title = pytext.Strip(strings.TrimSuffix(title, " - Google Drive"))
	if title == "" {
		return nil
	}
	return &title
}
