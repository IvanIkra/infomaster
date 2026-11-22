package grades

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Grades on synthetic sheets shaped like the real ones (names are made up): the workbooks are the collector's
// testdata, written by openpyxl from the Python tests' builders.

const (
	url   = "https://docs.google.com/spreadsheets/d/abc123/edit#gid=0"
	other = "https://docs.google.com/spreadsheets/d/zzz999/edit"
	me    = "Иванов Иван"
)

func workbook(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "collectors", "grades", "testdata", name+".xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func views(t *testing.T, name string) []collector.TabView {
	t.Helper()
	found, err := collector.ReadViews(workbook(t, name), me)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func title(s string) *string { return &s }

func sheet(t *testing.T, name string) collector.Sheet {
	return collector.Sheet{ID: "abc123", URL: url, Title: title("ДМ ПИ"), Course: "DM", Student: me, Views: views(t, name)}
}

// spec is the layout Haiku would give, by the tab and where its columns are in the header.
func spec(prompt string) map[string]any {
	tab := strings.Split(prompt, `"`)[1]
	if tab == "ДЗ-1А" {
		return map[string]any{"skip": false, "reason": "", "header_rows": []int{1, 2}, "items": []any{
			map[string]any{"column": "F", "label": "HW 1A", "max": 7, "bonus_column": "G"}}}
	}
	shift := 0
	if strings.Contains(prompt, "C=Средний балл") {
		shift = 1
	}
	return map[string]any{"skip": false, "reason": "", "header_rows": []int{1, 2, 3, 99}, "items": []any{ // 99: not a row it was shown
		map[string]any{"column": string("EF"[shift]), "label": "Quiz 1", "max": 10, "bonus_column": nil},
		map[string]any{"column": string("GH"[shift]), "label": "Quiz 2", "max": 10, "bonus_column": nil}}}
}

type fakeLLM struct {
	*llm.Fake
	mu      sync.Mutex
	prompts []string
	err     error
}

func newLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{Fake: &llm.Fake{}}
	f.Respond = func(req llm.Request) (any, error) {
		if req.Task != "grades" || strings.Contains(req.System, "{budget}") {
			t.Errorf("a wrong request: %s", req.Task)
		}
		if req.System == ExplainSystem {
			return map[string]any{"cells": []any{}}, nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prompts = append(f.prompts, req.Prompt)
		if f.err != nil {
			return nil, f.err
		}
		return spec(req.Prompt), nil
	}
	return f
}

type testClock struct{ moment time.Time }

func (c *testClock) now() time.Time { return c.moment }

type env struct {
	grades   *Grades
	settings *settings.Settings
	llm      *fakeLLM
	clock    *testClock
}

func setup(t *testing.T) env {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := settings.New(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("grades_student", me)
	s.Set("grades_sheets", []any{map[string]any{"url": url, "label": "DM"}})
	model := newLLM(t)
	now := &testClock{time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
	g := New(db, s, model)
	g.Now = now.now
	return env{g, s, model, now}
}

func (e env) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := e.grades.DB.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tab(found []collector.TabView, name string) collector.TabView {
	for _, v := range found {
		if v.Tab == name {
			return v
		}
	}
	return collector.TabView{}
}

// --- reading ------------------------------------------------------------------------------------------------

func TestTheLayoutPromptKeepsTheTopRowsAndNotTheName(t *testing.T) {
	dz := tab(views(t, "book_default"), "ДЗ-1А")
	prompt := LayoutPrompt(dz)
	if !strings.Contains(prompt, "row 3: A=260 | B=Образцов Нагляд Примерович") { // the model tells the header from students
		t.Fatal(prompt)
	}
	if !strings.HasSuffix(prompt, "student row (row 5): D=1 | E=0.7 | F=5.9 | G=1") {
		t.Fatal(prompt)
	}
	for _, v := range views(t, "book_default") {
		if strings.Contains(LayoutPrompt(v), "Иванов") { // the own name is not sent
			t.Fatal(v.Tab)
		}
	}
}

func decodeSpec(t *testing.T, m map[string]any) Spec {
	t.Helper()
	var s Spec
	if err := pyjson.Unmarshal([]byte(pyjson.MustDumps(m)), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExtractNormalisesToTenAndKeepsBonus(t *testing.T) {
	dz := tab(views(t, "book_default"), "ДЗ-1А")
	items := Extract("sid", "DM", dz, decodeSpec(t, spec(`Tab: "ДЗ-1А"`)), 0) // header rows 1 and 2
	if len(items) != 1 {
		t.Fatal(items)
	}
	it := items[0]
	if it.Label != "DM HW 1A" || it.Value != 8.43 || *it.Bonus != 1 || it.Raw != 5.9 || it.Max != 7 ||
		it.Key != "sid|ДЗ-1А|F|Балл" {
		t.Fatalf("%+v", it)
	}
	if Budget("DM") != 20 || Budget("Very long course") != 8 {
		t.Fatal("budget")
	}
}

func TestPromptSpellsOutCyrillicLetterOrder(t *testing.T) {
	if !strings.Contains(LayoutSystem, "в→C") || !strings.Contains(LayoutSystem, `"HW 1C"`) {
		t.Fatal("letters")
	}
}

func TestLayoutKeyAsPythonHasIt(t *testing.T) {
	// hashlib.sha256(json.dumps(["abc123", "Квизы", 3], ensure_ascii=False).encode()).hexdigest()[:24]
	if got := LayoutKey("abc123", "Квизы", 20); got != "grades.layout:"+pythonLayoutHash+":20" {
		t.Fatal(got)
	}
}

// --- the layout ---------------------------------------------------------------------------------------------

func quizzes(t *testing.T, name string) collector.TabView { return tab(views(t, name), "Квизы") }

func TestLayoutStandsWhileTheHeaderRowsItNamedStay(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, err := e.grades.Layout(ctx, "abc123", quizzes(t, "book_default"), 20)
	if err != nil || !reflect.DeepEqual(s.HeaderRows, []int{1, 2, 3}) {
		t.Fatal(s, err)
	}
	e.grades.Layout(ctx, "abc123", quizzes(t, "book_quiz6"), 20)      // the own grade
	e.grades.Layout(ctx, "abc123", quizzes(t, "book_classmate4"), 20) // another student's, in the top rows
	if len(e.llm.prompts) != 1 {
		t.Fatal(len(e.llm.prompts))
	}
	e.grades.Layout(ctx, "abc123", quizzes(t, "book_default"), 12) // another budget: another label
	e.grades.Layout(ctx, "zzz999", quizzes(t, "book_default"), 20) // another sheet
	if len(e.llm.prompts) != 3 {
		t.Fatal(len(e.llm.prompts))
	}
	e.grades.Layout(ctx, "abc123", tab(views(t, "book_default"), "ДЗ-1А"), 20)
	e.grades.Layout(ctx, "abc123", tab(views(t, "book_total_itog_dz"), "ДЗ-1А"), 20) // the header changed
	if len(e.llm.prompts) != 5 {
		t.Fatal(len(e.llm.prompts))
	}
}

func TestAnAnswerTheModelCannotGiveSkipsTheTabUntilItChanges(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.llm.err = llm.NewError(llm.BadAnswer, "answer was cut at max_tokens")
	if s, _ := e.grades.Layout(ctx, "abc123", quizzes(t, "book_default"), 20); !s.Skip || !s.Failed {
		t.Fatal(s)
	}
	e.llm.err = nil
	if s, _ := e.grades.Layout(ctx, "abc123", quizzes(t, "book_default"), 20); !s.Skip || len(e.llm.prompts) != 1 {
		t.Fatal(s)
	}
	if s, _ := e.grades.Layout(ctx, "abc123", quizzes(t, "book_classmate4"), 20); s.Skip { // no header known: any change asks
		t.Fatal(s)
	}
	if len(e.llm.prompts) != 2 {
		t.Fatal(len(e.llm.prompts))
	}
}

func TestAFailedLayoutRecordsNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.llm.err = &llm.BudgetExceeded{Msg: "the daily LLM limit is spent"}
	if _, err := e.grades.Apply(ctx, sheet(t, "book_default")); !llm.IsBudget(err) {
		t.Fatal(err)
	}
	if e.count(t, "select count(*) from grade_sheets") != 0 {
		t.Fatal("recorded")
	}
	e.llm.err = nil
	read, err := e.grades.Apply(ctx, sheet(t, "book_default"))
	if err != nil || !read.First || read.Items != 2 || len(read.Unread) != 0 { // Quiz 2 is a placeholder zero
		t.Fatal(read, err)
	}
}

// --- new grades -----------------------------------------------------------------------------------------------

func item(key, label string, value float64, bonus *float64, position int) Item {
	return Item{Key: key, Tab: "T", Header: key, Label: label, Value: value, Raw: value, Max: 10, Bonus: bonus,
		Position: position}
}

func f(x float64) *float64 { return &x }

type event struct {
	label    string
	value    float64
	previous *float64
}

func journal(t *testing.T, e env) []event {
	rows, err := e.grades.DB.Query("select label, value, previous from grade_events order by id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []event
	for rows.Next() {
		var ev event
		var previous *float64
		rows.Scan(&ev.label, &ev.value, &previous)
		ev.previous = previous
		out = append(out, ev)
	}
	return out
}

func labels(t *testing.T, e env) []string {
	t.Helper()
	items, err := e.grades.Items("abc123")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

func fresh(t *testing.T, e env) []NewGrade {
	t.Helper()
	listed, err := e.grades.New()
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

func TestFirstReadIsABaselineThenChangesAreNew(t *testing.T) {
	e := setup(t)
	first, err := e.grades.Record("abc123", title("ДМ"), []Item{item("k1", "DM HW 1A", 8.43, nil, 0),
		item("k2", "DM Quiz 1", 0, nil, 0)})
	if err != nil || !first || len(fresh(t, e)) != 0 {
		t.Fatal(first, err)
	}
	if got := labels(t, e); !reflect.DeepEqual(got, []string{"DM HW 1A"}) { // the zero is a placeholder
		t.Fatal(got)
	}
	e.clock.moment = e.clock.moment.Add(24 * time.Hour)
	first, _ = e.grades.Record("abc123", title("ДМ"), []Item{item("k1", "DM HW 1A", 9, nil, 0),
		item("k2", "DM Quiz 1", 8, nil, 1), item("k3", "DM Quiz 2", 0, nil, 2)})
	if first {
		t.Fatal("a second baseline")
	}
	got := map[string]float64{}
	for _, x := range fresh(t, e) {
		got[x.Label] = float64(x.Value)
	}
	if !reflect.DeepEqual(got, map[string]float64{"DM HW 1A": 9, "DM Quiz 1": 8}) {
		t.Fatal(got)
	}
	events := map[string]event{}
	for _, ev := range journal(t, e) {
		events[ev.label] = ev
	}
	if *events["DM HW 1A"].previous != 8.43 || events["DM Quiz 1"].previous != nil {
		t.Fatal(events)
	}
	if text := pyjson.MustDumps(fresh(t, e)[0]); !strings.HasPrefix(text, `{"key": "k`) ||
		!strings.Contains(text, `"value": `) || !strings.Contains(text, `"seen_at": "2026-10-21T12:00:00"`) {
		t.Fatal(text)
	}
	e.clock.moment = e.clock.moment.Add(8 * 24 * time.Hour)
	if len(fresh(t, e)) != 0 { // out of the window
		t.Fatal(fresh(t, e))
	}
}

// D72: a zero counts once half the others have points in the column; with fewer it is still a placeholder.
func TestAZeroOfAWorkGradedForTheGroupIsReal(t *testing.T) {
	e := setup(t)
	graded := func(key, label string, share *float64) Item {
		return Item{Key: key, Tab: "T", Header: key, Label: label, Max: 10, Graded: share}
	}
	e.grades.Record("abc123", title("ДМ"), []Item{graded("k1", "DM HW 1", f(0.8)), graded("k2", "DM HW 2", f(0)),
		graded("k3", "DM HW 3", f(0.3)), graded("k4", "DM HW 4", nil)})
	if got := labels(t, e); !reflect.DeepEqual(got, []string{"DM HW 1"}) {
		t.Fatal(got)
	}
	e.clock.moment = e.clock.moment.Add(24 * time.Hour)
	e.grades.Record("abc123", title("ДМ"), []Item{graded("k1", "DM HW 1", f(0.8)), graded("k3", "DM HW 3", f(0.6))})
	if got := fresh(t, e); len(got) != 1 || got[0].Label != "DM HW 3" { // graded for the group now: a new grade, a zero
		t.Fatal(got)
	}
}

func TestTheNewestEventOfAWorkIsGiven(t *testing.T) {
	e := setup(t)
	e.grades.Record("abc123", nil, nil)
	for _, value := range []float64{5, 6, 7} {
		e.clock.moment = e.clock.moment.Add(time.Hour)
		e.grades.Record("abc123", nil, []Item{item("k1", "DM HW 1", value, nil, 0)})
	}
	if got := fresh(t, e); len(got) != 1 || got[0].Value != 7 {
		t.Fatal(got)
	}
}

// A layout that took a test's sum for a work, then dropped it: the sum stood next to the grade.
func TestAWorkThatLeavesItsSheetLeavesTheNewGrades(t *testing.T) {
	e := setup(t)
	e.grades.Record("abc123", nil, nil)
	e.clock.moment = e.clock.moment.Add(time.Hour)
	e.grades.Record("abc123", nil, []Item{item("sum", "Calc Test 1", 1.65, nil, 0), item("grade", "Calc Test 1", 6.8, nil, 1)})
	e.clock.moment = e.clock.moment.Add(time.Hour)
	e.grades.Record("abc123", nil, []Item{item("grade", "Calc Test 1", 6.8, nil, 0)})
	if got := fresh(t, e); len(got) != 1 || got[0].Key != "grade" || got[0].Value != 6.8 {
		t.Fatal(got)
	}
}

func newPairs(t *testing.T, e env) [][2]any {
	var out [][2]any
	for _, x := range fresh(t, e) {
		out = append(out, [2]any{x.Label, float64(x.Value)})
	}
	return out
}

// A column inserted before the quizzes shifts their letters: Tablo would post them again.
func TestAMovedColumnIsNoNewGrade(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.grades.Apply(ctx, sheet(t, "book_default"))
	e.clock.moment = e.clock.moment.Add(time.Hour)
	e.grades.Apply(ctx, sheet(t, "book_inserted"))
	if len(fresh(t, e)) != 0 || !reflect.DeepEqual(labels(t, e), []string{"DM HW 1A", "DM Quiz 1"}) {
		t.Fatal(fresh(t, e), labels(t, e))
	}
	e.grades.Apply(ctx, sheet(t, "book_inserted_quiz9"))
	if got := newPairs(t, e); !reflect.DeepEqual(got, [][2]any{{"DM Quiz 1", 9.0}}) {
		t.Fatal(got)
	}
	e.clock.moment = e.clock.moment.Add(time.Hour)
	e.grades.Apply(ctx, sheet(t, "book_quiz9")) // moved back: the new grade goes along, not off the list
	if got := newPairs(t, e); !reflect.DeepEqual(got, [][2]any{{"DM Quiz 1", 9.0}}) {
		t.Fatal(got)
	}
}

// A tab hidden for a while, a row gone from a tab: Tablo deleted the grades and posted them again on return.
func TestAGradeThatLeavesAndComesBackIsNoNewGrade(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.grades.Apply(ctx, sheet(t, "book_default"))
	e.clock.moment = e.clock.moment.Add(time.Hour)
	whole := sheet(t, "book_default")
	hidden := whole
	hidden.Views = []collector.TabView{tab(whole.Views, "ДЗ-1А")}
	if read, _ := e.grades.Apply(ctx, hidden); read.First || read.Items != 1 {
		t.Fatal(read)
	}
	if got := labels(t, e); !reflect.DeepEqual(got, []string{"DM HW 1A"}) {
		t.Fatal(got)
	}
	e.grades.Apply(ctx, sheet(t, "book_default"))
	if len(fresh(t, e)) != 0 || len(labels(t, e)) != 2 {
		t.Fatal(fresh(t, e))
	}
	e.grades.Apply(ctx, hidden)
	e.grades.Apply(ctx, sheet(t, "book_quiz9")) // back with another grade: that one is new, with the grade before it
	got := journal(t, e)
	if len(got) != 1 || got[0].label != "DM Quiz 1" || got[0].value != 9 || *got[0].previous != 8 {
		t.Fatal(got)
	}
}

func TestAWorkListedTwiceCountsOnce(t *testing.T) {
	dz := tab(views(t, "book_default"), "ДЗ-1А")
	twice := decodeSpec(t, map[string]any{"skip": false, "reason": "", "items": []any{
		map[string]any{"column": "F", "label": "HW 1A", "max": 7, "bonus_column": nil},
		map[string]any{"column": "F", "label": "HW 1A again", "max": 7, "bonus_column": nil}}})
	if items := Extract("abc123", "DM", dz, twice, 0); len(items) != 1 || items[0].Label != "DM HW 1A" {
		t.Fatal(items)
	}
}

func TestATabTheModelCannotLayOutIsTold(t *testing.T) {
	e := setup(t)
	e.llm.err = llm.NewError(llm.BadAnswer, "answer was cut at max_tokens")
	read, err := e.grades.Apply(context.Background(), sheet(t, "book_default"))
	if err != nil || !read.First || read.Items != 0 || !reflect.DeepEqual(read.Unread, []string{"ДЗ-1А", "Квизы"}) {
		t.Fatal(read, err)
	}
}

func TestAWideTabGetsRoomForItsAnswer(t *testing.T) {
	e := setup(t)
	var asked []int
	e.llm.Respond = func(req llm.Request) (any, error) {
		asked = append(asked, req.MaxTokens)
		return spec(req.Prompt), nil
	}
	dz := tab(views(t, "book_default"), "ДЗ-1А")
	cells := map[string]string{}
	for i := 2; i < 202; i++ {
		cells[collector.Column(i)] = "Sep " + collector.Text(int64(i))
	}
	wide := collector.TabView{Tab: dz.Tab, Rows: []collector.Row{{Number: 1, Cells: cells}}, Mine: dz.Mine, NameCol: "B",
		OwnRow: 2}
	e.grades.Layout(context.Background(), "abc123", dz, 20)
	e.grades.Layout(context.Background(), "zzz999", wide, 20)
	if !reflect.DeepEqual(asked, []int{MaxTokens, 200 * TokensPerWork}) { // the own row's columns are among the 200
		t.Fatal(asked)
	}
}

// --- the owner's settings -----------------------------------------------------------------------------------------

func sheetsSetting(pairs ...string) []any {
	var out []any
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]any{"url": pairs[i], "label": pairs[i+1]})
	}
	return out
}

func TestConfigureForgetsRemovedSheetsAndEverythingForANewName(t *testing.T) {
	e := setup(t)
	woken := 0
	e.grades.Wake = func() { woken++ }
	items := func(sid string) int {
		got, err := e.grades.Items(sid)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	e.grades.Record("abc123", nil, []Item{item("k1", "DM HW", 8, nil, 0)})
	if err := e.grades.Configure(map[string]any{"grades_sheets": sheetsSetting(url, "DM", other, "Calc")}); err != nil {
		t.Fatal(err)
	}
	e.grades.Record("zzz999", nil, []Item{item("k2", "Calc Quiz", 7, nil, 0)})
	if items("abc123") != 1 || items("zzz999") != 1 || woken != 1 {
		t.Fatal(woken)
	}
	e.grades.Configure(map[string]any{"grades_sheets": sheetsSetting(other, "Calc")})
	if items("abc123") != 0 || items("zzz999") != 1 {
		t.Fatal("forgot the wrong sheet")
	}
	e.grades.Configure(map[string]any{"grades_student": "Иванов  Иван"}) // the same name: nothing forgotten
	if items("zzz999") != 1 {
		t.Fatal("forgot for the same name")
	}
	e.grades.Configure(map[string]any{"grades_student": "Петров Пётр"})
	if items("zzz999") != 0 || e.count(t, "select count(*) from grade_sheets") != 0 {
		t.Fatal("kept for a new name")
	}
	var settingsErr *settings.Error
	if err := e.grades.Configure(map[string]any{"grades_window_days": 0, "grades_student": "Кто-то"}); !errors.As(err, &settingsErr) {
		t.Fatal(err)
	}
	if name, _ := e.settings.String("grades_student"); name != "Петров Пётр" { // nothing saved when one value is wrong
		t.Fatal(name)
	}
	if err := e.grades.Configure(map[string]any{"llm_daily_limit_usd": 1}); !errors.As(err, &settingsErr) {
		t.Fatal(err)
	}
}

func TestASheetDroppedWhileReadRecordsNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	read := sheet(t, "book_default")
	e.settings.Set("grades_sheets", []any{})
	if got, _ := e.grades.Apply(ctx, read); got.First || got.Items != 0 {
		t.Fatal(got)
	}
	e.settings.Set("grades_sheets", sheetsSetting(url, "DM"))
	e.settings.Set("grades_student", "Петров Пётр")
	if got, _ := e.grades.Apply(ctx, read); got.First || got.Items != 0 {
		t.Fatal(got)
	}
	if e.count(t, "select count(*) from grade_items") != 0 {
		t.Fatal("recorded")
	}
}

// --- the collector and the processing together -------------------------------------------------------------------

// New grades reach the API's list; the owner hears only that the sheet is connected (D55).
func TestPollEndToEnd(t *testing.T) {
	e := setup(t)
	var mu sync.Mutex
	data, titles := workbook(t, "book_default"), 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/htmlview") {
			titles++
			w.Write([]byte(`<html><title>ДМ &quot;ПИ&quot; - Google Диск</title></html>`))
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	export, titleURL := collector.ExportURL, collector.TitleURL
	collector.ExportURL, collector.TitleURL = server.URL+"/d/{id}/export", server.URL+"/d/{id}/htmlview"
	defer func() { collector.ExportURL, collector.TitleURL = export, titleURL }()

	source := collector.New(e.settings)
	source.Now, source.Client, source.OnSheet = e.clock.now, server.Client(), e.grades.Apply
	var system []string
	source.Notify = func(line i18n.Phrase) { system = append(system, line.Ru) }
	ctx := context.Background()
	source.Poll(ctx) // the baseline
	st := source.Status().Sheets["abc123"]
	if *st.Title != `ДМ "ПИ"` || st.Tabs != 2 || st.Items != 2 || st.Error != nil {
		t.Fatalf("%+v", st)
	}
	if !reflect.DeepEqual(system, []string{`Таблица оценок «ДМ "ПИ"» подключена: вкладок со своей строкой — 2, ` +
		"оценок — 2. Они в линзах курсов в приложении."}) || len(fresh(t, e)) != 0 {
		t.Fatal(system)
	}
	mu.Lock()
	data = workbook(t, "book_hw63_quiz9")
	mu.Unlock()
	e.clock.moment = e.clock.moment.Add(time.Hour)
	source.Poll(ctx)
	var got []string
	for _, x := range fresh(t, e) {
		bonus := "None"
		if x.Bonus != nil {
			bonus = pyjson.MustDumps(*x.Bonus)
		}
		got = append(got, x.Label+" "+pyjson.MustDumps(pyjson.Float(math.Round(float64(x.Value)*10)/10))+" "+bonus)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"DM HW 1A 9.0 1.0", "DM Quiz 1 9.0 None"}) {
		t.Fatal(got)
	}
	if len(e.llm.prompts) != 2 || titles != 1 || len(system) != 1 { // one layout per tab, the title once
		t.Fatal(len(e.llm.prompts), titles, system)
	}
}

// --- the owner's hand (D44) ----------------------------------------------------------------------------------------

func TestTheViewAndTheOwnersLayout(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.grades.Apply(ctx, sheet(t, "book_default"))
	view, _ := e.grades.View("abc123")
	dz := view.Tabs[0]
	if view.Course != "DM" || dz.Tab != "ДЗ-1А" || dz.OwnRow != 5 || dz.By != "haiku" || !reflect.DeepEqual(dz.HeaderRows, []int{1, 2}) {
		t.Fatalf("%+v", dz)
	}
	var cols [][3]string
	for _, c := range dz.Columns {
		cols = append(cols, [3]string{c.Column, c.Header, c.Value})
	}
	if !reflect.DeepEqual(cols, [][3]string{{"D", "7 / 1а", "1"}, {"E", "0 / 1б", "0.7"}, {"F", "Балл", "5.9"}, {"G", "Балл*", "1"}}) {
		t.Fatal(cols)
	}
	works := pyjson.MustDumps(dz.Works)
	if works != `[{"column": "F", "label": "HW 1A", "max": 7.0, "bonus_column": "G", "header": "Балл", "raw": 5.9, `+
		`"value": 8.43, "graded": 1.0}]` { // both others have points in it
		t.Fatal(works)
	}
	if strings.Contains(pyjson.MustDumps(view.Tabs), "Образцов") { // another student's row is not kept
		t.Fatal("another student's row")
	}

	woken := 0
	e.grades.Wake = func() { woken++ }
	g := "G"
	mine := []LayoutItem{{Column: "F", Label: "HW 1A", Max: 7.0, BonusColumn: &g},
		{Column: "D", Label: "Problem 1a", Max: 1.0}}
	found, err := e.grades.SetLayout("abc123", "ДЗ-1А", false, mine)
	if err != nil || found.By != "you" || *found.Works[0].Value != 8.43 || *found.Works[1].Value != 10 || woken != 1 {
		t.Fatal(found, err)
	}
	if view, _ := e.grades.View("abc123"); view.Tabs[0].By != "you" {
		t.Fatal("not kept")
	}
	e.clock.moment = e.clock.moment.Add(time.Hour)
	e.grades.Apply(ctx, sheet(t, "book_total_itog")) // the header changed: the owner's layout stands, nothing is asked
	if len(e.llm.prompts) != 2 || !reflect.DeepEqual(labels(t, e), []string{"DM HW 1A", "DM Problem 1a", "DM Quiz 1"}) {
		t.Fatal(len(e.llm.prompts), labels(t, e))
	}
	wrong := mine[0]
	wrong.Column = "Z"
	if _, err := e.grades.SetLayout("abc123", "ДЗ-1А", false, []LayoutItem{wrong}); err == nil ||
		!strings.Contains(err.Error(), "no such column") {
		t.Fatal(err)
	}
	long := mine[0]
	long.Label = strings.Repeat("x", 40)
	if _, err := e.grades.SetLayout("abc123", "ДЗ-1А", false, []LayoutItem{long}); err == nil ||
		i18n.Reason(err, "ru") != "нужно название, до 20 символов" {
		t.Fatal(err)
	}
	if found, err := e.grades.SetLayout("abc123", "Нет такой", true, nil); found != nil || err != nil {
		t.Fatal(found, err)
	}

	found, _ = e.grades.ClearLayout("abc123", "ДЗ-1А")
	if found.By != "haiku" || len(found.Works) != 1 || found.Works[0].Label != "HW 1A" || woken != 2 {
		t.Fatal(found)
	}
	e.grades.Apply(ctx, sheet(t, "book_total_itog")) // Haiku is asked again: its cached layout watched another header
	if !reflect.DeepEqual(labels(t, e), []string{"DM HW 1A", "DM Quiz 1"}) {
		t.Fatal(labels(t, e))
	}

	e.grades.SetLayout("abc123", "Квизы", true, nil)
	e.grades.Forget([]string{"abc123"})
	if view, _ := e.grades.View("abc123"); view.Course != "" || len(view.Tabs) != 0 {
		t.Fatal(view)
	}
	var own Spec
	if ok, _ := e.grades.Store.GetJSON(OwnLayoutKey+"abc123|Квизы", &own); ok {
		t.Fatal("the owner's layout stayed")
	}
}

func TestTheOwnersGrades(t *testing.T) {
	e := setup(t)
	e.grades.Record("abc123", title("ДМ"), []Item{item("k1", "DM HW 1", 8, nil, 0)})
	e.clock.moment = e.clock.moment.Add(time.Hour)
	if ok, _ := e.grades.ChangeGrade("k1", "", 9.5, ""); !ok {
		t.Fatal("not changed")
	}
	shown, _ := e.grades.Items("abc123")
	if s := shown[0]; s.ID != "k1" || s.Value != 9.5 || !s.Edited || s.SheetValue != 8 || !s.New {
		t.Fatalf("%+v", s)
	}
	if got := fresh(t, e); len(got) != 1 || got[0].Key != "k1" || got[0].Label != "DM HW 1" || got[0].Value != 9.5 {
		t.Fatal(got)
	}
	e.grades.Record("abc123", title("ДМ"), []Item{item("k1", "DM HW 1", 8, nil, 0)}) // the sheet has the same grade: the change stands
	if shown, _ := e.grades.Items("abc123"); shown[0].Value != 9.5 {
		t.Fatal(shown)
	}
	e.clock.moment = e.clock.moment.Add(time.Hour)
	e.grades.Record("abc123", title("ДМ"), []Item{item("k1", "DM HW 1", 7, nil, 0)}) // the sheet changed it: the sheet wins
	if shown, _ := e.grades.Items("abc123"); shown[0].Value != 7 || shown[0].Edited {
		t.Fatal(shown)
	}

	added, _ := e.grades.AddGrade("Eng", "Essay", 8.5)
	manual, _ := e.grades.Manual()
	if text := pyjson.MustDumps(manual); text != `[{"id": "own-`+collector.Text(added)+`", "course": "Eng", "label": "Essay", "value": 8.5, "new": true}]` {
		t.Fatal(text)
	}
	found := false
	for _, x := range fresh(t, e) {
		found = found || x.Label == "Eng Essay" && x.Value == 8.5
	}
	if !found {
		t.Fatal(fresh(t, e))
	}
	id := "own-" + collector.Text(added)
	if ok, _ := e.grades.ChangeGrade(id, "Essay 2", 9, ""); !ok {
		t.Fatal("not changed")
	}
	if manual, _ := e.grades.Manual(); manual[0].Label != "Essay 2" {
		t.Fatal(manual)
	}
	if ok, _ := e.grades.ChangeGrade("nope", "", 5, ""); ok {
		t.Fatal("changed nothing")
	}
	if ok, _ := e.grades.DropGrade("own-99"); ok {
		t.Fatal("dropped nothing")
	}
	e.clock.moment = e.clock.moment.Add(8 * 24 * time.Hour)
	if manual, _ := e.grades.Manual(); manual[0].New || len(fresh(t, e)) != 0 {
		t.Fatal(manual)
	}
	if ok, _ := e.grades.DropGrade(id); !ok {
		t.Fatal("not dropped")
	}
	if manual, _ := e.grades.Manual(); len(manual) != 0 {
		t.Fatal(manual)
	}
	e.grades.ChangeGrade("k1", "", 10, "")
	e.grades.Forget([]string{"abc123"})
	if e.count(t, "select count(*) from own_grades") != 0 {
		t.Fatal("kept the owner's change")
	}
}

const pythonLayoutHash = "a9adc724eb02a1bb80552ca9"
