// Package app is the wiring: one process with the twin, the bot and two HTTP ports (§3, §8.1). The port of
// infomaster/app.py.
//
// New makes every part and sets every callback as the Python service does; Run serves the public port (the web
// app, the connector, the HSE App X callback, the library's links, the public feeds) and the internal one (the API
// for Tablo and HA), starts the background loops and watches Telegram: it ends on SIGINT or SIGTERM, or when the bot
// or the twin loses Telegram for good (Docker restarts the service then).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/actions"
	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	gradesrc "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/collectors/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/links"
	"github.com/IvanIkra/infomaster/go/internal/collectors/mail"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/collectors/wiki"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector"
	"github.com/IvanIkra/infomaster/go/internal/connector/catalog"
	"github.com/IvanIkra/infomaster/go/internal/connector/changes"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/feed"
	liblinks "github.com/IvanIkra/infomaster/go/internal/library/links"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/api"
	"github.com/IvanIkra/infomaster/go/internal/outputs/bot"
	dlwatch "github.com/IvanIkra/infomaster/go/internal/outputs/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/outputs/icalfeed"
	"github.com/IvanIkra/infomaster/go/internal/outputs/reminders"
	outsheets "github.com/IvanIkra/infomaster/go/internal/outputs/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/batches"
	"github.com/IvanIkra/infomaster/go/internal/processing/botsheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	hsedl "github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/images"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
	"github.com/IvanIkra/infomaster/go/internal/web"
	"github.com/IvanIkra/infomaster/go/internal/web/passkey"
)

// PruneEvery is the time between removals of raw messages past their 14 days.
const PruneEvery = 6 * time.Hour

// Notifier is what the service pushes (§8.6, D69): *web.Notifier; tests record it instead.
type Notifier interface {
	Deadline(d todo.Deadline, was string)
	Reminder(name string, due []todo.Deadline)
	Budget(notice llm.Notice)
	System(text i18n.Phrase, login string)
}

// Options are what the command line gives besides the environment.
type Options struct {
	Static string // the web client's directory; "" finds infomaster/web/static of the repository
}

// Service is every part of the service, wired.
type Service struct {
	Env     config.Env
	DataDir string

	Store           *store.Store
	Settings        *settings.Settings
	Journal         *llm.Journal
	Budget          *llm.Budget
	LLM             llm.Model
	Bot             *bot.Bot
	Inbox           *inbox.Inbox
	ChatSettings    *settings.ChatSettings
	Twin            *telegram.Twin
	News            *news.News
	Deadlines       *deadlines.Deadlines
	Subjects        *subjects.Subjects
	Ruz             *ruz.Ruz
	Schedule        *schedule.Schedule
	Batches         *batches.Batches
	Files           *files.Files
	Sheets          *sheets.Sheets
	SheetPublisher  *outsheets.Publisher
	Images          *images.Images
	Calendar        *icalfeed.CalendarFeed
	HseApp          *hseapp.HseApp
	HseAppDeadlines *hsedl.Deadlines
	Mail            *mail.Mail
	Wiki            *wiki.Wiki
	Merged          *merge.DeadlineList
	Watch           *dlwatch.Watch
	Reminders       *reminders.Reminders
	GradeSheets     *gradesrc.GradeSheets
	Grades          *grades.Grades
	Library         *library.Library
	Links           *liblinks.Links
	Feed            *feed.ChatMaterials
	BotSheets       *botsheets.BotSheets
	Connector       *connector.Connector
	Web             *web.WebApp
	Actions         *actions.Actions
	API             *api.API

	// Sources are the sources' statuses by name: GET /api/status and «Machine» read them.
	Sources []api.Source
	// Notifier is where the pushes go: the web app's notifier.
	Notifier Notifier
	// PublishSheets publishes the sheets waiting: the publisher's Publish.
	PublishSheets func(ctx context.Context) error

	public   http.Handler
	internal http.Handler
	tasks    sync.WaitGroup // refreshes started by a change of the dictionary
}

