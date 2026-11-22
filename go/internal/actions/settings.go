package actions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

func pyjsonDumps(value any) (string, error) { return pyjson.Dumps(httpx.PyValue(value)) }

// settingError is a SettingsError's refusal (str(e)), anything else as it is.
func settingError(err error) error {
	var bad *settings.Error
	if errors.As(err, &bad) {
		return httpx.BadRequest(err.Error())
	}
	return err
}

// --- the chats -------------------------------------------------------------------------------------

// Chats are every chat of the twin with its settings, enabled ones first, and the twin's, mail's and images'
// status.
func (a *Actions) Chats(_ context.Context) (any, error) {
	stats, err := a.Inbox.Chats()
	if err != nil {
		return nil, err
	}
	listed := []httpx.Object{}
	enabled := map[int]bool{}
	for i, chat := range stats {
		all, err := a.ChatSettings.All(chat.ID)
		if err != nil {
			return nil, err
		}
		o, _ := httpx.Localize(chat, "en").(httpx.Object)
		listed = append(listed, append(o, httpx.Field{Key: "settings", Value: all}))
		enabled[i] = all.Enabled
	}
	order := make([]int, len(listed))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(x, y int) int { return boolKey(!enabled[x]) - boolKey(!enabled[y]) })
	sorted := make([]httpx.Object, len(listed))
	for i, n := range order {
		sorted[i] = listed[n]
	}
	twin, err := a.required("telegram")
	if err != nil {
		return nil, err
	}
	mail, err := a.statusOr("mail")
	if err != nil {
		return nil, err
	}
	images, err := a.statusOr("images")
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "twin", Value: twin}, {Key: "mail", Value: mail}, {Key: "images", Value: images},
		{Key: "chats", Value: sorted}}, nil
}

