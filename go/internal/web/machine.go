package web

// «Machine» (§8.6, stage 16.6, D65): the web app's screen for how the service runs and how it is set up. The port of
// web/machine.py.
//
//   - the sources and their freshness: the twin, the mail, RUZ, HSE App X, the gradebooks, the wiki pages, the
//     batches, the images and the LLM limit, each ok, late, failing, off or waiting for a login, and what needs the
//     owner with the screen that fixes it (attention; the feed's status dot counts it);
//   - the journal: every change of the owner's in the app and of Claude's through the connector, with its undo;
//   - the settings, moved over from the Mini App: the twin's login and the chats, HSE App X, the gradebooks and their
//     tabs, the batches and news, the reminders, whole PDFs, images, the course dictionary, the LLM limit, the Claude
//     connector and the notifications.
//
// The screens read through the owner's actions (Actions: Python's actions.py), as Claude does; a change of a setting
// is the connector's write tool (Connector.Act): checked once, journaled with its undo like every other change. What
// has no tool is the owner's alone and is not journaled: the twin's and HSE App X's logins, the LLM limit, reading a
// gradebook now and revoking a connection of Claude's.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/api"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// LateMinutes: a source is late when its last good read is older than this many minutes (its poll and some slack).
var LateMinutes = map[string]int{"mail": 30, "ruz": 3 * 60, "hseapp": 60, "grades": 2 * 60, "wiki": 9 * 60}

// Journal is how many changes «Machine» lists.
const Journal = 100

// SettingTools are the groups of settings and the connector's tool that changes each.
var SettingTools = map[string]string{"general": "update_general_settings", "reminders": "update_reminders",
	"images": "update_images", "pdf": "update_pdf_limits", "grades": "update_grade_settings",
	"notifications": "update_notifications"}

// General are the general settings «Processing» shows (actions.GENERAL without llm_prices).
var General = []string{"batch_hours", "news_ttl_hours", "language", "about_me", "wiki_pages"}

// BatchHours are the choices of batch_hours (settings.BATCH_HOURS): the batches are tied to the hours of the day
// (D19), so N divides 24.
var BatchHours = []int{1, 2, 3, 4, 6, 8, 12, 24}

// ReminderFields are the reminders' settings by the names the screen gives them.
var ReminderFields = httpx.Object{{Key: "enabled", Value: "deadline_reminders"}, {Key: "eve", Value: "reminder_eve"},
	{Key: "day", Value: "reminder_day"}}

// PDFLimits are the whole PDFs' settings.
var PDFLimits = []string{"pdf_pages", "pdf_tokens"}

// Actions are the owner's actions «Machine» reads through (Python's actions.py, called by call_action). Each answers
// a JSON-ready value (an object); a refusal is a *tools.ToolError or an *httpx.HTTPError, and the screen answers it
// 400 with its text.
type Actions interface {
	Chats(ctx context.Context) (any, error)
	Senders(ctx context.Context, chatID string) (any, error)
	TwinLogin(ctx context.Context, body map[string]any) (any, error)
	TwinCancel(ctx context.Context) (any, error)
	TwinLogout(ctx context.Context) (any, error)
	HseApp(ctx context.Context) (any, error)
	UpdateHseApp(ctx context.Context, body map[string]any) (any, error)
	HseAppLink(ctx context.Context, body map[string]any) (any, error)
	HseAppLogin(ctx context.Context, body map[string]any) (any, error)
	Subjects(ctx context.Context) (any, error)
	LLM(ctx context.Context) (any, error)
	UpdateLLM(ctx context.Context, body map[string]any) (any, error)
	RaiseLLM(ctx context.Context) (any, error)
	GradeSheets(ctx context.Context) (any, error)
	Gradebook(ctx context.Context, sid string) (any, error)
	ReadGradebook(ctx context.Context, sid string) (any, error)
}

