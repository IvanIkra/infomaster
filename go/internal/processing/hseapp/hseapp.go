// Package hseapp keeps HSE App X deadlines in English, and the lists the merge reads (§5.2, §6.4; Tablo's labels of
// §5.1, §10.3): the port of processing/hseapp.py.
//
// After every poll of the collector the disciplines go through the course dictionary (§6.2), whose abbr starts the
// labels. Every deadline, active or done, without a label in the cache goes to Haiku in one call (task
// "hseapp_labels", Tablo's prompt word for word). A label over the budget gets one retry with the reason; the budget is
// counted in characters like the batches' labels until the check by the font comes. Labels are cached in kv for good,
// keyed like Tablo's (LabelVersion, budget, title, discipline, abbr), so a new abbr in the dictionary asks again. A
// call cut or refused is split in halves to find the deadline that breaks it; that one keeps its Russian title.
// Haiku down or the limit spent: deadlines without a cached label show their Russian title and are asked again at the
// next poll, while the cached ones keep their labels.
//
// The lists live in kv "hseapp.deadlines" ({fetched_at, items, done}, rows {id, label, due, course, title} by date and
// title), so a restart has them before the first poll and a failed poll keeps them. `course` is the discipline as HSE
// App X names it, like `deadlines.course`: the merge takes its abbr from the dictionary (§5.5). `title` is the Russian
// original. Done ones are never shown: they hide copies of the same work from the other sources (§5.5).
package hseapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	collector "github.com/IvanIkra/infomaster/go/internal/collectors/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// ListsKey is the kv key of the lists.
const ListsKey = "hseapp.deadlines"

// LabelVersion is Tablo's: the prompt is the same. Bump when it changes, to drop the cached labels.
const LabelVersion = 2

// MaxTokens is the least room for an answer.
const MaxTokens = 2000

// TokensPerLabel: {"id": …, "label": …} of a long id; a semester of done ones must not cut the answer.
const TokensPerLabel = 60

// LabelChars is LABEL_CHARS of processing/batches.py (the budget of a deadline's label), the one definition: the
// batches and the sheets alias it, and the cache keys hash it.
const LabelChars = 15

// System is Tablo's prompt, word for word.
const System = `You write deadline titles for a student's e-ink dashboard (HSE University, Moscow, first year, Software Engineering).

Input: deadlines from the university app, in Russian: the title and, when known, the course.
Output: for each one a short English label that fits the given number of characters.

Style:
- Telegraphic English, no articles, digits instead of words.
- Start with the course abbreviation when the course is known or obvious from the title, then the work: "DM HW 3".
- Course abbreviations: Alg (Алгебра, линал, линейная алгебра), Calc (Математический анализ, матан), DM (Дискретная ` +
	`математика), Prog (программирование, C++, Python), Eng (английский), Hist (история), Safety (БЖД, безопасность ` +
	`жизнедеятельности), PE (физкультура). Other courses: a clear short English name.
- When the course comes with an abbreviation, start with exactly that abbreviation.
- Work abbreviations: HW (ДЗ, домашнее задание), Quiz, Test (контрольная, КР), Midterm, Exam, Lab, Essay, Project, ` +
	`Sem (семинар).
- Drop words that add nothing on a deadline list: "Дедлайн", "сдать", "по".
- Keep numbers and letters of the work: "ДЗ-2б" -> "HW 2B".
- Known acronyms: СОП (student survey on teaching quality) -> "Course survey".

Examples:
- "Дедлайн ДЗ 5 по Дискретной Математике" (Дискретная математика) -> "DM HW 5"
- "ДЗ 2 по Линалу" (Алгебра) -> "Alg HW 2"
- "Доп листок матан" -> "Calc extra sheet"
- "Эссе по истории" -> "Hist essay"
- "Записаться на майнор" -> "Minor sign-up"

Never exceed the character budget: shorten words rather than cut them.`

