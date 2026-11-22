package hseapp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	collector "github.com/IvanIkra/infomaster/go/internal/collectors/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

func item(title, expiresAt, discipline, status, id string) any {
	m := map[string]any{"id": id, "title": title, "description": "", "discipline": nil, "expires_at": expiresAt,
		"completion_status": status, "is_pinned": false, "groups": []any{}, "emails": []any{}}
	if discipline != "" {
		m["discipline"] = discipline
	}
	return m
}

var items = []any{
	item("Дедлайн ДЗ 3 по  Дискретной Математике", "2026-10-22T20:59:00.000Z",
		"Дискретная математика (2026/2027 модули: 1,2,3,4) (Иванов И.И.)", "ACTIVE", "8f1c2a0e"),
	item("Доп лист Матан", "2026-10-19T20:59:01.000Z", "", "ACTIVE", "91aa07d3"), // overdue: kept, the screen picks
	item("Done one", "2026-10-25T20:59:00.000Z", "", "COMPLETED", "c0ffee01"),
}

var done = []any{
	item("ДЗ 2 по БЖД", "2026-10-13T20:59:00.000Z",
		"Безопасность жизнедеятельности (2026/2027 модуль: 1) (Петров П.П.)", "COMPLETED_LATE", "b2d70c44"),
	item("Active one slipped in", "2026-10-25T20:59:00.000Z", "", "ACTIVE", "ac71ve00"),
}

var defaultLabels = map[string]string{"Дедлайн ДЗ 3 по Дискретной Математике": "DM HW 3",
	"Доп лист Матан": "Calc extra", "ДЗ 2 по БЖД": "Safety HW 2"}

var names = map[string][2]string{"Дискретная математика": {"Discrete Math", "DM"},
	"Безопасность жизнедеятельности": {"Life Safety", "Safety"}}

const titleDM = "Дедлайн ДЗ 3 по Дискретной Математике"

func parse(list []any, completed bool) []collector.Raw { return collector.Parse(list, completed) }

// namer is Haiku for the dictionary; err fails it.
func namer(err error) *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, line := range strings.Split(req.Prompt, "\n") {
			if rest, ok := strings.CutPrefix(line, "- id "); ok {
				index, name, _ := strings.Cut(rest, ": ")
				if named, ok := names[strings.Trim(name, `"`)]; ok {
					out = append(out, map[string]any{"id": index, "title": named[0], "abbr": named[1]})
				}
			}
		}
		return map[string]any{"items": out}, nil
	}}
}

// labeler is Haiku for the labels: labels the titles it knows; errs are returned first, broken titles break a call.
type labeler struct {
	mu        sync.Mutex
	labels    map[string]string
	prompts   []string
	maxTokens []int
	errs      []error
	broken    map[string]bool
}

func newLabeler() *labeler {
	l := &labeler{labels: map[string]string{}, broken: map[string]bool{}}
	for k, v := range defaultLabels {
		l.labels[k] = v
	}
	return l
}

func (l *labeler) model() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if req.Task != "hseapp_labels" || req.System != System || string(req.Schema.(json.RawMessage)) != string(Schema) {
			panic("request")
		}
		l.prompts = append(l.prompts, req.Prompt)
		l.maxTokens = append(l.maxTokens, req.MaxTokens)
		if len(l.errs) > 0 {
			err := l.errs[0]
			l.errs = l.errs[1:]
			return nil, err
		}
		out := []map[string]any{}
		for _, line := range strings.Split(req.Prompt, "\n") {
			if rest, ok := strings.CutPrefix(line, "- id "); ok {
				id, rest, _ := strings.Cut(rest, ": ")
				title := strings.Split(rest, `"`)[1]
				if l.broken[title] {
					return nil, llm.NewError(llm.BadAnswer, "request declined (cyber)")
				}
				if label, ok := l.labels[title]; ok {
					out = append(out, map[string]any{"id": id, "label": label})
				}
			}
		}
		return map[string]any{"items": out}, nil
	}}
}

// queued answers in order; an error in the list is returned instead.
type queued struct {
	answers []any
	prompts []string
}