// Parts are the services of the public port «Machine» and the shelves read; a missing one (nil, false) is a part
// the service runs without, as an absent key of Python's app.
type Parts struct {
	Sources      []api.Source // the sources' statuses, as /api/status has them; nil: none wired
	Env          *config.Env  // the service's environment: the calendar's secrets
	Budget       *llm.Budget
	Settings     *settings.Settings
	Inbox        *inbox.Inbox
	ChatSettings *settings.ChatSettings
	Library      *library.Library // the library behind the signed links (its disk in «Machine»)
	Twin         bool             // the twin runs
	Grades       bool             // the gradebooks run
	Reminders    bool             // the deadlines' reminders run
	HseApp       bool             // HSE App X runs
	Sheets       SheetTopic       // the sheets' publisher; nil: the sheets are off
	Actions      Actions
}

// unavailable is aiohttp's 503 of a part not running.
func unavailable() error {
	return httpx.Fail(http.StatusServiceUnavailable, "this part of infomaster is not running")
}

// toObject is a JSON-ready value as an ordered object in a language (its Phrases written in it).
func toObject(value any, lang string) (httpx.Object, error) {
	localized := httpx.Localize(value, lang)
	text, err := pyjson.Dumps(httpx.PyValue(localized))
	if err != nil {
		return nil, err
	}
	decoded, err := httpx.DecodeOrdered([]byte(text))
	if err != nil {
		return nil, err
	}
	o, _ := decoded.(httpx.Object)
	return o, nil
}

func field(o httpx.Object, key string) any {
	if o == nil {
		return nil
	}
	return o.Get(key)
}

func str(value any) string {
	s, _ := value.(string)
	return s
}

// statuses are the sources' statuses by name, in order, as objects in the language.
func (w *WebApp) statuses(lang string) ([]string, map[string]httpx.Object, error) {
	names, found := []string{}, map[string]httpx.Object{}
	for _, source := range w.Parts.Sources {
		value, err := source.Status()
		if err != nil {
			return nil, nil, err
		}
		o, err := toObject(value, lang)
		if err != nil {
			return nil, nil, err
		}
		if o == nil {
			o = httpx.Object{}
		}
		names = append(names, source.Name)
		found[source.Name] = o
	}
	return names, found, nil
}

func (w *WebApp) now() time.Time {
	seconds := w.Seconds()
	return time.Unix(0, int64(seconds*1e9)).UTC()
}

func (w *WebApp) late(name string, okAt any) bool {
	minutes, ok := LateMinutes[name]
	text := str(okAt)
	if !ok || text == "" {
		return false
	}
	at, err := clock.ParseLocal(text)
	if err != nil {
		return false
	}
	return w.now().Sub(at) > time.Duration(minutes)*time.Minute
}

func values(o httpx.Object) []httpx.Object {
	out := []httpx.Object{}
	for _, f := range o {
		if v, ok := f.Value.(httpx.Object); ok {
			out = append(out, v)
		}
	}
	return out
}

