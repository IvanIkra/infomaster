// Package merge makes one list of deadlines for every consumer (§5.5, §5.6, §6.4, D4, D8, D10): the port of Tablo's
// merge (processing/merge.py).
//
// Sources, the most reliable first: HSE App X (kv "hseapp.deadlines", processing/hseapp), the Midterms of RUZ (source
// "ical"), then every deadline from mail and every one from the chats as a group of its own (two posts may carry the
// same work, and it must be merged), then the owner's own (§5.4). The mail, chat and Midterm rows come from the
// `deadlines` table: in force and not older than three days, by date, mail before the chats.
//
// Tablo's rules, unchanged:
//   - the course a deadline is compared by is the abbr from the dictionary (our rows keep the course as the source
//     names it, so it is mapped here), else the first word of the title; the work is the title without that course
//     prefix;
//   - the same work: equal titles, or the same course and one work inside the other by whole words ("hw 2" is in
//     "hw 2 late", not in "hw 20" or "hw 2b"); the same deadline: the same work on the same date;
//   - merge: a deadline joins unless it is the same as one kept already; one group's items are not compared together;
//   - done: a deadline from RUZ, mail or the chats leaves when HSE App X has the same work completed within 14 days
//     (an extension moves the date in a letter while the app keeps the old one);
//   - moved (ours): for the same reason, a deadline of HSE App X leaves when mail or the chats have the same work on
//     another date within 14 days: the letter's date stands;
//   - hidden: everything but the own ones leaves when it is the same as a hidden one (ours also when it comes from the
//     same row as a hidden one: a rename does not bring it back);
//   - shown: upcoming by date, then the overdue ones newest first for 3 days (1 for mail and the chats: they cannot
//     tell that the work was handed in).
//
// The same list, in the same order, goes to GET /api/deadlines for HA and Tablo, the web app and the connector. The
// owner's changes (HA, the web app, Claude) go through here too and tell `OnChange` which deadlines they put into the
// list or moved, so they are no news to push (outputs/deadlines).
package merge

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// DoneWindowDays: an extension moves the date in a letter, while HSE App X keeps the old one.
const DoneWindowDays = 14

// OverdueDays is how long an overdue deadline stays shown.
const OverdueDays = 3

// OverdueDaysBySource: mail and the chats cannot tell that the work was handed in.
var OverdueDaysBySource = map[string]int{"mail": 1, "tg": 1}

// NewsOrder is the news sources, more reliable first when duplicates are merged.
var NewsOrder = []string{"mail", "tg"}

type Deadline = todo.Deadline

// course is the course abbreviation, or the first word of the title when there is none ('Safety HW 2').
func course(d Deadline) string {
	if d.Course != nil && *d.Course != "" {
		return pytext.Casefold(*d.Course)
	}
	first, _, _ := strings.Cut(d.Title, " ")
	return pytext.Casefold(first)
}

func squeezed(title string) string { return strings.Join(pytext.Split(pytext.Casefold(title)), " ") }

// work is the title without the course prefix, for comparing: 'Prog Midterm' -> 'midterm'.
func work(d Deadline) string {
	title, c := squeezed(d.Title), course(d)
	if strings.HasPrefix(title, c+" ") {
		title = title[len(c)+1:]
	}
	return title
}

// SameWork is the same course and the same work, whatever the date.
func SameWork(a, b Deadline) bool {
	if squeezed(a.Title) == squeezed(b.Title) {
		return true
	}
	wa, wb := work(a), work(b)
	return wa != "" && wb != "" && course(a) == course(b) && (contains(wa, wb) || contains(wb, wa))
}

