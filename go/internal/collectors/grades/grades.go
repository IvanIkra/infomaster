// Package grades is the teachers' grade sheets (§5.2, §6.7, Tablo's §3.1, §3.3): the collector reads, the
// processing lays out.
//
// The sheets are open to anyone with the link (the setting grades_sheets): each is downloaded as xlsx every 30
// minutes. A Google Sheet is exported as xlsx and its title is read once from the HTML view; a Dropbox link to an
// .xlsx file is downloaded as it is (dl=1) and its title is the file's name without the extension.
//
// In every visible tab the student's own row is found by the name (grades_student); of a tab the top rows above it
// (at most TopRows) and that row are kept (TabView). Where the header ends among the top rows is the model's to say,
// so the top rows go to it whole, other students' rows among them (§10); the own name does not. The layout by Haiku,
// the 0-10 scale and what counts as a new grade are processing/grades's: OnSheet gets a sheet read and says what
// came of it (Read). The own row's formulas and the teachers' comments on its cells are read too (17.6, D75), still
// streaming: a full load of a large gradebook takes more memory than the server's container has.
//
// Failures count per sheet: the download, a file that is no workbook (a sheet closed to the link gives a login page,
// a deleted or private Dropbox link an HTML page), no own row in any tab (a wrong name, or the row is not there yet:
// the grades kept are not dropped for it), the layout (Haiku, the limit). A failed sheet keeps its grades as they
// were. A sheet failing for 12 hours is told to the owner once (pushed), and once more when it reads again; the first
// read of a sheet is told too. New grades are not pushed: the web app and the API show them (D55).
package grades

import (
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	fx "github.com/IvanIkra/infomaster/go/internal/processing/formulas"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

const (
	XLSXMax         = 20 << 20  // bytes of a downloaded sheet (D79: the container has 256 MB)
	UnzippedMax     = 120 << 20 // bytes of its parts unpacked, all together: a small file may unpack to gigabytes
	RowsMax         = 5000      // of a tab read: a gradebook is far smaller
	ColumnsMax      = 400       // one cell far away pads every row
	TopRows         = 15        // rows of a tab above the own row the model sees: the header is somewhere among them
	CourseFromTitle = 8         // characters of the sheet's title that name the course when its label is not set
	NoteChars       = 1000
)

// --- reading a workbook --------------------------------------------------------------------------------------

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

// Norm is lower case, ё as е, punctuation but "-" as spaces: "ИВАНОВ  Иван," -> "иванов иван".
func Norm(text string) string {
	text = strings.ReplaceAll(strings.ToLower(text), "ё", "е")
	text = strings.Map(func(r rune) rune {
		if isWord(r) || pytext.IsSpace(r) || r == '-' {
			return r
		}
		return ' '
	}, text)
	return strings.Join(pytext.Split(text), " ")
}

// Matches reports a cell that starts with the words of the name: "Иванов Иван" finds "ИВАНОВ Иван Петрович".
func Matches(cell any, student []string) bool {
	text, ok := cell.(string)
	if !ok {
		return false
	}
	words := pytext.Split(Norm(text))
	return len(words) >= len(student) && slices.Equal(words[:len(student)], student)
}

// Text is a cell as text, as Python's str() has it: 7 for a whole number, 2026-09-17 for a date.
func Text(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case time.Time:
		if v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0 {
			return v.Format("2006-01-02")
		}
		return v.Format("2006-01-02 15:04")
	case float64:
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			return strconv.FormatFloat(v, 'f', 0, 64)
		}
		return fx.Repr(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return pytext.Strip(v)
	case fmt.Stringer:
		return pytext.Strip(v.String())
	}
	return ""
}

// Column is 0 -> A, 26 -> AA.
func Column(index int) string { return fx.ColName(index) }

// TabView is what is kept of one tab: its top rows and the student's own row.
type TabView struct {
	Tab     string
	Rows    []Row          // the top rows that are not empty
	Mine    map[string]any // column letter -> raw value in the student's row, without the name
	NameCol string
	OwnRow  int // its number in the tab
	// Column letter -> the share of the other students with a number above zero there (17.3, D72): whether a work
	// is graded for the group, so the student's zero in it is a real one. Only the share is kept, no one's grade.
	Graded map[string]float64
	// 17.6: the own row's formulas (column letter -> "=..."), and the teachers' comments on its cells (column
	// letter -> text, without the author). Never another row's comments.
	Formulas map[string]string
	Notes    map[string]string
}

