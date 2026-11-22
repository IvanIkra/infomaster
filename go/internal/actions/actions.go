// Package actions is the owner's actions and readings (§8.5, §8.6, D69). The port of infomaster/actions.py.
//
// They were the Mini App's API; the Mini App is gone (D69) and they stay as the one place of the rules: the
// connector's tools call them (connector/catalog), and so do the web app's screens and «Machine», through the tools
// or directly for what has no tool. An own sheet's upload, its deletion and joining are the shelves' (internal/web)
// and the connector's, on the sheets' publisher itself. Each action answers a JSON-ready value (Phrases in it are
// written in the app's language by the caller: httpx.JSONLang, Localize) or a refusal: an *httpx.HTTPError of 400 or
// 404 with its text, in the app's language (D41). Any other error is an internal one (500).
//
// What they cover: the merged deadlines with the owner's own ones and the hidden ones (§5.4, §5.5), the timetable as
// the feeds have it with the owner's own events and corrections (§6.5), the grades, the sheets' fields (§6.7, §6.3),
// and the settings: the twin's account with its login and logout, the chats and their settings (D12), the course
// dictionary (D3, D33), the HSE App X login (§5.2, D7), the limits of reading a PDF, the gradebooks, the LLM spending
// and its daily limit (§7, D40), the general settings (D41, D52).
//
// The public port (PublicApp) also serves /hseapp/callback, where the browser comes back after the HSE login: the
// PKCE state of the login link is its protection, and it answers with a short page in the app's language. And
// /calendar/<CALENDAR_PUBLIC_SECRET>/main.ics and optional.ics for one's own calendar (see icalfeed, D43), and the
// library's signed links.
package actions

