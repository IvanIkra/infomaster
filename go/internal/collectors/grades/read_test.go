package grades

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	fx "github.com/IvanIkra/infomaster/go/internal/processing/formulas"
)

// The workbooks in testdata are written by openpyxl from the Python tests' builders (names are made up); each
// .json beside one is what the Python service's read_views made of it.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func typed(v any) []any {
	switch x := v.(type) {
	case bool:
		return []any{"bool", x}
	case int64:
		return []any{"int", float64(x)}
	case float64:
		return []any{"float", fx.Repr(x)}
	case string:
		return []any{"str", x}
	case time.Time:
		return []any{"datetime", x.Format("2006-01-02 15:04:05")}
	case TimeOfDay:
		return []any{"time", x.String()}
	case Span:
		return []any{"timedelta", x.String()}
	}
	return []any{"?", v}
}

func TestReadViewsAsPythonDoes(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		var golden struct {
			Student string           `json:"student"`
			Views   []map[string]any `json:"views"`
		}
		raw, _ := os.ReadFile(file)
		if err := json.Unmarshal(raw, &golden); err != nil {
			t.Fatal(err)
		}
		views, err := ReadViews(fixture(t, name), golden.Student)
		if err != nil {
			t.Fatal(name, err)
		}
		got := []map[string]any{}
		for _, v := range views {
			rows := []any{}
			for _, r := range v.Rows {
				cells := map[string]any{}
				for c, text := range r.Cells {
					cells[c] = text
				}
				rows = append(rows, []any{float64(r.Number), cells})
			}
			mine := map[string]any{}
			for c, x := range v.Mine {
				mine[c] = typed(x)
			}
			graded := map[string]any{}
			for c, x := range v.Graded {
				graded[c] = x
			}
			formulas, notes := map[string]any{}, map[string]any{}
			for c, x := range v.Formulas {
				formulas[c] = x
			}
			for c, x := range v.Notes {
				notes[c] = x
			}
			got = append(got, map[string]any{"tab": v.Tab, "rows": rows, "mine": mine, "name_col": v.NameCol,
				"own_row": float64(v.OwnRow), "graded": graded, "formulas": formulas, "notes": notes})
		}
		if !reflect.DeepEqual(got, golden.Views) {
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(golden.Views)
			t.Errorf("%s:\n go %s\npy %s", name, a, b)
		}
	}
}

