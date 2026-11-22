// Package subjects is the course dictionary (§6.2, §2.2, D3): one for the timetable, the deadlines and the iCal feed.
//
// A source names a course its own way: «Математический анализ» in HSE App X, *Calculus* in RUZ, «матан» in a chat.
// Each cleaned name gets a short English `title` (the timetable, Tablo's Classes column) and an `abbr` of up to six
// characters (the prefix of deadline labels, the key of the merge, processing/merge). Haiku names new courses in one
// call that also shows the whole dictionary, so the same course in Russian and in English, or under a short form like
// «ВвП», gets the same answer (§10.1). Every prompt that names courses (the batches) gets the whole dictionary as
// DictionaryLines gives it: one line per course with all the names it goes by.
//
// Entries set by hand in the web app are `manual`: Haiku never overwrites them, and deleting one lets Haiku name the
// course again. A change by hand tells OnManual, so the Midterm labels follow it at once, not at the next RUZ poll
// (app). When Haiku is down or the limit is spent, names show as the source gives them and are asked again at the
// next RUZ poll, batch run or HSE App X poll. The error of a failed naming is kept for each caller with the names it
// left unnamed: it stays while any of them waits, and goes once they are named (by Haiku or by hand) or the caller no
// longer needs them.
package subjects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// TitleChars is the subject column of Tablo's Classes block (§2.1): its width / 8.1 px is 12-13, and "Intro to Prog"
// fits. Counted in characters until the check by the font comes.
const TitleChars = 13

// AbbrChars is the longest abbreviation.
const AbbrChars = 6

// ManualTitleChars: a title set by hand may be longer: the owner decides, Tablo cuts it with "…".
const ManualTitleChars = 40

// MaxTokens is the smallest max_tokens of a naming call.
const MaxTokens = 2000

// Error is a change by hand the dictionary refuses (Python's SubjectError, a ValueError).
type Error struct{ Phrase i18n.Phrase }

func (e *Error) Error() string { return e.Phrase.En }

// Unwrap lets i18n.Reason find the Phrase.
func (e *Error) Unwrap() error { return &i18n.Error{Phrase: e.Phrase} }

// Subject is one entry of the dictionary.
type Subject struct {
	Title  string `json:"title"`
	Abbr   string `json:"abbr"`
	Manual bool   `json:"manual"`
}

// Named is an entry with the name it is kept under.
type Named struct {
	Name string
	Subject
}

// Entry is an entry as SetManual returns it: {name, title, abbr, manual}.
type Entry struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Abbr   string `json:"abbr"`
	Manual bool   `json:"manual"`
}

// Clean is 'Алгебра  (рус)' -> 'Алгебра'; it collapses whitespace.
func Clean(name string) string {
	name = strings.Join(pytext.Split(name), " ")
	if strings.HasSuffix(name, "(рус)") {
		name = strings.TrimRightFunc(strings.TrimSuffix(name, "(рус)"), pytext.IsSpace)
	}
	return pytext.Strip(name)
}

