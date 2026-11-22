package llm

import (
	"math"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

var haiku = settings.Price{Input: 0.10, Output: 0.50, LongInput: 0.50, LongOutput: 2.50}

// fakeNow is a settable now() for the budget; it starts at noon in Moscow, Tuesday 2026-10-20.
type fakeNow struct{ moment time.Time }

func (n *fakeNow) now() time.Time { return n.moment }

type fixture struct {
	store   *store.Store
	journal *Journal
	budget  *Budget
	now     *fakeNow
	notices []Notice
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	general, err := settings.New(s)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: s, journal: NewJournal(s),
		now: &fakeNow{time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}}
	f.budget = NewBudget(general, f.journal, s)
	f.budget.Now = f.now.now
	f.budget.Notify = func(n Notice) { f.notices = append(f.notices, n) }
	return f
}

func spend(t *testing.T, j *Journal, usd float64, at time.Time, task string, chatID *int64) {
	t.Helper()
	if task == "" {
		task = "batch"
	}
	if err := j.Add(Run{At: clock.UTCISO(at), Task: task, ChatID: chatID, Model: ModelID, OK: true,
		InputTokens: 1, OutputTokens: 1, CostUSD: pyjson.Float(usd), DurationS: 0.1}); err != nil {
		t.Fatal(err)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// must is the value of a call that cannot fail in the test.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func id(n int64) *int64 { return &n }