// Row is a top row: its number and its cells' texts by column letter.
type Row struct {
	Number int
	Cells  map[string]string
}

// Header is the rows the model named the header, in tab order.
func (v TabView) Header(numbers []int) []map[string]string {
	out := []map[string]string{}
	for _, row := range v.Rows {
		if slices.Contains(numbers, row.Number) {
			out = append(out, row.Cells)
		}
	}
	return out
}

// ColumnHeader is a column's header: its cells in the header rows, joined by " / ".
func (v TabView) ColumnHeader(col string, numbers []int) string {
	var parts []string
	for _, row := range v.Header(numbers) {
		if text := row[col]; text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " / ")
}

// RowCells is a row's cells (column letter -> text) in column order.
func RowCells(cells map[string]string) []string {
	cols := make([]string, 0, len(cells))
	for c := range cells {
		cols = append(cols, c)
	}
	SortColumns(cols)
	return cols
}

// SortColumns sorts column letters left to right.
func SortColumns(cols []string) {
	slices.SortFunc(cols, func(a, b string) int { return fx.ColIndex(a) - fx.ColIndex(b) })
}

// Positive reports a cell with a number above zero.
func Positive(value any) bool {
	switch v := value.(type) {
	case nil, bool:
		return false
	case float64:
		return v > 0
	case int64:
		return v > 0
	case string:
		x, ok := fx.PyFloat(strings.ReplaceAll(pytext.Strip(v), ",", "."))
		return ok && x > 0
	}
	return false
}

// Name is two to four words of letters: a person.
var Name = regexp.MustCompile(`^[\p{L}\p{Nl}\p{No}][\p{L}\p{N}_.\-]*(?: [\p{L}\p{Nl}\p{No}][\p{L}\p{N}_.\-]*){1,3}$`)

// IsPersonName reports a name column's text that is a person's name, not a header's words.
func IsPersonName(text string) bool {
	return Name.MatchString(strings.Join(pytext.Split(text), " ")) && !strings.ContainsFunc(text, unicode.IsDigit)
}

var groupHeaders = map[string]bool{"группа": true, "group": true} // a stream's sheet is graded group by group

// groups is each row's group when a column above the own row is headed «Группа»: the last group named at or above
// the row (a group's number stands once at its first row, or on every row). nil without such a column.
func groups(rows [][]any, ownI, nameJ int) []string {
	j := -1
	for _, row := range rows[:ownI] {
		for k, c := range row {
			if s, ok := c.(string); ok && k != nameJ && groupHeaders[pytext.Casefold(strings.Join(pytext.Split(s), " "))] {
				j = k
				break
			}
		}
		if j >= 0 {
			break
		}
	}
	if j < 0 {
		return nil
	}
	out := make([]string, len(rows))
	current := ""
	for i, row := range rows {
		if j < len(row) && Text(row[j]) != "" {
			current = Text(row[j])
		}
		out[i] = current
	}
	return out
}

// GradedShares is for each column of the own row the share of the other students (a row whose name column holds a
// person's name, not a header's words) who have a number above zero in it. In a stream's sheet with a group column
// they are the student's own group (D80): another group not graded yet does not make the own group's zero a
// placeholder.
func GradedShares(rows [][]any, ownI, nameJ int) map[string]float64 {
	var others []int
	for i, row := range rows {
		if i == ownI || nameJ >= len(row) {
			continue
		}
		if s, ok := row[nameJ].(string); ok && IsPersonName(s) {
			others = append(others, i)
		}
	}
	if group := groups(rows, ownI, nameJ); group != nil && group[ownI] != "" {
		var same []int
		for _, i := range others {
			if group[i] == group[ownI] {
				same = append(same, i)
			}
		}
		if len(same) > 0 {
			others = same
		}
	}
	out := map[string]float64{}
	if len(others) == 0 {
		return out
	}
	for j, value := range rows[ownI] {
		if j == nameJ || Text(value) == "" {
			continue
		}
		n := 0
		for _, i := range others {
			if j < len(rows[i]) && Positive(rows[i][j]) {
				n++
			}
		}
		out[Column(j)] = fx.Round(float64(n)/float64(len(others)), 3)
	}
	return out
}

