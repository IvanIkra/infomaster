package llm

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

func addFailed(t *testing.T, j *Journal) {
	t.Helper()
	run := must(j.Runs(1, ""))[0]
	boom := "boom"
	run.OK, run.Error, run.CostUSD = false, &boom, 0
	if err := j.Add(run); err != nil {
		t.Fatal(err)
	}
}

func TestTotalsByTaskAndChat(t *testing.T) {
	f := setup(t)
	now := f.now.now()
	spend(t, f.journal, 0.02, now, "batch", id(-1001))
	spend(t, f.journal, 0.03, now, "batch", id(-1002))
	spend(t, f.journal, 0.01, now, "subjects", nil)
	addFailed(t, f.journal)
	totals := must(f.journal.Totals(now.Add(-time.Hour)))
	if totals.Runs != 4 || totals.Failed != 1 || totals.CostUSD != 0.06 {
		t.Fatalf("%+v", totals)
	}
	if totals.ByTask[0].Task != "batch" || totals.ByTask[0].Runs != 2 { // the costliest first
		t.Fatalf("%+v", totals.ByTask)
	}
	if len(totals.ByChat) != 2 || totals.ByChat[0].ChatID != -1002 || totals.ByChat[1].ChatID != -1001 {
		t.Fatalf("%+v", totals.ByChat)
	}
}

func TestEmpty(t *testing.T) {
	f := setup(t)
	totals := must(f.journal.Totals(f.now.now()))
	text, _ := json.Marshal(totals)
	if string(text) != `{"runs":0,"failed":0,"cost_usd":0.0,"tokens":0,"by_task":[],"by_chat":[]}` {
		t.Fatal(string(text))
	}
}

func TestRunsRoundTrip(t *testing.T) {
	f := setup(t)
	spend(t, f.journal, 0.5, f.now.now(), "pdf", id(-1001))
	addFailed(t, f.journal)
	var ok int
	var chat any
	f.store.DB.QueryRow("select ok, chat_id from llm_runs where id = 1").Scan(&ok, &chat)
	if ok != 1 {
		t.Fatal(ok)
	}
	runs := must(f.journal.Runs(100, "pdf"))
	if len(runs) != 2 || runs[0].OK || *runs[0].Error != "boom" || !runs[1].OK || runs[1].Error != nil ||
		*runs[1].ChatID != -1001 || runs[1].CostUSD != 0.5 {
		t.Fatalf("%+v", runs)
	}
	spend(t, f.journal, 0.1, f.now.now(), "image", nil)
	var null any = 1
	f.store.DB.QueryRow("select chat_id from llm_runs where task = 'image'").Scan(&null)
	if null != nil {
		t.Fatal(null)
	}
	if n := must(f.journal.CountSince(f.now.now().Add(-time.Hour), "image")); n != 1 {
		t.Fatal(n)
	}
}

func TestUsagePeriods(t *testing.T) {
	f := setup(t)
	now := f.now.now() // Tuesday 2026-10-20, noon in Moscow
	spend(t, f.journal, 1.0, now, "", nil)
	spend(t, f.journal, 2.0, now.AddDate(0, 0, -1), "", nil)  // Monday: this week
	spend(t, f.journal, 4.0, now.AddDate(0, 0, -3), "", nil)  // Saturday: this month
	spend(t, f.journal, 8.0, now.AddDate(0, 0, -30), "", nil) // September
	usage := must(f.journal.Usage(now))
	got := []pyjson.Float{usage.Today.CostUSD, usage.Week.CostUSD, usage.Month.CostUSD}
	if !reflect.DeepEqual(got, []pyjson.Float{1, 3, 7}) {
		t.Fatal(got)
	}
}

func TestDaily(t *testing.T) {
	// Moscow days: 00:30 in Moscow is still the evening before in UTC, and belongs to its Moscow day.
	f := setup(t)
	now := f.now.now()
	spend(t, f.journal, 0.10, now, "", nil)
	addFailed(t, f.journal)
	spend(t, f.journal, 0.20, time.Date(now.Year(), now.Month(), now.Day(), 21, 30, 0, 0, time.UTC), "", nil) // Oct 21
	spend(t, f.journal, 0.30, now.Add(-(11*time.Hour + 30*time.Minute)), "", nil)                             // Oct 20
	spend(t, f.journal, 0.40, now.AddDate(0, 0, -2), "", nil)
	spend(t, f.journal, 9.00, now.AddDate(0, 0, -7), "", nil) // before the week asked for
	days := must(f.journal.Daily(now, 7))
	var names []string
	for _, d := range days {
		names = append(names, d.Day)
	}
	if !reflect.DeepEqual(names, []string{"2026-10-14", "2026-10-15", "2026-10-16", "2026-10-17", "2026-10-18",
		"2026-10-19", "2026-10-20"}) {
		t.Fatal(names)
	}
	if days[6] != (Day{"2026-10-20", 3, 1, 0.4}) {
		t.Fatalf("%+v", days[6])
	}
	if days[4].CostUSD != 0.4 || days[0] != (Day{"2026-10-14", 0, 0, 0}) {
		t.Fatalf("%+v", days)
	}
	text, _ := json.Marshal(days[0])
	if string(text) != `{"day":"2026-10-14","runs":0,"failed":0,"cost_usd":0.0}` {
		t.Fatal(string(text))
	}
}
