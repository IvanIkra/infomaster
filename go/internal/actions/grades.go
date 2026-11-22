package actions

import (
	"context"
	"errors"
	"fmt"

	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// gradesRead is the grades' source status in the app's language: {sheets: {sid: read}, llm}.
func (a *Actions) gradesRead() (httpx.Object, httpx.Object, error) {
	status, err := a.statusObject("grades")
	if err != nil {
		return nil, nil, err
	}
	read, _ := field(status, "sheets").(httpx.Object)
	return status, read, nil
}

// SheetMap is a grade sheet as the settings hold it: {url, label, name?}.
func SheetMap(s settings.GradeSheet) map[string]any {
	out := map[string]any{"url": s.URL, "label": s.Label}
	if s.Name != "" {
		out["name"] = s.Name
	}
	return out
}

func (a *Actions) gradesState() (any, error) {
	g := a.Grades
	if g == nil {
		return nil, errNotRunning
	}
	status, read, err := a.gradesRead()
	if err != nil {
		return nil, err
	}
	configured, err := g.Settings.GradeSheets()
	if err != nil {
		return nil, err
	}
	sheets := []httpx.Object{}
	for _, sheet := range configured {
		sid, err := collector.SheetID(sheet.URL)
		if err != nil {
			return nil, err
		}
		items, err := g.Items(sid)
		if err != nil {
			return nil, err
		}
		o := httpx.Object{{Key: "id", Value: sid}, {Key: "url", Value: sheet.URL}, {Key: "label", Value: sheet.Label},
			{Key: "name", Value: sheet.Name}}
		if fields, ok := field(read, sid).(httpx.Object); ok {
			o = spread(o, fields)
		}
		sheets = append(sheets, spread(o, httpx.Object{{Key: "grades", Value: items}}))
	}
	student, err := g.Settings.Get("grades_student")
	if err != nil {
		return nil, err
	}
	window, err := g.Settings.Get("grades_window_days")
	if err != nil {
		return nil, err
	}
	manual, err := g.Manual()
	if err != nil {
		return nil, err
	}
	model := any(true)
	if has(status, "llm") {
		model = field(status, "llm")
	}
	return httpx.Object{{Key: "student", Value: student}, {Key: "window_days", Value: window},
		{Key: "llm", Value: model}, {Key: "sheets", Value: sheets}, {Key: "manual", Value: manual},
		{Key: "label_chars", Value: settings.SheetLabelChars}}, nil
}

// spread is {**o, **more}: a key there already keeps its place and takes the new value.
func spread(o httpx.Object, more httpx.Object) httpx.Object {
	out := append(httpx.Object{}, o...)
	for _, f := range more {
		replaced := false
		for i := range out {
			if out[i].Key == f.Key {
				out[i].Value, replaced = f.Value, true
			}
		}
		if !replaced {
			out = append(out, f)
		}
	}
	return out
}

// GradeSheets are the grades: the name, the sheets with what was read in them, the window of new grades.
func (a *Actions) GradeSheets(_ context.Context) (any, error) { return a.gradesState() }

// gradeSetting is the setting of a grades' field ("" for none).
func gradeSetting(key string) string {
	for _, f := range GradeSettings {
		if f.Key == key {
			return f.Name
		}
	}
	return ""
}

// UpdateGrades is {student?, sheets?: [{url, label, name?}], window_days?}: all checked before any is saved; a sheet
// removed, or all of them for a new name, start again from a baseline. The sheets are read at once.
func (a *Actions) UpdateGrades(_ context.Context, body map[string]any) (any, error) {
	if len(body) == 0 || !only(body, "student", "sheets", "window_days") {
		return nil, httpx.BadRequest("expected {student?, sheets?, window_days?}")
	}
	if a.Grades == nil {
		return nil, errNotRunning
	}
	values := map[string]any{}
	for key, value := range body {
		values[gradeSetting(key)] = value
	}
	if err := a.Grades.Configure(values); err != nil {
		var bad *settings.Error
		if errors.As(err, &bad) { // a sheet's link: a Phrase
			return nil, httpx.BadRequest(i18n.Reason(err, a.Lang()))
		}
		return nil, err
	}
	return a.gradesState()
}

// configured is the grade sheets as set, and the index of the one of the id; 404 for a sheet not set.
func (a *Actions) configured(sid string) ([]settings.GradeSheet, int, error) {
	if a.Grades == nil {
		return nil, 0, errNotRunning
	}
	sheets, err := a.Grades.Settings.GradeSheets()
	if err != nil {
		return nil, 0, err
	}
	for i, sheet := range sheets {
		if id, err := collector.SheetID(sheet.URL); err == nil && id == sid {
			return sheets, i, nil
		}
	}
	return nil, 0, httpx.NotFound("no such grade sheet")
}

// gradebookState is a gradebook (D44): its name and course, how its last read went, and each tab with the own
// row's cells, the works laid out in them, and whose the layout is.
func (a *Actions) gradebookState(sid string) (any, error) {
	sheets, i, err := a.configured(sid)
	if err != nil {
		return nil, err
	}
	sheet, g := sheets[i], a.Grades
	_, reads, err := a.gradesRead()
	if err != nil {
		return nil, err
	}
	read, _ := field(reads, sid).(httpx.Object)
	view, err := g.View(sid)
	if err != nil {
		return nil, err
	}
	course := view.Course
	if course == "" {
		var title *string
		if t, ok := field(read, "title").(string); ok {
			title = &t
		}
		course = collector.ShortCourse(sheet.Label, title, sid)
	}
	items, err := g.Items(sid)
	if err != nil {
		return nil, err
	}
	student, err := g.Settings.Get("grades_student")
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "id", Value: sid}, {Key: "url", Value: sheet.URL}, {Key: "label", Value: sheet.Label},
		{Key: "name", Value: sheet.Name}, {Key: "title", Value: field(read, "title")},
		{Key: "ok_at", Value: field(read, "ok_at")}, {Key: "error", Value: field(read, "error")},
		{Key: "student", Value: student}, {Key: "course", Value: course},
		{Key: "label_chars", Value: settings.SheetLabelChars}, {Key: "label_budget", Value: grades.Budget(course)},
		{Key: "items", Value: len(items)}, {Key: "tabs", Value: view.Tabs}}, nil
}

