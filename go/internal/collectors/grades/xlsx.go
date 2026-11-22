package grades

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	fx "github.com/IvanIkra/infomaster/go/internal/processing/formulas"
)

// The workbook is read the way openpyxl reads it in read-only mode, so that every cell has the type and the text
// the Python service gave it: a header's date reads "2026-09-17", not its serial number, and a grade's key stays the
// same after the switch.

const (
	mainNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	relsNS = "http://schemas.openxmlformats.org/package/2006/relationships"
	docNS  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
)

// TimeOfDay is a cell openpyxl reads as a datetime.time: a time format with a serial below 1.
type TimeOfDay time.Duration

// String is Python's str of a time: 12:30:00, or 12:30:00.500000.
func (t TimeOfDay) String() string {
	d := time.Duration(t)
	h, m, s, us := int(d/time.Hour), int(d/time.Minute)%60, int(d/time.Second)%60, int(d/time.Microsecond)%1e6
	text := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text
}

// Span is a cell openpyxl reads as a datetime.timedelta: a format like [h]:mm.
type Span time.Duration

// String is Python's str of a timedelta: 1 day, 2:00:00.
func (s Span) String() string {
	us := int64(time.Duration(s) / time.Microsecond)
	days := us / 86_400_000_000
	rest := us % 86_400_000_000
	if rest < 0 {
		days--
		rest += 86_400_000_000
	}
	secs, micros := rest/1e6, rest%1e6
	text := fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	if micros != 0 {
		text += fmt.Sprintf(".%06d", micros)
	}
	if days != 0 {
		plural := "s"
		if days == 1 || days == -1 {
			plural = ""
		}
		text = fmt.Sprintf("%d day%s, %s", days, plural, text)
	}
	return text
}

// notWorkbook is the error of data that is no workbook: a login page, a broken file.
func notWorkbook(kind string) error {
	return &ReadError{fmt.Sprintf("not a workbook, is the sheet open to anyone with the link? (%s)", kind)}
}

// ReadError is a sheet that cannot be read (ValueError in the Python service): no workbook, too big.
type ReadError struct{ Msg string }

func (e *ReadError) Error() string { return e.Msg }

// CheckZip refuses a workbook whose parts unpack to more than UnzippedMax (a zip bomb), or that is no zip.
func CheckZip(data []byte) (*zip.Reader, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, notWorkbook("BadZipFile")
	}
	var unpacked uint64
	for _, f := range z.File {
		unpacked += f.UncompressedSize64
	}
	if unpacked > UnzippedMax {
		return nil, &ReadError{fmt.Sprintf("the workbook unpacks to %d MB: too big to read", unpacked>>20)}
	}
	return z, nil
}

type book struct {
	files       map[string]*zip.File
	part        string // the workbook's own part
	sheets      []sheetInfo
	strings     []string
	dateStyles  map[int]bool
	deltaStyles map[int]bool
	date1904    bool
}

type sheetInfo struct {
	name, state, part string
}

type missingPart struct{ name string }

func (e *missingPart) Error() string { return "no part " + e.name }

func (b *book) open(name string) (io.ReadCloser, error) {
	f := b.files[name]
	if f == nil {
		return nil, &missingPart{name}
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, int64(f.UncompressedSize64)), rc}, nil // never past the size checked
}

func (b *book) read(name string) ([]byte, error) {
	rc, err := b.open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

type relation struct{ kind, target string }

// relTarget is a relationship's part: absolute from the package's root, or relative to the part's folder.
func relTarget(part, to string) string {
	if strings.HasPrefix(to, "/") {
		return strings.TrimLeft(to, "/")
	}
	return path.Clean(path.Join(path.Dir(part), to))
}

// rels are the relationships of a part by id, in the file's order of ids.
func (b *book) rels(part string) (map[string]relation, error) {
	name := path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
	if path.Dir(part) == "." {
		name = "_rels/" + path.Base(part) + ".rels"
	}
	if b.files[name] == nil {
		return map[string]relation{}, nil
	}
	data, err := b.read(name)
	if err != nil {
		return nil, err
	}
	out := map[string]relation{}
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && t.Name.Space == relsNS && t.Name.Local == "Relationship" {
				out[attr(t, "", "Id")] = relation{attr(t, "", "Type"), relTarget(part, attr(t, "", "Target"))}
			}
		case xml.EndElement:
			depth--
		}
	}
}

