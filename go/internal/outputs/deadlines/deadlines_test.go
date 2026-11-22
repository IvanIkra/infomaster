package deadlines

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	dl "github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = int64(-1001)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func ptr[T any](v T) *T { return &v }

type fixture struct {
	*Watch
	store *store.Store
	mu    sync.Mutex
	told  []string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	now := func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC() }
	sub := subjects.New(s, &llm.Fake{})
	merged := merge.New(s, dl.Open(s), hseapp.New(s, sub, &llm.Fake{}), sub)
	merged.Now = now
	f := &fixture{Watch: New(s, merged), store: s}
	f.Now = now
	merged.OnChange = f.Changed
	f.Notify = func(d todo.Deadline, was string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.told = append(f.told, fmt.Sprintf("%s|%s", d.Title, was))
	}
	return f
}

func (f *fixture) lists(items ...[3]string) {
	rows := []hseapp.Row{}
	for _, item := range items {
		rows = append(rows, hseapp.Row{ID: item[0], Label: item[1], Due: item[2], Title: item[1]})
	}
	must(0, f.store.PutJSON(hseapp.ListsKey, hseapp.Lists{Items: rows, Done: []hseapp.Row{}}))
}

func (f *fixture) add(label, due string, msgID int64) int64 {
	return must(f.Merged.Deadlines.Add(dl.New{Source: "tg", ChatID: ptr(chat), MsgIDs: []int64{msgID}, Label: label,
		Due: due}, f.Now()))
}

func (f *fixture) refresh(t *testing.T) {
	t.Helper()
	if err := f.Refresh(); err != nil {
		t.Fatal(err)
	}
}

func TestNewMovedAndQuiet(t *testing.T) {
	f := setup(t)
	f.lists([3]string{"h1", "DM HW 3", "2026-10-22"}, [3]string{"h2", "Calc HW 1", "2026-10-23"})
	f.refresh(t) // the first look takes the list as it is
	if len(f.told) != 0 {
		t.Fatal(f.told)
	}
	var shown []Entry
	must(f.store.GetJSON(ShownKey, &shown))
	if len(shown) != 2 || shown[0] != (Entry{Ref: "hseapp:h1", Title: "DM HW 3", Due: "2026-10-22", Source: "hseapp"}) {
		t.Fatal(shown)
	}

	f.lists([3]string{"h1", "DM HW 3", "2026-10-22"}, [3]string{"h2", "Calc HW 1", "2026-10-23"},
		[3]string{"h3", "Eng essay", "2026-10-25"})
	f.refresh(t)
	if !slices.Equal(f.told, []string{"Eng essay|"}) {
		t.Fatal(f.told)
	}

	must(f.Merged.Add("Dentist", todo.Date(2026, 10, 20), "personal", "")) // the owner's: no news
	f.Settle()
	if len(f.told) != 1 {
		t.Fatal(f.told)
	}

	// A rename is no news, a new date is a move.
	f.lists([3]string{"h1", "DM homework 3", "2026-10-22"}, [3]string{"h2", "Calc HW 1", "2026-10-24"},
		[3]string{"h3", "Eng essay", "2026-10-25"})
	f.refresh(t)
	if !slices.Equal(f.told, []string{"Eng essay|", "Calc HW 1|2026-10-23"}) {
		t.Fatal(f.told)
	}

	// A source's quiet ones join silently.
	id := f.add("Alg HW 2", "2026-10-24", 1)
	f.Quiet([]string{fmt.Sprintf("dl:%d", id)})
	f.add("Alg HW 3", "2026-10-26", 2)
	f.refresh(t)
	if !slices.Equal(f.told[2:], []string{"Alg HW 3|"}) {
		t.Fatal(f.told)
	}
}

func TestAMoveOfTheChatsAndACopyTakingThePlace(t *testing.T) {
	f := setup(t)
	old := f.add("Alg HW 1", "2026-10-21", 1)
	f.add("Prog HW 1", "2026-10-22", 2)
	f.refresh(t)

	newer := f.add("Alg HW 1", "2026-10-24", 3) // a new row moves the old one
	must(f.Merged.Deadlines.Move(old, newer))
	f.refresh(t)
	if !slices.Equal(f.told, []string{"Alg HW 1|2026-10-21"}) {
		t.Fatal(f.told)
	}

	// The same work on the same date in another row takes the place: no news.
	copied := f.add("Prog HW 1", "2026-10-22", 4)
	var prog int64
	for _, row := range must(f.Merged.Deadlines.Listed(todo.Date(2026, 10, 1))) {
		if row.Label == "Prog HW 1" && row.ID != copied {
			prog = row.ID
		}
	}
	must(f.Merged.Deadlines.Drop([]int64{prog}, f.Now()))
	f.refresh(t)
	if len(f.told) != 1 {
		t.Fatal(f.told)
	}
}

func TestToMidnight(t *testing.T) {
	f := setup(t)
	if got := f.toMidnight(); got != 12*time.Hour+MidnightDelay {
		t.Fatal(got)
	}
}