import (
	"errors"
	"net/http"
	"slices"

	"github.com/IvanIkra/infomaster/go/internal/collectors/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/links"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/api"
	"github.com/IvanIkra/infomaster/go/internal/outputs/icalfeed"
	"github.com/IvanIkra/infomaster/go/internal/outputs/reminders"
	sheetspub "github.com/IvanIkra/infomaster/go/internal/outputs/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// The limits of the owner's fields.
const (
	TitleLimit       = 200 // characters of an own deadline's title
	CourseKeyLimit   = 200 // an own deadline's course key (the web app, §8.6)
	SheetCourseLimit = 60  // a sheet's course
	SheetNumberLimit = 60  // the number, or a short title instead
	GradeLabelLimit  = 40
	EventTitleLimit  = 120
	RoomLimit        = 300 // a room, or a meeting's link
	KindLimit        = 40  // the kind is the type of the feeds once schedule.TypeOf has read it
	ChartDays        = 30
)

// General are the general settings of no other group.
var General = []string{"batch_hours", "news_ttl_hours", "llm_prices", "language", "about_me", "wiki_pages"}

// GradeSettings are the grades' settings by the names the actions give them, in order.
var GradeSettings = []struct{ Key, Name string }{{"student", "grades_student"}, {"sheets", "grades_sheets"},
	{"window_days", "grades_window_days"}}

// PDFLimits are the limits of reading a PDF whole.
var PDFLimits = []string{"pdf_pages", "pdf_tokens"}

// EventFields are the fields of an event the owner changes.
var EventFields = []string{"title", "kind", "start", "end", "room", "required", "cancelled"}

// ReminderFields are the reminders' settings by the names the actions give them, in order.
var ReminderFields = []struct{ Key, Name string }{{"enabled", "deadline_reminders"}, {"eve", "reminder_eve"},
	{"day", "reminder_day"}}

// Services are what the public port's app holds (Python's app keys). A nil one is a part the service runs without;
// Settings may be nil (English, no settings), and so may HseApp, Sheets, Grades, Twin, Calendar, Reminders and the
// library with its Links.
type Services struct {
	Env          config.Env
	Budget       *llm.Budget
	Journal      *llm.Journal
	Inbox        *inbox.Inbox
	ChatSettings *settings.ChatSettings
	Sources      []api.Source // the sources' statuses by name, as /api/status has them
	Subjects     *subjects.Subjects
	Schedule     *schedule.Schedule
	HseApp       *hseapp.HseApp
	Deadlines    *merge.DeadlineList
	Settings     *settings.Settings
	Sheets       *sheetspub.Publisher
	Grades       *grades.Grades
	Twin         *telegram.Twin
	Calendar     *icalfeed.CalendarFeed
	Reminders    *reminders.Reminders
	Library      *library.Library // with Links: the library's signed links and the connector's library tools
	Links        *links.Links
}

// Actions are the owner's actions over the services.
type Actions struct {
	Services
}

// New is the actions over the services; a nil Library is the one of Links.
func New(s Services) *Actions {
	if s.Library == nil && s.Links != nil { // Python takes the library from its links
		s.Library = s.Links.Library
	}
	return &Actions{Services: s}
}

// PublicApp is the public port's app (public_app): the actions over the services, and a mux with the HSE App X
// callback, the library's links (with Links) and the feeds for one's own calendar (with Calendar and
// CALENDAR_PUBLIC_SECRET). The connector (Connector.Attach) and the web app attach themselves to the same mux; serve
// it through httpx.Router.
func PublicApp(s Services) (*Actions, *http.ServeMux) {
	a := New(s)
	mux := http.NewServeMux()
	a.Routes(mux)
	return a, mux
}

// Routes adds the public port's own routes to a mux.
func (a *Actions) Routes(mux *http.ServeMux) {
	if a.Links != nil {
		a.Links.Routes(mux)
	}
	if a.Services.HseApp != nil {
		mux.HandleFunc("GET "+hseapp.CallbackPath, a.Services.HseApp.Callback)
	}
	if a.Calendar != nil {
		icalfeed.Routes(mux, a.Env.CalendarPublicSecret, a.Calendar, icalfeed.PublicFeeds)
	}
}

// Lang is the app's language (D41); English where the app has no settings.
func (a *Actions) Lang() string { return httpx.Lang(a.Settings) }

// errNotRunning is a part the app runs without: Python's KeyError of a missing app key, a 500.
var errNotRunning = errors.New("this part of infomaster is not running")

// badRequest is a 400 in the app's language.
func (a *Actions) badRequest(en, ru string) error { return httpx.BadRequest(i18n.Tr(a.Lang(), en, ru)) }

// status is a source's status, ok false when the app has no such source.
func (a *Actions) status(name string) (any, bool, error) {
	for _, source := range a.Sources {
		if source.Name == name {
			value, err := source.Status()
			return value, true, err
		}
	}
	return nil, false, nil
}

// statusOr is sources.get(name, dict)(): the status, an empty object for a source the app does not have.
func (a *Actions) statusOr(name string) (any, error) {
	value, ok, err := a.status(name)
	if !ok {
		return httpx.Object{}, nil
	}
	return value, err
}

// required is sources[name](): a missing source is an internal error, as Python's KeyError.
func (a *Actions) required(name string) (any, error) {
	value, ok, err := a.status(name)
	if !ok {
		return nil, errors.New("no source " + name)
	}
	return value, err
}

// statusObject is a source's status as an ordered object in the app's language (empty when the app has none).
func (a *Actions) statusObject(name string) (httpx.Object, error) {
	value, err := a.statusOr(name)
	if err != nil {
		return nil, err
	}
	o, err := ToObject(value, a.Lang())
	if o == nil {
		o = httpx.Object{}
	}
	return o, err
}

// ToObject is a JSON-ready value as the client reads it: an ordered object (or any decoded JSON value) with its
// Phrases in the language and its floats as Python writes them. What call_action gives a tool.
func ToObject(value any, lang string) (httpx.Object, error) {
	decoded, err := Decoded(value, lang)
	if err != nil {
		return nil, err
	}
	o, _ := decoded.(httpx.Object)
	return o, nil
}

// Decoded is a JSON-ready value written as the actions answer it (Phrases in the language) and read back, objects
// as ordered httpx.Objects: json.loads(response.body) of call_action.
func Decoded(value any, lang string) (any, error) {
	text, err := pyjsonDumps(httpx.Localize(value, lang))
	if err != nil {
		return nil, err
	}
	return httpx.DecodeOrdered([]byte(text))
}

// field is a value of an ordered object, nil when it is not there.
func field(o httpx.Object, key string) any {
	if o == nil {
		return nil
	}
	return o.Get(key)
}

// has reports whether an ordered object has the key.
func has(o httpx.Object, key string) bool {
	return slices.ContainsFunc(o, func(f httpx.Field) bool { return f.Key == key })
}

// truthy is Python's bool() of a decoded JSON value.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int64:
		return v != 0
	case float64:
		return v != 0
	case httpx.Object:
		return len(v) > 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// keys are the keys of a body.
func keys(body map[string]any) []string {
	out := make([]string, 0, len(body))
	for key := range body {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// only reports whether every key of the body is one of names.
func only(body map[string]any, names ...string) bool {
	for key := range body {
		if !slices.Contains(names, key) {
			return false
		}
	}
	return true
}

// exactly reports whether the body has these keys and no others.
func exactly(body map[string]any, names ...string) bool {
	if len(body) != len(names) {
		return false
	}
	return only(body, names...)
}