func (q *queued) model() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		q.prompts = append(q.prompts, req.Prompt)
		answer := q.answers[0]
		q.answers = q.answers[1:]
		if err, ok := answer.(error); ok {
			return nil, err
		}
		return answer, nil
	}}
}

type fixture struct {
	*Deadlines
	store   *store.Store
	labeler *labeler
}

func setup(t *testing.T, namerErr error) *fixture {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	l := newLabeler()
	d := New(s, subjects.New(s, namer(namerErr)), l.model())
	d.Now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC() }
	return &fixture{Deadlines: d, store: s, labeler: l}
}

func (f *fixture) poll(t *testing.T, active, done []collector.Raw) bool {
	t.Helper()
	changed, err := f.AfterPoll(context.Background(), active, done)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func (f *fixture) labels(t *testing.T) []string {
	t.Helper()
	rows, err := f.Current()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row.Label)
	}
	return out
}

func ptr(s string) *string { return &s }

func TestLabelKey(t *testing.T) {
	if got := LabelKey(collector.Raw{Title: "Эссе по истории", Discipline: "История"}, 15, "Hist"); got !=
		"hseapp.label:c847ecca6db52d85" {
		t.Fatal(got)
	}
	if got := LabelKey(collector.Raw{Title: "Эссе"}, 15, ""); got != "hseapp.label:bc4f219cb40b68c0" {
		t.Fatal(got)
	}
}

func TestLabelsAndLists(t *testing.T) {
	f := setup(t, nil)
	var changes []bool
	f.OnChange = func(_ context.Context, first bool) error { changes = append(changes, first); return nil }
	if rows, _ := f.Current(); len(rows) != 0 {
		t.Fatal(rows)
	}
	if st, _ := f.Status(); st != (Status{}) {
		t.Fatal(st)
	}
	if !f.poll(t, parse(items, false), parse(done, true)) {
		t.Fatal("changed")
	}
	if len(f.labeler.prompts) != 1 { // active and done in one call
		t.Fatal(f.labeler.prompts)
	}
	prompt := f.labeler.prompts[0]
	lines := strings.Split(prompt, "\n")
	if lines[0] != "Budget: at most 15 characters per label." ||
		!strings.Contains(prompt, `- id 8f1c2a0e: "Дедлайн ДЗ 3 по Дискретной Математике" (course: Дискретная математика, `+
			`abbreviation: DM)`) || !slices.Contains(lines, `- id 91aa07d3: "Доп лист Матан"`) ||
		strings.Contains(prompt, "Иванов") {
		t.Fatal(prompt)
	}
	current, _ := f.Current()
	want := []Row{
		{ID: "91aa07d3", Label: "Calc extra", Due: "2026-10-19", Title: "Доп лист Матан"},
		{ID: "8f1c2a0e", Label: "DM HW 3", Due: "2026-10-22", Course: ptr("Дискретная математика"), Title: titleDM}}
	if !sameRows(current, want) {
		t.Fatal(current)
	}
	doneRows, _ := f.Done()
	if !sameRows(doneRows, []Row{{ID: "b2d70c44", Label: "Safety HW 2", Due: "2026-10-13",
		Course: ptr("Безопасность жизнедеятельности"), Title: "ДЗ 2 по БЖД"}}) {
		t.Fatal(doneRows)
	}
	lists, _ := f.Lists()
	if *lists.FetchedAt != "2026-10-20T12:00:00" || !slices.Equal(changes, []bool{true}) {
		t.Fatal(lists.FetchedAt, changes)
	}
	if st, _ := f.Status(); st != (Status{Items: 2, Done: 1}) {
		t.Fatal(st)
	}
	var raw string
	f.store.DB.QueryRow("select value from kv where key = ?", ListsKey).Scan(&raw)
	if !strings.HasPrefix(raw, `{"fetched_at": "2026-10-20T12:00:00", "items": [{"id": "91aa07d3", "label": `+
		`"Calc extra", "due": "2026-10-19", "course": null, "title": "Доп лист Матан"}`) {
		t.Fatal(raw)
	}

	if f.poll(t, parse(items, false), parse(done, true)) || len(f.labeler.prompts) != 1 { // cached for good
		t.Fatal("no call, no change")
	}
	moved := map[string]any{}
	for k, v := range items[0].(map[string]any) {
		moved[k] = v
	}
	moved["expires_at"] = "2026-10-23T20:59:00Z"
	if !f.poll(t, parse([]any{moved}, false), nil) || len(f.labeler.prompts) != 1 ||
		!slices.Equal(changes, []bool{true, false}) {
		t.Fatal("moved")
	}
	current, _ = f.Current()
	doneRows, _ = f.Done()
	if current[0].Due != "2026-10-23" || len(doneRows) != 0 {
		t.Fatal(current, doneRows)
	}
}

