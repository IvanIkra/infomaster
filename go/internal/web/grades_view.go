package web

// The grades of a course's lens (stage 17.3, D71) and a gradebook's calculator (17.6): the part of views.py that
// reads the gradebooks.

import (
	"math"
	"sort"
	"strconv"
	"strings"

	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/processing/calc"
	fx "github.com/IvanIkra/infomaster/go/internal/processing/formulas"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

type book struct {
	settings.GradeSheet
	ID string
}

// books are the gradebooks of a course: by the course their view names, their label, or a name of the course.
func (d *Data) books(key string, names []string) ([]book, error) {
	found := []book{}
	if d.Grades == nil {
		return found, nil
	}
	sheets, err := d.Grades.Settings.GradeSheets()
	if err != nil {
		return nil, err
	}
	for _, sheet := range sheets {
		sid, err := collector.SheetID(sheet.URL)
		if err != nil {
			continue
		}
		view, err := d.Grades.View(sid)
		if err != nil {
			return nil, err
		}
		course := view.Course
		if course == "" {
			course = sheet.Label
		}
		key2, err := d.Key(course)
		if err != nil {
			return nil, err
		}
		match := key2 == key && key2 != ""
		for _, name := range names {
			if match {
				break
			}
			if match, err = d.Schedule.SameCourse(course, name); err != nil {
				return nil, err
			}
		}
		if !match && pytext.Casefold(subjects.Clean(sheet.Label)) == pytext.Casefold(key) {
			match = true
		}
		if match {
			found = append(found, book{sheet, sid})
		}
	}
	return found, nil
}

func floatPtr(f *pyjson.Float) any {
	if f == nil {
		return nil
	}
	return *f
}

// round1 is Python's round(x, 1).
func round1(x float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 1, 64), 64)
	return f
}