// New makes every part and sets every callback; nothing connects to Telegram or anywhere else until Run.
func New(env config.Env, dataDir string, options Options) (*Service, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil { // sessions and tokens: the owner only (§10)
		return nil, err
	}
	st, err := store.Open(filepath.Join(dataDir, "infomaster.db"))
	if err != nil {
		return nil, err
	}
	s := &Service{Env: env, DataDir: dataDir, Store: st}
	if err := s.build(options); err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) build(options Options) error {
	env, st := s.Env, s.Store
	var err error
	if s.Settings, err = settings.New(st); err != nil {
		return err
	}
	s.Journal = llm.NewJournal(st)
	s.Budget = llm.NewBudget(s.Settings, s.Journal, st)
	s.LLM = llm.NewHaiku(s.Budget, s.Journal)
	s.Bot = bot.New(env, bot.SessionPath(s.DataDir), s.language)
	s.Inbox = inbox.Open(st)
	s.ChatSettings = settings.NewChats(st)
	s.Twin = telegram.New(env, s.Inbox, s.ChatSettings, telegram.SessionPath(s.DataDir), s.toSystem)
	s.Twin.NotifyLogin = s.twinNotice
	s.News, s.Deadlines = news.Open(st), deadlines.Open(st)
	s.Subjects = subjects.New(st, s.LLM)
	s.Ruz = ruz.New(env.RuzICalURL, st)
	s.Schedule = schedule.New(st, s.Ruz, events.Open(st), s.Deadlines, s.Subjects, s.Inbox)
	s.Batches = batches.New(st, s.Settings, s.ChatSettings, s.Inbox, s.News, s.Deadlines, s.Schedule, s.LLM, nil)
	// PDFs come through the twin when a batch needs them; the sheets among them are published (§6.3).
	s.Files = files.New(st, s.DataDir)
	s.Files.Download = s.twinDownload(files.MaxSize) // the size of now, not the stored one
	s.Files.FetchURL = links.Fetch                   // PDFs behind links of study chats and letters (D59)
	s.Sheets = sheets.New(st, s.Settings, s.ChatSettings, s.Inbox, s.Files, s.Deadlines, s.Subjects, s.LLM, nil)
	s.SheetPublisher = outsheets.New(s.Sheets)
	s.PublishSheets = s.SheetPublisher.Publish
	s.Batches.Files, s.Batches.Sheets = s.Files, s.Sheets
	// Images of chats with «Картинки» come the same way and are read by Haiku within their daily number (D12).
	s.Images = images.New(s.Settings, s.Budget, s.Files, s.LLM, nil)
	s.Batches.Images = s.Images
	s.Calendar = icalfeed.New(s.Schedule)
	// From the calendar: an entry's card in the web app, which quotes its source message too (D69).
	s.Calendar.EditLink = func(uid string) string { return env.PublicURL + "/web/#/c?uid=" + pytext.Quote(uid, "") }
	s.HseApp = hseapp.New(st, s.Settings, env.PublicURL+hseapp.CallbackPath, nil)
	s.HseAppDeadlines = hsedl.New(st, s.Subjects, s.LLM)
	s.Mail = mail.New(env, st, s.Settings, s.Inbox, nil)
	s.Mail.Files = s.Files // a letter's PDF attachment is kept when it comes (D59)
	// Course wiki pages (D60): their new sheets and homework come as notices in the mailbox.
	s.Wiki = wiki.New(st, s.Settings, s.Inbox, nil)
	// The one list of deadlines (§5.5): the API for Tablo and HA, the web app and the connector read it.
	s.Merged = merge.New(st, s.Deadlines, s.HseAppDeadlines, s.Subjects)
	s.Watch = dlwatch.New(st, s.Merged)
	s.Reminders = reminders.New(st, s.Merged, s.Settings)
	// Grades from teachers' Google Sheets (§6.7): the web app and the API show them.
	s.GradeSheets = gradesrc.New(s.Settings)
	s.Grades = grades.New(st, s.Settings, s.LLM)
	// GET /api/status and «Machine» read these by name.
	s.Sources = []api.Source{
		{Name: "telegram", Status: s.twinStatus},
		{Name: "batches", Status: func() (any, error) { return s.Batches.Status() }},
		{Name: "ruz", Status: s.RuzStatus},
		{Name: "hseapp", Status: s.HseAppStatus},
		{Name: "mail", Status: func() (any, error) { return s.Mail.Status() }},
		{Name: "sheets", Status: func() (any, error) { return s.Sheets.Status() }},
		{Name: "grades", Status: s.GradesStatus},
		{Name: "wiki", Status: func() (any, error) { return s.Wiki.Status() }},
		{Name: "images", Status: func() (any, error) { return s.Images.Status() }},
	}
	// The library (D58): raw material of the courses and the knowledge base Claude makes of it; files by links.
	if s.Library, err = library.New(st, s.DataDir); err != nil {
		return err
	}
	s.Library.SameCourse = s.sameCourse
	if s.Links, err = liblinks.New(st, s.Library, env.PublicURL); err != nil {
		return err
	}
	s.Links.Kept = s.Files.ByHash // a sheet's kept PDF by a signed link (D69)
	// Its sources (§6.8): materials the batches find in study chats (files by the twin, after the run), every
	// sheet's PDF, and what the owner sends the bot (files by the bot itself).
	if s.Feed, err = feed.New(st, s.Library, s.Inbox, nil); err != nil {
		return err
	}
	s.Feed.Download = s.twinDownload(library.MaxFile)
	s.Batches.Materials = s.Feed
	s.Sheets.Library = s.Library
	// A PDF sent to the bot that is a sheet goes to the sheets with its caption (D73), the rest to the inbox.
	s.BotSheets = botsheets.New(s.SheetPublisher, s.Library)
	s.Bot.OnMaterial = func(ctx context.Context, m bot.Material, download bot.Download) (string, error) {
		message := feed.BotMessage{ChatID: m.ChatID, MsgID: m.MsgID, At: m.At, Text: m.Text, Files: m.Files,
			Forward: m.Forward}
		return s.BotSheets.Take(ctx, message, botsheets.Download(download))
	}
	// The MCP connector for claude.ai (§8.5, D57): its sign-in code comes from the bot to the owner's chat.
	if s.Connector, err = connector.New(env, st, s.News, s.toOwner, s.language, nil, nil); err != nil {
		return err
	}
	// The web app (§8.6, D61): its first login on a device is a code from the bot too.
	data := web.NewData(st, s.Schedule, s.Merged)
	data.Library, data.Grades, data.Links, data.Lang, data.News = s.Library, s.Grades, s.Links, s.language, s.News
	data.TTLHours = s.newsTTL
	if s.Web, err = web.New(env, st, s.toOwner, s.language, nil, data, s.LLM, options.Static); err != nil {
		return err
	}
	s.Web.Connector = s.Connector // the owner's changes are the connector's tools, journaled with undo
	s.Web.Auth.Verifier = passkey.Verifier{}
	s.BotSheets.Journal = func(tool string, arguments map[string]any, summary string, undo []botsheets.Undo) {
		var steps []tools.Step
		if undo != nil {
			steps = make([]tools.Step, len(undo))
			for i, step := range undo {
				steps[i] = tools.Step{Tool: step.Tool, Arguments: step.Arguments}
			}
		}
		if _, err := s.Connector.Journal(tool, arguments, summary, steps, "", changes.ViaWeb); err != nil {
			slog.Error("journaling the sheet sent to the bot failed", "err", err)
		}
	}
	s.BotSheets.CanUndo = s.Connector.Registry.HasOp
	// Web Push (§8.6): the only notifications (D69).
	s.Notifier = s.Web.Notifier
	s.Web.Notifier.Settings = s.Settings // the kinds pushed and the quiet hours («Machine», D65)
	s.Watch.Notify = func(d todo.Deadline, was string) { s.Notifier.Deadline(d, was) }
	s.Reminders.Notify = func(name string, due []todo.Deadline) { s.Notifier.Reminder(name, due) }
	s.Budget.Notify = func(notice llm.Notice) { s.Notifier.Budget(notice) }
	s.Inbox.Wake = s.Batches.Wake
	s.Batches.Publish = s.publish
	s.Batches.Notify = s.toSystem
	s.Batches.Mail = s.Mail.Configured()
	s.Mail.Notify = s.toSystem
	s.Wiki.Notify = s.toSystem
	s.Ruz.OnPoll = s.afterPoll
	s.Ruz.Notify = s.toSystem
	s.Schedule.OnMidterms = s.publishMidterms
	s.HseApp.OnPoll = func(ctx context.Context, active, done []hseapp.Raw) error {
		_, err := s.HseAppDeadlines.AfterPoll(ctx, active, done)
		return err
	}
	s.HseApp.Notify = s.hseappNotice
	s.HseAppDeadlines.OnChange = s.hseappChanged
	s.Merged.OnChange = s.Watch.Changed
	// A sheet without a due date is due before its course's next seminar, and follows the timetable (§6.3).
	s.Sheets.Schedule = s.Schedule
	s.Sheets.OnChange = s.Watch.Changed
	s.Schedule.Followers = append(s.Schedule.Followers, s.Sheets.Follow)
	s.GradeSheets.OnSheet = s.Grades.Apply
	s.GradeSheets.Notify = s.toSystem
	s.Grades.Wake = s.GradeSheets.Wake
	s.Subjects.OnManual = s.dictionaryChanged
	s.ports()
	return nil
}

