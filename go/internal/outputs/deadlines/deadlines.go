// Package deadlines tells what is news in the deadlines (§6.4, D24, D69): a new or moved deadline goes to the web
// app's notifier (Web Push). The port of outputs/deadlines.py.
//
// The merged list (processing/merge: the one Tablo and HA get) is compared with the one seen last (kv
// "deadlines.shown") by the row each deadline comes from (`Deadline.Ref`), so a rename (a Midterm once the dictionary
// knows its course, an HSE App X label once Haiku answers) is no news, and a move of the chats or RUZ (a new row that
// moves the old one) is a move. A copy of the same work and date taking the place of another is no news either; a
// deadline that leaves the list is told nothing.
//
// Quiet: the first comparison (nothing seen yet) takes the list as it is, and so do the first poll of HSE App X and
// the first RUZ poll for what they bring, so a source joining floods nothing. The owner's own changes (the web app,
// Claude, HA) are no news either.
//
// Several tasks change deadlines (the batches, the RUZ and HSE App X polls, the owner): one refresh runs at a time
// under a lock and compares the list as it is then, so nothing is told twice. At midnight the past rows are pruned.
package deadlines

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// ShownKey is the list as seen last: [{ref, title, due, source, course}].
const ShownKey = "deadlines.shown"

// MidnightDelay is how long past midnight the daily look runs.
const MidnightDelay = 5 * time.Second

// Entry is a deadline as kept in ShownKey.
type Entry struct {
	Ref    string  `json:"ref"`
	Title  string  `json:"title"`
	Due    string  `json:"due"`
	Source string  `json:"source"`
	Course *string `json:"course"`
}

// EntryOf is a deadline as kept.
func EntryOf(d todo.Deadline) Entry {
	return Entry{Ref: d.Ref, Title: d.Title, Due: todo.DueText(d.Due), Source: d.Source, Course: d.Course}
}

// FromEntry is a kept deadline back, enough for merge.Same.
func FromEntry(e Entry) (todo.Deadline, error) {
	due, err := todo.ParseDate(e.Due)
	return todo.Deadline{Title: e.Title, Due: due, Category: "study", Source: e.Source, Course: e.Course, Ref: e.Ref},
		err
}

// News is a change worth telling: Was is the date it moved from, "" for a new one.
type News struct {
	Deadline todo.Deadline
	Was      string
}

// Watch compares the list and tells the notifier.
type Watch struct {
	store  *store.Store
	Merged *merge.DeadlineList
	Now    func() time.Time
	// Notify gets a new deadline (was "") or a moved one (was: its old date): the web app's notifier (§8.6).
	Notify func(d todo.Deadline, was string)

	lock  sync.Mutex // one refresh at a time
	qmu   sync.Mutex
	quiet map[string]bool
	tasks sync.WaitGroup
}

// New is the watch of a merged list.
func New(s *store.Store, merged *merge.DeadlineList) *Watch {
	return &Watch{store: s, Merged: merged, Now: clock.NowUTC, quiet: map[string]bool{}}
}

// Quiet marks deadlines a source brought as no news: the next refresh takes them into the list silently. Call it
// right after they are saved, before the next refresh can start; never while a transaction is open: Refresh reads the
// store under the same lock, so it would wait for the one connection while the caller waits for the lock.
func (w *Watch) Quiet(refs []string) {
	w.qmu.Lock()
	defer w.qmu.Unlock()
	for _, ref := range refs {
		w.quiet[ref] = true
	}
}

// Changed is merge.DeadlineList.OnChange: the owner's change is taken in, quietly.
func (w *Watch) Changed(refs map[string]bool) {
	quiet := make([]string, 0, len(refs))
	for ref := range refs {
		quiet = append(quiet, ref)
	}
	w.Quiet(quiet)
	w.tasks.Add(1)
	go func() {
		defer w.tasks.Done()
		if err := w.Refresh(); err != nil {
			slog.Error("refreshing the deadlines failed", "error", err)
		}
	}()
}

// Settle waits for the refreshes Changed started (the tests).
func (w *Watch) Settle() { w.tasks.Wait() }