func attr(t xml.StartElement, space, local string) string {
	for _, a := range t.Attr {
		if a.Name.Local == local && (space == "" && a.Name.Space == "" || a.Name.Space == space) {
			return a.Value
		}
	}
	return ""
}

func hasAttr(t xml.StartElement, local string) (string, bool) {
	for _, a := range t.Attr {
		if a.Name.Local == local && a.Name.Space == "" {
			return a.Value, true
		}
	}
	return "", false
}

func openBook(z *zip.Reader) (*book, error) {
	b := &book{files: map[string]*zip.File{}, dateStyles: map[int]bool{}, deltaStyles: map[int]bool{}}
	for _, f := range z.File {
		b.files[f.Name] = f
	}
	b.part = "xl/workbook.xml"
	if root, err := b.rels(""); err == nil {
		for _, r := range sortedRels(root) {
			if strings.HasSuffix(r.kind, "/officeDocument") {
				b.part = r.target
				break
			}
		}
	}
	data, err := b.read(b.part)
	if err != nil {
		return nil, err
	}
	rels, err := b.rels(b.part)
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		t, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch t.Name.Local {
		case "workbookPr":
			v := attr(t, "", "date1904")
			b.date1904 = v == "1" || strings.EqualFold(v, "true")
		case "sheet":
			rel, ok := rels[attr(t, docNS, "id")]
			if !ok || b.files[rel.target] == nil || strings.Contains(rel.kind, "chartsheet") {
				continue
			}
			state := attr(t, "", "state")
			if state == "" {
				state = "visible"
			}
			b.sheets = append(b.sheets, sheetInfo{attr(t, "", "name"), state, rel.target})
		}
	}
	shared, styles := "xl/sharedStrings.xml", "xl/styles.xml"
	for _, r := range sortedRels(rels) {
		if strings.HasSuffix(r.kind, "/sharedStrings") {
			shared = r.target
		}
		if strings.HasSuffix(r.kind, "/styles") {
			styles = r.target
		}
	}
	if b.files[shared] != nil {
		if b.strings, err = b.readStrings(shared); err != nil {
			return nil, err
		}
	}
	if b.files[styles] != nil {
		if err := b.readStyles(styles); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func sortedRels(rels map[string]relation) []relation {
	keys := make([]string, 0, len(rels))
	for k := range rels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]relation, len(keys))
	for i, k := range keys {
		out[i] = rels[k]
	}
	return out
}

// richText reads the element dec is in (si, is): its own t and the t of its runs, not the phonetic ones.
func richText(dec *xml.Decoder) (string, error) {
	var out strings.Builder
	depth, inT := 0, false
	var parents []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			parents = append(parents, t.Name.Local)
			inT = t.Name.Local == "t" && (depth == 1 || depth == 2 && parents[0] == "r")
		case xml.EndElement:
			if depth == 0 {
				return out.String(), nil
			}
			depth--
			parents = parents[:len(parents)-1]
			inT = false
		case xml.CharData:
			if inT {
				out.Write(t)
			}
		}
	}
}

func (b *book) readStrings(name string) ([]string, error) {
	rc, err := b.open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	var out []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if t, ok := tok.(xml.StartElement); ok && t.Name.Local == "si" {
			text, err := richText(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, strings.ReplaceAll(text, "x005F_", ""))
		}
	}
}

// Number formats openpyxl knows by their ids; the date and time ones decide a cell's type.
var builtinFormats = map[int]string{0: "General", 1: "0", 2: "0.00", 3: "#,##0", 4: "#,##0.00",
	5: `"$"#,##0_);("$"#,##0)`, 6: `"$"#,##0_);[Red]("$"#,##0)`, 7: `"$"#,##0.00_);("$"#,##0.00)`,
	8: `"$"#,##0.00_);[Red]("$"#,##0.00)`, 9: "0%", 10: "0.00%", 11: "0.00E+00", 12: "# ?/?", 13: "# ??/??",
	14: "mm-dd-yy", 15: "d-mmm-yy", 16: "d-mmm", 17: "mmm-yy", 18: "h:mm AM/PM", 19: "h:mm:ss AM/PM", 20: "h:mm",
	21: "h:mm:ss", 22: "m/d/yy h:mm", 37: "#,##0_);(#,##0)", 38: "#,##0_);[Red](#,##0)",
	39: "#,##0.00_);(#,##0.00)", 40: "#,##0.00_);[Red](#,##0.00)", 41: `_(* #,##0_);_(* \(#,##0\);_(* "-"_);_(@_)`,
	42: `_("$"* #,##0_);_("$"* \(#,##0\);_("$"* "-"_);_(@_)`, 43: `_(* #,##0.00_);_(* \(#,##0.00\);_(* "-"??_);_(@_)`,
	44: `_("$"* #,##0.00_)_("$"* \(#,##0.00\)_("$"* "-"??_)_(@_)`, 45: "mm:ss", 46: "[h]:mm:ss", 47: "mmss.0",
	48: "##0.0E+0", 49: "@"}