// sources are each source: {key, state: ok | late | error | login | off | idle, ok_at, error, count}, the order of
// the screen. A source the service runs without is left out.
func (w *WebApp) sources(lang string) ([]httpx.Object, error) {
	_, status, err := w.statuses(lang)
	if err != nil {
		return nil, err
	}
	listed := []httpx.Object{}
	add := func(key, state string, okAt, failure, count any) {
		if state == "ok" && w.late(key, okAt) {
			state = "late"
		}
		listed = append(listed, httpx.Object{{Key: "key", Value: key}, {Key: "state", Value: state},
			{Key: "ok_at", Value: okAt}, {Key: "error", Value: failure}, {Key: "count", Value: count}})
	}
	choose := func(pairs ...any) string { // condition, state, ..., the last state
		for i := 0; i+1 < len(pairs); i += 2 {
			if pairs[i].(bool) {
				return pairs[i+1].(string)
			}
		}
		return pairs[len(pairs)-1].(string)
	}
	if twin, ok := status["telegram"]; ok {
		var on any
		if w.Parts.Inbox != nil && w.Parts.ChatSettings != nil {
			chats, err := w.Parts.Inbox.Chats()
			if err != nil {
				return nil, err
			}
			n := 0
			for _, chat := range chats {
				enabled, err := w.Parts.ChatSettings.Enabled(chat.ID)
				if err != nil {
					return nil, err
				}
				if enabled {
					n++
				}
			}
			on = n
		}
		add("twin", choose(!truthy(field(twin, "account")), "login", truthy(field(twin, "error")), "error", "ok"),
			field(twin, "ok_at"), field(twin, "error"), on)
	}
	if mail, ok := status["mail"]; ok {
		add("mail", choose(!truthy(field(mail, "configured")), "off", truthy(field(mail, "error")), "error",
			truthy(field(mail, "ok_at")), "ok", "idle"), field(mail, "ok_at"), field(mail, "error"), field(mail, "letters"))
	}
	if ruz, ok := status["ruz"]; ok {
		add("ruz", choose(truthy(field(ruz, "error")), "error", truthy(field(ruz, "ok_at")), "ok", "idle"),
			field(ruz, "ok_at"), field(ruz, "error"), field(ruz, "classes"))
	}
	if hse, ok := status["hseapp"]; ok {
		add("hseapp", choose(!truthy(field(hse, "enabled")), "off", truthy(field(hse, "login_needed")), "login",
			truthy(field(hse, "error")), "error", truthy(field(hse, "ok_at")), "ok", "idle"), field(hse, "ok_at"),
			field(hse, "error"), field(hse, "items"))
	}
	if books, ok := status["grades"]; ok {
		sheets, _ := field(books, "sheets").(httpx.Object)
		var failing []httpx.Object
		var read []string
		for _, sheet := range values(sheets) {
			if truthy(field(sheet, "error")) {
				failing = append(failing, sheet)
			}
			if at := str(field(sheet, "ok_at")); at != "" {
				read = append(read, at)
			}
		}
		var okAt, failure any
		if len(read) > 0 {
			okAt = slices.Min(read)
		}
		if len(failing) > 0 {
			failure = field(failing[0], "error")
		}
		add("grades", choose(len(sheets) == 0, "off", len(failing) > 0, "error", len(read) > 0, "ok", "idle"), okAt,
			failure, len(sheets))
	}
	if wiki, ok := status["wiki"]; ok {
		pages, _ := field(wiki, "pages").(httpx.Object)
		var failure any
		for _, page := range values(pages) {
			if truthy(field(page, "error")) {
				failure = field(page, "error")
				break
			}
		}
		add("wiki", choose(len(pages) == 0, "off", failure != nil, "error", truthy(field(wiki, "ok_at")), "ok", "idle"),
			field(wiki, "ok_at"), failure, len(pages))
	}
	if batches, ok := status["batches"]; ok {
		add("batches", choose(truthy(field(batches, "error")), "error", "ok"), field(batches, "ok_at"),
			field(batches, "error"), field(batches, "queue"))
	}
	if images, ok := status["images"]; ok {
		add("images", choose(!truthy(field(images, "daily")), "off", truthy(field(images, "error")), "error", "ok"),
			field(images, "ok_at"), field(images, "error"), field(images, "today"))
	}
	if w.Parts.Budget != nil {
		budget, err := w.Parts.Budget.Status()
		if err != nil {
			return nil, err
		}
		add("llm", choose(budget.Level == llm.Hard, "error", budget.Level != llm.OK && budget.Level != "", "late", "ok"),
			nil, nil, nil)
	}
	return listed, nil
}

