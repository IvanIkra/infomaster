package grades

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/calc"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

// Stage 17.6: the own row's formulas and comments of a gradebook, and the calculator (names and texts made up).
//
// The workbook is written by openpyxl, which keeps no computed values: the formula cells are read without them, as a
// Google export with a formula showing nothing would be, and the calculator counts them from their formulas.

const formulasStudent = "Тестов Тест"

func gradebookViews(t *testing.T) []collector.TabView {
	t.Helper()
	found, err := collector.ReadViews(workbook(t, "gradebook"), formulasStudent)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func layouts(t *testing.T) map[string]Spec {
	return map[string]Spec{
		"ДЗ-1": decodeSpec(t, map[string]any{"header_rows": []int{1, 2}, "items": []any{
			map[string]any{"column": "F", "label": "HW 1", "max": 3, "bonus_column": "G"}}}),
		"КР1": decodeSpec(t, map[string]any{"header_rows": []int{1}, "items": []any{
			map[string]any{"column": "F", "label": "Test 1", "max": 10, "bonus_column": nil}}}),
		"КР2": decodeSpec(t, map[string]any{"header_rows": []int{1}, "items": []any{
			map[string]any{"column": "F", "label": "Test 2", "max": 10, "bonus_column": nil}}}),
		"Итоги": decodeSpec(t, map[string]any{"skip": true, "header_rows": []int{1}, "items": []any{}}),
	}
}

// computed is what Google's export would have computed in the own row (openpyxl writes none).
func computed(views []collector.TabView) []collector.TabView {
	values := map[string]map[string]any{"ДЗ-1": {"F": 1.5, "G": int64(1)}, "КР1": {"F": 4.67}, "КР2": {"F": int64(0)},
		"Итоги": {"C": 4.67, "D": int64(0), "E": int64(5), "F": int64(3), "G": int64(1), "H": int64(2)}}
	var out []collector.TabView
	for _, v := range views {
		mine := map[string]any{}
		for c, x := range v.Mine {
			mine[c] = x
		}
		for c, x := range values[v.Tab] {
			mine[c] = x
		}
		v.Mine = mine
		out = append(out, v)
	}
	return out
}

func kept(t *testing.T, views []collector.TabView) calc.View {
	wanted := calc.Referenced(views)
	specs := layouts(t)
	view := calc.View{Course: "DM", Constants: calc.Constants(views)}
	for _, v := range views {
		view.Tabs = append(view.Tabs, Describe(v, specs[v.Tab], wanted[v.Tab]))
	}
	return view
}

func TestTheOwnRowsFormulasAndCommentsAreReadAndNoOneElses(t *testing.T) {
	views := gradebookViews(t)
	byTab := map[string]collector.TabView{}
	for _, v := range views {
		byTab[v.Tab] = v
	}
	if got := byTab["ДЗ-1"].Formulas; !reflect.DeepEqual(got, map[string]string{"F": "=SUM(C4:E4)-G4",
		"G": `=SUMIF($C$2:$E$2,"*~*",C4:E4)`}) {
		t.Fatal(got)
	}
	if got := byTab["ДЗ-1"].Notes; !reflect.DeepEqual(got, map[string]string{"D": "пример стоит проверить подстановкой",
		"F": "в целом хорошо"}) {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(byTab["КР1"].Notes, map[string]string{"E": "ход решения ясен, ответ не получен"}) ||
		len(byTab["КР2"].Notes) != 0 {
		t.Fatal(byTab["КР1"].Notes, byTab["КР2"].Notes)
	}
	// the header's cells the formulas read are kept, a classmate's row is not (and its formula is not counted)
	if got := calc.Constants(views); !reflect.DeepEqual(got, map[string]string{"ДЗ-1!C2": "1а", "ДЗ-1!D2": "1б",
		"ДЗ-1!E2": "2*", "ДЗ-1!C1": "3"}) {
		t.Fatal(got)
	}
	book := calc.NewBook(kept(t, views))
	if book.Value("Итоги", "H", nil) != nil || book.Value("Итоги", "E", nil) != 5.0 {
		t.Fatal(book.Value("Итоги", "H", nil), book.Value("Итоги", "E", nil))
	}
	// a formula cell without a value has its column, under its header
	var cols []string
	for _, c := range Describe(byTab["КР1"], layouts(t)["КР1"], nil).Columns {
		cols = append(cols, c.Column)
	}
	if !reflect.DeepEqual(cols, []string{"A", "C", "D", "E", "F"}) {
		t.Fatal(cols)
	}
}

func given(pairs map[string]float64) map[string]*float64 {
	out := map[string]*float64{}
	for k, v := range pairs {
		out[k] = &v
	}
	return out
}

func TestTheCalculatorCountsTheGradeAndWhatIsNeeded(t *testing.T) {
	book := calc.NewBook(kept(t, computed(gradebookViews(t))))
	targets := book.Targets()
	if got := book.DefaultTarget(targets); got != "Итоги!F" { // the grade, not the flag on top of it
		t.Fatal(got)
	}
	for cell, want := range map[[2]string]string{{"Итоги", "F"}: "ROUND(0.35·КР1 + 0.35·КР2 + 0.3·ДЗ; 0)",
		{"ДЗ-1", "F"}: "SUM(1а…2*) − HW 1*", {"Итоги", "E"}: "10·ДЗ-1: HW 1 / 3"} {
		if got := book.Written(cell[0], cell[1]); got != want {
			t.Error(got)
		}
	}
	if got := book.Value("Итоги", "F", nil); got != 3.0 { // 0.35·4.67 + 0 + 0.3·5 = 3.13
		t.Fatal(got)
	}
	inputs := book.Inputs("Итоги!F", nil)
	var cells []string
	for _, in := range inputs {
		cells = append(cells, in.Cell)
	}
	if !reflect.DeepEqual(cells, []string{"КР1!F", "КР2!F", "ДЗ-1!F"}) { // the works, not their problems
		t.Fatal(cells)
	}
	if x := float64(inputs[0].Sheet.(pyjson.Float)); math.Abs(x-4.67) > 0.01 || *inputs[2].Max != 3 {
		t.Fatal(inputs)
	}
	if got := book.Value("Итоги", "F", given(map[string]float64{"КР2!F": 8})); got != 6.0 {
		t.Fatal(got)
	}
	// for a 6: 0.35·x ≥ 5.5 − 1.63 − 1.5, at least 6.77 on the second test
	if got := pyjson.MustDumps(book.Solve("Итоги!F", "КР2!F", 6, nil)); got != `{"need": 6.77, "reached": true, "max": 10.0}` {
		t.Fatal(got)
	}
	if book.Value("Итоги", "F", given(map[string]float64{"КР2!F": 6.77})) != 6.0 ||
		book.Value("Итоги", "F", given(map[string]float64{"КР2!F": 6.76})) != 5.0 {
		t.Fatal("the bounds")
	}
	if got := pyjson.MustDumps(book.Solve("Итоги!F", "КР2!F", 9, nil)); got != `{"need": null, "reached": false, "max": 10.0}` {
		t.Fatal(got)
	}
	if got := book.Solve("Итоги!F", "КР2!F", 3, nil); *got.Need != 0 {
		t.Fatal(got)
	}
	// a test not written yet: its problems are inputs, empty, under their headers; the group's number is none
	var got [][3]any
	for _, in := range book.Inputs("КР2!F", nil) {
		got = append(got, [3]any{in.Cell, in.Name, in.Sheet})
	}
	if !reflect.DeepEqual(got, [][3]any{{"КР2!C", "1", nil}, {"КР2!D", "2", nil}, {"КР2!E", "3", nil}}) {
		t.Fatal(got)
	}
	if got := book.Value("КР2", "F", given(map[string]float64{"КР2!C": 5, "КР2!D": 4})); got != 6.0 {
		t.Fatal(got)
	}
}

func TestTheFormulasAreExplainedOnceWhileTheyStay(t *testing.T) {
	e := setup(t)
	calls := 0
	e.llm.Respond = func(req llm.Request) (any, error) {
		calls++
		var cells []any
		for _, line := range strings.Split(req.Prompt, "\n") {
			cell := strings.Split(line, " | ")[0]
			cells = append(cells, map[string]any{"cell": cell, "en": "how " + cell, "ru": "как"})
		}
		return map[string]any{"cells": cells}, nil
	}
	ctx := context.Background()
	view := kept(t, gradebookViews(t))
	how := e.grades.Explain(ctx, "abc123", view)
	if _, ok := how["КР1!E"]; ok || how["Итоги!F"] != (calc.Explained{En: "how Итоги!F", Ru: "как"}) { // a formula only
		t.Fatal(how)
	}
	if again := e.grades.Explain(ctx, "abc123", view); !reflect.DeepEqual(again, how) || calls != 1 {
		t.Fatal(calls)
	}
	view.Tabs[0].Formulas["F"] = "=SUM(C4:D4)"
	e.grades.Explain(ctx, "abc123", view)
	if calls != 2 {
		t.Fatal(calls)
	}
}
