package web

// The grades of a course's lens and the calculator (tests/test_grade_formulas.py's web part, and
// test_web_fixes.py's test_grades_with_their_parts): the gradebook of test_grade_formulas as Python's views gave it
// (testdata/grades_vectors.json, made by the Python service), and the same answers from Go.

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

type gradesVectors struct {
	URL   string          `json:"url"`
	View  json.RawMessage `json:"view"`
	Items []struct {
		Key, Tab, Header, Label string
		Value, Raw, Max         float64
		Bonus, Graded           *float64
		Position                int
	} `json:"items"`
	Grades    any `json:"grades"`
	Calc      any `json:"calc"`
	CalcOther any `json:"calc_other"`
}

func gradesHarness(t *testing.T) (*harness, *grades.Grades, *settings.Settings) {
	var g *grades.Grades
	var s *settings.Settings
	h := newHarness(t, options{screens: true, connector: true, data: func(h *harness, d *Data) {
		must(h.sch.Subjects.SetManual("Discrete Mathematics", "Discrete Mathematics", "DM"))
		s = must(settings.New(h.store))
		g = grades.New(h.store, s, nil)
		g.Now = h.clock.now
		d.Grades = g
	}})
	return h.serve().in(), g, s
}

// same compares two JSON values as decoded JSON.
func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	norm := func(v any) any {
		text := must(pyjson.Dumps(httpx.PyValue(v)))
		var out any
		noErr(t, json.Unmarshal([]byte(text), &out))
		return out
	}
	if g, w := norm(got), norm(want); !reflect.DeepEqual(g, w) {
		t.Fatalf("%s differs:\n go: %s\npy: %s", what, must(json.Marshal(g)), must(json.Marshal(w)))
	}
}

func TestTheGradeCardAndTheCalculatorAsPythonGaveThem(t *testing.T) {
	var vectors gradesVectors
	noErr(t, json.Unmarshal(must(os.ReadFile("testdata/grades_vectors.json")), &vectors))
	h, g, s := gradesHarness(t)
	must(s.Set("grades_sheets", []any{map[string]any{"url": vectors.URL, "label": "DM"}}))
	var view any
	noErr(t, json.Unmarshal(vectors.View, &view))
	noErr(t, h.store.PutJSON(grades.ViewKey+"abc123", view))
	items := []grades.Item{}
	for _, i := range vectors.Items {
		items = append(items, grades.Item{Key: i.Key, Tab: i.Tab, Header: i.Header, Label: i.Label, Value: i.Value,
			Raw: i.Raw, Max: i.Max, Bonus: i.Bonus, Position: i.Position, Graded: i.Graded})
	}
	must(g.Record("abc123", ptr("DM"), items))

	got := must(h.app.Data.courseGrades("DM", nil))
	sameJSON(t, "grades", got, vectors.Grades)
	sameJSON(t, "the lens's grades", h.read("/course?key=DM")["grades"], vectors.Grades)

	calc := h.ask("POST", "/course/calc", map[string]any{"key": "DM", "book": "abc123", "values": map[string]any{"КР2!F": 8},
		"solve": map[string]any{"cell": "КР2!F", "goal": 6}}, 200)
	sameJSON(t, "calc", calc, vectors.Calc)
	other := h.ask("POST", "/course/calc", map[string]any{"key": "DM", "book": "abc123",
		"values": map[string]any{"ДЗ-1!C": 99}}, 200)
	sameJSON(t, "calc of no input", other, vectors.CalcOther)
	h.ask("POST", "/course/calc", map[string]any{"key": "DM", "book": "abc123", "target": "Итоги!Z"}, 404)
	h.ask("POST", "/course/calc", map[string]any{"key": "DM", "book": "nope"}, 404)
	h.ask("POST", "/course/calc", map[string]any{"key": "DM", "book": "abc123", "values": map[string]any{"x": "8"}}, 400)
}

