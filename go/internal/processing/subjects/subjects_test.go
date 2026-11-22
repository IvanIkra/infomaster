package subjects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

var names = map[string][2]string{
	"Математический анализ": {"Calculus", "Calc"},
	"Алгебра":                                {"Algebra", "Alg"},
	"Algebra":                                {"Algebra", "Alg"},
	"Discrete Mathematics":                   {"Discrete Math", "DM"},
	"Introduction into Software Engineering": {"Intro to SE", "SE"},
	"Основы российской государственности": {"Rus Statehood", "RSF"},
	"Foundations of Russian Statehood":    {"Rus Statehood", "RSF"},
	"Введение в программирование":         {"Intro to Prog", "Prog"},
	"Calculus": {"Calculus", "Calc"},
}

// namer is Haiku for the dictionary: names the courses it knows, or fails with err.
type namer struct {
	mu      sync.Mutex
	prompts []string
	err     error
	answer  func(prompt string) (any, error)
}

func (n *namer) model() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		n.mu.Lock()
		n.prompts = append(n.prompts, req.Prompt)
		err, answer := n.err, n.answer
		n.mu.Unlock()
		if req.Task != "subjects" {
			panic("task " + req.Task)
		}
		if err != nil {
			return nil, err
		}
		if answer != nil {
			return answer(req.Prompt)
		}
		return nameAll(req.Prompt), nil
	}}
}

func nameAll(prompt string) any {
	items := []map[string]any{}
	for _, line := range strings.Split(prompt, "\n") {
		if rest, ok := strings.CutPrefix(line, "- id "); ok {
			index, name, _ := strings.Cut(rest, ": ")
			if named, ok := names[strings.Trim(name, `"`)]; ok {
				items = append(items, map[string]any{"id": index, "title": named[0], "abbr": named[1]})
			}
		}
	}
	return map[string]any{"items": items}
}

func (n *namer) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.prompts)
}

func setup(t *testing.T) (*Subjects, *namer) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	n := &namer{}
	return New(s, n.model()), n
}

// must is a value whose error fails the test (a panic fails it).
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func noErr(err error) {
	if err != nil {
		panic(err)
	}
}

func TestSystemIsPythons(t *testing.T) {
	sum := sha256.Sum256([]byte(System))
	if hex.EncodeToString(sum[:]) != "58e45c358ae03928a876b70eab19938a89380397fc2e0f33072b886c5a9eb833" {
		t.Fatal("SYSTEM differs from the Python prompt")
	}
}

func TestClean(t *testing.T) {
	if Clean("  Алгебра  (рус) ") != "Алгебра" || Clean("Algebra") != "Algebra" || Clean("") != "" {
		t.Fatal("clean")
	}
}

func TestOneCallWithTheDictionary(t *testing.T) {
	ctx := context.Background()
	s, n := setup(t)
	got := must(s.Resolve(ctx, []string{"Algebra", "", ""}))
	if len(got) != 1 || got["Algebra"].Abbr != "Alg" {
		t.Fatalf("%v", got)
	}
	must(s.Resolve(ctx, []string{"Algebra", "Алгебра (рус)", "Введение в программирование"}))
	if len(n.prompts) != 2 {
		t.Fatal(n.prompts)
	}
	if !strings.Contains(n.prompts[1], `- Alg "Algebra": "Algebra"`) ||
		!strings.Contains(n.prompts[1], "- id 0: \"Алгебра\"\n- id 1: \"Введение в программирование\"") ||
		!strings.HasPrefix(n.prompts[1], "Title budget: at most 13 characters.") {
		t.Fatal(n.prompts[1])
	}
	must(s.Resolve(ctx, []string{"Алгебра", "Algebra"}))
	if len(n.prompts) != 2 {
		t.Fatal("all known: no call")
	}
	if must(s.Title("Алгебра (рус)")) != "Algebra" || must(s.Abbr("Введение в программирование")) != "Prog" {
		t.Fatal("lookups")
	}
	if must(s.Title("Матан")) != "Матан" || must(s.Abbr("Матан")) != "" {
		t.Fatal("not named yet: as the source")
	}
}