var timedeltaRe = regexp.MustCompile(`(?i)\[hh?\]|\[mm?\]|\[ss?\]`)

// isDateFormat is openpyxl's: the first section, quoted text and locale brackets left out, has a date letter not
// escaped by "_" or "\".
func isDateFormat(format string) bool {
	format, _, _ = strings.Cut(format, ";")
	var kept []rune
	s := []rune(format)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if j := strings.IndexRune(string(s[i+1:]), '"'); j >= 0 {
				i += len([]rune(string(s[i+1:])[:j])) + 1
				continue
			}
		case '[':
			rest := string(s[i:])
			if !(strings.HasPrefix(rest, "[h]") || strings.HasPrefix(rest, "[hh]") || strings.HasPrefix(rest, "[m]") ||
				strings.HasPrefix(rest, "[mm]") || strings.HasPrefix(rest, "[s]") || strings.HasPrefix(rest, "[ss]")) {
				if j := strings.IndexRune(rest, ']'); j >= 0 {
					i += len([]rune(rest[:j]))
					continue
				}
			}
		}
		kept = append(kept, s[i])
	}
	for i, r := range kept {
		if strings.ContainsRune("dmhysDMHYS", r) && (i == 0 || kept[i-1] != '_' && kept[i-1] != '\\') {
			return true
		}
	}
	return false
}

func (b *book) readStyles(name string) error {
	data, err := b.read(name)
	if err != nil {
		return err
	}
	custom := map[int]string{}
	var styles []int
	dec := xml.NewDecoder(bytes.NewReader(data))
	inXfs := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "numFmt":
				id, err := strconv.Atoi(attr(t, "", "numFmtId"))
				if err == nil {
					custom[id] = attr(t, "", "formatCode")
				}
			case "cellXfs":
				inXfs = true
			case "xf":
				if inXfs {
					id, _ := strconv.Atoi(attr(t, "", "numFmtId"))
					styles = append(styles, id)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "cellXfs" {
				inXfs = false
			}
		}
	}
	for idx, id := range styles {
		format, ok := custom[id]
		if !ok {
			format, ok = builtinFormats[id]
		}
		if !ok {
			continue
		}
		if isDateFormat(format) {
			b.dateStyles[idx] = true
		}
		if timedeltaRe.MatchString(strings.SplitN(format, ";", 2)[0]) {
			b.deltaStyles[idx] = true
		}
	}
	return nil
}

// --- cells -------------------------------------------------------------------------------------------------------

type rawCell struct {
	col   int // 1-based
	value any // in the pass that reads formulas, a formula cell's is its formula
}

// otherFormula is an array or data table formula: no string, so no formula of the own row.
type otherFormula struct{}

type rawRow struct {
	idx   int
	cells []rawCell
}

var (
	windowsEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	macEpoch     = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	refRe        = regexp.MustCompile(`^\$?([A-Za-z]{1,3})\$?([0-9]+)$`)
)

