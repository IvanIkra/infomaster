package settings

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// Error is a value a setting does not take, or a setting that does not exist (SettingsError). One raised with a
// Phrase reads in the owner's language through i18n.Reason; Error() is the English.
type Error struct {
	Phrase    i18n.Phrase
	localized bool
}

func (e *Error) Error() string { return e.Phrase.En }

// Unwrap gives i18n.Reason the Phrase of an error raised with one.
func (e *Error) Unwrap() error {
	if e.localized {
		return &i18n.Error{Phrase: e.Phrase}
	}
	return nil
}

// NewError is a SettingsError with an English text, as SettingsError("...") in the Python service.
func NewError(msg string) error { return &Error{Phrase: i18n.P(msg, msg)} }

// PhraseError is a SettingsError raised with a Phrase.
func PhraseError(p i18n.Phrase) error { return &Error{Phrase: p, localized: true} }

func errorf(format string, args ...any) error { return NewError(fmt.Sprintf(format, args...)) }

func money(value any) (Float, error) {
	v, ok := number(value)
	if !ok || v < 0 {
		return 0, errorf("expected a non-negative number of dollars, got %s", repr(value))
	}
	return Float(v), nil
}

func positiveMoney(value any) (Float, error) {
	v, err := money(value)
	if err != nil {
		return 0, err
	}
	if v == 0 {
		return 0, NewError("the limit must be above zero")
	}
	return v, nil
}

// PriceKeys are the fields of a model's price, $ per million tokens; long: prompts over 100K.
var PriceKeys = []string{"input", "output", "long_input", "long_output"}

// Price is one model's entry of llm_prices, $ per million tokens.
type Price struct {
	Input      Float `json:"input"`
	Output     Float `json:"output"`
	LongInput  Float `json:"long_input"`
	LongOutput Float `json:"long_output"`
}

func prices(value any) (map[string]Price, error) {
	shape := NewError("expected {model: {input, output, long_input, long_output}}")
	var models map[string]any
	switch v := value.(type) {
	case map[string]any:
		models = v
	case map[string]Price:
		models = make(map[string]any, len(v))
		for model, p := range v {
			models[model] = map[string]any{"input": p.Input, "output": p.Output, "long_input": p.LongInput,
				"long_output": p.LongOutput}
		}
	default:
		return nil, shape
	}
	if len(models) == 0 {
		return nil, shape
	}
	result := make(map[string]Price, len(models))
	names := make([]string, 0, len(models))
	for model := range models {
		names = append(names, model)
	}
	slices.Sort(names)
	for _, model := range names {
		price, ok := models[model].(map[string]any)
		same := ok && len(price) == len(PriceKeys)
		for _, key := range PriceKeys {
			if _, has := price[key]; !has {
				same = false
			}
		}
		if !same {
			return nil, errorf("%s: expected exactly %s", model, strings.Join(PriceKeys, ", "))
		}
		var fields [4]Float
		for i, key := range PriceKeys {
			v, err := money(price[key])
			if err != nil {
				return nil, err
			}
			fields[i] = v
		}
		result[model] = Price{fields[0], fields[1], fields[2], fields[3]}
	}
	return result, nil
}

// BatchHours are the choices of N: batches are tied to the hours of the day (D19), so N divides 24.
var BatchHours = []int{1, 2, 3, 4, 6, 8, 12, 24}

func batchHour(value any) (int, bool) {
	n, ok := integer(value)
	if !ok || !slices.Contains(BatchHours, int(n)) {
		return 0, false
	}
	return int(n), true
}

func generalBatchHours(value any) (int, error) {
	n, ok := batchHour(value)
	if !ok {
		return 0, errorf("expected one of %s, got %s", tupleRepr(BatchHours), repr(value))
	}
	return n, nil
}

func hours(value any) (int, error) {
	n, ok := integer(value)
	if !ok || n < 1 || n > 168 {
		return 0, errorf("expected a whole number of hours from 1 to 168, got %s", repr(value))
	}
	return int(n), nil
}

func whole(low, high int, what string) func(any) (int, error) {
	return func(value any) (int, error) {
		n, ok := integer(value)
		if !ok || n < int64(low) || n > int64(high) {
			return 0, errorf("expected %s from %d to %d, got %s", what, low, high, repr(value))
		}
		return int(n), nil
	}
}