// Schema is the answer's JSON schema, in Python's key order.
var Schema = json.RawMessage(`{"type": "object", "properties": {"items": {"type": "array", "items": {"type": "object", ` +
	`"properties": {"id": {"type": "string"}, "label": {"type": "string"}}, "required": ["id", "label"], ` +
	`"additionalProperties": false}}}, "required": ["items"], "additionalProperties": false}`)

// LabelKey is the kv key of a cached label.
func LabelKey(raw collector.Raw, budget int, abbr string) string {
	text := fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s", LabelVersion, budget, raw.Title, raw.Discipline, abbr)
	sum := sha256.Sum256([]byte(text))
	return "hseapp.label:" + hex.EncodeToString(sum[:])[:16]
}

// Prompt is the call's prompt; courses: discipline -> abbr from the dictionary; tooLong: id -> the label that was too
// wide.
func Prompt(raws []collector.Raw, budget int, tooLong map[string]string, courses map[string]string) string {
	lines := []string{fmt.Sprintf("Budget: at most %d characters per label.", budget), ""}
	for _, raw := range raws {
		course := ""
		if raw.Discipline != "" {
			course = " (course: " + raw.Discipline
			if abbr := courses[raw.Discipline]; abbr != "" {
				course += ", abbreviation: " + abbr + ")"
			} else {
				course += ")"
			}
		}
		lines = append(lines, fmt.Sprintf(`- id %s: "%s"%s`, raw.ID, raw.Title, course))
		if label, ok := tooLong[raw.ID]; ok {
			lines = append(lines, fmt.Sprintf(`  Your previous label "%s" was too wide for the screen; make it shorter.`,
				label))
		}
	}
	return strings.Join(lines, "\n")
}

// Fits reports whether a label is within the budget.
func Fits(label string) bool { return pytext.Len(label) <= LabelChars }

// Row is a deadline of the lists.
type Row struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Due    string  `json:"due"`
	Course *string `json:"course"` // the discipline; nil for none
	Title  string  `json:"title"`
}

// Lists is the kv value.
type Lists struct {
	FetchedAt *string `json:"fetched_at"`
	Items     []Row   `json:"items"`
	Done      []Row   `json:"done"`
}

// Status is what the status of HSE App X adds.
type Status struct {
	Items       int     `json:"items"`
	Done        int     `json:"done"`
	LabelsError *string `json:"labels_error"`
}

// Deadlines is HSE App X's lists.
type Deadlines struct {
	store    *store.Store
	Subjects *subjects.Subjects
	LLM      llm.Model
	Now      func() time.Time
	// OnChange runs after a poll that changed the lists; first is true when there were none before (the first poll: a
	// baseline).
	OnChange func(ctx context.Context, first bool) error

	mu          sync.Mutex
	labelsError *string
}

// New gives the lists over a store.
func New(s *store.Store, sub *subjects.Subjects, model llm.Model) *Deadlines {
	return &Deadlines{store: s, Subjects: sub, LLM: model, Now: clock.NowUTC}
}

// --- for the merge, the status and the web app -------------------------------------------

// Lists are both lists as saved.
func (d *Deadlines) Lists() (Lists, error) {
	var lists Lists
	found, err := d.store.GetJSON(ListsKey, &lists)
	if err != nil || !found {
		return Lists{Items: []Row{}, Done: []Row{}}, err
	}
	if lists.Items == nil {
		lists.Items = []Row{}
	}
	if lists.Done == nil {
		lists.Done = []Row{}
	}
	return lists, nil
}

// Current are the active deadlines, overdue ones too (the screen picks, §5.6).
func (d *Deadlines) Current() ([]Row, error) {
	lists, err := d.Lists()
	return lists.Items, err
}

// Done are the ones completed in the app: never shown, they hide their copies from the other sources (§5.5).
func (d *Deadlines) Done() ([]Row, error) {
	lists, err := d.Lists()
	return lists.Done, err
}

// LabelsError is the last labelling error; "" for none.
func (d *Deadlines) LabelsError() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.labelsError == nil {
		return ""
	}
	return *d.labelsError
}

