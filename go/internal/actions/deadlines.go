package actions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// Row is the chats', mail's or RUZ's row of a deadline of the list (its ref dl:<id>); nil for HSE App X and own ones.
func (a *Actions) Row(d todo.Deadline) (*deadlines.Deadline, error) {
	rest, ok := strings.CutPrefix(d.Ref, "dl:")
	if !ok || rest == "" || strings.ContainsFunc(rest, func(r rune) bool { return r < '0' || r > '9' }) {
		return nil, nil
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return nil, nil
	}
	return a.Deadlines.Deadlines.Get(id)
}

// origin is the chat or the letter a deadline of the chats or the mail came from, with the link to its message.
func (a *Actions) origin(d todo.Deadline) (httpx.Object, error) {
	none := httpx.Object{{Key: "from", Value: nil}, {Key: "link", Value: nil}, {Key: "message", Value: nil}}
	row, err := a.Row(d)
	if err != nil || row == nil || row.Source != "tg" && row.Source != "mail" {
		return none, err
	}
	told := schedule.OfDeadline(row)
	from, err := a.Schedule.ToldBy(told)
	if err != nil {
		return nil, err
	}
	link, err := a.Schedule.Link(told)
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "from", Value: from}, {Key: "link", Value: nonEmpty(link)},
		{Key: "message", Value: schedule.Ref(told)}}, nil
}

func (a *Actions) deadlinesState() (any, error) {
	merged := a.Deadlines
	today := merged.Today()
	visible, err := merged.Visible()
	if err != nil {
		return nil, err
	}
	items := []httpx.Object{}
	for _, d := range visible {
		origin, err := a.origin(d)
		if err != nil {
			return nil, err
		}
		item := httpx.Object{{Key: "uid", Value: todo.UID(d)}, {Key: "title", Value: d.Title},
			{Key: "due", Value: todo.DueText(d.Due)}, {Key: "category", Value: d.Category},
			{Key: "source", Value: d.Source}, {Key: "course", Value: d.Name},
			{Key: "overdue", Value: todo.DueText(d.Due) < todo.DueText(today)}, {Key: "assumed", Value: d.Assumed}}
		items = append(items, append(item, origin...))
	}
	ownList, err := merged.Todo.Own()
	if err != nil {
		return nil, err
	}
	own := []httpx.Object{}
	for _, d := range ownList {
		own = append(own, httpx.Object{{Key: "uid", Value: d.Key}, {Key: "title", Value: d.Title},
			{Key: "due", Value: todo.DueText(d.Due)}, {Key: "category", Value: d.Category}})
	}
	rows, err := merged.Todo.HiddenRows()
	if err != nil {
		return nil, err
	}
	hidden := []httpx.Object{}
	for _, row := range rows {
		hidden = append(hidden, httpx.Object{{Key: "uid", Value: row.UID}, {Key: "title", Value: row.Title},
			{Key: "due", Value: row.Due}, {Key: "source", Value: row.Source}})
	}
	hse, err := a.statusObject("hseapp")
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "items", Value: items}, {Key: "own", Value: own}, {Key: "hidden", Value: hidden},
		{Key: "today", Value: todo.DueText(today)},
		{Key: "hseapp", Value: httpx.Object{{Key: "login_needed", Value: truthy(field(hse, "login_needed"))},
			{Key: "error", Value: field(hse, "error")}}}}, nil
}

// ownFields are the title, due and category of an own deadline: every one of them, unless some.
type ownFields struct {
	title, category  *string
	due              time.Time
	hasDue, anything bool
}

func (a *Actions) ownFields(body map[string]any, some bool) (ownFields, error) {
	var f ownFields
	if _, ok := body["title"]; !some || ok {
		title, ok := body["title"].(string)
		if !ok || squeeze(title) == "" || pytext.Len(title) > TitleLimit {
			return f, a.badRequest(fmt.Sprintf("a title is needed, up to %d characters", TitleLimit),
				fmt.Sprintf("нужно название, до %d символов", TitleLimit))
		}
		f.title, f.anything = &title, true
	}
	if _, ok := body["due"]; !some || ok {
		text, _ := body["due"].(string)
		due, err := fromISODate(text)
		if err != nil {
			return f, a.badRequest("the due date is a date like 2026-10-22", "срок — дата вида 2026-10-22")
		}
		f.due, f.hasDue, f.anything = due, true, true
	}
	if _, ok := body["category"]; !some || ok {
		category, ok := body["category"].(string)
		if !ok || !slices.Contains(todo.Categories, category) {
			return f, a.badRequest("the list: study or personal", "список: учёба (study) или личное (personal)")
		}
		f.category, f.anything = &category, true
	}
	return f, nil
}

// Listed is a deadline of the list by uid; an own one also when the merge left it out as a copy of another. A 404
// when it is not there.
func (a *Actions) Listed(uid string) (todo.Deadline, error) {
	d, err := a.Deadlines.Find(uid)
	if err != nil {
		return todo.Deadline{}, err
	}
	if d != nil {
		return *d, nil
	}
	own, err := a.Deadlines.Todo.Own()
	if err != nil {
		return todo.Deadline{}, err
	}
	for _, d := range own {
		if d.Key == uid {
			return d, nil
		}
	}
	return todo.Deadline{}, httpx.NotFound(i18n.Tr(a.Lang(), "this deadline is off the list already",
		"этого дедлайна уже нет в списке"))
}