// System is the system prompt of a naming call.
const System = "You name university courses for a student's e-ink dashboard and calendar (HSE University, Moscow, " +
	"first year, Software Engineering).\n" +
	"\n" +
	"Input: course names as the university gives them, in Russian or in English, some long.\n" +
	"Output: for each one\n" +
	"- title: a short English course name for the timetable that fits the given number of characters;\n" +
	"- abbr: a course abbreviation of at most 6 characters, used as a prefix in deadline labels (\"Calc HW 5\").\n" +
	"\n" +
	"Style:\n" +
	"- Plain English, no articles. Shorten words rather than cut them: \"Intro to Prog\", not \"Introduction t\".\n" +
	"- Keep the usual name when it fits: \"Calculus\", \"Algebra\".\n" +
	"- Abbreviations: Alg (Алгебра, линейная алгебра), Calc (Математический анализ), DM (Дискретная математика), " +
	"Prog (программирование), SE (программная инженерия), Eng (английский), Hist (история), Safety (БЖД), PE " +
	"(физкультура). Other courses: a clear short abbreviation.\n" +
	"- The same course in Russian and in English gets the same title and abbr. Courses already in the dictionary " +
	"are listed with all their names: reuse their title and abbr for the same course.\n" +
	"- A new name is often another name of a listed course: a short form or an abbreviation (\"ВвП\", \"матан\", " +
	"\"линал\"), a translation, a part or a variant of it (\"Введение в программирование (практика)\"). Then give " +
	"exactly the title and abbr of that course. A new title only for a course that is really new.\n" +
	"\n" +
	"Examples:\n" +
	"- \"Математический анализ\" -> title \"Calculus\", abbr \"Calc\"\n" +
	"- \"Введение в программирование\" -> title \"Intro to Prog\", abbr \"Prog\"\n" +
	"- \"Discrete Mathematics\" -> title \"Discrete Math\", abbr \"DM\"\n" +
	"- \"История России\" -> title \"Russian Hist\", abbr \"Hist\"\n" +
	"\n" +
	"Never exceed the character budget for the title."

// Schema is the answer's JSON schema, its properties in the order the model writes them.
var Schema = json.RawMessage(`{"type": "object", "properties": {"items": {"type": "array", "items": {"type": ` +
	`"object", "properties": {"id": {"type": "string"}, "title": {"type": "string"}, "abbr": {"type": "string"}}, ` +
	`"required": ["id", "title", "abbr"], "additionalProperties": false}}}, "required": ["items"], ` +
	`"additionalProperties": false}`)

// DictionaryLines is the dictionary for a prompt: one line per course, its title and abbr, then every name it goes by.
func DictionaryLines(known []Named) []string {
	type course struct {
		abbr, title string
		names       []string
	}
	var courses []*course
	index := map[[2]string]*course{}
	for _, n := range known {
		key := [2]string{n.Abbr, n.Title}
		c := index[key]
		if c == nil {
			c = &course{abbr: n.Abbr, title: n.Title}
			index[key] = c
			courses = append(courses, c)
		}
		c.names = append(c.names, n.Name)
	}
	slices.SortStableFunc(courses, func(a, b *course) int {
		return strings.Compare(pytext.Casefold(a.abbr), pytext.Casefold(b.abbr))
	})
	lines := make([]string, 0, len(courses))
	for _, c := range courses {
		quoted := make([]string, len(c.names))
		for i, name := range c.names {
			quoted[i] = `"` + name + `"`
		}
		lines = append(lines, fmt.Sprintf(`- %s "%s": `, c.abbr, c.title)+strings.Join(quoted, ", "))
	}
	return lines
}

// Prompt is the prompt of a naming call; tooLong holds the titles of a first answer that were too wide.
func Prompt(names []string, known []Named, tooLong map[string]string) string {
	lines := []string{fmt.Sprintf("Title budget: at most %d characters.", TitleChars), ""}
	if len(known) > 0 {
		lines = append(lines, `Already in the dictionary (abbr "title": the names it goes by):`)
		lines = append(lines, DictionaryLines(known)...)
		lines = append(lines, "")
	}
	lines = append(lines, "New courses:")
	for i, name := range names {
		lines = append(lines, fmt.Sprintf(`- id %d: "%s"`, i, name))
		if wide, ok := tooLong[name]; ok {
			lines = append(lines, fmt.Sprintf(`  Your previous title "%s" was too wide for the screen; make it shorter.`,
				wide))
		}
	}
	return strings.Join(lines, "\n")
}

