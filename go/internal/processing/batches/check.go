package batches

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/library/feed"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// --- which messages go to the model ---------------------------------------------------

func kind(f inbox.File) string {
	k, _ := f["kind"].(string)
	return k
}

func anyKind(files []inbox.File, kinds []string) bool {
	return slices.ContainsFunc(files, func(f inbox.File) bool { return slices.Contains(kinds, kind(f)) })
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Readable: something to read: a text, a file with a name, a letter's subject.
func Readable(m *inbox.Message) bool {
	return pytext.Strip(m.Text) != "" || anyKind(m.Files, ReadableFiles) || text(m.Subject) != ""
}

func listed(m *inbox.Message, people []int64) bool {
	return m.SenderID != nil && slices.Contains(people, *m.SenderID)
}

// Important is the `important` filter (§5.1): listed people, pins, files, dates.
func Important(m *inbox.Message, people []int64) bool {
	return listed(m, people) || m.Pinned || anyKind(m.Files, ImportantFiles) || HasDate(m.Text)
}

// Urgent is the `urgent` trigger (D19): a pin, a PDF or a listed person.
func Urgent(m *inbox.Message, people []int64) bool {
	return listed(m, people) || m.Pinned || anyKind(m.Files, UrgentFiles)
}

// --- the date words of the `important` filter -------------------------------------------

// HasDate is Python's DATE.search: dates, times and words of time in Russian, with Unicode word boundaries
// (Go's \b knows ASCII only):
//
//	\b\d{1,2}[./]\d{1,2}\b | \b\d{1,2}:\d{2}\b | \b\d{1,2}\s+(?:янв|фев|мар|апр|ма[йя]|июн|июл|авг|сен|окт|ноя|дек)
//	| \b(?:сегодня|завтра|послезавтра|понедельник|вторник|сред[уаые]|четверг|пятниц|суббот|воскресень|дедлайн|дд\b)
//
// case-insensitive.
func HasDate(s string) bool {
	r := []rune(s)
	for i := range r {
		if isWord(r[i]) && (i == 0 || !isWord(r[i-1])) && dateAt(r, i) {
			return true
		}
	}
	return false
}

var months = []string{"янв", "фев", "мар", "апр", "май", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}

var words = []string{"сегодня", "завтра", "послезавтра", "понедельник", "вторник", "среду", "среда", "среды", "среде",
	"четверг", "пятниц", "суббот", "воскресень", "дедлайн"}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func boundary(r []rune, i int) bool { // \b at i
	before := i > 0 && isWord(r[i-1])
	after := i < len(r) && isWord(r[i])
	return before != after
}

func digits(r []rune, i int) int { // how many decimal digits from i
	n := 0
	for i+n < len(r) && unicode.IsDigit(r[i+n]) {
		n++
	}
	return n
}

func prefixFold(r []rune, i int, word string) bool {
	w := []rune(word)
	if i+len(w) > len(r) {
		return false
	}
	for j, c := range w {
		if unicode.ToLower(r[i+j]) != c && unicode.ToUpper(c) != r[i+j] {
			return false
		}
	}
	return true
}

func dateAt(r []rune, i int) bool {
	for _, w := range words {
		if prefixFold(r, i, w) {
			return true
		}
	}
	if prefixFold(r, i, "дд") && boundary(r, i+2) {
		return true
	}
	n := digits(r, i)
	for a := 1; a <= min(2, n); a++ {
		j := i + a
		if j < len(r) && (r[j] == '.' || r[j] == '/') {
			m := digits(r, j+1)
			for b := 1; b <= min(2, m); b++ {
				if boundary(r, j+1+b) {
					return true
				}
			}
		}
		if j < len(r) && r[j] == ':' && digits(r, j+1) >= 2 && boundary(r, j+3) {
			return true
		}
		k := j
		for k < len(r) && pytext.IsSpace(r[k]) {
			k++
		}
		if k > j {
			for _, month := range months {
				if prefixFold(r, k, month) {
					return true
				}
			}
		}
	}
	return false
}

// --- checking the answer -------------------------------------------------------------

// News is the news of an item.
type News struct {
	Full       string `json:"full"`
	Short      string `json:"short"`
	Importance int    `json:"importance"`
	Replaces   string `json:"replaces"`
}

// Deadline is a deadline of an item.
type Deadline struct {
	Course string `json:"course"`
	Label  string `json:"label"`
	Due    string `json:"due"`
	Moves  string `json:"moves"`
}

// Event is an event of an item; Start "" (a cancellation alone) and End "" are none.
type Event struct {
	Course   string `json:"course"`
	Title    string `json:"title"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Room     string `json:"room"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Moves    string `json:"moves"`
	Cancels  string `json:"cancels"`
}

// Item is an item of the answer that can be used.
type Item struct {
	MsgIDs    []int64         `json:"msg_ids"`
	News      *News           `json:"news"`
	Deadlines []Deadline      `json:"deadlines"`
	Events    []Event         `json:"events"`
	Sheets    []sheets.Found  `json:"sheets"`
	Materials []feed.Material `json:"materials"`
}

// decode reads an answer keeping its numbers as they were written (an int is not a float).
func decode(raw []byte) (map[string]any, error) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var out map[string]any
	if err := d.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// pyInt is a JSON integer (not a bool, not a float), as Python's isinstance(value, int) reads it.
func pyInt(value any) (int64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return i, err == nil
}

// asID is a msg_id as Python's `in` compares it: an integral number.
func asID(value any) (int64, bool) {
	if i, ok := pyInt(value); ok {
		return i, true
	}
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

func pyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

// orEmpty is Python's str(value or "").
func orEmpty(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case bool:
		if !v {
			return ""
		}
	case json.Number:
		if f, err := v.Float64(); err == nil && f == 0 {
			return ""
		}
	case []any:
		if len(v) == 0 {
			return ""
		}
	case map[string]any:
		if len(v) == 0 {
			return ""
		}
	}
	return pyStr(value)
}

func str(value any) string { // (value or "") for a string field; anything else is no text
	s, _ := value.(string)
	return s
}

func squeeze(s string) string { return strings.Join(pytext.Split(s), " ") }

// cleanText is a field of the model's answer on one line, within `limit` characters.
func cleanText(value any, limit int) string { return pytext.Cut(squeeze(orEmpty(value)), limit) }

var (
	dayPattern    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	minutePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}$`)
	refPattern    = regexp.MustCompile(`^[ec][0-9]+$`)
	movesPattern  = regexp.MustCompile(`^d[0-9]+$`)
)

// due is a YYYY-MM-DD that is a real date; "" otherwise.
func due(value any) string {
	s, ok := value.(string)
	if !ok || !dayPattern.MatchString(s) {
		return ""
	}
	if _, err := time.Parse(clock.DateLayout, s); err != nil {
		return ""
	}
	return s
}

// moment is '2026-10-23T18:10' -> local ISO with seconds; with dayAlone, '2026-10-23' stays a date (an all-day
// event); "" for anything else.
func moment(value any, dayAlone bool) string {
	s := pytext.Strip(str(value))
	if minutePattern.MatchString(s) {
		if t, err := time.Parse("2006-01-02T15:04", s); err == nil {
			return t.Format(clock.LocalLayout)
		}
	}
	if dayAlone && dayPattern.MatchString(s) {
		if _, err := time.Parse(clock.DateLayout, s); err == nil {
			return s
		}
	}
	return ""
}

// onDay: a move told by its day alone keeps the time of what it moves; an all-day event stays one.
func onDay(start, timed string) string {
	if !schedule.AllDay(start) || schedule.AllDay(timed) {
		return start
	}
	return start + "T" + string([]rune(timed)[11:])
}

// ref is e12 or c34; anything else is no reference.
func ref(value any) string {
	s := pytext.Strip(str(value))
	if refPattern.MatchString(s) {
		return s
	}
	return ""
}

const (
	CourseChars = 80  // a course name as the model gives it: it enters the dictionary and every later prompt (D79)
	RoomChars   = 200 // a room, or a meeting's link
	KindChars   = 40
	TextChars   = 400 // a label or a title past its retry: Tablo's budget is far below
)

func checkEvent(raw map[string]any) *Event {
	cancels := ref(raw["cancels"])
	moves := ""
	if cancels == "" {
		moves = ref(raw["moves"])
	}
	title, start := pytext.Strip(str(raw["title"])), moment(raw["start"], true)
	end := ""
	if start != "" && !schedule.AllDay(start) {
		end = moment(raw["end"], false)
	}
	if cancels == "" && start == "" {
		return nil // a new event or a move needs its time
	}
	if cancels == "" && moves == "" && title == "" {
		return nil // an event of its own needs a name; a move or a cancellation keeps the listed one's
	}
	if end != "" && (start == "" || end <= start) {
		end = ""
	}
	required, ok := raw["required"].(bool)
	if !ok {
		required = true
	}
	k := strings.ToLower(cleanText(raw["kind"], KindChars))
	if k == "" {
		k = "event"
	}
	return &Event{Course: cleanText(raw["course"], CourseChars), Title: cleanText(title, TextChars), Start: start,
		End: end, Room: cleanText(raw["room"], RoomChars), Kind: k, Required: required, Moves: moves,
		Cancels: cancels}
}

// Quoted is a name someone else wrote (a sender, a file, a forward), quoted on one line: it cannot pass for the
// head's own marks ("listed person", "pinned") or start a line of its own (D79).
func Quoted(value *string) string {
	t := strings.ReplaceAll(squeeze(text(value)), `"`, "'")
	if t == "" {
		return ""
	}
	return `"` + t + `"`
}