// Deadlines are the merged list as Tablo and HA get it, the own ones, the hidden ones.
func (a *Actions) DeadlinesList(_ context.Context) (any, error) { return a.deadlinesState() }

// AddDeadline is {title, due, category, course?}: a new own deadline; `course`, its key, comes from the web app
// (§8.6).
func (a *Actions) AddDeadline(_ context.Context, body map[string]any) (any, error) {
	f, err := a.ownFields(body, false)
	if err != nil {
		return nil, err
	}
	course := ""
	if value, ok := body["course"]; ok && value != nil {
		text, isText := value.(string)
		if !isText || pytext.Len(text) > CourseKeyLimit {
			return nil, httpx.BadRequest(fmt.Sprintf("course: up to %d characters", CourseKeyLimit))
		}
		course = text
	}
	if _, err := a.Deadlines.Add(*f.title, f.due, *f.category, course); err != nil {
		return nil, err
	}
	return a.deadlinesState()
}

// EditDeadline is {title?, due?, category?}: an own one changes; one from a source is hidden and an own copy takes
// its place. A sheet's assumed date (§6.3) given another date is the sheet's real date: with the title and list as
// they are, the deadline itself moves to it and stays the chats'; the sheet takes the date either way.
func (a *Actions) EditDeadline(_ context.Context, uid string, body map[string]any) (any, error) {
	f, err := a.ownFields(body, true)
	if err != nil {
		return nil, err
	}
	if !f.anything {
		return nil, httpx.BadRequest("expected {title?, due?, category?}")
	}
	d, err := a.Listed(uid)
	if err != nil {
		return nil, err
	}
	var sheetID *int64
	if f.hasDue {
		if sheetID, err = a.assumedSheet(d); err != nil {
			return nil, err
		}
	}
	if sheetID == nil || f.title != nil && *f.title != d.Title || f.category != nil && *f.category != d.Category {
		category := ""
		if f.category != nil {
			category = *f.category
		}
		if _, err := a.Deadlines.Edit(d, f.title, f.due, category); err != nil {
			return nil, err
		}
	}
	if sheetID != nil {
		due := todo.DueText(f.due)
		if _, err := a.Sheets.Edit(*sheetID, map[string]*string{"due": &due}); err != nil {
			return nil, err
		}
	}
	return a.deadlinesState()
}

// assumedSheet is the sheet whose assumed date a deadline is, when the sheets are wired.
func (a *Actions) assumedSheet(d todo.Deadline) (*int64, error) {
	if !d.Assumed || !strings.HasPrefix(d.Ref, "dl:") || a.Sheets == nil {
		return nil, nil
	}
	row, err := a.Row(d)
	if err != nil || row == nil {
		return nil, err
	}
	return row.SheetID, nil
}

// DeleteDeadline deletes an own one; one from a source is hidden for good (as in HA).
func (a *Actions) DeleteDeadline(_ context.Context, uid string) (any, error) {
	d, err := a.Listed(uid)
	if err != nil {
		return nil, err
	}
	if err := a.Deadlines.Remove(d, "", ""); err != nil {
		return nil, err
	}
	return a.deadlinesState()
}

// UnhideDeadline is «Restore»: one hidden row less; the deadline is back if its source still has it.
func (a *Actions) UnhideDeadline(_ context.Context, uid string) (any, error) {
	found, err := a.Deadlines.Unhide(uid)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, httpx.NotFound(i18n.Tr(a.Lang(), "no such hidden deadline", "такого скрытого дедлайна нет"))
	}
	return a.deadlinesState()
}

// --- sheets -----------------------------------------------------------------------------------------

func (a *Actions) sheetsState() (any, error) {
	if a.Sheets == nil || a.Settings == nil {
		return nil, errNotRunning
	}
	listing, err := a.Sheets.Sheets.Listing()
	if err != nil {
		return nil, err
	}
	status, err := a.required("sheets")
	if err != nil {
		return nil, err
	}
	limits := httpx.Object{}
	for _, name := range PDFLimits {
		value, err := a.Settings.Get(name)
		if err != nil {
			return nil, err
		}
		limits = append(limits, httpx.Field{Key: name, Value: value})
	}
	return httpx.Object{{Key: "sheets", Value: listing}, {Key: "status", Value: status},
		{Key: "limits", Value: limits}}, nil
}

// SheetsList is the sheets with their versions and the limits of reading them.
func (a *Actions) SheetsList(_ context.Context) (any, error) { return a.sheetsState() }