// Gradebook is one gradebook.
func (a *Actions) Gradebook(_ context.Context, sid string) (any, error) { return a.gradebookState(sid) }

// UpdateGradebook is {name?, label?}: the owner's name of a gradebook and the course in front of its grades.
func (a *Actions) UpdateGradebook(_ context.Context, sid string, body map[string]any) (any, error) {
	if len(body) == 0 || !only(body, "name", "label") {
		return nil, httpx.BadRequest("expected {name?, label?}")
	}
	sheets, i, err := a.configured(sid)
	if err != nil {
		return nil, err
	}
	listed := []any{}
	for n, sheet := range sheets {
		m := SheetMap(sheet)
		if n == i {
			for key, value := range body {
				m[key] = value
			}
		}
		listed = append(listed, m)
	}
	if err := a.Grades.Configure(map[string]any{"grades_sheets": listed}); err != nil {
		return nil, settingError(err)
	}
	return a.gradebookState(sid)
}

// ReadGradebook is «Read now»: the gradebooks are read at once, not in 30 minutes.
func (a *Actions) ReadGradebook(_ context.Context, sid string) (any, error) {
	if _, _, err := a.configured(sid); err != nil {
		return nil, err
	}
	if a.Grades.Wake != nil {
		a.Grades.Wake()
	}
	return a.gradebookState(sid)
}

// layoutItems are {skip, works: [{column, label, max, bonus_column}]} of the owner's layout of a tab.
func layoutItems(body map[string]any) (bool, []grades.LayoutItem, error) {
	skip, isBool := body["skip"].(bool)
	listed, isList := body["works"].([]any)
	if !exactly(body, "skip", "works") || !isBool || !isList {
		return false, nil, httpx.BadRequest("expected {skip, works: [{column, label, max, bonus_column}]}")
	}
	items, columns := []grades.LayoutItem{}, map[string]bool{}
	bad := httpx.BadRequest("a work: {column, label, max > 0, bonus_column or null}, each column once")
	for _, value := range listed {
		work, ok := value.(map[string]any)
		if !ok || !exactly(work, "column", "label", "max", "bonus_column") {
			return false, nil, bad
		}
		column, okColumn := work["column"].(string)
		label, okLabel := work["label"].(string)
		var maximum any
		switch m := work["max"].(type) {
		case int64:
			if m > 0 {
				maximum = m
			}
		case float64:
			if m > 0 {
				maximum = pyjson.Float(m)
			}
		}
		var bonus *string
		switch b := work["bonus_column"].(type) {
		case nil:
		case string:
			bonus = &b
		default:
			return false, nil, bad
		}
		if !okColumn || !okLabel || maximum == nil || columns[column] {
			return false, nil, bad
		}
		columns[column] = true
		items = append(items, grades.LayoutItem{Column: column, Label: squeeze(label), Max: maximum,
			BonusColumn: bonus})
	}
	return skip, items, nil
}