func (d *Deadlines) setError(message *string) {
	d.mu.Lock()
	d.labelsError = message
	d.mu.Unlock()
}

// Status is {items, done, labels_error}.
func (d *Deadlines) Status() (Status, error) {
	lists, err := d.Lists()
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{Items: len(lists.Items), Done: len(lists.Done), LabelsError: d.labelsError}, err
}

// --- after a poll ---------------------------------------------------------------------------

// AfterPoll is HseApp.OnPoll: name the courses, label the deadlines, keep both lists. True when they changed.
func (d *Deadlines) AfterPoll(ctx context.Context, active, done []collector.Raw) (bool, error) {
	every := slices.Concat(active, done)
	var disciplines []string
	for _, raw := range every {
		if raw.Discipline != "" && !slices.Contains(disciplines, raw.Discipline) {
			disciplines = append(disciplines, raw.Discipline)
		}
	}
	slices.Sort(disciplines)
	if err := d.Subjects.TryResolve(ctx, disciplines, "hseapp"); err != nil {
		return false, err
	}
	courses := map[string]string{}
	for _, name := range disciplines {
		abbr, err := d.Subjects.Abbr(name)
		if err != nil {
			return false, err
		}
		if abbr != "" {
			courses[name] = abbr
		}
	}
	labels, err := d.Labels(ctx, every, courses)
	if err != nil {
		return false, err
	}
	rows := func(raws []collector.Raw) []Row {
		sorted := slices.Clone(raws)
		slices.SortStableFunc(sorted, func(a, b collector.Raw) int {
			if c := a.Due.Compare(b.Due); c != 0 {
				return c
			}
			return strings.Compare(a.Title, b.Title)
		})
		out := []Row{}
		for _, raw := range sorted {
			label, ok := labels[raw.ID]
			if !ok {
				label = raw.Title
			}
			var course *string
			if raw.Discipline != "" {
				discipline := raw.Discipline
				course = &discipline
			}
			out = append(out, Row{ID: raw.ID, Label: label, Due: raw.Due.Format(clock.DateLayout), Course: course,
				Title: raw.Title})
		}
		return out
	}
	var saved Lists
	found, err := d.store.GetJSON(ListsKey, &saved)
	if err != nil {
		return false, err
	}
	items, doneRows := rows(active), rows(done)
	changed := !found || !sameRows(saved.Items, items) || !sameRows(saved.Done, doneRows)
	fetched := clock.LocalISO(d.Now())
	if err := d.store.PutJSON(ListsKey, Lists{FetchedAt: &fetched, Items: items, Done: doneRows}); err != nil {
		return false, err
	}
	if changed {
		slog.Info("HSE App X lists changed", "deadlines", len(items), "completed", len(doneRows))
		if d.OnChange != nil {
			if err := d.OnChange(ctx, !found); err != nil {
				slog.Error("publishing the HSE App X changes failed", "error", err)
			}
		}
	}
	return changed, nil
}

func sameRows(a, b []Row) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// Labels are labels by id: cached ones, and Haiku's for the rest. One without a label is left out: its title shows.
func (d *Deadlines) Labels(ctx context.Context, raws []collector.Raw, courses map[string]string) (
	map[string]string, error) {
	keys := map[string]string{}
	for _, raw := range raws {
		keys[raw.ID] = LabelKey(raw, LabelChars, courses[raw.Discipline])
	}
	result := map[string]string{}
	var order []string
	todo := map[string]collector.Raw{}
	for _, raw := range raws {
		var cached string
		if _, err := d.store.GetJSON(keys[raw.ID], &cached); err != nil {
			return nil, err
		}
		if cached != "" {
			result[raw.ID] = cached
			continue
		}
		if _, ok := todo[raw.ID]; !ok {
			order = append(order, raw.ID)
		}
		todo[raw.ID] = raw
	}
	if len(todo) == 0 {
		d.setError(nil)
		return result, nil
	}
	asked := make([]collector.Raw, 0, len(order))
	for _, id := range order {
		asked = append(asked, todo[id])
	}
	got, err := d.askAll(ctx, asked, courses)
	var llmErr *llm.Error
	switch {
	case err == nil:
	case llm.IsBudget(err): // the budget tells the owner itself; asked again at the next poll
		slog.Info("HSE App X labels wait for the LLM limit", "error", err)
		return result, nil
	case errors.As(err, &llmErr):
		message := err.Error()
		d.setError(&message)
		slog.Warn("HSE App X labels failed; the Russian titles show until the next poll", "error", err)
		return result, nil
	default:
		return nil, err
	}
	d.setError(nil)
	for _, id := range order {
		label, ok := got[id]
		if !ok {
			continue
		}
		if err := d.store.PutJSON(keys[id], label); err != nil {
			return nil, err
		}
		result[id] = label
	}
	slog.Info("HSE App X labels", "asked", len(todo), "given", len(got))
	return result, nil
}