func TestANewAbbrAsksAgain(t *testing.T) {
	f := setup(t, nil)
	f.poll(t, parse(items[:1], false), nil)
	if _, err := f.Subjects.SetManual("Дискретная математика", "Discrete Math", "DMath"); err != nil {
		t.Fatal(err)
	}
	f.labeler.labels[titleDM] = "DMath HW 3"
	f.poll(t, parse(items[:1], false), nil)
	if !strings.Contains(f.labeler.prompts[1], "abbreviation: DMath") || f.labels(t)[0] != "DMath HW 3" {
		t.Fatal(f.labeler.prompts, f.labels(t))
	}
}

func TestLabelTooWideGetsOneRetry(t *testing.T) {
	f := setup(t, nil)
	q := &queued{answers: []any{
		map[string]any{"items": []any{map[string]any{"id": "8f1c2a0e", "label": "DM homework three"}}},
		map[string]any{"items": []any{map[string]any{"id": "8f1c2a0e", "label": "DM HW 3"},
			map[string]any{"id": "91aa07d3", "label": "Other"}}}}}
	f.LLM = q.model()
	f.poll(t, parse(items, false), nil)
	if got := strings.Split(q.prompts[1], "\n")[2:]; !slices.Equal(got, []string{
		`- id 8f1c2a0e: "Дедлайн ДЗ 3 по Дискретной Математике" (course: Дискретная математика, abbreviation: DM)`,
		`  Your previous label "DM homework three" was too wide for the screen; make it shorter.`}) {
		t.Fatal(got)
	}
	// only the wide one is retried; the other one got no label and waits for the next poll
	if got := f.labels(t); !slices.Equal(got, []string{"Доп лист Матан", "DM HW 3"}) {
		t.Fatal(got)
	}
}

func TestAFailedRetryKeepsTheWideLabel(t *testing.T) {
	f := setup(t, nil)
	q := &queued{answers: []any{
		map[string]any{"items": []any{map[string]any{"id": "8f1c2a0e", "label": "DM homework three"}}},
		llm.NewError(llm.Failed, "Anthropic API error 529: Overloaded")}}
	f.LLM = q.model()
	f.poll(t, parse(items[:1], false), nil)
	if f.labels(t)[0] != "DM homework three" || f.LabelsError() != "" {
		t.Fatal(f.labels(t), f.LabelsError())
	}
	f.poll(t, parse(items[:1], false), nil) // cached: no answers left
	if len(q.prompts) != 2 {
		t.Fatal(q.prompts)
	}
}

func TestOddAnswersAreSkipped(t *testing.T) {
	f := setup(t, nil)
	q := &queued{answers: []any{
		json.RawMessage(`{"items": [{"id": "zzz", "label": "Stray"}, {"id": "8f1c2a0e", "label": "  "}, "junk", ` +
			`{"id": "91aa07d3", "label": " Calc   extra "}]}`),
		map[string]any{"items": []any{map[string]any{"id": "8f1c2a0e", "label": "DM HW 3"}}}}}
	f.LLM = q.model()
	f.poll(t, parse(items, false), nil)
	if got := f.labels(t); !slices.Equal(got, []string{"Calc extra", titleDM}) {
		t.Fatal(got)
	}
	f.poll(t, parse(items, false), nil) // asks again for the one left without a label
	if strings.Contains(q.prompts[1], `"Доп лист Матан"`) {
		t.Fatal(q.prompts[1])
	}
	if got := f.labels(t); !slices.Equal(got, []string{"Calc extra", "DM HW 3"}) {
		t.Fatal(got)
	}
}