// ports makes both apps: the public one with the owner's actions, the connector and the web app; the internal API.
func (s *Service) ports() {
	var mux *http.ServeMux
	s.Actions, mux = actions.PublicApp(actions.Services{
		Env: s.Env, Budget: s.Budget, Journal: s.Journal, Inbox: s.Inbox, ChatSettings: s.ChatSettings,
		Sources: s.Sources, Subjects: s.Subjects, Schedule: s.Schedule, HseApp: s.HseApp, Deadlines: s.Merged,
		Settings: s.Settings, Sheets: s.SheetPublisher, Grades: s.Grades, Twin: s.Twin, Calendar: s.Calendar,
		Reminders: s.Reminders, Links: s.Links, // the links' routes come with it
	})
	catalog.Register(s.Connector, s.Actions) // the tools, before the port serves
	s.Connector.Attach(mux, s.Actions)       // the MCP connector (§8.5)
	s.Web.Parts = web.Parts{
		Sources: s.Sources, Env: &s.Env, Budget: s.Budget, Settings: s.Settings, Inbox: s.Inbox,
		ChatSettings: s.ChatSettings, Library: s.Library, Twin: true, Grades: true, Reminders: true, HseApp: true,
		Sheets:  web.Publisher{P: s.SheetPublisher, CanUndo: s.Connector.Registry.HasOp},
		Actions: s.Actions,
	}
	s.Web.Attach(mux) // so does the web app (§8.6)
	s.public = s.Web.Wrap(httpx.Router(mux))

	s.API = api.New(s.Env, s.Budget, s.Sources, s.Calendar, s.Merged)
	s.API.Grades, s.API.News, s.API.Settings = s.Grades, s.News, s.Settings
	s.internal = s.API.Handler()
}