var clockRe = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)

// clockTime: '21:00'; '9:30' becomes '09:30'.
func clockTime(value any) (string, error) {
	text, ok := value.(string)
	var match []string
	if ok {
		match = clockRe.FindStringSubmatch(pytext.Strip(text))
	}
	if match == nil {
		return "", errorf("expected a time of day HH:MM, got %s", repr(value))
	}
	hour, _ := strconv.Atoi(match[1])
	return fmt.Sprintf("%02d:%s", hour, match[2]), nil
}

var (
	googleSheet = regexp.MustCompile(`/spreadsheets/d/([A-Za-z0-9_-]+)`) // the id of a Google Sheet in its link
	// A Dropbox share link to a file: /scl/fi/<id>/<name> (since 2023) or the older /s/<id>/<name>; a folder's is
	// /scl/fo/ or /sh/. dl.dropboxusercontent.com serves the same paths.
	dropboxHosts  = []string{"www.dropbox.com", "dropbox.com", "dl.dropbox.com", "dl.dropboxusercontent.com"}
	dropboxFile   = regexp.MustCompile(`^/(?:scl/fi|s)/([A-Za-z0-9_-]+)/([^/]+)/?$`)
	dropboxFolder = regexp.MustCompile(`^/(?:scl/fo|sh)/`)
)

// DropboxPrefix starts a Dropbox sheet's id: the prefix and the id of its link, apart from Google's ids.
const DropboxPrefix = "dropbox-"

// NotASheet is the error of a link that is neither a Google Sheet nor a Dropbox .xlsx file.
var NotASheet = i18n.P(
	"expected a link to a Google Sheet (https://docs.google.com/spreadsheets/d/…) or a Dropbox link to an .xlsx file "+
		"(https://www.dropbox.com/scl/fi/…)",
	"нужна ссылка на Google Таблицу (https://docs.google.com/spreadsheets/d/…) или ссылка Dropbox на файл .xlsx "+
		"(https://www.dropbox.com/scl/fi/…)")

const (
	SheetNameChars  = 60
	SheetLabelChars = 10 // the course in front of a grade's label: "DM HW 1A" (Tablo's budget of 23 is for both)
)

// student is the name as the grade sheets have it, "Иванов Иван"; "" turns the grades off.
func student(value any) (string, error) {
	text, ok := value.(string)
	if !ok || pytext.Len(text) > 100 {
		return "", NewError("expected the student's name as the sheets have it, at most 100 characters")
	}
	return squeeze(text), nil
}

// splitURL is urlsplit's scheme (lower case), host name (lower case) and path, as written.
func splitURL(link string) (scheme, host, path string) {
	if i := strings.Index(link, ":"); i > 0 && validScheme(link[:i]) {
		scheme, link = strings.ToLower(link[:i]), link[i+1:]
	}
	if strings.HasPrefix(link, "//") {
		link = link[2:]
		end := strings.IndexAny(link, "/?#")
		if end < 0 {
			end = len(link)
		}
		netloc := link[:end]
		link = link[end:]
		if at := strings.LastIndex(netloc, "@"); at >= 0 {
			netloc = netloc[at+1:]
		}
		if _, bracketed, ok := strings.Cut(netloc, "["); ok {
			host, _, _ = strings.Cut(bracketed, "]")
		} else {
			host, _, _ = strings.Cut(netloc, ":")
		}
		host = strings.ToLower(host)
	}
	if end := strings.IndexAny(link, "?#"); end >= 0 {
		link = link[:end]
	}
	return scheme, host, link
}

func validScheme(text string) bool {
	for i, c := range text {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (i == 0 || !(c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.')) {
			return false
		}
	}
	return true
}

// unquote is urllib's unquote: %XX decoded as UTF-8 (bad bytes replaced), anything else kept.
func unquote(text string) string {
	if !strings.Contains(text, "%") {
		return text
	}
	var out []byte
	for i := 0; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if b, err := strconv.ParseUint(text[i+1:i+3], 16, 8); err == nil {
				out = append(out, byte(b))
				i += 2
				continue
			}
		}
		out = append(out, text[i])
	}
	return strings.ToValidUTF8(string(out), "�")
}