// SetLayout is the owner's layout of a tab: it stands over Haiku's; the gradebook is read again at once.
func (a *Actions) SetLayout(_ context.Context, sid, tab string, body map[string]any) (any, error) {
	if _, _, err := a.configured(sid); err != nil {
		return nil, err
	}
	skip, items, err := layoutItems(body)
	if err != nil {
		return nil, err
	}
	found, err := a.Grades.SetLayout(sid, tab, skip, items)
	var refused *i18n.Error
	if errors.As(err, &refused) {
		return nil, httpx.BadRequest(i18n.Reason(err, a.Lang()))
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, httpx.NotFound("no such tab")
	}
	return a.gradebookState(sid)
}

// ClearLayout gives the tab back to Haiku.
func (a *Actions) ClearLayout(_ context.Context, sid, tab string) (any, error) {
	if _, _, err := a.configured(sid); err != nil {
		return nil, err
	}
	found, err := a.Grades.ClearLayout(sid, tab)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, httpx.NotFound("no such tab")
	}
	return a.gradebookState(sid)
}

// gradeFields are the course and the label (the ones named) and the value of a grade given by hand.
func (a *Actions) gradeFields(body map[string]any, names ...string) (map[string]string, float64, error) {
	fields := map[string]string{}
	for _, limit := range []struct {
		name  string
		chars int
	}{{"course", settings.SheetLabelChars}, {"label", GradeLabelLimit}} {
		wanted := false
		for _, name := range names {
			wanted = wanted || name == limit.name
		}
		if !wanted {
			continue
		}
		value, ok := body[limit.name].(string)
		if !ok || squeeze(value) == "" || pytext.Len(value) > limit.chars {
			return nil, 0, a.badRequest(fmt.Sprintf("%s: up to %d characters", limit.name, limit.chars),
				fmt.Sprintf("%s: до %d символов", limit.name, limit.chars))
		}
		fields[limit.name] = squeeze(value)
	}
	var value float64
	switch v := body["value"].(type) {
	case int64:
		value = float64(v)
	case float64:
		value = v
	default:
		return nil, 0, a.badRequest("a grade from 0 to 10", "оценка от 0 до 10")
	}
	if !(value >= 0 && value <= 10) {
		return nil, 0, a.badRequest("a grade from 0 to 10", "оценка от 0 до 10")
	}
	return fields, settings.Round(value, 2), nil
}

// AddGrade is {course, label, value}: a grade added by hand.
func (a *Actions) AddGrade(_ context.Context, body map[string]any) (any, error) {
	if !exactly(body, "course", "label", "value") {
		return nil, httpx.BadRequest("expected {course, label, value}")
	}
	fields, value, err := a.gradeFields(body, "course", "label")
	if err != nil {
		return nil, err
	}
	if a.Grades == nil {
		return nil, errNotRunning
	}
	if _, err := a.Grades.AddGrade(fields["course"], fields["label"], value); err != nil {
		return nil, err
	}
	return a.gradesState()
}

// ChangeGrade is {value, label?, course?}: a grade added by hand changed, or a sheet's grade changed by the owner.
func (a *Actions) ChangeGrade(_ context.Context, grade string, body map[string]any) (any, error) {
	if _, ok := body["value"]; !ok || !only(body, "value", "label", "course") {
		return nil, httpx.BadRequest("expected {value, label?, course?}")
	}
	fields, value, err := a.gradeFields(body, keys(body)...)
	if err != nil {
		return nil, err
	}
	if a.Grades == nil {
		return nil, errNotRunning
	}
	found, err := a.Grades.ChangeGrade(grade, fields["label"], value, fields["course"])
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, httpx.NotFound("no such grade")
	}
	return a.gradesState()
}

// DropGrade deletes a grade added by hand, or brings a sheet's grade back as the sheet has it.
func (a *Actions) DropGrade(_ context.Context, grade string) (any, error) {
	if a.Grades == nil {
		return nil, errNotRunning
	}
	found, err := a.Grades.DropGrade(grade)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, httpx.NotFound("no such grade")
	}
	return a.gradesState()
}
