package llm

import (
	"errors"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"reflect"
	"testing"
	"time"
)

func levels(notices []Notice) []Level {
	out := []Level{}
	for _, n := range notices {
		out = append(out, n.Level)
	}
	return out
}

func exceeded(t *testing.T, err error) *BudgetExceeded {
	t.Helper()
	var e *BudgetExceeded
	if !errors.As(err, &e) {
		t.Fatalf("expected BudgetExceeded, got %v", err)
	}
	return e
}

func TestCost(t *testing.T) {
	for _, c := range []struct{ got, want float64 }{
		{CostUSD(haiku, 100_000, 0, 0, 0), 0.01},
		{CostUSD(haiku, 0, 1_000_000, 0, 0), 0.50},
		{CostUSD(haiku, 0, 0, 100_000, 0), 0.0125},
		// over 100K tokens of prompt, cache included: the long rates
		{CostUSD(haiku, 50_000, 0, 0, 60_000), (50_000 + 6_000) * 0.50 / 1e6},
		{CostUSD(haiku, 1_000_000, 0, 0, 0), 0.50},
	} {
		if !approx(c.got, c.want) {
			t.Errorf("%v != %v", c.got, c.want)
		}
	}
}

func TestEstimateIsPessimistic(t *testing.T) {
	// 2 chars per token: 10 000 chars -> 5 000 tokens in, plus the whole max_tokens out
	if got := Estimate(haiku, 10_000, 8_000); !approx(got, 0.0045) {
		t.Fatal(got)
	}
	if got := Estimate(haiku, 9_999, 8_000); !approx(got, 0.0045) { // ceil
		t.Fatal(got)
	}
}

func TestReserveCountsUntilRelease(t *testing.T) {
	f := setup(t)
	ticket := must(f.budget.Reserve("batch", 0.30))
	if spent := must(f.budget.Spent()); !approx(spent, 0.30) {
		t.Fatal(spent)
	}
	noErr(t, f.budget.Release(ticket))
	if spent := must(f.budget.Spent()); spent != 0 {
		t.Fatal(spent)
	}
	if len(f.notices) != 0 {
		t.Fatal(f.notices)
	}
}

func TestSoftLevelStopsOptionalTasks(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.41, f.now.now(), "", nil)
	f.budget.Release(must(f.budget.Reserve("batch", 0.01)))
	if level := must(f.budget.Level()); level != Soft {
		t.Fatal(level)
	}
	if !reflect.DeepEqual(levels(f.notices), []Level{Soft}) {
		t.Fatal(f.notices)
	}
	_, err := f.budget.Reserve("image", 0.001)
	if e := exceeded(t, err); e.Msg != "optional LLM tasks are off above 80% of the daily limit" {
		t.Fatal(e.Msg)
	}
	f.budget.Release(must(f.budget.Reserve("batch", 0.01))) // required tasks go on
	if len(f.notices) != 1 {                                // announced once a day
		t.Fatal(f.notices)
	}
}

func TestRequiredCallThatDoesNotFitStopsEverything(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.30, f.now.now(), "", nil)
	_, err := f.budget.Reserve("batch", 0.25)
	exceeded(t, err)
	if level := must(f.budget.Level()); level != Hard {
		t.Fatal(level)
	}
	if !reflect.DeepEqual(levels(f.notices), []Level{Hard}) {
		t.Fatal(f.notices)
	}
	_, err = f.budget.Reserve("subjects", 0.001)
	exceeded(t, err)
	var notified struct {
		Day   string   `json:"day"`
		Value []string `json:"value"`
	}
	f.store.GetJSON("llm.notified", &notified)
	if !reflect.DeepEqual(notified.Value, []string{"hard", "soft"}) {
		t.Fatal(notified)
	}
}

func TestOptionalCallThatDoesNotFitStopsNothing(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.10, f.now.now(), "", nil)
	_, err := f.budget.Reserve("pdf", 0.45)
	if !exceeded(t, err).DoesNotFit {
		t.Fatal("not DoesNotFit")
	}
	if level := must(f.budget.Level()); level != OK {
		t.Fatal(level)
	}
	if len(f.notices) != 0 {
		t.Fatal(f.notices)
	}
}