// attention is what needs the owner: {text, screen} in the app's language; screen is a route of «Machine», or nil
// for what only the server's .env and logs fix.
func (w *WebApp) attention(c *call) ([]httpx.Object, error) {
	lang := w.Lang()
	_, status, err := w.statuses(lang)
	if err != nil {
		return nil, err
	}
	found := []httpx.Object{}
	add := func(en, ru string, screen any) {
		found = append(found, httpx.Object{{Key: "text", Value: i18n.Tr(lang, en, ru)}, {Key: "screen", Value: screen}})
	}
	if twin, ok := status["telegram"]; ok && !truthy(field(twin, "account")) {
		add("The twin is not logged in: no chat is read", "Твинк не вошёл: чаты не читаются", "machine/chats")
	} else if failure := str(field(twin, "error")); failure != "" {
		add("The twin: "+failure, "Твинк: "+failure, "machine/chats")
	}
	mail := status["mail"]
	if failure := str(field(mail, "error")); truthy(field(mail, "configured")) && failure != "" {
		add("The mail is not read: "+failure, "Почта не читается: "+failure, nil)
	}
	if truthy(field(status["hseapp"], "login_needed")) {
		add("HSE App X needs a login", "HSE App X: нужен вход", "machine/hseapp")
	}
	if failure := str(field(status["ruz"], "error")); failure != "" {
		add("RUZ is not read: "+failure, "РУЗ не читается: "+failure, nil)
	}
	if sheets, ok := field(status["grades"], "sheets").(httpx.Object); ok {
		for _, f := range sheets {
			sheet, _ := f.Value.(httpx.Object)
			if truthy(field(sheet, "error")) {
				name := str(field(sheet, "title"))
				if name == "" {
					name = f.Key
				}
				add("A gradebook is not read: "+name, "Таблица оценок не читается: "+name, "machine/gradebook?id="+f.Key)
			}
		}
	}
	if pages, ok := field(status["wiki"], "pages").(httpx.Object); ok {
		for _, f := range pages {
			page, _ := f.Value.(httpx.Object)
			if truthy(field(page, "error")) {
				name := str(field(page, "title"))
				if name == "" {
					name = f.Key
				}
				add("A wiki page is not read: "+name, "Страница вики не читается: "+name, "machine/wiki")
			}
		}
	}
	if failure := str(field(status["images"], "error")); failure != "" {
		add("Reading images failed: "+failure, "Чтение картинок не удалось: "+failure, "machine/processing?at=images")
	}
	if w.Parts.Budget != nil {
		budget, err := w.Parts.Budget.Status()
		if err != nil {
			return nil, err
		}
		if budget.Level == llm.Hard {
			add("The LLM limit is spent: calls wait until midnight", "Лимит LLM исчерпан: вызовы стоят до полуночи",
				"machine/llm")
		} else if budget.Level != llm.OK && budget.Level != "" {
			add("Over 80 % of the LLM limit is spent", "Потрачено больше 80 % лимита LLM", "machine/llm")
		}
	}
	if env := w.Parts.Env; env != nil && env.CalendarSecret == "" && env.CalendarPublicSecret == "" {
		add("The calendar feeds are off: no CALENDAR_SECRET in .env",
			"Ленты календаря выключены: в .env нет CALENDAR_SECRET", nil)
	}
	return found, nil
}

// --- the routes --------------------------------------------------------------------------------------------------

func (w *WebApp) machineRoutes() []route {
	return []route{
		{"GET", "", false, overview},
		{"GET", "/journal", false, journal},
		{"GET", "/chats", false, chats},
		{"PATCH", "/chats/{chat_id}", false, updateChat},
		{"GET", "/chats/{chat_id}/senders", false, senders},
		{"POST", "/twin/{step}", false, twin},
		{"GET", "/hseapp", false, hseapp},
		{"PATCH", "/hseapp", false, updateHseapp},
		{"POST", "/hseapp/link", false, hseappLink},
		{"POST", "/hseapp/login", false, hseappLogin},
		{"GET", "/settings", false, settingsScreen},
		{"PATCH", "/settings/{group}", false, updateSettings},
		{"GET", "/courses", false, coursesScreen},
		{"PUT", "/courses", false, putCourse},
		{"POST", "/courses/remove", false, removeCourse},
		{"GET", "/llm", false, llmScreen},
		{"PATCH", "/llm", false, updateLLM},
		{"POST", "/llm/raise", false, raiseLLM},
		{"GET", "/grades", false, gradesScreen},
		{"POST", "/grades", false, addGradebook},
		{"GET", "/grades/{sid}", false, gradebook},
		{"PATCH", "/grades/{sid}", false, updateGradebook},
		{"DELETE", "/grades/{sid}", false, removeGradebook},
		{"POST", "/grades/{sid}/read", false, readGradebook},
		{"PUT", "/grades/{sid}/tabs/{tab}", false, setTab},
		{"DELETE", "/grades/{sid}/tabs/{tab}", false, resetTab},
		{"GET", "/connector", false, connectorScreen},
		{"DELETE", "/connector/{grant_id}", false, revoke},
	}
}