// DropboxFile is (the link's id, the file name) of a Dropbox share link to a file; ok is false for a link that is
// not Dropbox's. An error for a folder, for a Dropbox link that is not a share link and for a file that is not .xlsx.
func DropboxFile(link string) (id, name string, ok bool, err error) {
	scheme, host, path := splitURL(pytext.Strip(link))
	if scheme != "https" || !slices.Contains(dropboxHosts, host) {
		return "", "", false, nil
	}
	if dropboxFolder.MatchString(path) {
		return "", "", true, PhraseError(i18n.P("this is a link to a Dropbox folder: share the .xlsx file itself",
			"это ссылка на папку Dropbox: нужна ссылка на сам файл .xlsx"))
	}
	found := dropboxFile.FindStringSubmatch(path)
	if found == nil {
		return "", "", true, PhraseError(NotASheet)
	}
	name = unquote(found[2])
	if !strings.HasSuffix(strings.ToLower(name), ".xlsx") {
		return "", "", true, PhraseError(NotXLSX(name))
	}
	return found[1], name, true, nil
}

// NotXLSX is the error of a Dropbox file that is not a workbook.
func NotXLSX(name string) i18n.Phrase {
	return i18n.P(fmt.Sprintf("the Dropbox file «%s» is not an .xlsx workbook", name),
		fmt.Sprintf("файл Dropbox «%s» — не книга .xlsx", name))
}

// GradeSheetID is the id a grade sheet is known by: a Google Sheet's id, or DropboxPrefix and the id of a Dropbox
// link. An error for any other link.
func GradeSheetID(link string) (string, error) {
	link = pytext.Strip(link)
	id, _, ok, err := DropboxFile(link)
	if err != nil {
		return "", err
	}
	if ok {
		return DropboxPrefix + id, nil
	}
	found := googleSheet.FindStringSubmatch(link)
	if !strings.HasPrefix(link, "https://docs.google.com/") || found == nil {
		return "", PhraseError(NotASheet)
	}
	return found[1], nil
}

// GradeSheet is one entry of grades_sheets: label is the course's short name, "" for the start of the sheet's
// title; name is the owner's for the web app (D44), kept only when given.
type GradeSheet struct {
	URL   string `json:"url"`
	Label string `json:"label"`
	Name  string `json:"name,omitempty"`
}

// gradeSheets: Google Sheets open to anyone with the link, or Dropbox share links to .xlsx files.
func gradeSheets(value any) ([]GradeSheet, error) {
	shape := NewError("expected a list of {url, label}")
	var items []map[string]any
	switch v := value.(type) {
	case []GradeSheet:
		for _, s := range v {
			item := map[string]any{"url": s.URL, "label": s.Label}
			if s.Name != "" {
				item["name"] = s.Name
			}
			items = append(items, item)
		}
	case []map[string]any:
		items = v
	case []any:
		for _, s := range v {
			item, ok := s.(map[string]any)
			if !ok {
				return nil, shape
			}
			items = append(items, item)
		}
	default:
		return nil, shape
	}
	sheets, ids := []GradeSheet{}, map[string]bool{}
	for _, sheet := range items {
		for key := range sheet {
			if key != "url" && key != "label" && key != "name" {
				return nil, shape
			}
		}
		link, ok := sheet["url"].(string)
		label, okLabel := optionalString(sheet, "label")
		name, okName := optionalString(sheet, "name")
		if !ok || !okLabel || !okName {
			return nil, shape
		}
		link = pytext.Strip(link)
		sid, err := GradeSheetID(link)
		if err != nil {
			return nil, err
		}
		if ids[sid] {
			return nil, NewError("the same sheet is listed twice")
		}
		ids[sid] = true
		label = squeeze(label)
		if pytext.Len(label) > SheetLabelChars {
			return nil, errorf("a course label has at most %d characters, got %s", SheetLabelChars, repr(label))
		}
		name = squeeze(name)
		if pytext.Len(name) > SheetNameChars {
			return nil, errorf("a sheet's name has at most %d characters", SheetNameChars)
		}
		sheets = append(sheets, GradeSheet{URL: link, Label: label, Name: name})
	}
	return sheets, nil
}

func optionalString(m map[string]any, key string) (string, bool) {
	v, has := m[key]
	if !has {
		return "", true
	}
	text, ok := v.(string)
	return text, ok
}

// WikiPages is the most course pages of a wiki.
const WikiPages = 10

var wikiPage = regexp.MustCompile(`^https?://[^/\s]+/\S+$`)