// Run looks at the start (the very first look takes the list as it is), then every midnight: own and hidden rows long
// past are removed. It returns when ctx is done.
func (w *Watch) Run(ctx context.Context) {
	for {
		if err := w.Merged.Prune(); err != nil {
			slog.Error("pruning the deadlines failed", "error", err)
		} else if err := w.Refresh(); err != nil {
			slog.Error("refreshing the deadlines failed", "error", err)
		}
		timer := time.NewTimer(w.toMidnight())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Refresh compares the list with the one seen last and tells the notifier what is news.
func (w *Watch) Refresh() error {
	w.lock.Lock()
	defer w.lock.Unlock()
	// The list and the quiet ones are taken together: what is quieted later waits for the next refresh.
	w.qmu.Lock()
	shown, err := w.Merged.Visible()
	quiet := w.quiet
	if err == nil {
		w.quiet = map[string]bool{}
	}
	w.qmu.Unlock()
	if err != nil {
		return err
	}
	var before []Entry
	found, err := w.store.GetJSON(ShownKey, &before)
	if err != nil {
		return err
	}
	entries := make([]Entry, 0, len(shown))
	for _, d := range shown {
		entries = append(entries, EntryOf(d))
	}
	if err := w.store.PutJSON(ShownKey, entries); err != nil { // first: a failure is not told twice
		return err
	}
	if !found || w.Notify == nil {
		return nil
	}
	news, err := w.News(before, shown, quiet)
	if err != nil {
		return err
	}
	for _, n := range news {
		w.notify(n)
	}
	return nil
}

func (w *Watch) notify(n News) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("notifying of a deadline failed", "error", r)
		}
	}()
	w.Notify(n.Deadline, n.Was)
}

// News is every change worth telling, in list order.
func (w *Watch) News(before []Entry, shown []todo.Deadline, quiet map[string]bool) ([]News, error) {
	refs := map[string]bool{}
	for _, d := range shown {
		refs[d.Ref] = true
	}
	was := map[string]Entry{} // a shown deadline -> as it was seen last
	var lost []Entry
	for _, e := range before {
		if refs[e.Ref] {
			was[e.Ref] = e
		} else {
			lost = append(lost, e)
		}
	}
	for _, e := range append([]Entry(nil), lost...) { // a move of the chats or RUZ: the old row points at the new one
		newer, err := w.newer(e.Ref)
		if err != nil {
			return nil, err
		}
		if _, seen := was[newer]; newer != "" && refs[newer] && !seen {
			was[newer] = e
			lost = removeEntry(lost, e)
		}
	}
	var joined []todo.Deadline
	for _, d := range shown {
		if _, ok := was[d.Ref]; !ok {
			joined = append(joined, d)
		}
	}
	isNew := map[string]bool{}
	for _, d := range joined { // the same work on the same date in another row takes the place: no news
		taken := false
		for i, e := range lost {
			old, err := FromEntry(e)
			if err != nil {
				return nil, err
			}
			if merge.Same(d, old) {
				lost = append(lost[:i:i], lost[i+1:]...)
				taken = true
				break
			}
		}
		if !taken {
			isNew[d.Ref] = true
		}
	}
	result := []News{}
	for _, d := range shown {
		if quiet[d.Ref] {
			continue
		}
		if isNew[d.Ref] {
			result = append(result, News{Deadline: d})
		} else if e, ok := was[d.Ref]; ok && e.Due != todo.DueText(d.Due) { // the same date: a rename, no news
			result = append(result, News{Deadline: d, Was: e.Due})
		}
	}
	return result, nil
}

func removeEntry(list []Entry, e Entry) []Entry {
	for i, x := range list {
		if x.Ref == e.Ref && x.Title == e.Title && x.Due == e.Due && x.Source == e.Source &&
			(x.Course == nil) == (e.Course == nil) && (x.Course == nil || *x.Course == *e.Course) {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

// newer is the ref of the row that moved a row of the chats, mail or RUZ, or "".
func (w *Watch) newer(ref string) (string, error) {
	text, ok := strings.CutPrefix(ref, "dl:")
	if !ok {
		return "", nil
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return "", err
	}
	row, err := w.Merged.Deadlines.Latest(id)
	if err != nil || row == nil {
		return "", err
	}
	if newer := "dl:" + strconv.FormatInt(row.ID, 10); newer != ref {
		return newer, nil
	}
	return "", nil
}

func (w *Watch) toMidnight() time.Duration {
	local := w.Now().In(clock.TZ)
	next := local.AddDate(0, 0, 1)
	midnight := time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, clock.TZ)
	return midnight.Sub(local) + MidnightDelay
}