// mini is an action's answer as an object; its refusal as a 400 with its text.
func (c *call) mini(run func(Actions) (any, error)) (httpx.Object, error) {
	if c.app.Parts.Actions == nil {
		return nil, unavailable()
	}
	answer, err := run(c.app.Parts.Actions)
	if text, refused := tools.Refusal(err); refused {
		return nil, httpx.BadRequest(text)
	}
	var failed *httpx.HTTPError
	if errors.As(err, &failed) {
		return nil, httpx.BadRequest(failed.Error())
	}
	if err != nil {
		return nil, err
	}
	o, err := toObject(answer, c.app.Lang())
	if err != nil {
		return nil, err
	}
	if o == nil {
		o = httpx.Object{}
	}
	return o, nil
}

// actChange is a setting changed through the connector's tool, journaled: {id, summary, undo, note}.
func (c *call) actChange(tool string, arguments map[string]any) (httpx.Object, error) {
	connector := c.app.Connector
	if connector == nil || connector.Registry.Tool(tool) == nil {
		return nil, httpx.Fail(http.StatusServiceUnavailable, "changes are not wired")
	}
	id, output, err := connector.Act(c.ctx(), tool, arguments, "web")
	if text, refused := tools.Refusal(err); refused {
		return nil, httpx.BadRequest(text)
	}
	if err != nil {
		return nil, err
	}
	return change(id, output), nil
}

// withChange answers an object with the change at its end.
func withChange(c *call, answer httpx.Object, changed httpx.Object) error {
	return c.jsonLang(append(answer, httpx.Field{Key: "change", Value: changed}))
}

// --- the overview and the journal ----------------------------------------------------------------------------

func overview(c *call) error {
	w, parts := c.app, c.app.Parts
	attention, err := w.attention(c)
	if err != nil {
		return err
	}
	sources, err := w.sources(w.Lang())
	if err != nil {
		return err
	}
	var budget, connections, usage any
	if parts.Budget != nil {
		status, err := parts.Budget.Status()
		if err != nil {
			return err
		}
		o := c.app.object(status)
		if parts.Budget.Settings != nil {
			limit, err := parts.Budget.Settings.Get("llm_daily_limit_usd")
			if err != nil {
				return err
			}
			for i := range o {
				if o[i].Key == "limit_usd" {
					o[i].Value = limit
				}
			}
		}
		budget = o
	}
	if w.Connector != nil {
		listed, err := w.Connector.OAuth.Connections()
		if err != nil {
			return err
		}
		connections = len(listed)
	}
	devices, err := w.Push.Devices()
	if err != nil {
		return err
	}
	if parts.Library != nil {
		found, err := parts.Library.Usage()
		if err != nil {
			return err
		}
		usage = found
	}
	lang := w.Lang()
	if parts.Settings != nil {
		if lang, err = parts.Settings.String("language"); err != nil {
			return err
		}
	}
	return c.jsonLang(httpx.Object{{Key: "attention", Value: attention}, {Key: "sources", Value: sources},
		{Key: "llm", Value: budget}, {Key: "connections", Value: connections}, {Key: "push", Value: len(devices)},
		{Key: "library", Value: usage},
		{Key: "has", Value: httpx.Object{{Key: "twin", Value: parts.Twin}, {Key: "settings", Value: parts.Settings != nil},
			{Key: "grades", Value: parts.Grades}, {Key: "reminders", Value: parts.Reminders},
			{Key: "pdf", Value: parts.Sheets != nil}, {Key: "connector", Value: w.Connector != nil},
			{Key: "hseapp", Value: parts.HseApp && parts.Sources != nil}}},
		{Key: "lang", Value: lang}})
}

func (w *WebApp) object(value any) httpx.Object {
	o, _ := httpx.Localize(value, w.Lang()).(httpx.Object)
	return o
}