// askAll is one call for all; a bad answer (cut or refused) is split in halves. A deadline the model cannot answer for
// on its own keeps its title, cached like a label: the same input would fail again.
func (d *Deadlines) askAll(ctx context.Context, raws []collector.Raw, courses map[string]string) (
	map[string]string, error) {
	got, err := d.ask(ctx, raws, courses, nil)
	if llm.IsBadAnswer(err) {
		if len(raws) == 1 {
			slog.Warn("an HSE App X label failed; the title stays", "id", raws[0].ID, "error", err)
			return map[string]string{raws[0].ID: raws[0].Title}, nil
		}
		half := len(raws) / 2
		first, err := d.askAll(ctx, raws[:half], courses)
		if err != nil {
			return nil, err
		}
		second, err := d.askAll(ctx, raws[half:], courses)
		if err != nil {
			return nil, err
		}
		for id, label := range second {
			first[id] = label
		}
		return first, nil
	}
	if err != nil {
		return nil, err
	}
	wide := map[string]string{}
	var again []collector.Raw
	for _, raw := range raws {
		if label, ok := got[raw.ID]; ok && !Fits(label) {
			wide[raw.ID] = label
			again = append(again, raw)
		}
	}
	if len(wide) > 0 {
		retried, err := d.ask(ctx, again, courses, wide)
		var llmErr *llm.Error
		switch {
		case err == nil:
			for id, label := range retried {
				if _, ok := wide[id]; ok {
					got[id] = label
				}
			}
		case llm.IsBudget(err) || errors.As(err, &llmErr): // the wide labels stand; Tablo cuts them with "…"
			slog.Info("the retry for width failed, the first labels stay", "error", err)
		default:
			return nil, err
		}
	}
	return got, nil
}

func (d *Deadlines) ask(ctx context.Context, raws []collector.Raw, courses map[string]string,
	tooLong map[string]string) (map[string]string, error) {
	text, err := d.LLM.JSON(ctx, llm.Request{Task: "hseapp_labels", System: System,
		Prompt: Prompt(raws, LabelChars, tooLong, courses), Schema: Schema,
		MaxTokens: max(MaxTokens, TokensPerLabel*len(raws))})
	if err != nil {
		return nil, err
	}
	var answer map[string]any
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&answer); err != nil {
		return nil, llm.NewError(llm.BadAnswer, "the answer is not a JSON object: "+err.Error())
	}
	ids := map[string]bool{}
	for _, raw := range raws {
		ids[raw.ID] = true
	}
	result := map[string]string{}
	list, _ := answer["items"].([]any)
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id := pyStr(item["id"])
		label := ""
		if truthy(item["label"]) {
			label = strings.Join(pytext.Split(pyStr(item["label"])), " ")
		}
		if ids[id] && label != "" {
			result[id] = label
		}
	}
	return result, nil
}

// pyStr is Python's str() of a JSON value, enough for ids and labels.
func pyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return v.String()
	default:
		text, _ := json.Marshal(v)
		return string(text)
	}
}

// truthy is Python's truth of a JSON value.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case bool:
		return v
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}