func TestWithoutHaikuCachedLabelsStayAndTheRestWaits(t *testing.T) {
	f := setup(t, nil)
	f.poll(t, parse(items[:1], false), nil)
	f.labeler.errs = []error{llm.NewError(llm.Failed, "ANTHROPIC_API_KEY is not set")}
	f.poll(t, parse(items[:2], false), nil)
	if got := f.labels(t); !slices.Equal(got, []string{"Доп лист Матан", "DM HW 3"}) {
		t.Fatal(got)
	}
	if st, _ := f.Status(); st.LabelsError == nil || *st.LabelsError != "ANTHROPIC_API_KEY is not set" {
		t.Fatal(st)
	}
	f.poll(t, parse(items[:2], false), nil) // the next poll asks again, for the new one only
	last := f.labeler.prompts[len(f.labeler.prompts)-1]
	if got := f.labels(t); !slices.Equal(got, []string{"Calc extra", "DM HW 3"}) || f.LabelsError() != "" ||
		!strings.Contains(last, `"Доп лист Матан"`) || strings.Contains(last, "Дискретной") {
		t.Fatal(got, last)
	}
}

func TestBudgetExceededWaitsQuietly(t *testing.T) {
	f := setup(t, nil)
	f.labeler.errs = []error{&llm.BudgetExceeded{Msg: "the daily LLM limit is spent, calls wait for midnight"}}
	f.poll(t, parse(items, false), nil)
	if got := f.labels(t); !slices.Equal(got, []string{"Доп лист Матан", titleDM}) || f.LabelsError() != "" {
		t.Fatal(got)
	}
}

func TestABadAnswerIsSplitToFindTheDeadline(t *testing.T) {
	f := setup(t, nil)
	f.labeler.broken["Доп лист Матан"] = true
	f.poll(t, parse(items, false), parse(done, true))
	lists, _ := f.Lists()
	got := map[string]string{}
	for _, row := range slices.Concat(lists.Items, lists.Done) {
		got[row.ID] = row.Label
	}
	if fmt.Sprint(got) != fmt.Sprint(map[string]string{"8f1c2a0e": "DM HW 3", "91aa07d3": "Доп лист Матан",
		"b2d70c44": "Safety HW 2"}) {
		t.Fatal(got)
	}
	calls := len(f.labeler.prompts)
	f.poll(t, parse(items, false), parse(done, true))
	if len(f.labeler.prompts) != calls { // the broken one keeps its title for good: no call every poll
		t.Fatal("asked again")
	}
}

func TestALongListGetsRoomToAnswer(t *testing.T) {
	f := setup(t, nil)
	var many []any
	for n := range 100 {
		many = append(many, item(fmt.Sprintf("ДЗ %d", n), "2026-10-30T20:59:00Z", "", "ACTIVE", fmt.Sprintf("id-%d", n)))
	}
	f.poll(t, nil, parse(many, false))
	if !slices.Equal(f.labeler.maxTokens, []int{100 * TokensPerLabel}) {
		t.Fatal(f.labeler.maxTokens)
	}
	f.poll(t, parse(items[1:2], false), nil)
	if f.labeler.maxTokens[len(f.labeler.maxTokens)-1] != MaxTokens {
		t.Fatal(f.labeler.maxTokens)
	}
}

func TestDictionaryDownLabelsWithoutAbbr(t *testing.T) {
	f := setup(t, llm.NewError(llm.Failed, "cannot reach the Anthropic API"))
	f.poll(t, parse(items[:1], false), nil)
	if !strings.Contains(f.labeler.prompts[0], "(course: Дискретная математика)") || f.Subjects.LastError() == "" {
		t.Fatal(f.labeler.prompts[0])
	}
}