// UpdateSheets is {pdf_pages?, pdf_tokens?}: the limits of reading a PDF whole; both are checked before either is
// saved.
func (a *Actions) UpdateSheets(_ context.Context, body map[string]any) (any, error) {
	if len(body) == 0 || !only(body, PDFLimits...) {
		return nil, httpx.BadRequest("expected {pdf_pages?, pdf_tokens?}")
	}
	if a.Settings == nil {
		return nil, errNotRunning
	}
	checked, err := checkAll(body, PDFLimits, same)
	if err != nil {
		return nil, err
	}
	if err := a.setAll(checked); err != nil {
		return nil, err
	}
	return a.sheetsState()
}

// sheetFields are the owner's course, number, due date and summary of a sheet; an empty course or due date clears
// it.
func (a *Actions) sheetFields(body map[string]any) (map[string]*string, error) {
	fields := map[string]*string{}
	for _, limit := range []struct {
		name  string
		chars int
	}{{"course", SheetCourseLimit}, {"number", SheetNumberLimit}, {"summary", sheets.SummaryChars}} {
		value, ok := body[limit.name]
		if !ok {
			continue
		}
		s, isText := value.(string)
		if !isText || pytext.Len(s) > limit.chars {
			return nil, httpx.BadRequest(fmt.Sprintf("%s: text up to %d characters", limit.name, limit.chars))
		}
		if limit.name == "summary" {
			s = pytext.Strip(s)
		} else {
			s = squeeze(s)
		}
		if s == "" && limit.name != "number" {
			fields[limit.name] = nil
		} else {
			fields[limit.name] = &s
		}
	}
	if due, ok := body["due"]; ok {
		if due == nil || due == "" {
			fields["due"] = nil
		} else {
			s, _ := due.(string)
			day, err := fromISODate(s)
			if err != nil {
				return nil, a.badRequest("the due date is a date like 2026-10-22", "срок — дата вида 2026-10-22")
			}
			text := todo.DueText(day)
			fields["due"] = &text
		}
	}
	return fields, nil
}

// EditSheet is some of {course, number, due, summary}: the owner's change; the caption of its message follows.
func (a *Actions) EditSheet(_ context.Context, sheetID string, body map[string]any) (any, error) {
	if len(body) == 0 || !only(body, "course", "number", "due", "summary") {
		return nil, httpx.BadRequest("expected some of course, number, due, summary")
	}
	id, err := pyInt(sheetID)
	if err != nil {
		return nil, httpx.NotFound("")
	}
	fields, err := a.sheetFields(body)
	if err != nil {
		return nil, err
	}
	if a.Sheets == nil {
		return nil, errNotRunning
	}
	sheet, err := a.Sheets.Edit(id, fields)
	if err != nil {
		return nil, err
	}
	if sheet == nil {
		return nil, httpx.NotFound("no such sheet")
	}
	return a.sheetsState()
}

// --- HSE App X -------------------------------------------------------------------------------------

func (a *Actions) hseappState() (any, error) {
	if a.Services.HseApp == nil {
		return nil, errNotRunning
	}
	status, err := a.required("hseapp")
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "status", Value: status}, {Key: "callback", Value: a.Services.HseApp.CallbackURL}}, nil
}

// HseApp is the source's status (never the tokens) and where the browser comes back to.
func (a *Actions) HseApp(_ context.Context) (any, error) { return a.hseappState() }

// UpdateHseApp is {enabled: bool}: the general setting hseapp_enabled.
func (a *Actions) UpdateHseApp(_ context.Context, body map[string]any) (any, error) {
	enabled, ok := body["enabled"].(bool)
	if !exactly(body, "enabled") || !ok {
		return nil, httpx.BadRequest("expected {enabled: true|false}")
	}
	if a.Services.HseApp == nil {
		return nil, errNotRunning
	}
	if err := a.Services.HseApp.SetEnabled(enabled); err != nil {
		return nil, settingError(err)
	}
	return a.hseappState()
}

// HseAppLink is {app?: bool} -> {url}: a login link that comes back to our callback, or with `app` to the app's own
// address (its return address is pasted by hand). Each link holds a PKCE state; the last five work.
func (a *Actions) HseAppLink(_ context.Context, body map[string]any) (any, error) {
	app := false
	if value, ok := body["app"]; ok {
		flag, isBool := value.(bool)
		if !isBool {
			return nil, httpx.BadRequest("expected {app: true|false}")
		}
		app = flag
	}
	if a.Services.HseApp == nil {
		return nil, errNotRunning
	}
	return httpx.Object{{Key: "url", Value: a.Services.HseApp.LoginLink(app)}}, nil
}

// HseAppLogin is {url}: the return address pasted by hand, when the browser did not come back by itself.
func (a *Actions) HseAppLogin(ctx context.Context, body map[string]any) (any, error) {
	returned, ok := body["url"].(string)
	if !ok || pytext.Strip(returned) == "" {
		return nil, a.badRequest("paste the address the browser came back to",
			"вставьте адрес, на который вернулся браузер")
	}
	if a.Services.HseApp == nil {
		return nil, errNotRunning
	}
	if err := a.Services.HseApp.Login(ctx, returned); err != nil {
		var refused *i18n.Error
		if errors.As(err, &refused) {
			return nil, httpx.BadRequest(i18n.Reason(err, a.Lang()))
		}
		return nil, err
	}
	return a.hseappState()
}