func importance(value any) int {
	if i, ok := pyInt(value); ok {
		return int(min(5, max(1, i)))
	}
	return 3
}

func objects(value any) []map[string]any {
	list, _ := value.([]any)
	out := []map[string]any{}
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// checkMaterials are the materials of an answer's item: one per message of the batch, none of a message that is a
// sheet.
func checkMaterials(raw map[string]any, msgIDs map[int64]bool, found []sheets.Found) []feed.Material {
	taken := map[int64]bool{}
	for _, s := range found {
		taken[s.MsgID] = true
	}
	var order []int64
	byID := map[int64]feed.Material{}
	for _, m := range objects(raw["materials"]) {
		id, ok := asID(m["msg_id"])
		if !ok || !msgIDs[id] || taken[id] {
			continue
		}
		k, _ := m["kind"].(string)
		if !slices.Contains(feed.MaterialKinds, k) {
			k = "file"
		}
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = feed.Material{MsgID: id, Course: cleanText(m["course"], CourseChars), Kind: k,
			Title: pytext.Cut(squeeze(orEmpty(m["title"])), MaterialTitleChars)}
	}
	out := []feed.Material{}
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// Check are the items of the answer that can be used: known messages, some news, a deadline with a valid date, an
// event with a valid time, a sheet of a message with a PDF read (`pdfs`) or, when asked for (`materials`), course
// material.
func Check(answer map[string]any, msgIDs, pdfs map[int64]bool, materials bool) []Item {
	items := []Item{}
	for _, raw := range objects(answer["items"]) {
		ids := []int64{}
		list, _ := raw["msg_ids"].([]any)
		for _, v := range list {
			if id, ok := asID(v); ok && msgIDs[id] && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		var item = Item{MsgIDs: ids, Deadlines: []Deadline{}, Events: []Event{}, Sheets: []sheets.Found{},
			Materials: []feed.Material{}}
		rawNews, _ := raw["news"].(map[string]any)
		full, short := pytext.Strip(str(rawNews["full"])), pytext.Strip(str(rawNews["short"]))
		if full != "" || short != "" {
			n := &News{Full: full, Short: short, Importance: importance(rawNews["importance"]),
				Replaces: pytext.Strip(str(rawNews["replaces"]))}
			if n.Full == "" {
				n.Full = short
			}
			if n.Short == "" {
				n.Short = full
			}
			item.News = n
		}
		for _, d := range objects(raw["deadlines"]) {
			label, day := cleanText(d["label"], TextChars), due(d["due"])
			moves := pytext.Strip(str(d["moves"]))
			if !movesPattern.MatchString(moves) {
				moves = ""
			}
			if label != "" && day != "" {
				item.Deadlines = append(item.Deadlines, Deadline{Course: cleanText(d["course"], CourseChars),
					Label: label, Due: day, Moves: moves})
			}
		}
		for _, e := range objects(raw["events"]) {
			if ev := checkEvent(e); ev != nil {
				item.Events = append(item.Events, *ev)
			}
		}
		var order []int64
		found := map[int64]sheets.Found{}
		for _, s := range objects(raw["sheets"]) {
			id, ok := asID(s["msg_id"])
			if !ok || !pdfs[id] {
				continue
			}
			if _, seen := found[id]; !seen {
				order = append(order, id)
			}
			var d *string
			if day := due(s["due"]); day != "" {
				d = &day
			}
			found[id] = sheets.Found{MsgID: id, Course: cleanText(s["course"], CourseChars),
				Number: pytext.Cut(squeeze(orEmpty(s["number"])), 60), Due: d}
		}
		for _, id := range order {
			item.Sheets = append(item.Sheets, found[id])
		}
		if materials {
			item.Materials = checkMaterials(raw, msgIDs, item.Sheets)
		}
		if item.News != nil || len(item.Deadlines) > 0 || len(item.Events) > 0 || len(item.Sheets) > 0 ||
			len(item.Materials) > 0 {
			items = append(items, item)
		}
	}
	return items
}

// TooLong is what does not fit the budgets of Tablo's screen (§2.1); counted in characters until the font comes.
func TooLong(item Item) []string {
	var reasons []string
	if n := item.News; n != nil {
		if l := pytext.Len(n.Full); l > FullChars {
			reasons = append(reasons, fmt.Sprintf("full has %d characters, keep it within %d", l, FullChars-8))
		}
		if l := pytext.Len(n.Short); l > Line {
			reasons = append(reasons, fmt.Sprintf("short has %d characters, keep it within %d", l, Line-3))
		}
	}
	for _, d := range item.Deadlines {
		if l := pytext.Len(d.Label); l > LabelChars {
			reasons = append(reasons, fmt.Sprintf("deadline label \"%s\" has %d characters, keep it within %d", d.Label,
				l, LabelChars-1))
		}
	}
	for _, e := range item.Events {
		if e.Moves == "" && e.Cancels == "" && pytext.Len(e.Title) > EventTitleChars {
			reasons = append(reasons, fmt.Sprintf("event title \"%s\" has %d characters, keep it within %d", e.Title,
				pytext.Len(e.Title), EventTitleChars))
		}
	}
	return reasons
}

// Fit is after the one retry: a version that fits stands in for one that does not.
func Fit(item *Item) {
	n := item.News
	if n == nil {
		return
	}
	if pytext.Len(n.Full) > FullChars && pytext.Len(n.Short) <= Line {
		n.Full = n.Short
	} else if pytext.Len(n.Short) > Line && pytext.Len(n.Full) <= Line {
		n.Short = n.Full
	}
}

// --- the timetable in the prompt -------------------------------------------------------

func span(start, end string) string {
	begin, _ := clock.ParseLocal(start)
	if schedule.AllDay(start) {
		return begin.Format("Mon 2006-01-02") + " (all day)"
	}
	out := begin.Format("Mon 2006-01-02 15:04")
	if end != "" {
		finish, _ := clock.ParseLocal(end)
		out += "-" + finish.Format("15:04")
	}
	return out
}

// EventLine is an event as the model sees it.
func EventLine(e *events.Event) string {
	facts := []string{e.Kind}
	if e.Room != "" {
		facts = append(facts, e.Room)
	}
	if e.Required {
		facts = append(facts, "required")
	} else {
		facts = append(facts, "optional")
	}
	if e.Cancelled {
		facts = append(facts, "cancelled")
	}
	if e.Source == events.Own {
		facts = append(facts, "own")
	}
	return fmt.Sprintf("- e%d: %s \"%s\" ", e.ID, span(e.Start, e.EndText()), e.Title) + strings.Join(facts, ", ")
}

// ClassLine is a class as the model sees it, with what chats did to it already.
func ClassLine(item schedule.UpcomingClass) string {
	name := `"` + item.Title + `"`
	if item.Subject != item.Title {
		name += " (" + item.Subject + ")"
	}
	line := fmt.Sprintf("- c%d: %s %s %s", item.ID, span(item.Start, item.End), name, item.Type)
	if item.Room != "" {
		line += ", " + item.Room
	}
	correction := item.Correction
	switch {
	case item.Cancelled:
		line += ", cancelled"
	case correction != nil && correction.Cancelled:
		line += ", cancelled by a chat message"
	case correction != nil && !schedule.MovedBack(item.Row, correction):
		line += ", moved by a chat message to " + span(correction.Start, correction.EndText())
		if correction.Room != "" {
			line += ", " + correction.Room
		}
	}
	return line
}

// --- letters -------------------------------------------------------------------------

// University is the university's mail domain.
const University = "hse.ru"

var address = regexp.MustCompile(`<([^<>\s@]+@([^<>\s@]+))>$`)

// Official: a letter of the university (its address at hse.ru or a subdomain) or the wiki's notice (stored by the
// service itself, under a negative id IMAP never gives). A display name is anyone's to write: the address decides.
func Official(m *inbox.Message) bool {
	if m.MsgID < 0 {
		return true
	}
	found := address.FindStringSubmatch(text(m.SenderName))
	domain := ""
	if found != nil {
		domain = strings.ToLower(found[2])
	}
	return domain == University || strings.HasSuffix(domain, "."+University)
}

// Reread: read before: since then edited, or stored anew past its 14 days because it was edited (D74), when what it
// gave may still be in force.
func Reread(m *inbox.Message) bool { return m.ParsedAt != nil || text(m.EditedAt) != "" }

// SplitLines is Python's str.splitlines.
func SplitLines(s string) []string {
	var lines []string
	var cur []rune
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			lines, cur = append(lines, string(cur)), nil
		case '\r':
			if i+1 < len(r) && r[i+1] == '\n' {
				i++
			}
			lines, cur = append(lines, string(cur)), nil
		default:
			cur = append(cur, r[i])
		}
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return lines
}