// fromExcel is openpyxl's from_excel: a serial as a datetime, a time of day, or a span; ok false outside the dates
// Python has (the cell reads "#VALUE!" then).
func (b *book) fromExcel(value float64, delta bool) (any, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 3e6 {
		return nil, false
	}
	if delta {
		us := math.RoundToEven(value * 86_400e6)
		if r := math.Mod(us, 1e6); r != 0 {
			secs := math.Floor(us / 1e6)
			us = secs*1e6 + math.RoundToEven(math.Mod(us-secs*1e6, 1e6)/1e3)*1e3
		}
		return Span(time.Duration(us) * time.Microsecond), true
	}
	day := math.Floor(value)
	fraction := value - day
	ms := math.RoundToEven(fraction * 86_400 * 1000)
	diff := time.Duration(ms) * time.Millisecond
	if value >= 0 && value < 1 && diff < 24*time.Hour {
		return TimeOfDay(diff), true
	}
	epoch := windowsEpoch
	if b.date1904 {
		epoch = macEpoch
	} else if value > 0 && value < 60 {
		day++
	}
	moment := epoch.AddDate(0, 0, int(day)).Add(diff)
	if moment.Year() < 1 || moment.Year() > 9999 {
		return nil, false
	}
	return moment, true
}

var isoRe = regexp.MustCompile(`^(?:(\d{4})-(\d{2})-(\d{2}))?T?(?:(\d{2}):(\d{2})(?::(\d{2})(\.\d{1,3})?)?)?Z?`)

func fromISO(text string) any {
	m := isoRe.FindStringSubmatch(text)
	if m == nil || m[0] == "" {
		return text
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	var us int
	if m[7] != "" {
		frac := (m[7][1:] + "000000")[:6]
		us = n(frac)
	}
	clock := time.Duration(n(m[4]))*time.Hour + time.Duration(n(m[5]))*time.Minute +
		time.Duration(n(m[6]))*time.Second + time.Duration(us)*time.Microsecond
	if m[1] == "" {
		return TimeOfDay(clock)
	}
	return time.Date(n(m[1]), time.Month(n(m[2])), n(m[3]), 0, 0, 0, 0, time.UTC).Add(clock)
}

func castNumber(text string) (any, error) {
	if strings.ContainsAny(text, ".Ee") {
		x, ok := fx.PyFloat(text)
		if !ok {
			return nil, fmt.Errorf("could not convert string to float: '%s'", text)
		}
		return x, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		if x, ok := fx.PyFloat(text); ok {
			return x, nil // an integer past int64
		}
		return nil, fmt.Errorf("invalid literal for int(): '%s'", text)
	}
	return n, nil
}

type cellXML struct {
	t, r, s  string
	v        *string
	f        *formulaXML
	inline   *string
	hasValue bool
}

type formulaXML struct {
	text, kind, si string
}

// readCell reads the c element dec is in.
func readCell(dec *xml.Decoder, start xml.StartElement) (cellXML, error) {
	c := cellXML{t: attr(start, "", "t"), r: attr(start, "", "r"), s: attr(start, "", "s")}
	for {
		tok, err := dec.Token()
		if err != nil {
			return c, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "v":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return c, err
				}
				if c.v == nil {
					c.v = &text
				}
			case "f":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return c, err
				}
				if c.f == nil {
					c.f = &formulaXML{text, attr(t, "", "t"), attr(t, "", "si")}
				}
			case "is":
				text, err := richText(dec)
				if err != nil {
					return c, err
				}
				if c.inline == nil {
					c.inline = &text
				}
			default:
				if err := dec.Skip(); err != nil {
					return c, err
				}
			}
		case xml.EndElement:
			return c, nil
		}
	}
}

// sheetReader parses the rows of a worksheet part one by one, as openpyxl's WorkSheetParser does.
type sheetReader struct {
	b        *book
	dec      *xml.Decoder
	closer   io.Closer
	formulas bool // data_only=False: a formula cell's value is its formula
	rowN     int
	colN     int
	shared   map[string]*translator
}

func (b *book) sheet(part string, formulas bool) (*sheetReader, error) {
	rc, err := b.open(part)
	if err != nil {
		return nil, err
	}
	return &sheetReader{b: b, dec: xml.NewDecoder(rc), closer: rc, formulas: formulas,
		shared: map[string]*translator{}}, nil
}

func (sr *sheetReader) Close() error { return sr.closer.Close() }