func boolKey(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpdateImages is {daily}: how many images are read a day in chats with «Картинки» (D12); 0 reads none.
func (a *Actions) UpdateImages(_ context.Context, body map[string]any) (any, error) {
	if !exactly(body, "daily") {
		return nil, httpx.BadRequest("expected {daily}")
	}
	if a.Settings == nil {
		return nil, errNotRunning
	}
	daily, err := a.Settings.Set("images_daily", body["daily"])
	if err != nil {
		return nil, settingError(err)
	}
	value, ok, err := a.status("images")
	if !ok {
		return httpx.Object{{Key: "daily", Value: daily}}, nil
	}
	return value, err
}

// chatID is the chat of a path: 404 for no number, "no such chat" for a chat the twin does not know.
func (a *Actions) chatID(text string) (int64, error) {
	id, err := pyInt(text)
	if err != nil {
		return 0, httpx.NotFound("")
	}
	chat, err := a.Inbox.Chat(id)
	if err != nil {
		return 0, err
	}
	if chat == nil {
		return 0, httpx.NotFound("no such chat")
	}
	return id, nil
}

// UpdateChat changes some settings of a chat: {name: value}; all of them are checked before any is saved.
func (a *Actions) UpdateChat(_ context.Context, chatID string, body map[string]any) (any, error) {
	id, err := a.chatID(chatID)
	if err != nil {
		return nil, err
	}
	all, err := a.ChatSettings.Update(id, body)
	if err != nil {
		return nil, settingError(err)
	}
	return all, nil
}

// Senders are who wrote in the chat, to pick the important people; chosen ones who have not written yet are
// listed too.
func (a *Actions) Senders(_ context.Context, chatID string) (any, error) {
	id, err := a.chatID(chatID)
	if err != nil {
		return nil, err
	}
	listed, err := a.Inbox.Senders(id)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, sender := range listed {
		seen[sender.ID] = true
	}
	all, err := a.ChatSettings.All(id)
	if err != nil {
		return nil, err
	}
	for _, person := range all.People {
		if !seen[person] {
			listed = append(listed, inbox.Sender{ID: person})
		}
	}
	if listed == nil {
		listed = []inbox.Sender{}
	}
	return listed, nil
}

// --- the course dictionary -----------------------------------------------------------------------

// Subjects is the course dictionary: every entry; Haiku's can be overridden by hand, manual ones edited or deleted.
func (a *Actions) Subjects(_ context.Context) (any, error) {
	all, err := a.Services.Subjects.All()
	if err != nil {
		return nil, err
	}
	entries := []httpx.Object{}
	for _, s := range all {
		entries = append(entries, httpx.Object{{Key: "name", Value: s.Name}, {Key: "title", Value: s.Title},
			{Key: "abbr", Value: s.Abbr}, {Key: "manual", Value: s.Manual}})
	}
	var failure any
	if text := a.Services.Subjects.LastError(); text != "" {
		failure = text
	}
	return httpx.Object{{Key: "subjects", Value: entries}, {Key: "error", Value: failure},
		{Key: "title_chars", Value: subjects.TitleChars}, {Key: "abbr_chars", Value: subjects.AbbrChars}}, nil
}

func text(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

// PutSubject adds or changes an entry by hand: {name, title, abbr}; it becomes manual.
func (a *Actions) PutSubject(_ context.Context, body map[string]any) (any, error) {
	entry, err := a.Services.Subjects.SetManual(text(body, "name"), text(body, "title"), text(body, "abbr"))
	var refused *subjects.Error
	if errors.As(err, &refused) {
		return nil, httpx.BadRequest(i18n.Reason(err, a.Lang()))
	}
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// DeleteSubject drops an entry: {name}. Haiku names the course again when a source brings it next time.
func (a *Actions) DeleteSubject(_ context.Context, body map[string]any) (any, error) {
	name, ok := body["name"].(string)
	if !ok {
		return nil, httpx.NotFound("no such entry")
	}
	deleted, err := a.Services.Subjects.Delete(name)
	if err != nil {
		return nil, err
	}
	if !deleted {
		return nil, httpx.NotFound("no such entry")
	}
	return httpx.Object{{Key: "deleted", Value: name}}, nil
}

// --- the twin's account --------------------------------------------------------------------------

// TwinLogin is {phone}: ask for a code; {code}, then {password} if the account has one: sign in. It answers the
// twin's status.
func (a *Actions) TwinLogin(ctx context.Context, body map[string]any) (any, error) {
	if a.Twin == nil {
		return nil, errNotRunning
	}
	var err error
	switch {
	case exactly(body, "phone"):
		err = a.Twin.SendCode(ctx, pyStr(body["phone"]))
	case exactly(body, "code"):
		err = a.Twin.SignIn(ctx, pyStr(body["code"]), "")
	case exactly(body, "password"):
		err = a.Twin.SignIn(ctx, "", pyStr(body["password"]))
	default:
		return nil, httpx.BadRequest("expected {phone}, {code} or {password}")
	}
	if err != nil {
		return nil, a.loginError(err)
	}
	return a.Twin.Status(), nil
}

func (a *Actions) loginError(err error) error {
	var refused *telegram.LoginError
	if errors.As(err, &refused) {
		return httpx.BadRequest(i18n.Reason(err, a.Lang()))
	}
	return err
}

// TwinCancel drops a login begun.
func (a *Actions) TwinCancel(_ context.Context) (any, error) {
	if a.Twin == nil {
		return nil, errNotRunning
	}
	a.Twin.CancelLogin()
	return a.Twin.Status(), nil
}

// TwinLogout logs the twin out.
func (a *Actions) TwinLogout(ctx context.Context) (any, error) {
	if a.Twin == nil {
		return nil, errNotRunning
	}
	if err := a.Twin.Logout(ctx); err != nil {
		return nil, a.loginError(err)
	}
	return a.Twin.Status(), nil
}

// --- general settings ----------------------------------------------------------------------------

func (a *Actions) generalState() (any, error) {
	out := httpx.Object{}
	for _, name := range General {
		value, err := a.Settings.Get(name)
		if err != nil {
			return nil, err
		}
		out = append(out, httpx.Field{Key: name, Value: value})
	}
	return append(out, httpx.Field{Key: "batch_choices", Value: settings.BatchHours},
		httpx.Field{Key: "languages", Value: i18n.Languages}), nil
}

// General is the general settings, the batch choices and the languages.
func (a *Actions) General(_ context.Context) (any, error) {
	if a.Settings == nil {
		return nil, errNotRunning
	}
	return a.generalState()
}

// checkAll checks the values of the names (in the names' order) before any is saved: the values to keep.
func checkAll(body map[string]any, names []string, name func(string) string) ([]httpx.Field, error) {
	checked := []httpx.Field{}
	for _, key := range names {
		value, ok := body[key]
		if !ok {
			continue
		}
		kept, err := settings.Check(name(key), value)
		if err != nil {
			return nil, settingError(err)
		}
		checked = append(checked, httpx.Field{Key: name(key), Value: kept})
	}
	return checked, nil
}

func same(name string) string { return name }

func (a *Actions) setAll(checked []httpx.Field) error {
	for _, f := range checked {
		if _, err := a.Settings.Set(f.Key, f.Value); err != nil {
			return settingError(err)
		}
	}
	return nil
}

// UpdateGeneral is any of batch_hours, news_ttl_hours, llm_prices, the language, about_me and wiki_pages; all are
// checked before any is saved.
func (a *Actions) UpdateGeneral(_ context.Context, body map[string]any) (any, error) {
	if len(body) == 0 || !only(body, General...) {
		return nil, httpx.BadRequest("expected some of " + strings.Join(General, ", "))
	}
	if a.Settings == nil {
		return nil, errNotRunning
	}
	checked, err := checkAll(body, General, same)
	if err != nil {
		return nil, err
	}
	if err := a.setAll(checked); err != nil {
		return nil, err
	}
	return a.generalState()
}

// --- the deadline reminders ----------------------------------------------------------------------

func (a *Actions) remindersState() (any, error) {
	out := httpx.Object{}
	for _, f := range ReminderFields {
		value, err := a.Settings.Get(f.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, httpx.Field{Key: f.Key, Value: value})
	}
	return out, nil
}

// ReminderSetting is the setting of a reminders' field ("" for none).
func ReminderSetting(key string) string {
	for _, f := range ReminderFields {
		if f.Key == key {
			return f.Name
		}
	}
	return ""
}

func reminderKeys() []string {
	out := []string{}
	for _, f := range ReminderFields {
		out = append(out, f.Key)
	}
	return out
}

// ReminderSettings are the reminders (D47): on or off, the eve's time and the morning's.
func (a *Actions) ReminderSettings(_ context.Context) (any, error) {
	if a.Settings == nil {
		return nil, errNotRunning
	}
	return a.remindersState()
}

// UpdateReminders is some of {enabled, eve: "HH:MM", day: "HH:MM"}; the loop waits for the new times at once.
func (a *Actions) UpdateReminders(_ context.Context, body map[string]any) (any, error) {
	if len(body) == 0 || !only(body, reminderKeys()...) {
		return nil, httpx.BadRequest("expected some of enabled, eve, day")
	}
	if a.Settings == nil {
		return nil, errNotRunning
	}
	checked, err := checkAll(body, reminderKeys(), ReminderSetting)
	if err != nil {
		return nil, err
	}
	if err := a.setAll(checked); err != nil {
		return nil, err
	}
	if a.Reminders == nil {
		return nil, errNotRunning
	}
	a.Reminders.Reschedule()
	return a.remindersState()
}

// --- the LLM spending ----------------------------------------------------------------------------

func (a *Actions) llmState() (any, error) {
	budget, lang := a.Budget, a.Lang()
	now := budget.Now() // one clock with the limit
	chatTitle := func(chatID int64) (string, error) {
		if chatID == inbox.Mailbox {
			return i18n.Tr(lang, "Mail", "Почта"), nil
		}
		chat, err := a.Inbox.Chat(chatID)
		if err != nil {
			return "", err
		}
		if chat != nil {
			return chat.Title, nil
		}
		return i18n.Tr(lang, fmt.Sprintf("chat %d", chatID), fmt.Sprintf("чат %d", chatID)), nil
	}
	usage, err := a.Journal.Usage(now)
	if err != nil {
		return nil, err
	}
	periods := httpx.Object{}
	for _, p := range []struct {
		name   string
		totals llm.Totals
	}{{"today", usage.Today}, {"week", usage.Week}, {"month", usage.Month}} {
		byTask, byChat := []httpx.Object{}, []httpx.Object{}
		for _, row := range p.totals.ByTask {
			byTask = append(byTask, append(httpx.Object{{Key: "task", Value: row.Task}}, entry(row.Entry)...))
		}
		for _, row := range p.totals.ByChat {
			title, err := chatTitle(row.ChatID)
			if err != nil {
				return nil, err
			}
			byChat = append(byChat, append(httpx.Object{{Key: "id", Value: row.ChatID}, {Key: "title", Value: title}},
				entry(row.Entry)...))
		}
		periods = append(periods, httpx.Field{Key: p.name, Value: append(entry(p.totals.Entry),
			httpx.Field{Key: "by_task", Value: byTask}, httpx.Field{Key: "by_chat", Value: byChat})})
	}
	today := clock.LocalDay(now)
	monthDays := time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	stopped, err := budget.StoppedDays()
	if err != nil {
		return nil, err
	}
	daily, err := a.Journal.Daily(now, ChartDays)
	if err != nil {
		return nil, err
	}
	days := []httpx.Object{}
	for _, day := range daily {
		_, stop := stopped[day.Day]
		days = append(days, httpx.Object{{Key: "day", Value: day.Day}, {Key: "runs", Value: day.Runs},
			{Key: "failed", Value: day.Failed}, {Key: "cost_usd", Value: day.CostUSD}, {Key: "stopped", Value: stop}})
	}
	status, err := budget.Status()
	if err != nil {
		return nil, err
	}
	limit, err := budget.Settings.Get("llm_daily_limit_usd")
	if err != nil {
		return nil, err
	}
	forecast := settings.Round(float64(usage.Month.CostUSD)/float64(today.Day())*float64(monthDays), 4)
	return httpx.Object{{Key: "spent_usd", Value: status.SpentUSD}, {Key: "limit_usd", Value: status.LimitUSD},
		{Key: "raised_usd", Value: status.RaisedUSD}, {Key: "level", Value: status.Level},
		{Key: "daily_limit_usd", Value: limit}, {Key: "soft_share", Value: pyjson.Float(llm.SoftShare)},
		{Key: "periods", Value: periods}, {Key: "days", Value: days},
		{Key: "month_forecast_usd", Value: pyjson.Float(forecast)}}, nil
}

func entry(e llm.Entry) httpx.Object {
	return httpx.Object{{Key: "runs", Value: e.Runs}, {Key: "failed", Value: e.Failed},
		{Key: "cost_usd", Value: e.CostUSD}, {Key: "tokens", Value: e.Tokens}}
}

// LLM is the LLM spending: today, the week, the month, by task and chat, and the limit.
func (a *Actions) LLM(_ context.Context) (any, error) { return a.llmState() }

// UpdateLLM is {daily_limit_usd}: the limit of every day; today's raise stays on top of it.
func (a *Actions) UpdateLLM(_ context.Context, body map[string]any) (any, error) {
	if !exactly(body, "daily_limit_usd") {
		return nil, httpx.BadRequest("expected {daily_limit_usd}")
	}
	if a.Settings == nil {
		return nil, errNotRunning
	}
	if _, err := a.Settings.Set("llm_daily_limit_usd", body["daily_limit_usd"]); err != nil {
		return nil, settingError(err)
	}
	if err := a.Budget.LimitChanged(); err != nil {
		return nil, err
	}
	return a.llmState()
}

// RaiseLLM is one more daily limit until midnight («Machine → LLM»).
func (a *Actions) RaiseLLM(_ context.Context) (any, error) {
	limit, err := a.Budget.Settings.Float("llm_daily_limit_usd")
	if err != nil {
		return nil, err
	}
	if _, err := a.Budget.RaiseToday(limit); err != nil {
		return nil, err
	}
	return a.llmState()
}