func TestDictionaryLines(t *testing.T) {
	known := []Named{{"Введение в программирование", Subject{"Intro to Prog", "Prog", false}},
		{"Algebra", Subject{"Algebra", "Alg", false}}, {"ВвП", Subject{"Intro to Prog", "Prog", true}},
		{"Алгебра", Subject{"Algebra", "Alg", false}}}
	want := []string{`- Alg "Algebra": "Algebra", "Алгебра"`, `- Prog "Intro to Prog": "Введение в программирование", "ВвП"`}
	if got := DictionaryLines(known); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	if len(DictionaryLines(nil)) != 0 {
		t.Fatal("empty")
	}
}

func TestWideTitleGetsOneRetry(t *testing.T) {
	s, n := setup(t)
	n.answer = func(prompt string) (any, error) {
		title := "Discrete Mathematics"
		if strings.Contains(prompt, "too wide") {
			title = "Discrete Math"
		}
		return map[string]any{"items": []any{map[string]any{"id": "0", "title": title, "abbr": "DMath-long"}}}, nil
	}
	got := must(s.Resolve(context.Background(), []string{"Discrete Mathematics"}))
	if got["Discrete Mathematics"] != (Subject{"Discrete Math", "DMath-", false}) {
		t.Fatal(got)
	}
	if !strings.Contains(n.prompts[1], `Your previous title "Discrete Mathematics" was too wide for the screen`) {
		t.Fatal(n.prompts[1])
	}
}

func TestFailedRetryKeepsTheWideTitle(t *testing.T) {
	s, n := setup(t)
	calls := 0
	n.answer = func(string) (any, error) {
		calls++
		if calls == 2 {
			return nil, llm.NewError(llm.Failed, "down")
		}
		return map[string]any{"items": []any{map[string]any{"id": "0", "title": "Discrete Mathematics", "abbr": "DM"}}},
			nil
	}
	got := must(s.Resolve(context.Background(), []string{"Discrete Mathematics"}))
	if got["Discrete Mathematics"].Title != "Discrete Mathematics" {
		t.Fatal(got)
	}
}

func TestBadItemsAreSkipped(t *testing.T) {
	s, n := setup(t)
	n.answer = func(string) (any, error) {
		return map[string]any{"items": []any{
			map[string]any{"id": "-1", "title": "X", "abbr": "X"}, map[string]any{"id": "7", "title": "X", "abbr": "X"},
			map[string]any{"id": "one", "title": "X", "abbr": "X"}, map[string]any{"id": "0", "title": " ", "abbr": "A"},
			map[string]any{"id": "1", "title": "History", "abbr": "Hist"}}}, nil
	}
	got := must(s.Resolve(context.Background(), []string{"История", "Алгебра"}))
	if len(got) != 1 || got["История"].Abbr != "Hist" { // ids follow the sorted names
		t.Fatal(got)
	}
	if must(s.Get("Алгебра")) != nil {
		t.Fatal("its title came blank: asked again next time")
	}
}

func TestConcurrentResolvesAskEachNameOnce(t *testing.T) {
	s, n := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	n.answer = func(prompt string) (any, error) {
		once.Do(func() { close(started); <-release }) // RUZ and a batch wait on Haiku at once, as at start-up
		return nameAll(prompt), nil
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.Resolve(ctx, []string{"Algebra"}) }()
	<-started
	go func() { defer wg.Done(); s.Resolve(ctx, []string{"Algebra", "Discrete Mathematics"}) }()
	close(release)
	wg.Wait()
	if n.count() != 2 || !strings.Contains(n.prompts[1], `- id 0: "Discrete Mathematics"`) {
		t.Fatal(n.prompts)
	}
}