// wikiPages: addresses of course pages of a MediaWiki (D60): http(s), each once.
func wikiPages(value any) ([]string, error) {
	texts, ok := stringList(value)
	if !ok || len(texts) > WikiPages {
		return nil, errorf("expected a list of at most %d page addresses", WikiPages)
	}
	pages := []string{}
	for _, v := range texts {
		link := pytext.Strip(v)
		if !wikiPage.MatchString(link) {
			return nil, errorf("not a wiki page's address: %s", repr(link))
		}
		if !slices.Contains(pages, link) {
			pages = append(pages, link)
		}
	}
	return pages, nil
}

func stringList(value any) ([]string, bool) {
	switch v := value.(type) {
	case []string:
		return v, true
	case []any:
		texts := make([]string, len(v))
		for i, item := range v {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			texts[i] = text
		}
		return texts, true
	}
	return nil, false
}

const (
	AboutMeChars  = 500 // what the student tells the models about themselves
	ChatNoteChars = 300
)

// text is free text for the models' prompts: lines kept, spaces within them squeezed, empty lines dropped.
func text(limit int) func(any) (string, error) {
	return func(value any) (string, error) {
		raw, ok := value.(string)
		if !ok {
			return "", NewError("expected text")
		}
		var lines []string
		for _, line := range splitLines(raw) {
			if line = squeeze(line); line != "" {
				lines = append(lines, line)
			}
		}
		result := strings.Join(lines, "\n")
		if n := pytext.Len(result); n > limit {
			return "", errorf("at most %d characters, got %d", limit, n)
		}
		return result, nil
	}
}

func flag(value any) (bool, error) {
	v, ok := value.(bool)
	if !ok {
		return false, errorf("expected true or false, got %s", repr(value))
	}
	return v, nil
}

func language(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !slices.Contains(i18n.Languages, text) {
		return "", errorf("expected one of %s, got %s", strings.Join(i18n.Languages, ", "), repr(value))
	}
	return text, nil
}

// PushKinds are what the web app may push (§8.6).
var PushKinds = []string{"deadlines", "reminders", "failures", "llm"}

// pushKinds: some of PushKinds, in their order; an empty list pushes nothing.
func pushKinds(value any) ([]string, error) {
	texts, ok := stringList(value)
	for _, t := range texts {
		if !slices.Contains(PushKinds, t) {
			ok = false
		}
	}
	if !ok {
		return nil, errorf("expected some of %s", strings.Join(PushKinds, ", "))
	}
	kinds := []string{}
	for _, kind := range PushKinds {
		if slices.Contains(texts, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds, nil
}

// quietHours: '23:00-08:00': the notifications of these hours come without a sound; ” for none.
func quietHours(value any) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, ok := value.(string)
	if !ok || strings.Count(raw, "-") != 1 {
		return "", errorf("expected HH:MM-HH:MM or nothing, got %s", repr(value))
	}
	first, second, _ := strings.Cut(raw, "-")
	start, err := clockTime(first)
	if err != nil {
		return "", err
	}
	end, err := clockTime(second)
	if err != nil {
		return "", err
	}
	if start == end {
		return "", NewError("the quiet hours start and end at the same time")
	}
	return start + "-" + end, nil
}

func filter(value any) (string, error) {
	if value != "all" && value != "important" {
		return "", errorf("expected all or important, got %s", repr(value))
	}
	return value.(string), nil
}

func people(value any) ([]int64, error) {
	bad := NewError("expected a list of Telegram user ids")
	var items []any
	switch v := value.(type) {
	case []int64:
		for _, id := range v {
			items = append(items, id)
		}
	case []int:
		for _, id := range v {
			items = append(items, id)
		}
	case []any:
		items = v
	default:
		return nil, bad
	}
	ids := []int64{}
	for _, item := range items {
		id, ok := integer(item)
		if !ok {
			return nil, bad
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func chatBatchHours(value any) (*int, error) {
	if value == nil {
		return nil, nil
	}
	if p, ok := value.(*int); ok {
		if p == nil {
			return nil, nil
		}
		value = *p
	}
	n, ok := batchHour(value)
	if !ok {
		return nil, errorf("expected null (the general N) or one of %s, got %s", tupleRepr(BatchHours), repr(value))
	}
	return &n, nil
}