func TestReadViewsKeepsTheTopRowsAndTheOwnRow(t *testing.T) {
	views, err := ReadViews(fixture(t, "book_default"), "Иванов Иван")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Tab != "ДЗ-1А" || views[1].Tab != "Квизы" { // the hidden tab is skipped
		t.Fatal(views)
	}
	dz := views[0]
	if len(dz.Rows) != 4 || dz.Rows[3].Number != 4 || dz.OwnRow != 5 || dz.Mine["F"] != 5.9 {
		t.Fatalf("%+v", dz)
	}
	want := []map[string]string{{"A": "Группа", "B": "Студент", "C": "Группа*", "D": "1а", "E": "1б", "F": "Балл",
		"G": "Балл*"}}
	if got := dz.Header([]int{2}); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// D79: a far cell pads no row, a sheet past RowsMax is cut, and a zip bomb is refused before it unpacks.
func TestACraftedWorkbookStaysSmall(t *testing.T) {
	views, err := ReadViews(fixture(t, "far"), "Иванов Иван Сергеевич")
	if err != nil || len(views) != 1 || views[0].OwnRow != 2 || !reflect.DeepEqual(views[0].Mine, map[string]any{"B": int64(7)}) {
		t.Fatal(views, err)
	}
	var bomb bytes.Buffer
	z := zip.NewWriter(&bomb)
	w, _ := z.Create("xl/sharedStrings.xml")
	w.Write(bytes.Repeat([]byte("0"), UnzippedMax+1))
	z.Close()
	if _, err := ReadViews(bomb.Bytes(), "Иванов"); err == nil || !strings.Contains(err.Error(), "unpacks") {
		t.Fatal(err)
	}
	for _, page := range []string{"<html>login</html>", "<html>Sign in</html>"} {
		_, err := ReadViews([]byte(page), "Иванов Иван")
		var readErr *ReadError
		if !errors.As(err, &readErr) || !strings.Contains(err.Error(), "not a workbook") {
			t.Fatal(err)
		}
	}
	// a zip that is no workbook
	var empty bytes.Buffer
	z = zip.NewWriter(&empty)
	w, _ = z.Create("readme.txt")
	w.Write([]byte("hi"))
	z.Close()
	if _, err := ReadViews(empty.Bytes(), "Иванов"); err == nil || !strings.Contains(err.Error(), "not a workbook") {
		t.Fatal(err)
	}
}

// rowsBook is a workbook of one tab whose sheet XML is given: shared formulas, missing rows and rows past RowsMax.
func rowsBook(t *testing.T, sheetData string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	files := map[string]string{
		"_rels/.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
			`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` +
			`<sheet name="T" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<sheetData>` + sheetData + `</sheetData></worksheet>`,
	}
	for name, text := range files {
		w, _ := z.Create(name)
		w.Write([]byte(text))
	}
	z.Close()
	return buf.Bytes()
}

func TestSharedFormulasAreMovedToTheOwnRow(t *testing.T) {
	sheet := `<row r="1"><c r="A1" t="inlineStr"><is><t>Студент</t></is></c><c r="B1"><v>1</v></c></row>` +
		`<row r="2"><c r="A2" t="inlineStr"><is><t>Петров Пётр</t></is></c><c r="B2"><v>1</v></c><c r="C2"><v>2</v></c>` +
		`<c r="D2"><f t="shared" ref="D2:D4" si="0">SUM(B2:C2)*$B$1  +  'It''s'!A2</f><v>3</v></c>` +
		`<c r="E2"><f t="shared" ref="E2:E4" si="1">B2&amp;"x B2"</f></c></row>` +
		`<row r="4"><c r="A4" t="inlineStr"><is><t>Иванов Иван</t></is></c><c r="B4"><v>4</v></c>` +
		`<c r="D4"><f t="shared" si="0"/><v>9</v></c><c r="E4"><f t="shared" si="1"/></c>` +
		`<c r="F4"><f t="array" ref="F4">B4:C4</f></c></row>`
	views, err := ReadViews(rowsBook(t, sheet), "Иванов Иван")
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	// as openpyxl's Translator moves them: the run of spaces shrinks to one, "$" stays, the string stays
	want := map[string]string{"D": "=SUM(B4:C4)*$B$1 + 'It''s'!A4", "E": `=B4&"x B2"`}
	if v := views[0]; !reflect.DeepEqual(v.Formulas, want) || v.OwnRow != 4 || v.Mine["D"] != int64(9) {
		t.Fatalf("%+v", v)
	}
}

func TestRowsPastTheLimitAreNotRead(t *testing.T) {
	var sheet strings.Builder
	for r := 1; r <= RowsMax+10; r += 1000 {
		sheet.WriteString(`<row r="` + strconv.Itoa(r) + `"><c t="inlineStr"><is><t>x</t></is></c></row>`)
	}
	sheet.WriteString(`<row r="` + strconv.Itoa(RowsMax+5) + `"><c t="inlineStr"><is><t>Иванов Иван</t></is></c><c><v>1</v></c></row>`)
	views, err := ReadViews(rowsBook(t, sheet.String()), "Иванов Иван")
	if err != nil || len(views) != 0 {
		t.Fatal(views, err)
	}
}

func TestNameMatchingHandlesCaseAndYo(t *testing.T) {
	student := []string{"иванов", "иван"}
	if !Matches("ИВАНОВ  Иван Сергеевич", student) || !Matches("Иванов Иван", student) ||
		Matches("Иванова Ирина", student) || Matches(7.0, student) {
		t.Fatal("matches")
	}
	if Norm("Пётр") != "петр" || Norm("ИВАНОВ  Иван,") != "иванов иван" || Column(0) != "A" || Column(26) != "AA" {
		t.Fatal("norm")
	}
}

func TestGradedSharesCountTheOthersNotTheHeader(t *testing.T) {
	rows := [][]any{{nil, "Max за обыкновенные / сложные:", int64(7)},
		{"Группа", "Студент", "Балл"},
		{int64(260), "Образцов Нагляд Примерович", int64(5)},
		{nil, "Петров Пётр", int64(0)},
		{nil, "сидорова анна", nil},
		{nil, "Иванов Иван", int64(0)}}
	if got := GradedShares(rows, 5, 1); !reflect.DeepEqual(got, map[string]float64{"C": fx.Round(1.0/3, 3)}) {
		t.Fatal(got) // of the three others, one has points
	}
}

var stream = [][]any{{nil, nil, int64(10)},
	{"Группа", "Студент", "Балл"},
	{int64(101), "Петров Пётр", int64(0)},
	{nil, "Сидорова Анна", int64(0)},
	{nil, "Орлов Олег", int64(0)},
	{nil, "Белова Белла", int64(0)},
	{int64(102), "Козлова Катя", int64(7)},
	{nil, "Иванов Иван", int64(0)},
	{nil, "Зайцев Захар", int64(9)},
	{nil, "Лисина Лиза", int64(0)}}

// Another group not graded yet does not make the own group's zero a placeholder (D80).
func TestGradedSharesCountTheOwnGroupOfAStream(t *testing.T) {
	if got := GradedShares(stream, 7, 1); got["C"] != fx.Round(2.0/3, 3) {
		t.Fatal(got)
	}
	if got := GradedShares(stream, 3, 1); got["C"] != 0 { // the own group not graded yet: still a placeholder
		t.Fatal(got)
	}
}

func TestGradedSharesWithoutAGroupColumnCountTheWholeTab(t *testing.T) {
	var rows [][]any
	for _, row := range stream {
		rows = append(rows, append([]any{nil}, row[1:]...))
	}
	rows[1][0] = "Действительная группа" // not the group column
	if got := GradedShares(rows, 7, 1); got["C"] != fx.Round(2.0/7, 3) {
		t.Fatal(got)
	}
}

func TestText(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{{nil, ""}, {7.0, "7"}, {5.9, "5.9"}, {int64(3), "3"}, {true, "True"}, {"  x ", "x"},
		{time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC), "2026-09-17"},
		{time.Date(2026, 9, 17, 14, 5, 33, 0, time.UTC), "2026-09-17 14:05"},
		{TimeOfDay(12*time.Hour + 30*time.Minute), "12:30:00"}, {Span(26*time.Hour + 30*time.Minute), "1 day, 2:30:00"},
		{1e20, "100000000000000000000"}} {
		if got := Text(c.v); got != c.want {
			t.Errorf("%v: %q", c.v, got)
		}
	}
	if !Positive("1,5") || Positive("0") || Positive(true) || !Positive(int64(2)) || Positive(time.Now()) {
		t.Fatal("positive")
	}
	if got := SplitLines("a\r\nb\u0085c\n"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatal(got)
	}
}

func TestTranslatorAsOpenpyxls(t *testing.T) {
	trans, err := newTranslator("=foo+Sheet1!bar+A1:A3+1E+5+2:3+B:C", "A2")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := trans.translate("B3"); err != nil || got != "=foo+bar+B2:B4+1E+5+3:4+C:D" {
		t.Fatal(got, err)
	}
	trans, _ = newTranslator("=A1", "B2")
	if _, err := trans.translate("B1"); err == nil { // no row above the first
		t.Fatal("translated out of the sheet")
	}
}