// PublicHandler is the public port: the web app, the connector, the HSE App X callback, the library's links and
// the public feeds.
func (s *Service) PublicHandler() http.Handler { return s.public }

// InternalHandler is the internal port: the API for Tablo and HA, and the feeds behind CALENDAR_SECRET.
func (s *Service) InternalHandler() http.Handler { return s.internal }

// --- callbacks -----------------------------------------------------------------------------------------------

// language is the app's language (D41), read anew each time: «Machine» may change it.
func (s *Service) language() string { return httpx.Lang(s.Settings) }

func (s *Service) newsTTL() int {
	hours, err := s.Settings.Int("news_ttl_hours")
	if err != nil {
		slog.Error("reading news_ttl_hours failed", "err", err)
		return 24
	}
	return hours
}

func (s *Service) toSystem(text i18n.Phrase) { s.Notifier.System(text, "") }

// toOwner is a message of the bot to the owner's private chat: a login code of the web app or the connector.
func (s *Service) toOwner(ctx context.Context, text string) error { return s.Bot.Send(ctx, text) }

// twinNotice is Twin.NotifyLogin: the twin needs a login, in «Machine → Twin and chats».
func (s *Service) twinNotice(text i18n.Phrase) { s.Notifier.System(text, "twin") }

// hseappNotice is HseApp.Notify: a line about HSE App X; one that asks for a login leads to «Machine → HSE App X».
func (s *Service) hseappNotice(text i18n.Phrase, login bool) {
	if login {
		s.Notifier.System(text, "hseapp")
	} else {
		s.Notifier.System(text, "")
	}
}