func TestGradesWithTheirParts(t *testing.T) {
	h, g, s := gradesHarness(t)
	must(s.Set("grades_sheets", []any{map[string]any{"url": "https://docs.google.com/spreadsheets/d/abc123/edit#gid=0",
		"label": "DM"}}))
	cells := func(triples ...[3]string) []any {
		out := []any{}
		for _, c := range triples {
			out = append(out, map[string]any{"column": c[0], "header": c[1], "value": c[2]})
		}
		return out
	}
	work := func(col, label string, top, raw float64, bonus string) map[string]any {
		w := map[string]any{"column": col, "label": label, "max": top, "bonus_column": nil, "header": label, "raw": raw,
			"value": float64(int(raw/top*1000+0.5)) / 100}
		if bonus != "" {
			w["bonus_column"], w["header"] = bonus, "Score"
		}
		return w
	}
	hw2 := work("F", "HW 2", 5, 0, "G")
	hw2["graded"] = 0.3 // a third of the group graded: being graded
	noErr(t, h.store.PutJSON(grades.ViewKey+"abc123", map[string]any{"course": "DM", "tabs": []any{
		map[string]any{"tab": "HW-1", "skip": false, "columns": cells([3]string{"D", "5 / 1a", "1"},
			[3]string{"E", "1b", "0.5"}, [3]string{"F", "Score", "1.5"}, [3]string{"G", "Score*", "0"}),
			"works": []any{work("F", "HW 1", 2, 1.5, "G")}},
		map[string]any{"tab": "HW-2", "skip": false, "columns": cells([3]string{"F", "Score", "0"},
			[3]string{"G", "Score*", "0"}), "works": []any{hw2}},
		map[string]any{"tab": "Seminars", "skip": false, "columns": cells([3]string{"B", "Sem 1", "8"},
			[3]string{"C", "Sem 2", "0"}, [3]string{"D", "Sem 3", "6"}),
			"works": []any{work("B", "Sem 1", 10, 8, ""), work("C", "Sem 2", 10, 0, ""), work("D", "Sem 3", 10, 6, "")}},
		map[string]any{"tab": "Totals", "skip": true, "columns": cells([3]string{"D", "Midterm", "4.5"})},
		map[string]any{"tab": "Empty", "skip": true, "columns": []any{}}}}))
	must(g.Record("abc123", ptr("DM"), []grades.Item{
		{Key: "abc123|HW-1|F|Score", Tab: "HW-1", Header: "Score", Label: "DM HW 1", Value: 7.5, Raw: 1.5, Max: 2, Position: 0},
		{Key: "abc123|Seminars|B|Sem 1", Tab: "Seminars", Header: "Sem 1", Label: "DM Sem 1", Value: 8, Raw: 8, Max: 10, Position: 1},
		{Key: "abc123|Seminars|D|Sem 3", Tab: "Seminars", Header: "Sem 3", Label: "DM Sem 3", Value: 6, Raw: 6, Max: 10, Position: 2}}))
	gradesView := obj(h.read("/course?key=DM")["grades"])
	works := map[string]map[string]any{}
	order := []string{}
	for _, w := range list(gradesView["works"]) {
		works[obj(w)["label"].(string)] = obj(w)
		order = append(order, obj(w)["label"].(string))
	}
	sameJSON(t, "order", order, []string{"DM HW 1", "HW 2", "DM Sem 1", "Sem 2", "DM Sem 3"})
	sameJSON(t, "parts", works["DM HW 1"]["parts"], []any{map[string]any{"header": "1a", "value": "1", "note": nil},
		map[string]any{"header": "1b", "value": "0.5", "note": nil}})
	if works["DM HW 1"]["value"] != 7.5 || works["DM HW 1"]["bonus"] != "0" || works["DM HW 1"]["pending"] != false {
		t.Fatal(works["DM HW 1"])
	}
	if works["HW 2"]["pending"] != true || works["HW 2"]["value"] != nil || len(list(works["HW 2"]["parts"])) != 0 ||
		works["HW 2"]["checking"] != true || works["Sem 2"]["checking"] != false || works["Sem 2"]["pending"] != true ||
		len(list(works["DM Sem 3"]["parts"])) != 0 {
		t.Fatal(works)
	}
	if gradesView["count"] != int64(3) || gradesView["pending"] != int64(2) || gradesView["average"] != 7.2 {
		t.Fatal(gradesView)
	}
	book := obj(list(gradesView["books"])[0])
	sameJSON(t, "summaries", book["summaries"], []any{map[string]any{"tab": "Totals", "cells": []any{
		map[string]any{"header": "Midterm", "value": "4.5", "note": nil, "how": nil}}}})
	if book["calc"] != false {
		t.Fatal("no formulas: no calculator")
	}
}