// isWord is Python's \w.
func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// contains is whole words only: 'hw 2' is in 'hw 2 late', not in 'hw 20' or 'hw 2b'.
func contains(part, whole string) bool {
	for start := 0; start <= len(whole); {
		i := strings.Index(whole[start:], part)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(part)
		before, _ := utf8.DecodeLastRuneInString(whole[:i])
		after, _ := utf8.DecodeRuneInString(whole[end:])
		if (i == 0 || !isWord(before)) && (end == len(whole) || !isWord(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(whole[i:])
		start = i + max(size, 1)
	}
	return false
}

func sameDay(a, b time.Time) bool { return todo.DueText(a) == todo.DueText(b) }

// days is a - b in whole days.
func days(a, b time.Time) int {
	return int(todo.DateOf(a).Sub(todo.DateOf(b)).Hours() / 24)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Same is the same deadline: the same work on the same date.
func Same(a, b Deadline) bool { return sameDay(a.Due, b.Due) && SameWork(a, b) }

// MergeDeadlines is one list from several sources, most reliable first; a duplicate of a kept deadline is dropped.
func MergeDeadlines(sources ...[]Deadline) []Deadline {
	kept := []Deadline{}
	for _, items := range sources {
		before := kept
		for _, d := range items {
			if !slices.ContainsFunc(before, func(k Deadline) bool { return Same(d, k) }) {
				kept = append(kept, d)
			}
		}
	}
	return kept
}

func filter(deadlines []Deadline, keep func(Deadline) bool) []Deadline {
	out := []Deadline{}
	for _, d := range deadlines {
		if keep(d) {
			out = append(out, d)
		}
	}
	return out
}

// DropDone hides copies of work completed in HSE App X: same course and work, dates up to 14 days apart.
func DropDone(deadlines, done []Deadline) []Deadline {
	return filter(deadlines, func(d Deadline) bool {
		return d.Source == "hseapp" || d.Source == "ha" || !slices.ContainsFunc(done, func(x Deadline) bool {
			return abs(days(d.Due, x.Due)) <= DoneWindowDays && SameWork(d, x)
		})
	})
}

// DropMoved hides HSE App X's date of work that a letter or a chat gives another date: up to 14 days apart.
func DropMoved(deadlines []Deadline) []Deadline {
	told := filter(deadlines, func(d Deadline) bool { return slices.Contains(NewsOrder, d.Source) })
	return filter(deadlines, func(d Deadline) bool {
		return d.Source != "hseapp" || !slices.ContainsFunc(told, func(x Deadline) bool {
			return !sameDay(x.Due, d.Due) && abs(days(d.Due, x.Due)) <= DoneWindowDays && SameWork(d, x)
		})
	})
}

// Hides is Tablo's rule, the same deadline; and ours, the same row, so a renamed one stays hidden.
func Hides(h, d Deadline) bool { return Same(d, h) || h.Ref != "" && h.Ref == d.Ref }

// DropHidden hides what was deleted or marked done, also when it comes back from another source. The owner's own
// deadlines are deleted, not hidden: an edited copy of a hidden one stays.
func DropHidden(deadlines, hidden []Deadline) []Deadline {
	return filter(deadlines, func(d Deadline) bool {
		return d.Source == "ha" || !slices.ContainsFunc(hidden, func(h Deadline) bool { return Hides(h, d) })
	})
}

// OverdueDaysOf is how long a deadline stays shown past its date.
func OverdueDaysOf(d Deadline) int {
	if n, ok := OverdueDaysBySource[d.Source]; ok {
		return n
	}
	return OverdueDays
}

// Visible is upcoming by date first, then overdue ones for a few days after the due date, the newest first. `today`
// is a date (todo.Date).
func Visible(deadlines []Deadline, today time.Time) []Deadline {
	todayText := todo.DueText(today)
	upcoming := filter(deadlines, func(d Deadline) bool { return todo.DueText(d.Due) >= todayText })
	slices.SortStableFunc(upcoming, func(a, b Deadline) int {
		return strings.Compare(todo.DueText(a.Due),
			todo.DueText(b.Due))
	})
	overdue := filter(deadlines, func(d Deadline) bool {
		due := todo.DueText(d.Due)
		return todo.DueText(todo.DateOf(today).AddDate(0, 0, -OverdueDaysOf(d))) <= due && due < todayText
	})
	slices.SortStableFunc(overdue, func(a, b Deadline) int {
		return strings.Compare(todo.DueText(b.Due),
			todo.DueText(a.Due))
	})
	return append(upcoming, overdue...)
}

// DeadlineList is the merged list over our tables.
type DeadlineList struct {
	store     *store.Store
	Deadlines *deadlines.Deadlines
	HseApp    *hseapp.Deadlines
	Subjects  *subjects.Subjects
	Todo      *todo.Todo
	Now       func() time.Time
	// OnChange runs after a change by the owner: the refs it put into the shown list or moved there (no news: no push
	// quietly). It is called synchronously, so it only schedules the work.
	OnChange func(refs map[string]bool)
}

// New is the list over a store.
func New(s *store.Store, dl *deadlines.Deadlines, h *hseapp.Deadlines, sub *subjects.Subjects) *DeadlineList {
	return &DeadlineList{store: s, Deadlines: dl, HseApp: h, Subjects: sub, Todo: todo.Open(s), Now: clock.NowUTC}
}

// Today is the Moscow date, as a todo date.
func (l *DeadlineList) Today() time.Time { return todo.DateOf(clock.LocalDay(l.Now())) }

// --- the sources ----------------------------------------------------------------------

// abbrs is course name -> abbr, from one read of the dictionary.
func (l *DeadlineList) abbrs() (func(name *string) *string, error) {
	all, err := l.Subjects.All()
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	for _, n := range all {
		known[n.Name] = n.Abbr
	}
	return func(name *string) *string {
		if name == nil || *name == "" {
			return nil
		}
		if abbr, ok := known[subjects.Clean(*name)]; ok {
			return &abbr
		}
		return nil
	}, nil
}

func hseappDeadlines(rows []hseapp.Row, abbr func(*string) *string) ([]Deadline, error) {
	out := []Deadline{}
	for _, row := range rows {
		due, err := todo.ParseDate(row.Due)
		if err != nil {
			return nil, err
		}
		out = append(out, Deadline{Title: row.Label, Due: due, Category: "study", Source: "hseapp",
			Course: abbr(row.Course), Ref: "hseapp:" + row.ID, Name: row.Course})
	}
	return out, nil
}

// Sources are the merge's groups in order, and the completed ones of HSE App X.
func (l *DeadlineList) Sources() ([][]Deadline, []Deadline, error) {
	abbr, err := l.abbrs()
	if err != nil {
		return nil, nil, err
	}
	listed, err := l.Deadlines.Listed(l.Today().Add(-deadlines.Lookback))
	if err != nil {
		return nil, nil, err
	}
	var ical, news []Deadline
	for _, row := range listed {
		due, err := todo.ParseDate(row.Due)
		if err != nil {
			return nil, nil, err
		}
		d := Deadline{Title: row.Label, Due: due, Category: "study", Source: row.Source, Course: abbr(row.Course),
			Ref: "dl:" + strconv.FormatInt(row.ID, 10), Name: row.Course, Assumed: row.Assumed}
		if d.Source == "ical" {
			ical = append(ical, d)
		} else if slices.Contains(NewsOrder, d.Source) {
			news = append(news, d)
		}
	}
	slices.SortStableFunc(news, func(a, b Deadline) int {
		return slices.Index(NewsOrder, a.Source) - slices.Index(NewsOrder, b.Source)
	})
	lists, err := l.HseApp.Lists()
	if err != nil {
		return nil, nil, err
	}
	current, err := hseappDeadlines(lists.Items, abbr)
	if err != nil {
		return nil, nil, err
	}
	done, err := hseappDeadlines(lists.Done, abbr)
	if err != nil {
		return nil, nil, err
	}
	own, err := l.Todo.Own()
	if err != nil {
		return nil, nil, err
	}
	groups := [][]Deadline{current, ical}
	for _, d := range news {
		groups = append(groups, []Deadline{d})
	}
	return append(groups, own), done, nil
}

// --- the list ---------------------------------------------------------------------------

// Unhidden is merged, without the work done in HSE App X and its dates moved by a letter or a chat, the hidden ones
// still in.
func (l *DeadlineList) Unhidden() ([]Deadline, error) {
	groups, done, err := l.Sources()
	if err != nil {
		return nil, err
	}
	return DropMoved(DropDone(MergeDeadlines(groups...), done)), nil
}

// All is the final list (Tablo's hub.deadlines()): merged, without what is done in HSE App X, moved or hidden.
func (l *DeadlineList) All() ([]Deadline, error) {
	unhidden, err := l.Unhidden()
	if err != nil {
		return nil, err
	}
	hidden, err := l.Todo.Hidden()
	if err != nil {
		return nil, err
	}
	return DropHidden(unhidden, hidden), nil
}

// Visible is what the screen, HA's to-do lists and the pinned list show, in their order.
func (l *DeadlineList) Visible() ([]Deadline, error) {
	all, err := l.All()
	if err != nil {
		return nil, err
	}
	return Visible(all, l.Today()), nil
}

// Item is a deadline of GET /api/deadlines.
type Item struct {
	UID      string `json:"uid"`
	Title    string `json:"title"`
	Due      string `json:"due"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Overdue  bool   `json:"overdue"`
	Assumed  bool   `json:"assumed"`
}

// Items is GET /api/deadlines: the full list in screen order, past Tablo's 5 rows; `assumed`: a sheet's date the
// service assumed, its course's next seminar (§6.3).
func (l *DeadlineList) Items() ([]Item, error) {
	shown, err := l.Visible()
	if err != nil {
		return nil, err
	}
	today := todo.DueText(l.Today())
	out := []Item{}
	for _, d := range shown {
		due := todo.DueText(d.Due)
		out = append(out, Item{UID: todo.UID(d), Title: d.Title, Due: due, Category: d.Category, Source: d.Source,
			Overdue: due < today, Assumed: d.Assumed})
	}
	return out, nil
}

// Find is a deadline of the final list by its uid, shown or not; nil for none.
func (l *DeadlineList) Find(uid string) (*Deadline, error) {
	all, err := l.All()
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if todo.UID(d) == uid {
			return &d, nil
		}
	}
	return nil, nil
}

// IsHidden reports whether a hidden row hides the deadline.
func (l *DeadlineList) IsHidden(d Deadline) (bool, error) {
	if d.Source == "ha" {
		return false, nil
	}
	hidden, err := l.Todo.Hidden()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(hidden, func(h Deadline) bool { return Hides(h, d) }), nil
}

// --- the owner's changes ----------------------------------------------------------------

// Add is a new own deadline, of a course (its key) when the web app gives one ("" for none); a *todo.ValueError for an
// empty title or an unknown category.
func (l *DeadlineList) Add(title string, due time.Time, category, course string) (Deadline, error) {
	return change(l, func(t *todo.Todo) (Deadline, error) { return t.Add(title, due, category, course) })
}

// Remove is done or deleted: an own one is deleted, one from a source is hidden for good (why, when the web app says:
// done, not_mine, or duplicate of `kept`; "" for none).
func (l *DeadlineList) Remove(d Deadline, reason, kept string) error {
	_, err := change(l, func(t *todo.Todo) (bool, error) { return true, t.Remove(d, reason, kept) })
	return err
}

// Edit gives a new title (nil keeps it), date (zero keeps it) or list ("" keeps it); one from a source becomes an own
// copy. A *todo.ValueError for an empty title.
func (l *DeadlineList) Edit(d Deadline, title *string, due time.Time, category string) (Deadline, error) {
	return change(l, func(t *todo.Todo) (Deadline, error) { return t.Edit(d, title, due, category) })
}

// Unhide is «Вернуть» of the web app: one hidden row less.
func (l *DeadlineList) Unhide(uid string) (bool, error) {
	return change(l, func(t *todo.Todo) (bool, error) { return t.Unhide(uid) })
}

// Restore is «Restore»: every hidden row that hides this deadline goes.
func (l *DeadlineList) Restore(d Deadline) (bool, error) {
	return change(l, func(t *todo.Todo) (bool, error) {
		rows, err := t.HiddenRows()
		if err != nil {
			return false, err
		}
		removed := false
		for _, row := range rows {
			due, err := todo.ParseDate(row.Due)
			if err != nil {
				return false, err
			}
			h := Deadline{Title: row.Title, Due: due, Category: "study", Source: row.Source, Course: row.Course}
			if row.Ref != nil {
				h.Ref = *row.Ref
			}
			if !Hides(h, d) {
				continue
			}
			ok, err := t.Unhide(row.UID)
			if err != nil {
				return false, err
			}
			removed = removed || ok
		}
		return removed, nil
	})
}

// Prune removes the own and hidden rows 30 days past their date: they are off every list.
func (l *DeadlineList) Prune() error {
	today := l.Today()
	return l.store.InTx(func(tx *sql.Tx) error { return l.Todo.With(tx).Prune(today) })
}

// change runs an owner's change and cleans up the rows long past in one transaction; it tells OnChange what it
// brought into the list.
func change[T any](l *DeadlineList, action func(*todo.Todo) (T, error)) (T, error) {
	var result T
	before, err := l.shown()
	if err != nil {
		return result, err
	}
	today := l.Today()
	err = l.store.InTx(func(tx *sql.Tx) error {
		t := l.Todo.With(tx)
		var err error
		if result, err = action(t); err != nil {
			return err
		}
		return t.Prune(today)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	after, err := l.shown()
	if err != nil {
		return result, err
	}
	if l.OnChange != nil {
		refs := map[string]bool{}
		for ref, due := range after {
			if was, ok := before[ref]; !ok || was != due {
				refs[ref] = true
			}
		}
		l.OnChange(refs)
	}
	return result, nil
}

func (l *DeadlineList) shown() (map[string]string, error) {
	shown, err := l.Visible()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range shown {
		out[d.Ref] = todo.DueText(d.Due)
	}
	return out, nil
}
