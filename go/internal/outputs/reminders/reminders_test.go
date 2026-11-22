package reminders

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	dl "github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = int64(-1001234567890)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

type fixture struct {
	*Reminders
	store  *store.Store
	moment time.Time
	told   []string
}

// at is Moscow time, October 2026.
func (f *fixture) at(day, hour, minute int) {
	f.moment = time.Date(2026, 10, day, hour, minute, 5, 0, clock.TZ).UTC()
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	f := &fixture{store: s}
	f.at(20, 12, 0)
	now := func() time.Time { return f.moment }
	sub := subjects.New(s, &llm.Fake{})
	merged := merge.New(s, dl.Open(s), hseapp.New(s, sub, &llm.Fake{}), sub)
	merged.Now = now
	f.Reminders = New(s, merged, must(settings.New(s)))
	f.Now = now
	f.Notify = func(name string, due []todo.Deadline) {
		var titles []string
		for _, d := range due {
			titles = append(titles, d.Title)
		}
		f.told = append(f.told, name+": "+strings.Join(titles, ", "))
	}
	must(0, f.Start()) // Tue Oct 20 12:00: the morning's slot has come, the eve's is yesterday's
	return f
}

func (f *fixture) add(label, due string, msgID int64) int64 {
	return must(f.Merged.Deadlines.Add(dl.New{Source: "tg", ChatID: &[]int64{chat}[0], MsgIDs: []int64{msgID},
		Course: "Матанализ", Label: label, Due: due}, f.Now()))
}

func (f *fixture) tick(t *testing.T) {
	t.Helper()
	if err := f.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTheEveAndTheMorning(t *testing.T) {
	f := setup(t)
	f.add("Calc HW 5", "2026-10-21", 3)
	f.add("Calc HW 6", "2026-10-22", 4)
	must(f.Merged.Add("Dentist", todo.Date(2026, 10, 21), "personal", ""))
	f.tick(t)
	if len(f.told) != 0 { // the first start: what has come is no news
		t.Fatal(f.told)
	}
	f.at(20, 20, 0)
	f.tick(t)
	if !slices.Equal(f.told, []string{"eve: Calc HW 5, Dentist"}) {
		t.Fatal(f.told)
	}
	f.tick(t)
	if len(f.told) != 1 { // once per slot
		t.Fatal(f.told)
	}
	f.at(21, 9, 0)
	f.tick(t)
	if f.told[1] != "day: Calc HW 5, Dentist" {
		t.Fatal(f.told)
	}
	var raw string
	must(0, f.store.DB.QueryRow("select value from kv where key = ?", DoneKey).Scan(&raw))
	if raw != `{"day": "2026-10-21", "eve": "2026-10-20"}` {
		t.Fatal(raw)
	}
}

func TestNothingDueAndOff(t *testing.T) {
	f := setup(t)
	f.at(20, 20, 0)
	f.tick(t) // nothing due tomorrow: nothing told
	f.add("Calc HW 5", "2026-10-22", 3)
	must(f.Settings.Set("deadline_reminders", false))
	f.at(21, 21, 0)
	f.tick(t)
	done := map[string]string{}
	must(f.store.GetJSON(DoneKey, &done))
	if len(f.told) != 0 || done["eve"] != "2026-10-21" { // off: not sent later
		t.Fatal(f.told, done)
	}
}

func TestAMissedSlotComesLateTheSameDay(t *testing.T) {
	f := setup(t)
	f.add("Calc HW 5", "2026-10-21", 3)
	f.at(20, 23, 30) // the service was down at 20:00
	f.tick(t)
	if !slices.Equal(f.told, []string{"eve: Calc HW 5"}) {
		t.Fatal(f.told)
	}
	must(f.Settings.Set("reminder_eve", "23:45")) // a later time today: the eve is sent already
	f.at(20, 23, 50)
	f.tick(t)
	if len(f.told) != 1 {
		t.Fatal(f.told)
	}
}

func TestHiddenDeadlinesAreNotReminded(t *testing.T) {
	f := setup(t)
	f.add("Calc HW 5", "2026-10-21", 3)
	f.add("Calc KR", "2026-10-21", 5)
	for _, d := range must(f.Merged.Visible()) {
		if d.Title == "Calc HW 5" {
			must(0, f.Merged.Remove(d, "", ""))
		}
	}
	f.at(20, 20, 0)
	f.tick(t)
	if !slices.Equal(f.told, []string{"eve: Calc KR"}) {
		t.Fatal(f.told)
	}
}

func TestAFailureDoesNotEndTheLoop(t *testing.T) {
	f := setup(t)
	var ticks int
	var waits []time.Duration
	f.Reminders.tick = func(context.Context) error {
		ticks++
		return errors.New("a broken time")
	}
	f.Reminders.wait = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return len(waits) < 2
	}
	f.Run(context.Background())
	if ticks != 2 || !slices.Equal(waits, []time.Duration{RetryAfter, RetryAfter}) {
		t.Fatal(ticks, waits)
	}
}

func TestTheWaitIsToTheNextSlot(t *testing.T) {
	f := setup(t)
	f.at(20, 12, 0)
	got := must(f.toNext())
	if got != 8*time.Hour { // 20:00:05 from 12:00:05
		t.Fatal(got)
	}
}