func TestSpentByJournalReachesHard(t *testing.T) {
	f := setup(t)
	ticket := must(f.budget.Reserve("batch", 0.01))
	spend(t, f.journal, 0.60, f.now.now(), "", nil) // the real cost came out above the estimate
	f.budget.Release(ticket)
	if level := must(f.budget.Level()); level != Hard {
		t.Fatal(level)
	}
	if f.notices[len(f.notices)-1].Level != Hard {
		t.Fatal(f.notices)
	}
}

func TestRaiseToday(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.45, f.now.now(), "", nil)
	_, err := f.budget.Reserve("batch", 0.10)
	exceeded(t, err)
	if limit := must(f.budget.RaiseToday(0.50)); !approx(limit, 1.00) {
		t.Fatal(limit)
	}
	if level := must(f.budget.Level()); level != OK {
		t.Fatal(level)
	}
	f.budget.Release(must(f.budget.Reserve("batch", 0.10)))
	if status := must(f.budget.Status()); status.RaisedUSD != 0.5 {
		t.Fatal(status)
	}
	var text string
	f.store.DB.QueryRow("select value from kv where key = 'llm.raised'").Scan(&text)
	if text != `{"day": "2026-10-20", "value": 0.5}` {
		t.Fatal(text)
	}
}

func TestNewDayStartsClean(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.60, f.now.now(), "", nil)
	f.budget.RaiseToday(0.50)
	_, err := f.budget.Reserve("batch", 0.50)
	exceeded(t, err)
	f.now.moment = f.now.moment.Add(12 * time.Hour) // past Moscow midnight
	if limit := must(f.budget.Limit()); limit != 0.50 {
		t.Fatal(limit)
	}
	if level := must(f.budget.Level()); level != OK {
		t.Fatal(level)
	}
	f.budget.Release(must(f.budget.Reserve("batch", 0.10)))
}

func TestDayIsMoscowDay(t *testing.T) {
	f := setup(t)
	f.now.moment = time.Date(2026, 10, 20, 21, 30, 0, 0, time.UTC)                      // 00:30 on the 21st in Moscow
	spend(t, f.journal, 0.20, time.Date(2026, 10, 20, 20, 59, 0, 0, time.UTC), "", nil) // 23:59 on the 20th
	spend(t, f.journal, 0.05, time.Date(2026, 10, 20, 21, 1, 0, 0, time.UTC), "", nil)
	if spent := must(f.budget.Spent()); !approx(spent, 0.05) {
		t.Fatal(spent)
	}
}

func TestStoppedDaysAndAChangedLimit(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.30, f.now.now(), "", nil)
	_, err := f.budget.Reserve("batch", 0.25)
	exceeded(t, err)
	if days := must(f.budget.StoppedDays()); !reflect.DeepEqual(days, map[string]pyjson.Float{"2026-10-20": 0.5}) {
		t.Fatal(days)
	}
	must(f.budget.Settings.Set("llm_daily_limit_usd", 1.0)) // the web app's limit: calls run again at once
	if level := must(f.budget.Level()); level != Hard {
		t.Fatal(level)
	}
	noErr(t, f.budget.LimitChanged())
	if level := must(f.budget.Level()); level != OK {
		t.Fatal(level)
	}
	f.budget.Release(must(f.budget.Reserve("batch", 0.25)))
	f.now.moment = f.now.moment.AddDate(0, 0, 1)
	spend(t, f.journal, 0.95, f.now.now(), "", nil)
	ticket := must(f.budget.Reserve("batch", 0.04))
	spend(t, f.journal, 0.05, f.now.now(), "", nil) // the call cost more than it reserved: the limit is reached after it
	f.budget.Release(ticket)
	if level := must(f.budget.Level()); level != Hard {
		t.Fatal(level)
	}
	want := map[string]pyjson.Float{"2026-10-20": 0.5, "2026-10-21": 1.0}
	if days := must(f.budget.StoppedDays()); !reflect.DeepEqual(days, want) {
		t.Fatal(days)
	}
	var text string
	f.store.DB.QueryRow("select value from kv where key = 'llm.days'").Scan(&text)
	if text != `{"2026-10-20": 0.5, "2026-10-21": 1.0}` {
		t.Fatal(text)
	}
}