// courseGrades is a course's grades, one list for the lens's tile and the grades' screen (stage 17.3, D71): every
// work its gradebooks lay out, in sheet order, with the points of its parts (the own row's cells between the work
// before it and its own column); the grades read with the owner's changes over them; a zero the sheet shows that no
// grade stands behind is `pending` («not graded yet», §6.7; `checking` while the group's work is being graded, D72)
// and counts in no average. Then the grades added by hand, and the tabs the layout skips that still have cells (a
// summary: «Итоги»).
func (d *Data) courseGrades(key string, names []string) (httpx.Object, error) {
	works, booksOut := []httpx.Object{}, []httpx.Object{}
	type counted struct {
		value   *float64
		pending bool
		new     bool
	}
	var tally []counted
	found, err := d.books(key, names)
	if err != nil {
		return nil, err
	}
	lang := d.lang()
	for _, b := range found {
		sid := b.ID
		items, err := d.Grades.Items(sid)
		if err != nil {
			return nil, err
		}
		read := map[string]grades.SheetItem{}
		for _, item := range items {
			read[item.ID] = item
		}
		summaries, seen := []httpx.Object{}, map[string]bool{}
		view, err := d.Grades.View(sid)
		if err != nil {
			return nil, err
		}
		sheet := calc.NewBook(view)
		told := func(tab, col string) any {
			if col == "" {
				return nil
			}
			line, has := view.How[tab+"!"+col]
			written := sheet.Written(tab, col)
			if !has && written == "" {
				return nil
			}
			var text any
			if has {
				text = line.En
				if lang == "ru" && line.Ru != "" {
					text = line.Ru
				}
			}
			return httpx.Object{{Key: "text", Value: text}, {Key: "formula", Value: nullable(written)}}
		}
		for _, tab := range view.Tabs {
			cells := tab.Columns
			notes := tab.Notes
			note := func(col string) any {
				if n, ok := notes[col]; ok {
					return n
				}
				return nil
			}
			if tab.NameCol != "" { // the grades are right of the name: the group's number is none
				kept := []calc.Column{}
				for _, c := range cells {
					if fx.ColIndex(c.Column) > fx.ColIndex(tab.NameCol) {
						kept = append(kept, c)
					}
				}
				cells = kept
			}
			if tab.Skip {
				if len(cells) > 0 {
					listed := []httpx.Object{}
					for _, c := range cells {
						listed = append(listed, httpx.Object{{Key: "header", Value: c.Header}, {Key: "value", Value: c.Value},
							{Key: "note", Value: note(c.Column)}, {Key: "how", Value: told(tab.Tab, c.Column)}})
					}
					summaries = append(summaries, httpx.Object{{Key: "tab", Value: tab.Tab}, {Key: "cells", Value: listed}})
				}
				continue
			}
			var laid []calc.Work
			own := map[string]bool{}
			for _, w := range tab.Works {
				if w.Column != "" {
					laid = append(laid, w)
					own[w.Column] = true
					if w.BonusColumn != nil && *w.BonusColumn != "" {
						own[*w.BonusColumn] = true
					}
				}
			}
			order := map[string]int{}
			for i, c := range cells {
				if _, ok := order[c.Column]; !ok {
					order[c.Column] = i
				}
			}
			position := func(col string) int {
				if i, ok := order[col]; ok {
					return i
				}
				return len(cells)
			}
			sort.SliceStable(laid, func(i, j int) bool { return position(laid[i].Column) < position(laid[j].Column) })
			before := -1
			for _, w := range laid {
				at, inOrder := order[w.Column]
				parts := []httpx.Object{}
				if inOrder {
					for _, c := range cells[before+1 : max(at, before+1)] {
						if own[c.Column] {
							continue
						}
						if _, noted := notes[c.Column]; c.Value == "" && (!noted || notes[c.Column] == "") {
							continue
						}
						parts = append(parts, httpx.Object{{Key: "header", Value: calc.Short(c.Header)},
							{Key: "value", Value: c.Value}, {Key: "note", Value: note(c.Column)}})
					}
					before = at
				}
				workKey := sid + "|" + tab.Tab + "|" + w.Column + "|" + w.Header
				item, isRead := read[workKey]
				var bonus any
				if w.BonusColumn != nil {
					for _, c := range cells {
						if c.Column == *w.BonusColumn {
							bonus = c.Value
							break
						}
					}
				}
				if !isRead && w.Raw == nil {
					continue // no number in the own row: nothing to show
				}
				seen[workKey] = true
				label := w.Label
				if label == "" {
					label = w.Header
				}
				var value, sheetValue any
				var valueOf *float64
				if isRead {
					label, value, sheetValue = item.Label, item.Value, item.SheetValue
					v := float64(item.Value)
					valueOf = &v
				}
				noteLines := []string{}
				for _, n := range []string{notes[w.Column], notes[deref(w.BonusColumn)]} {
					if n != "" {
						noteLines = append(noteLines, n)
					}
				}
				checking := !isRead && w.Graded != nil && 0 < float64(*w.Graded) && float64(*w.Graded) < grades.GradedShare
				works = append(works, httpx.Object{{Key: "id", Value: workKey}, {Key: "book", Value: sid},
					{Key: "tab", Value: tab.Tab}, {Key: "label", Value: label}, {Key: "value", Value: value},
					{Key: "raw", Value: floatPtr(w.Raw)}, {Key: "max", Value: w.Max}, {Key: "bonus", Value: bonus},
					{Key: "parts", Value: parts}, {Key: "pending", Value: !isRead},
					{Key: "note", Value: nullable(strings.Join(noteLines, "\n"))}, {Key: "how", Value: told(tab.Tab, w.Column)},
					{Key: "checking", Value: checking}, {Key: "new", Value: isRead && item.New},
					{Key: "edited", Value: isRead && item.Edited}, {Key: "sheet_value", Value: sheetValue},
					{Key: "own", Value: false}})
				tally = append(tally, counted{valueOf, !isRead, isRead && item.New})
			}
		}
		// a grade read whose tab the view no longer lays out: still shown, as the lens showed it
		for _, item := range items {
			if seen[item.ID] {
				continue
			}
			o := d.object(item)
			o = append(o, httpx.Field{Key: "book", Value: sid}, httpx.Field{Key: "tab", Value: ""},
				httpx.Field{Key: "parts", Value: []any{}}, httpx.Field{Key: "pending", Value: false},
				httpx.Field{Key: "checking", Value: false}, httpx.Field{Key: "own", Value: false})
			works = append(works, o)
			v := float64(item.Value)
			tally = append(tally, counted{&v, false, item.New})
		}
		booksOut = append(booksOut, httpx.Object{{Key: "id", Value: sid}, {Key: "url", Value: b.URL},
			{Key: "label", Value: b.Label}, {Key: "name", Value: b.Name}, {Key: "summaries", Value: summaries},
			{Key: "calc", Value: len(sheet.Targets()) > 0}})
	}
	if d.Grades != nil {
		// the grades added by hand: their course is a title, as «+» gives it
		manual, err := d.Grades.Manual()
		if err != nil {
			return nil, err
		}
		for _, g := range manual {
			course := deref(g.Course)
			gkey, err := d.Key(course)
			if err != nil {
				return nil, err
			}
			match := gkey == key && gkey != ""
			for _, name := range names {
				if match {
					break
				}
				if match, err = d.Schedule.SameCourse(course, name); err != nil {
					return nil, err
				}
			}
			if !match {
				continue
			}
			works = append(works, httpx.Object{{Key: "id", Value: g.ID}, {Key: "book", Value: nil}, {Key: "tab", Value: ""},
				{Key: "label", Value: g.Label}, {Key: "value", Value: g.Value}, {Key: "raw", Value: nil},
				{Key: "max", Value: nil}, {Key: "bonus", Value: nil}, {Key: "parts", Value: []any{}},
				{Key: "pending", Value: false}, {Key: "checking", Value: false}, {Key: "new", Value: g.New},
				{Key: "edited", Value: false}, {Key: "sheet_value", Value: nil}, {Key: "own", Value: true}})
			v := float64(g.Value)
			tally = append(tally, counted{&v, false, g.New})
		}
	}
	var values []float64
	pending, fresh := 0, 0
	for _, t := range tally {
		if !t.pending && t.value != nil {
			values = append(values, *t.value)
		}
		if t.pending {
			pending++
		}
		if t.new {
			fresh++
		}
	}
	var average any
	if len(values) > 0 {
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		average = pyjson.Float(round1(sum / float64(len(values))))
	}
	return httpx.Object{{Key: "count", Value: len(values)}, {Key: "pending", Value: pending}, {Key: "new", Value: fresh},
		{Key: "average", Value: average}, {Key: "works", Value: works}, {Key: "books", Value: booksOut}}, nil
}