// tabRows reads a tab's rows as openpyxl's iter_rows(values_only) gives them: missing rows empty, at most RowsMax
// rows of at most ColumnsMax cells.
func (b *book) tabRows(part string) ([][]any, error) {
	sr, err := b.sheet(part, false)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	var rows [][]any
	counter := 1
	for len(rows) < RowsMax {
		row, ok, err := sr.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		for ; counter < row.idx && len(rows) < RowsMax; counter++ {
			rows = append(rows, nil) // some rows are missing
		}
		if len(rows) >= RowsMax {
			break
		}
		if counter <= row.idx {
			rows = append(rows, rowValues(row, ColumnsMax))
			counter++
		}
	}
	return rows, nil
}

// ReadViews is the visible tabs that have the student's row. A *ReadError when the data is no workbook.
//
// Streaming, as the server's memory asks (a full load of a large gradebook took more than the container has): the
// values in one pass, the own row's formulas in a second, the comments from their parts.
func ReadViews(data []byte, student string) ([]TabView, error) {
	target := pytext.Split(Norm(student))
	z, err := CheckZip(data)
	if err != nil {
		return nil, err
	}
	b, err := openBook(z)
	if err != nil {
		return nil, notWorkbook(errorKind(err))
	}
	var views []TabView
	parts := map[string]string{}
	for _, ws := range b.sheets {
		if ws.state != "visible" {
			continue
		}
		rows, err := b.tabRows(ws.part)
		if err != nil {
			return nil, err
		}
		ownI, nameJ := -1, -1
	search:
		for i, r := range rows {
			for j, c := range r {
				if Matches(c, target) {
					ownI, nameJ = i, j
					break search
				}
			}
		}
		if ownI < 0 {
			continue
		}
		var top []Row
		for i, row := range rows[:min(ownI, TopRows)] {
			cells := map[string]string{}
			for j, c := range row {
				if text := Text(c); text != "" {
					cells[Column(j)] = text
				}
			}
			if len(cells) > 0 {
				top = append(top, Row{i + 1, cells})
			}
		}
		mine := map[string]any{}
		for j, c := range rows[ownI] {
			if j != nameJ && Text(c) != "" {
				mine[Column(j)] = c
			}
		}
		parts[ws.name] = ws.part
		views = append(views, TabView{Tab: ws.name, Rows: top, Mine: mine, NameCol: Column(nameJ), OwnRow: ownI + 1,
			Graded: GradedShares(rows, ownI, nameJ)})
	}
	if len(views) == 0 {
		return views, nil
	}
	ownRows := map[string]int{}
	for _, v := range views {
		ownRows[v.Tab] = v.OwnRow
	}
	formulas, err := b.ownFormulas(parts, ownRows)
	if err != nil { // the values are read: the formulas are an extra
		slog.Warn("the formulas of a sheet cannot be read", "error", err)
		formulas = map[string]map[string]string{}
	}
	notes, err := b.ownNotes(ownRows)
	if err != nil {
		slog.Warn("the comments of a sheet cannot be read", "error", err)
		notes = map[string]map[string]string{}
	}
	for i, v := range views {
		views[i].Formulas, views[i].Notes = map[string]string{}, map[string]string{}
		for c, f := range formulas[v.Tab] {
			if c != v.NameCol {
				views[i].Formulas[c] = f
			}
		}
		for c, n := range notes[v.Tab] {
			if c != v.NameCol {
				views[i].Notes[c] = n
			}
		}
	}
	return views, nil
}

func errorKind(err error) string {
	switch err.(type) {
	case *missingPart:
		return "KeyError"
	}
	if strings.Contains(fmt.Sprintf("%T", err), "xml") {
		return "ParseError"
	}
	return "Error"
}

// ownFormulas is the formulas of the own row of each tab: tab -> column letter -> "=...". A formula's text only.
func (b *book) ownFormulas(parts map[string]string, ownRows map[string]int) (map[string]map[string]string, error) {
	found := map[string]map[string]string{}
	for _, ws := range b.sheets {
		row, ok := ownRows[ws.name]
		if !ok || parts[ws.name] != ws.part {
			continue
		}
		cells, err := b.formulaRow(ws.part, row)
		if err != nil {
			return nil, err
		}
		found[ws.name] = map[string]string{}
		for j, c := range cells {
			if s, ok := c.(string); ok && strings.HasPrefix(s, "=") {
				found[ws.name][Column(j)] = s
			}
		}
	}
	return found, nil
}

func (b *book) formulaRow(part string, own int) ([]any, error) {
	sr, err := b.sheet(part, true)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	for {
		row, ok, err := sr.next()
		if err != nil {
			return nil, err
		}
		if !ok || row.idx > own {
			return nil, nil
		}
		if row.idx == own {
			return rowValues(row, ColumnsMax), nil
		}
	}
}