// journal is the newest changes first: the owner's in the app (via web) and Claude's (via claude).
func journal(c *call) error {
	listed := []httpx.Object{}
	if c.app.Connector != nil {
		recent, err := c.app.Connector.Changes.Recent(Journal, "")
		if err != nil {
			return err
		}
		for _, ch := range recent {
			listed = append(listed, httpx.Object{{Key: "id", Value: ch.ID}, {Key: "at", Value: ch.At},
				{Key: "tool", Value: ch.Tool}, {Key: "summary", Value: ch.Summary}, {Key: "via", Value: ch.Via},
				{Key: "undo", Value: ch.Undo != nil}, {Key: "note", Value: ch.Note},
				{Key: "undone_at", Value: ch.UndoneAt}, {Key: "undo_error", Value: ch.UndoError}})
		}
	}
	return c.jsonLang(httpx.Object{{Key: "changes", Value: listed}})
}

// --- the twin and the chats --------------------------------------------------------------------------------

func chats(c *call) error {
	if c.app.Parts.Sources == nil {
		return unavailable()
	}
	answer, err := c.mini(func(a Actions) (any, error) { return a.Chats(c.ctx()) })
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// updateChat is some settings of a chat: the connector's update_chat_settings.
func updateChat(c *call) error {
	chatID, err := c.id("chat_id")
	if err != nil {
		return err
	}
	body, err := c.body()
	if err != nil {
		return err
	}
	arguments := map[string]any{"chat_id": chatID}
	for key, value := range body {
		arguments[key] = value
	}
	changed, err := c.actChange("update_chat_settings", arguments)
	if err != nil {
		return err
	}
	if c.app.Parts.ChatSettings == nil {
		return unavailable()
	}
	all, err := c.app.Parts.ChatSettings.All(chatID)
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "settings", Value: all}, {Key: "change", Value: changed}})
}

func senders(c *call) error {
	if c.app.Parts.Actions == nil {
		return unavailable()
	}
	answer, err := c.app.Parts.Actions.Senders(c.ctx(), c.r.PathValue("chat_id"))
	if text, refused := tools.Refusal(err); refused {
		return httpx.BadRequest(text)
	}
	var failed *httpx.HTTPError
	if errors.As(err, &failed) {
		return httpx.BadRequest(failed.Error())
	}
	if err != nil {
		return err
	}
	return c.jsonLang(answer) // a list
}