// Fits tells whether a title fits the timetable.
func Fits(title string) bool { return pytext.Len(title) <= TitleChars }

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func inTx(q Querier, fn func(Querier) error) error {
	db, ok := q.(*sql.DB)
	if !ok {
		return fn(q)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type failure struct {
	who   string
	error string
	names []string
}

type shared struct {
	// One resolve at a time: RUZ and the batches resolve together at start-up, and the second one must see what the
	// first one added, or one course gets two answers.
	lock sync.Mutex
	mu   sync.Mutex
	// Caller ("ruz", "batches", "hseapp") -> the error of its last naming and the names it left unnamed, in the order
	// the callers first failed. Per caller, so one caller's success does not hide the names another one still waits
	// for.
	failed []failure
}

// Subjects is the dictionary.
type Subjects struct {
	db  Querier
	LLM llm.Model
	// OnManual runs after an entry is set or deleted by hand.
	OnManual func()
	state    *shared
}

// New opens the dictionary of the store.
func New(s *store.Store, model llm.Model) *Subjects {
	return &Subjects{db: s.DB, LLM: model, state: &shared{}}
}

// With is the same dictionary over a transaction (its reads see the transaction's writes).
func (s *Subjects) With(q Querier) *Subjects {
	c := *s
	c.db = q
	return &c
}

// LastError is why some names wait for Haiku; "" when none waits.
func (s *Subjects) LastError() string {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if len(s.state.failed) == 0 {
		return ""
	}
	return s.state.failed[0].error
}

// --- lookups ----------------------------------------------------------------

// Get is the entry of a name (cleaned first); nil when the dictionary has none.
func (s *Subjects) Get(name string) (*Subject, error) {
	var subject Subject
	err := s.db.QueryRow("select title, abbr, manual from subjects where name = ?", Clean(name)).
		Scan(&subject.Title, &subject.Abbr, &subject.Manual)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &subject, nil
}

// Title is the English title of a course; the name as the source gives it until the dictionary has it.
func (s *Subjects) Title(name string) (string, error) {
	subject, err := s.Get(name)
	if err != nil || subject == nil {
		return Clean(name), err
	}
	return subject.Title, nil
}

// Abbr is the abbreviation of a course; "" until the dictionary has it.
func (s *Subjects) Abbr(name string) (string, error) {
	subject, err := s.Get(name)
	if err != nil || subject == nil {
		return "", err
	}
	return subject.Abbr, nil
}

// All is the whole dictionary, by name.
func (s *Subjects) All() ([]Named, error) {
	rows, err := s.db.Query("select name, title, abbr, manual from subjects order by name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Named
	for rows.Next() {
		var n Named
		if err := rows.Scan(&n.Name, &n.Title, &n.Abbr, &n.Manual); err != nil {
			return nil, err
		}
		all = append(all, n)
	}
	return all, rows.Err()
}

// ByName is a dictionary as a map.
func ByName(known []Named) map[string]Subject {
	out := make(map[string]Subject, len(known))
	for _, n := range known {
		out[n.Name] = n.Subject
	}
	return out
}

// Unknown are the cleaned names the dictionary does not have, sorted; empty names are left out.
func (s *Subjects) Unknown(names []string) ([]string, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	known := ByName(all)
	return wanted(names, func(name string) bool { _, ok := known[name]; return !ok }), nil
}

// wanted is the sorted set of the cleaned, non-empty names that pass keep.
func wanted(names []string, keep func(string) bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		name = Clean(name)
		if name != "" && !seen[name] && keep(name) {
			seen[name] = true
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// --- manual entries (the web app) -----------------------------------------------

// SetManual adds or changes an entry by hand; it becomes manual. It returns the entry.
func (s *Subjects) SetManual(name, title, abbr string) (Entry, error) {
	name, title, abbr = Clean(name), strings.Join(pytext.Split(title), " "), strings.Join(pytext.Split(abbr), " ")
	if name == "" {
		return Entry{}, &Error{i18n.P("the course's name is needed", "нужно название курса")}
	}
	if title == "" || pytext.Len(title) > ManualTitleChars {
		return Entry{}, &Error{i18n.P(fmt.Sprintf("the English title: 1 to %d characters", ManualTitleChars),
			fmt.Sprintf("название на английском: от 1 до %d символов", ManualTitleChars))}
	}
	if abbr == "" || pytext.Len(abbr) > AbbrChars {
		return Entry{}, &Error{i18n.P(fmt.Sprintf("the abbreviation: 1 to %d characters", AbbrChars),
			fmt.Sprintf("сокращение: от 1 до %d символов", AbbrChars))}
	}
	if err := s.put(name, title, abbr, true); err != nil {
		return Entry{}, err
	}
	if err := s.settle(); err != nil {
		return Entry{}, err
	}
	s.changed()
	return Entry{Name: name, Title: title, Abbr: abbr, Manual: true}, nil
}

// Delete drops an entry; the next resolve asks Haiku about the name again.
func (s *Subjects) Delete(name string) (bool, error) {
	result, err := s.db.Exec("delete from subjects where name = ?", Clean(name))
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		s.changed()
	}
	return n > 0, nil
}

// Restore puts an entry back as it was, Haiku's or manual: an undo of the MCP connector (§8.5).
func (s *Subjects) Restore(name, title, abbr string, manual bool) error {
	err := inTx(s.db, func(q Querier) error {
		if _, err := q.Exec("delete from subjects where name = ?", Clean(name)); err != nil {
			return err
		}
		return putIn(q, Clean(name), title, abbr, manual)
	})
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

func (s *Subjects) changed() {
	if s.OnManual == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil { // the entry is saved; the labels follow at the next poll anyway
			slog.Error("following a manual dictionary change failed", "error", r)
		}
	}()
	s.OnManual()
}

func (s *Subjects) put(name, title, abbr string, manual bool) error {
	return putIn(s.db, name, title, abbr, manual)
}

// Put writes an entry as Haiku (manual false) or the owner (true) would: Haiku never overwrites a manual one.
func (s *Subjects) Put(name, title, abbr string, manual bool) error {
	return s.put(name, title, abbr, manual)
}

func putIn(q Querier, name, title, abbr string, manual bool) error {
	_, err := q.Exec("insert into subjects(name, title, abbr, manual) values(?, ?, ?, ?) "+
		"on conflict(name) do update set title = excluded.title, abbr = excluded.abbr, "+
		"manual = excluded.manual where excluded.manual or not subjects.manual",
		name, title, abbr, manual)
	return err
}

// --- Haiku ------------------------------------------------------------------------

// Resolve gives the entries of the names, asking Haiku about the new ones. Errors: *llm.BudgetExceeded or *llm.Error
// from the call.
func (s *Subjects) Resolve(ctx context.Context, names []string) (map[string]Subject, error) {
	s.state.lock.Lock()
	defer s.state.lock.Unlock()
	want := wanted(names, func(string) bool { return true })
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	known := ByName(all)
	var todo []string
	for _, name := range want {
		if _, ok := known[name]; !ok {
			todo = append(todo, name)
		}
	}
	if len(todo) > 0 {
		got, err := s.ask(ctx, todo, all, nil)
		if err != nil {
			return nil, err
		}
		wide := map[string]string{}
		var wideNames []string
		for _, name := range todo {
			if subject, ok := got[name]; ok && !Fits(subject.Title) {
				wide[name] = subject.Title
				wideNames = append(wideNames, name)
			}
		}
		if len(wide) > 0 {
			again, err := s.ask(ctx, wideNames, all, wide)
			switch {
			case err == nil:
				for name, subject := range again {
					got[name] = subject
				}
			case llm.IsBudget(err) || isLLMError(err): // the wide titles stand; Tablo cuts them with "…"
				slog.Info("the retry for width failed, the first titles stay", "error", err)
			default:
				return nil, err
			}
		}
		named := 0
		for _, name := range todo {
			if subject, ok := got[name]; ok {
				if err := s.put(name, subject.Title, subject.Abbr, false); err != nil {
					return nil, err
				}
				named++
			}
		}
		slog.Info(fmt.Sprintf("subjects: %d new names, %d named", len(todo), named))
		if all, err = s.All(); err != nil {
			return nil, err
		}
		known = ByName(all)
	}
	result := map[string]Subject{}
	for _, name := range want {
		if subject, ok := known[name]; ok {
			result[name] = subject
		}
	}
	return result, nil
}

func isLLMError(err error) bool {
	var e *llm.Error
	return errors.As(err, &e)
}

// TryResolve is Resolve for the RUZ poll (who "ruz"), the batches ("batches") and HSE App X ("hseapp"): a failure
// leaves the names as they are until the next try of the same caller. Errors other than the LLM's pass through.
func (s *Subjects) TryResolve(ctx context.Context, names []string, who string) error {
	unknown, err := s.Unknown(names)
	if err != nil {
		return err
	}
	if len(unknown) == 0 { // named meanwhile, or no longer needed: nothing of this caller waits
		s.forget(who)
		return nil
	}
	_, err = s.Resolve(ctx, unknown)
	switch {
	case err == nil:
		s.forget(who)
	case llm.IsBudget(err): // the budget tells the owner itself
		slog.Info("subject names wait for the LLM limit", "error", err)
	case isLLMError(err):
		s.fail(who, err.Error(), unknown)
		slog.Warn("subject names failed; the names stay as the sources give them", "error", err)
	default:
		return err
	}
	return nil
}

func (s *Subjects) forget(who string) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	s.state.failed = slices.DeleteFunc(s.state.failed, func(f failure) bool { return f.who == who })
}

func (s *Subjects) fail(who, message string, names []string) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	for i := range s.state.failed {
		if s.state.failed[i].who == who { // a dict keeps the place of a key it already has
			s.state.failed[i] = failure{who, message, names}
			return
		}
	}
	s.state.failed = append(s.state.failed, failure{who, message, names})
}

// settle: a caller whose waiting names all got named by hand waits no longer.
func (s *Subjects) settle() error {
	all, err := s.All()
	if err != nil {
		return err
	}
	known := ByName(all)
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	s.state.failed = slices.DeleteFunc(s.state.failed, func(f failure) bool {
		for _, name := range f.names {
			if _, ok := known[name]; !ok {
				return false
			}
		}
		return true
	})
	return nil
}

func (s *Subjects) ask(ctx context.Context, names []string, known []Named, tooLong map[string]string) (
	map[string]Subject, error) {
	raw, err := s.LLM.JSON(ctx, llm.Request{Task: "subjects", System: System, Prompt: Prompt(names, known, tooLong),
		Schema: Schema, MaxTokens: max(MaxTokens, 80*len(names))})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, llm.NewError(llm.BadAnswer, "the answer is not JSON: "+err.Error())
	}
	result := map[string]Subject{}
	for _, item := range answer.Items {
		index, ok := pyInt(item["id"])
		if !ok || index < 0 || index >= len(names) {
			continue
		}
		title := strings.Join(pytext.Split(pyStr(item["title"])), " ")
		abbr := pytext.Strip(pytext.Cut(strings.Join(pytext.Split(pyStr(item["abbr"])), " "), AbbrChars))
		if title != "" && abbr != "" {
			result[names[index]] = Subject{Title: title, Abbr: abbr}
		}
	}
	return result, nil
}

// pyInt is Python's int() of a JSON value.
func pyInt(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(pytext.Strip(v), "_", ""))
		return n, err == nil
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return 0, false
		}
		return int(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// pyStr is str(value or "") of a JSON value.
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
		return ""
	case float64:
		if v == 0 {
			return ""
		}
		if v == math.Trunc(v) && math.Abs(v) < 1e16 {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return fmt.Sprint(value)
}