// twinDownload is the twin's download of a message's file within a limit, its "not now" as the files' one.
func (s *Service) twinDownload(limit int64) func(ctx context.Context, chatID, msgID int64, path string) (bool,
	error) {
	return func(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
		ok, err := s.Twin.Download(ctx, chatID, msgID, path, limit)
		var notNow *telegram.NotNow
		if errors.As(err, &notNow) {
			err = &files.NotNow{Msg: notNow.Msg}
		}
		return ok, err
	}
}

// sameCourse is Schedule.SameCourse for the library; the plain names when the dictionary cannot be read.
func (s *Service) sameCourse(a, b string) bool {
	same, err := s.Schedule.SameCourse(a, b)
	if err != nil {
		slog.Error("comparing courses failed", "err", err)
		return a != "" && b != "" && pytext.Casefold(a) == pytext.Casefold(b)
	}
	return same
}

// publish is Batches.Publish: the sheets waiting (this run's and any a failure left), then the deadlines (the
// merged list, with what the run gave, the sheets read and the Midterms it moved).
func (s *Service) publish(ctx context.Context, _ batches.Changes) error {
	s.publishSheets(ctx)
	s.refreshDeadlines()
	return nil
}

// afterPoll is Ruz.OnPoll: the processing of the timetable. The Midterms of a first poll (the first one ever, or
// after the link changed) join the deadlines quietly, like the classes.
func (s *Service) afterPoll(ctx context.Context, diff ruz.Diff) error {
	midterms, err := s.Schedule.AfterPoll(ctx, diff)
	if err != nil {
		return err
	}
	if diff.First { // before any refresh: none may see them first
		refs := []string{}
		for _, id := range midterms.Deadlines {
			refs = append(refs, fmt.Sprintf("dl:%d", id))
		}
		for _, moved := range midterms.Moved {
			refs = append(refs, fmt.Sprintf("dl:%d", moved[1]))
		}
		for _, id := range midterms.Gone {
			refs = append(refs, fmt.Sprintf("dl:%d", id))
		}
		s.Watch.Quiet(refs)
	}
	s.publishSheets(ctx) // assumed due dates that followed their seminars (§6.3)
	s.refreshDeadlines() // renamed Midterms too
	return nil
}

// publishMidterms is Schedule.OnMidterms: the owner's hiding of a correction moved a Midterm.
func (s *Service) publishMidterms(context.Context, schedule.MidtermChanges) error {
	s.refreshDeadlines()
	return nil
}

// hseappChanged is HseAppDeadlines.OnChange: the lists of HSE App X changed. The first poll joins the deadlines
// quietly.
func (s *Service) hseappChanged(_ context.Context, first bool) error {
	if first { // before the refresh: it must not see them first
		current, err := s.HseAppDeadlines.Current()
		if err != nil {
			return err
		}
		refs := make([]string, len(current))
		for i, item := range current {
			refs[i] = "hseapp:" + item.ID
		}
		s.Watch.Quiet(refs)
	}
	s.refreshDeadlines()
	return nil
}