// next is the next row of the part; ok false at its end.
func (sr *sheetReader) next() (rawRow, bool, error) {
	for {
		tok, err := sr.dec.Token()
		if err == io.EOF {
			return rawRow{}, false, nil
		}
		if err != nil {
			return rawRow{}, false, err
		}
		t, ok := tok.(xml.StartElement)
		if !ok || t.Name.Local != "row" || t.Name.Space != mainNS {
			continue
		}
		if r, ok := hasAttr(t, "r"); ok {
			n, err := strconv.Atoi(r)
			if err != nil {
				f, ferr := strconv.ParseFloat(r, 64)
				if ferr != nil || f != math.Trunc(f) {
					return rawRow{}, false, fmt.Errorf("%s is not a valid row number", r)
				}
				n = int(f)
			}
			sr.rowN = n
		} else {
			sr.rowN++
		}
		sr.colN = 0
		row := rawRow{idx: sr.rowN}
		for {
			tok, err := sr.dec.Token()
			if err != nil {
				return rawRow{}, false, err
			}
			if _, end := tok.(xml.EndElement); end {
				break
			}
			ct, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if ct.Name.Local != "c" {
				if err := sr.dec.Skip(); err != nil {
					return rawRow{}, false, err
				}
				continue
			}
			c, err := readCell(sr.dec, ct)
			if err != nil {
				return rawRow{}, false, err
			}
			cell, err := sr.cell(c)
			if err != nil {
				return rawRow{}, false, err
			}
			row.cells = append(row.cells, cell)
		}
		return row, true, nil
	}
}

func (sr *sheetReader) cell(c cellXML) (rawCell, error) {
	kind := c.t
	if kind == "" {
		kind = "n"
	}
	style, _ := strconv.Atoi(c.s)
	if c.r != "" {
		m := refRe.FindStringSubmatch(c.r)
		if m == nil {
			return rawCell{}, fmt.Errorf("invalid cell coordinates (%s)", c.r)
		}
		sr.colN = fx.ColIndex(strings.ToUpper(m[1])) + 1
	} else {
		sr.colN++
	}
	out := rawCell{col: sr.colN}
	var value *string
	if kind != "inlineStr" && c.v != nil && *c.v != "" {
		value = c.v
	}
	if sr.formulas && c.f != nil {
		formula, err := sr.formula(c)
		if err != nil {
			return rawCell{}, err
		}
		out.value = formula
		return out, nil
	}
	switch {
	case value != nil:
		switch kind {
		case "n":
			n, err := castNumber(*value)
			if err != nil {
				return rawCell{}, err
			}
			out.value = n
			if sr.b.dateStyles[style] {
				x, _ := n.(float64)
				if i, ok := n.(int64); ok {
					x = float64(i)
				}
				if moment, ok := sr.b.fromExcel(x, sr.b.deltaStyles[style]); ok {
					out.value = moment
				} else {
					out.value = "#VALUE!"
				}
			}
		case "s":
			i, err := strconv.Atoi(strings.TrimSpace(*value))
			if err != nil || i < 0 || i >= len(sr.b.strings) {
				return rawCell{}, errors.New("list index out of range")
			}
			out.value = sr.b.strings[i]
		case "b":
			i, err := strconv.Atoi(strings.TrimSpace(*value))
			if err != nil {
				return rawCell{}, fmt.Errorf("invalid literal for int(): '%s'", *value)
			}
			out.value = i != 0
		case "d":
			out.value = fromISO(*value)
		default: // str, e and anything else: the text
			out.value = *value
		}
	case kind == "inlineStr" && c.inline != nil:
		out.value = *c.inline
	}
	return out, nil
}

// formula is openpyxl's parse_formula: "=" and the text; a shared formula's dependent cell gets the master's text
// moved to its place. An array or data table formula is no string, and so no formula of the own row.
func (sr *sheetReader) formula(c cellXML) (any, error) {
	value := "=" + c.f.text
	switch c.f.kind {
	case "array", "dataTable":
		return otherFormula{}, nil
	case "shared":
		if trans, ok := sr.shared[c.f.si]; ok {
			return trans.translate(c.r)
		}
		if value != "=" {
			trans, err := newTranslator(value, c.r)
			if err != nil {
				return "", err
			}
			sr.shared[c.f.si] = trans
		}
	}
	return value, nil
}

// rowValues is openpyxl's _get_row with values only: the cells up to the last one's column (at most limit), empty
// ones nil.
func rowValues(row rawRow, limit int) []any {
	if len(row.cells) == 0 {
		return nil
	}
	width := min(row.cells[len(row.cells)-1].col, limit)
	out := make([]any, width)
	for _, c := range row.cells {
		if c.col >= 1 && c.col <= width {
			out[c.col-1] = c.value
		}
	}
	return out
}