func TestManualEntries(t *testing.T) {
	ctx := context.Background()
	s, n := setup(t)
	entry := must(s.SetManual(" Алгебра (рус) ", " Linear  Alg ", "LA"))
	if entry != (Entry{"Алгебра", "Linear Alg", "LA", true}) {
		t.Fatal(entry)
	}
	noErr(s.put("Алгебра", "Algebra", "Alg", false)) // Haiku never overwrites a manual entry
	if must(s.Get("Алгебра")).Title != "Linear Alg" {
		t.Fatal("overwritten")
	}
	got := must(s.Resolve(ctx, []string{"Алгебра"}))
	if got["Алгебра"].Title != "Linear Alg" || len(n.prompts) != 0 {
		t.Fatal(got)
	}
	for _, bad := range [][3]string{{"", "X", "X"}, {"Матан", "", "C"}, {"Матан", strings.Repeat("C", 41), "C"},
		{"Матан", "Calculus", "Calcul8"}} {
		if _, err := s.SetManual(bad[0], bad[1], bad[2]); err == nil {
			t.Fatal(bad)
		} else if _, ok := err.(*Error); !ok {
			t.Fatal(err)
		}
	}
	if !must(s.Delete("Алгебра")) || must(s.Delete("Алгебра")) {
		t.Fatal("delete")
	}
	must(s.Resolve(ctx, []string{"Алгебра"}))
	if *must(s.Get("Алгебра")) != (Subject{"Algebra", "Alg", false}) {
		t.Fatal("Haiku named it again, and it is not manual")
	}
	must(s.Resolve(ctx, []string{"Calculus"}))
	must(s.SetManual("Calculus", "Calc", "Calc")) // a Haiku entry overridden by hand becomes manual
	if !must(s.Get("Calculus")).Manual {
		t.Fatal("not manual")
	}
}

func TestTryResolve(t *testing.T) {
	ctx := context.Background()
	s, n := setup(t)
	n.err = llm.NewError(llm.Failed, "ANTHROPIC_API_KEY is not set")
	noErr(s.TryResolve(ctx, []string{"Алгебра"}, "ruz"))
	if s.LastError() != "ANTHROPIC_API_KEY is not set" || must(s.Get("Алгебра")) != nil {
		t.Fatal(s.LastError())
	}
	n.err = &llm.BudgetExceeded{Msg: "spent"}
	noErr(s.TryResolve(ctx, []string{"Алгебра"}, "ruz")) // the budget tells the owner itself
	if s.LastError() != "ANTHROPIC_API_KEY is not set" {
		t.Fatal(s.LastError())
	}
	n.err = nil
	noErr(s.TryResolve(ctx, []string{"Алгебра"}, "ruz"))
	if s.LastError() != "" || must(s.Abbr("Алгебра")) != "Alg" {
		t.Fatal(s.LastError())
	}
	calls := len(n.prompts)
	noErr(s.TryResolve(ctx, []string{"Алгебра", "", ""}, "ruz"))
	if len(n.prompts) != calls {
		t.Fatal("nothing unknown: no call")
	}
}

func TestErrorGoesOnceNothingWaits(t *testing.T) {
	ctx := context.Background()
	s, n := setup(t)
	n.err = llm.NewError(llm.Failed, "ANTHROPIC_API_KEY is not set")
	noErr(s.TryResolve(ctx, []string{"Матан"}, "batches"))
	if s.LastError() != "ANTHROPIC_API_KEY is not set" {
		t.Fatal(s.LastError())
	}
	must(s.SetManual("Матан", "Calculus", "Calc")) // the owner names it by hand: nothing waits for Haiku
	if s.LastError() != "" {
		t.Fatal(s.LastError())
	}
	calls := len(n.prompts)
	noErr(s.TryResolve(ctx, []string{"Матан"}, "batches"))
	if s.LastError() != "" || len(n.prompts) != calls {
		t.Fatal("no call")
	}
	noErr(s.TryResolve(ctx, []string{"История"}, "batches"))
	noErr(s.TryResolve(ctx, []string{"Algebra"}, "ruz"))
	n.err = nil
	noErr(s.TryResolve(ctx, []string{"Algebra"}, "ruz"))
	if s.LastError() != "ANTHROPIC_API_KEY is not set" { // RUZ is fine; the batches' course still waits
		t.Fatal(s.LastError())
	}
	must(s.SetManual("Algebra", "Algebra", "Alg"))
	if s.LastError() != "ANTHROPIC_API_KEY is not set" { // another name: it still waits
		t.Fatal(s.LastError())
	}
	noErr(s.TryResolve(ctx, nil, "batches")) // its deadline aged out: the batches need no name any more
	if s.LastError() != "" {
		t.Fatal(s.LastError())
	}
}