// shownValue is a formula's value as JSON: a number as Python's float, a bool as 1.0 or 0.0.
func shownValue(v fx.Value) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return pyjson.Float(x)
	case bool:
		if x {
			return pyjson.Float(1)
		}
		return pyjson.Float(0)
	}
	return v
}

// Solve is the calculator's question: the least value of an input that brings the target to the goal.
type Solve struct {
	Cell string
	Goal float64
}

// Calc is the calculator of a gradebook of the course (17.6): the targets it may aim at, the target counted with the
// given values over the sheet's, its inputs, and with solve the least value of that input that brings the target to
// the goal. Nil for a gradebook that is not the course's, or a target it has not.
func (d *Data) Calc(key, bookID, target string, given map[string]*float64, solve *Solve) (httpx.Object, error) {
	if d.Grades == nil {
		return nil, nil
	}
	names, err := d.names(key)
	if err != nil {
		return nil, err
	}
	found, err := d.books(key, names)
	if err != nil {
		return nil, err
	}
	known := false
	for _, b := range found {
		known = known || b.ID == bookID
	}
	if !known {
		return nil, nil
	}
	view, err := d.Grades.View(bookID)
	if err != nil {
		return nil, err
	}
	sheet := calc.NewBook(view)
	targets := sheet.Targets()
	if target == "" {
		target = sheet.DefaultTarget(targets)
	}
	has := false
	for _, t := range targets {
		has = has || t.Cell == target
	}
	if !has {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, input := range sheet.Inputs(target, map[string]*float64{}) {
		allowed[input.Cell] = true
	}
	kept := map[string]*float64{}
	for cell, value := range given {
		if allowed[cell] {
			kept[cell] = value
		}
	}
	tab, col := calc.Split(target)
	listed := []httpx.Object{}
	for _, t := range targets {
		listed = append(listed, httpx.Object{{Key: "cell", Value: t.Cell}, {Key: "tab", Value: t.Tab},
			{Key: "name", Value: t.Name}, {Key: "value", Value: t.Value}})
	}
	answer := httpx.Object{{Key: "targets", Value: listed}, {Key: "target", Value: target},
		{Key: "value", Value: shownValue(sheet.Value(tab, col, kept))}, {Key: "how", Value: nullable(sheet.Written(tab, col))},
		{Key: "inputs", Value: sheet.Inputs(target, kept)}}
	if solve != nil && allowed[solve.Cell] {
		solved := sheet.Solve(target, solve.Cell, solve.Goal, kept)
		answer = append(answer, httpx.Field{Key: "solve", Value: httpx.Object{{Key: "cell", Value: solve.Cell},
			{Key: "goal", Value: pyjson.Float(solve.Goal)}, {Key: "need", Value: solved.Need},
			{Key: "reached", Value: solved.Reached}, {Key: "max", Value: solved.Max}}})
	}
	return answer, nil
}