// twin is POST /twin/login {phone} | {code} | {password}, /twin/cancel, /twin/logout: the twin's status after it.
func twin(c *call) error {
	if !c.app.Parts.Twin {
		return unavailable()
	}
	var run func(Actions) (any, error)
	switch c.r.PathValue("step") {
	case "login":
		body, err := c.body()
		if err != nil {
			return err
		}
		run = func(a Actions) (any, error) { return a.TwinLogin(c.ctx(), body) }
	case "cancel":
		run = func(a Actions) (any, error) { return a.TwinCancel(c.ctx()) }
	case "logout":
		run = func(a Actions) (any, error) { return a.TwinLogout(c.ctx()) }
	default:
		return httpx.NotFound("no such step")
	}
	answer, err := c.mini(run)
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// --- HSE App X -----------------------------------------------------------------------------------------------

func hseapp(c *call) error {
	if c.app.Parts.Sources == nil {
		return unavailable()
	}
	answer, err := c.mini(func(a Actions) (any, error) { return a.HseApp(c.ctx()) })
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

func withBody(c *call, run func(a Actions, body map[string]any) (any, error)) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	answer, err := c.mini(func(a Actions) (any, error) { return run(a, body) })
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

func updateHseapp(c *call) error {
	return withBody(c, func(a Actions, body map[string]any) (any, error) { return a.UpdateHseApp(c.ctx(), body) })
}

func hseappLink(c *call) error {
	return withBody(c, func(a Actions, body map[string]any) (any, error) { return a.HseAppLink(c.ctx(), body) })
}

func hseappLogin(c *call) error {
	return withBody(c, func(a Actions, body map[string]any) (any, error) { return a.HseAppLogin(c.ctx(), body) })
}

// --- settings ------------------------------------------------------------------------------------------------

// settingsOf is every setting of the «Processing» and «Notifications» screens, by the group its tool changes.
func (c *call) settingsOf() (httpx.Object, error) {
	s := c.app.Parts.Settings
	if s == nil {
		return nil, unavailable()
	}
	get := func(name string) any {
		value, err := s.Get(name)
		if err != nil {
			return nil
		}
		return value
	}
	general := httpx.Object{}
	for _, name := range General {
		general = append(general, httpx.Field{Key: name, Value: get(name)})
	}
	images := httpx.Object{{Key: "daily", Value: get("images_daily")}}
	data := httpx.Object{{Key: "general", Value: general}, {Key: "batch_choices", Value: BatchHours},
		{Key: "images", Value: nil},
		{Key: "notifications", Value: httpx.Object{{Key: "kinds", Value: get("push_kinds")},
			{Key: "quiet", Value: get("push_quiet")}}}}
	if c.app.Parts.Reminders {
		reminders := httpx.Object{}
		for _, f := range ReminderFields {
			reminders = append(reminders, httpx.Field{Key: f.Key, Value: get(f.Value.(string))})
		}
		data = append(data, httpx.Field{Key: "reminders", Value: reminders})
	}
	if c.app.Parts.Sheets != nil {
		pdf := httpx.Object{}
		for _, name := range PDFLimits {
			pdf = append(pdf, httpx.Field{Key: name, Value: get(name)})
		}
		data = append(data, httpx.Field{Key: "pdf", Value: pdf})
	}
	_, status, err := c.app.statuses(c.app.Lang())
	if err != nil {
		return nil, err
	}
	if wiki, ok := status["wiki"]; ok {
		data = append(data, httpx.Field{Key: "wiki", Value: field(wiki, "pages")})
	}
	if found, ok := status["images"]; ok {
		images = append(images, httpx.Field{Key: "today", Value: field(found, "today")})
	}
	data[2].Value = images
	return data, nil
}

func settingsScreen(c *call) error {
	data, err := c.settingsOf()
	if err != nil {
		return err
	}
	return c.jsonLang(data)
}

// updateSettings is PATCH /settings/{group} {name: value}: general, reminders, images, pdf, grades or
// notifications, through the group's connector tool; the settings after it and the change.
func updateSettings(c *call) error {
	tool, ok := SettingTools[c.r.PathValue("group")]
	if !ok {
		return httpx.NotFound("no such group of settings")
	}
	body, err := c.body()
	if err != nil {
		return err
	}
	changed, err := c.actChange(tool, body)
	if err != nil {
		return err
	}
	data, err := c.settingsOf()
	if err != nil {
		return err
	}
	return c.jsonLang(httpx.Object{{Key: "settings", Value: data}, {Key: "change", Value: changed}})
}

// --- the course dictionary -------------------------------------------------------------------------------------

func (c *call) subjects() (httpx.Object, error) {
	return c.mini(func(a Actions) (any, error) { return a.Subjects(c.ctx()) })
}

func coursesScreen(c *call) error {
	answer, err := c.subjects()
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// putCourse is {name, title, abbr}: a manual entry (the connector's set_course).
func putCourse(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	changed, err := c.actChange("set_course", map[string]any{"name": body["name"], "title": body["title"],
		"abbr": body["abbr"]})
	if err != nil {
		return err
	}
	answer, err := c.subjects()
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

// removeCourse is {name}: the entry dropped; Haiku names the course again when a source brings it.
func removeCourse(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	changed, err := c.actChange("delete_course", map[string]any{"name": body["name"]})
	if err != nil {
		return err
	}
	answer, err := c.subjects()
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

// --- the LLM limit -------------------------------------------------------------------------------------------

func llmScreen(c *call) error {
	answer, err := c.mini(func(a Actions) (any, error) { return a.LLM(c.ctx()) })
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

func updateLLM(c *call) error {
	if c.app.Parts.Settings == nil {
		return unavailable()
	}
	return withBody(c, func(a Actions, body map[string]any) (any, error) { return a.UpdateLLM(c.ctx(), body) })
}

func raiseLLM(c *call) error {
	answer, err := c.mini(func(a Actions) (any, error) { return a.RaiseLLM(c.ctx()) })
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// --- the gradebooks ------------------------------------------------------------------------------------------

func (c *call) gradeSheets() (httpx.Object, error) {
	return c.mini(func(a Actions) (any, error) { return a.GradeSheets(c.ctx()) })
}

func (c *call) gradebook(sid string) (httpx.Object, error) {
	return c.mini(func(a Actions) (any, error) { return a.Gradebook(c.ctx(), sid) })
}

func gradesScreen(c *call) error {
	if !c.app.Parts.Grades {
		return unavailable()
	}
	answer, err := c.gradeSheets()
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// addGradebook is {url, label?, name?}: the connector's add_gradebook.
func addGradebook(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	arguments := map[string]any{"url": body["url"]}
	for _, key := range []string{"label", "name"} {
		if truthy(body[key]) {
			arguments[key] = body[key]
		}
	}
	changed, err := c.actChange("add_gradebook", arguments)
	if err != nil {
		return err
	}
	answer, err := c.gradeSheets()
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

func gradebook(c *call) error {
	if !c.app.Parts.Grades {
		return unavailable()
	}
	answer, err := c.gradebook(c.r.PathValue("sid"))
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// updateGradebook is {name?, label?}.
func updateGradebook(c *call) error {
	sid := c.r.PathValue("sid")
	body, err := c.body()
	if err != nil {
		return err
	}
	arguments := map[string]any{"sheet_id": sid}
	for key, value := range body {
		arguments[key] = value
	}
	changed, err := c.actChange("update_gradebook", arguments)
	if err != nil {
		return err
	}
	answer, err := c.gradebook(sid)
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

func removeGradebook(c *call) error {
	changed, err := c.actChange("remove_gradebook", map[string]any{"sheet_id": c.r.PathValue("sid")})
	if err != nil {
		return err
	}
	answer, err := c.gradeSheets()
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

// readGradebook reads the gradebooks at once instead of in 30 minutes; nothing to journal.
func readGradebook(c *call) error {
	if !c.app.Parts.Grades {
		return unavailable()
	}
	sid := c.r.PathValue("sid")
	answer, err := c.mini(func(a Actions) (any, error) { return a.ReadGradebook(c.ctx(), sid) })
	if err != nil {
		return err
	}
	return c.jsonLang(answer)
}

// setTab is {skip, works: [{column, label, max, bonus_column}]}: the owner's layout of a tab, over Haiku's.
func setTab(c *call) error {
	sid, tab := c.r.PathValue("sid"), c.r.PathValue("tab")
	body, err := c.body()
	if err != nil {
		return err
	}
	changed, err := c.actChange("set_tab_layout", map[string]any{"sheet_id": sid, "tab": tab, "skip": body["skip"],
		"works": body["works"]})
	if err != nil {
		return err
	}
	answer, err := c.gradebook(sid)
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

func resetTab(c *call) error {
	sid, tab := c.r.PathValue("sid"), c.r.PathValue("tab")
	changed, err := c.actChange("reset_tab_layout", map[string]any{"sheet_id": sid, "tab": tab})
	if err != nil {
		return err
	}
	answer, err := c.gradebook(sid)
	if err != nil {
		return err
	}
	return withChange(c, answer, changed)
}

// --- the Claude connector ------------------------------------------------------------------------------------

func (c *call) connectorState() (httpx.Object, error) {
	if c.app.Connector == nil {
		return nil, httpx.Fail(http.StatusServiceUnavailable, "the connector is not running")
	}
	state, err := c.app.Connector.State()
	if err != nil {
		return nil, err
	}
	out := httpx.Object{}
	for _, f := range state {
		if f.Key != "changes" { // the journal has them, with the app's own
			out = append(out, f)
		}
	}
	return out, nil
}

func connectorScreen(c *call) error {
	state, err := c.connectorState()
	if err != nil {
		return err
	}
	return c.jsonLang(state)
}

func revoke(c *call) error {
	if c.app.Connector == nil {
		return httpx.Fail(http.StatusServiceUnavailable, "the connector is not running")
	}
	grantID, err := strconv.ParseInt(c.r.PathValue("grant_id"), 10, 64)
	if err != nil {
		return httpx.NotFound("")
	}
	revoked, err := c.app.Connector.OAuth.RevokeGrant(grantID)
	if err != nil {
		return err
	}
	if !revoked {
		return httpx.NotFound("no such connection")
	}
	return connectorScreen(c)
}