// dictionaryChanged is Subjects.OnManual: the Midterms take the new abbr now, not at the next RUZ poll; the list
// follows.
func (s *Service) dictionaryChanged() {
	if _, err := s.Schedule.SyncDeadlines(); err != nil {
		slog.Error("syncing the deadlines with the dictionary failed", "err", err)
	}
	s.tasks.Add(1)
	go func() {
		defer s.tasks.Done()
		s.refreshDeadlines()
	}()
}

// Settle waits for the refreshes a change of the dictionary started (tests, a stop).
func (s *Service) Settle() { s.tasks.Wait() }

func (s *Service) publishSheets(ctx context.Context) {
	if err := s.PublishSheets(ctx); err != nil {
		slog.Error("publishing the sheets failed", "err", err)
	}
}

func (s *Service) refreshDeadlines() {
	if err := s.Watch.Refresh(); err != nil {
		slog.Error("refreshing the deadlines failed", "err", err)
	}
}

// --- the statuses ---------------------------------------------------------------------------------------------

// twinKeys are the twin's status in the order Python writes them.
var twinKeys = []string{"ok_at", "error", "login_needed", "account", "login"}

func (s *Service) twinStatus() (any, error) {
	status := s.Twin.Status()
	out := httpx.Object{}
	for _, key := range twinKeys {
		if value, ok := status[key]; ok {
			out = append(out, httpx.Field{Key: key, Value: value})
		}
	}
	for key, value := range status { // anything the twin adds later
		if out.Get(key) == nil && !contains(twinKeys, key) {
			out = append(out, httpx.Field{Key: key, Value: value})
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// RuzStatus is RUZ and the naming of its subjects, which waits for Haiku (§4.2).
func (s *Service) RuzStatus() (any, error) {
	status, err := s.Ruz.Status()
	if err != nil {
		return nil, err
	}
	var subjectsError any
	if text := s.Subjects.LastError(); text != "" {
		subjectsError = text
	}
	return append(fields(status), httpx.Field{Key: "subjects_error", Value: subjectsError}), nil
}

// GradesStatus is the sheets (by id: never the links or the name) and the new grades in the window (§8.2).
func (s *Service) GradesStatus() (any, error) {
	source := s.GradeSheets.Status()
	if source.Sheets == nil {
		source.Sheets = map[string]gradesrc.Public{}
	}
	processing, err := s.Grades.Status()
	if err != nil {
		return nil, err
	}
	return append(fields(source), fields(processing)...), nil
}

// HseAppStatus is HSE App X: the login and the polls, the lists kept and their English labels (§5.1, §8.2).
func (s *Service) HseAppStatus() (any, error) {
	lists, err := s.HseAppDeadlines.Status()
	if err != nil {
		return nil, err
	}
	return append(fields(s.HseApp.Status()), fields(lists)...), nil
}

// fields is a status struct as an ordered object of its JSON fields, the values as they are (a Phrase stays one,
// for the caller's language): Python's {**a, **b} of two status dicts.
func fields(value any) httpx.Object {
	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	out := httpx.Object{}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		out = append(out, httpx.Field{Key: name, Value: v.Field(i).Interface()})
	}
	return out
}

// --- the loops ------------------------------------------------------------------------------------------------

func (s *Service) prune(ctx context.Context) {
	for {
		s.pruneOnce()
		select {
		case <-ctx.Done():
			return
		case <-time.After(PruneEvery):
		}
	}
}

func (s *Service) pruneOnce() {
	if removed, err := s.Inbox.Prune(clock.NowUTC()); err != nil {
		slog.Error("pruning raw messages failed", "err", err)
	} else if removed > 0 {
		slog.Info(fmt.Sprintf("removed %d raw messages older than 14 days", removed))
	}
	if removed, err := s.Files.Prune(clock.NowUTC()); err != nil {
		slog.Error("pruning downloaded files failed", "err", err)
	} else if removed > 0 {
		slog.Info(fmt.Sprintf("removed %d downloaded files older than 14 days", removed))
	}
	if removed, err := s.Library.Prune(); err != nil {
		slog.Error("pruning the library failed", "err", err)
	} else if removed > 0 {
		slog.Info(fmt.Sprintf("removed %d library drafts and removed items", removed))
	}
}
