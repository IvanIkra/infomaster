package merge

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = int64(-1001)

var D = todo.Date

func ptr[T any](v T) *T { return &v }

func dl(title string, due time.Time, source string, course string) Deadline {
	d := Deadline{Title: title, Due: due, Category: "study", Source: source}
	if course != "" {
		d.Course = &course
	}
	return d
}

func titles(ds []Deadline) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Title)
	}
	return out
}

func equal(a, b []Deadline) bool {
	return slices.EqualFunc(a, b, func(x, y Deadline) bool { return x.Equal(y) && x.Key == y.Key })
}

// --- Tablo's rules, case by case (its tests/test_deadlines.py) -------------------------------

func TestCompletedInHseappHidesCopyWithExtendedDate(t *testing.T) {
	done := []Deadline{dl("Safety HW 2", D(2026, 10, 13), "hseapp", "")} // the app keeps the first date
	mail := dl("Safety HW 2", D(2026, 10, 15), "mail", "")               // the letter carries the extension
	if got := DropDone([]Deadline{mail}, done); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestCompletedMatchesByCourseAndWork(t *testing.T) {
	done := []Deadline{dl("DM HW 3", D(2026, 10, 15), "hseapp", "DM")}
	if got := DropDone([]Deadline{dl("DM HW 3 late", D(2026, 10, 18), "tg", "DM")}, done); len(got) != 0 {
		t.Fatal(got)
	}
	kept := []Deadline{dl("DM HW 30", D(2026, 10, 18), "tg", "DM"), // another work
		dl("DM HW 3B", D(2026, 10, 18), "tg", "DM"),
		dl("Calc HW 3", D(2026, 10, 15), "tg", "Calc"), // another course
		dl("DM HW 3", D(2026, 11, 2), "tg", "DM")}      // more than 14 days apart: next term's HW 3
	if got := DropDone(kept, done); !equal(got, kept) {
		t.Fatal(got)
	}
}

func TestCompletedNeverHidesHseappOrOwnItems(t *testing.T) {
	item := dl("DM HW 3", D(2026, 10, 15), "hseapp", "DM")
	own := Deadline{Title: "DM HW 3", Due: D(2026, 10, 15), Category: "study", Source: "ha", Key: "ha-1"}
	if got := DropDone([]Deadline{item, own}, []Deadline{item}); !equal(got, []Deadline{item, own}) {
		t.Fatal(got)
	}
}

func TestALetterOrAChatMovesTheDateOfHseapp(t *testing.T) {
	app := dl("Safety HW 3", D(2026, 11, 16), "hseapp", "")
	mail := dl("Safety HW 3", D(2026, 11, 20), "mail", "Safety")
	tg := dl("DM HW 3 late", D(2026, 10, 18), "tg", "DM")
	kept := []Deadline{dl("Calc HW 3", D(2026, 11, 16), "hseapp", "Calc"),
		dl("DM HW 3", D(2026, 11, 2), "hseapp", "DM"),
		dl("Safety HW 3", D(2026, 11, 20), "ical", "")} // RUZ does not move it
	if got := DropMoved(append([]Deadline{app, mail}, kept...)); !equal(got, append([]Deadline{mail}, kept...)) {
		t.Fatal(got)
	}
	if got := DropMoved([]Deadline{dl("DM HW 3", D(2026, 10, 15), "hseapp", "DM"), tg}); !equal(got, []Deadline{tg}) {
		t.Fatal(got)
	}
	own := Deadline{Title: "Safety HW 3", Due: D(2026, 11, 20), Category: "study", Source: "ha", Key: "ha-1"}
	if got := DropMoved([]Deadline{app, own}); !equal(got, []Deadline{app, own}) { // the owner's own is no letter
		t.Fatal(got)
	}
}

func TestMergeStillNeedsTheSameDate(t *testing.T) {
	a, b := dl("Calc KR-1", D(2026, 10, 22), "hseapp", ""), dl("Calc KR-1", D(2026, 10, 23), "tg", "")
	if got := MergeDeadlines([]Deadline{a}, []Deadline{b}); !equal(got, []Deadline{a, b}) {
		t.Fatal(got)
	}
	oneWord := dl("Midterm", D(2026, 10, 22), "ical", "")
	if got := MergeDeadlines([]Deadline{oneWord}, []Deadline{dl("Exam", D(2026, 10, 22), "mail", "")}); got[1].Title !=
		"Exam" {
		t.Fatal(got)
	}
}

func TestMergeKeepsTheMoreReliableAndOneGroupWhole(t *testing.T) {
	hse := dl("DM HW 3", D(2026, 10, 22), "hseapp", "DM")
	tg := dl("dm  hw 3", D(2026, 10, 22), "tg", "")         // the title alone: case and spaces do not matter
	late := dl("DM HW 3 late", D(2026, 10, 22), "tg", "DM") // the work inside the other one, by words
	twins := []Deadline{dl("Calc HW 5", D(2026, 10, 22), "ical", ""), dl("Calc HW 5", D(2026, 10, 22), "ical", "")}
	if got := MergeDeadlines([]Deadline{hse}, []Deadline{tg}, []Deadline{late}, twins); !equal(got,
		append([]Deadline{hse}, twins...)) { // inside a group: not compared
		t.Fatal(got)
	}
}

func TestHiddenAlsoFromAnotherSourceButNeverOwn(t *testing.T) {
	hidden := []Deadline{dl("Calc HW 3", D(2026, 10, 21), "mail", "Calc")}
	tg := dl("Calc HW 3", D(2026, 10, 21), "tg", "Calc")
	otherDay := dl("Calc HW 3", D(2026, 10, 22), "tg", "Calc")
	own := Deadline{Title: "Calc HW 3", Due: D(2026, 10, 21), Category: "study", Source: "ha", Key: "ha-2"}
	if got := DropHidden([]Deadline{tg, otherDay, own}, hidden); !equal(got, []Deadline{otherDay, own}) {
		t.Fatal(got)
	}
}

func TestMailAndTelegramOverdueForOneDay(t *testing.T) {
	today := D(2026, 10, 18)
	shown := Visible([]Deadline{
		dl("Mail yesterday", D(2026, 10, 17), "mail", ""),
		dl("Mail 2 days ago", D(2026, 10, 16), "mail", ""),
		dl("TG 2 days ago", D(2026, 10, 16), "tg", ""),
		dl("HSE 3 days ago", D(2026, 10, 15), "hseapp", ""),
		dl("HSE 4 days ago", D(2026, 10, 14), "hseapp", ""),
		dl("Later", D(2026, 10, 25), "tg", ""),
		dl("Today", today, "ical", ""),
	}, today)
	if got := titles(shown); !slices.Equal(got, []string{"Today", "Later", "Mail yesterday", "HSE 3 days ago"}) {
		t.Fatal(got)
	}
}

func TestContainsByWholeWords(t *testing.T) {
	for _, c := range []struct {
		part, whole string
		want        bool
	}{{"hw 2", "hw 2 late", true}, {"hw 2", "hw 20", false}, {"hw 2", "hw 2b", false}, {"hw 2", "hw 22 hw 2", true},
		{"дз 2", "дз 2б", false}, {"дз 2", "старое дз 2", true}, {"hw_2", "hw_2", true}, {"2", "a_2", false}} {
		if got := contains(c.part, c.whole); got != c.want {
			t.Errorf("%q in %q: %v", c.part, c.whole, got)
		}
	}
}

func TestUID(t *testing.T) {
	item := dl("DM  HW 5", D(2026, 10, 22), "hseapp", "")
	sum := sha1.Sum([]byte("hseapp|dm hw 5|2026-10-22"))
	expected := "hseapp-" + hex.EncodeToString(sum[:])[:10]
	if todo.UID(item) != expected || todo.UID(dl("dm hw 5", D(2026, 10, 22), "hseapp", "")) != expected {
		t.Fatal(todo.UID(item))
	}
	if todo.UID(Deadline{Title: "Dentist", Due: D(2026, 10, 25), Category: "personal", Source: "ha",
		Key: "ha-7"}) != "ha-7" {
		t.Fatal("own")
	}
}

func TestConstantsAreTablos(t *testing.T) {
	if DoneWindowDays != 14 || OverdueDays != 3 || len(OverdueDaysBySource) != 2 || OverdueDaysBySource["mail"] != 1 ||
		OverdueDaysBySource["tg"] != 1 {
		t.Fatal("constants")
	}
}

// --- the list over our tables ------------------------------------------------------------------

type clockT struct{ moment time.Time }

func (c *clockT) now() time.Time { return c.moment }

type fixture struct {
	*DeadlineList
	store   *store.Store
	clock   *clockT
	changes []map[string]bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clockT{moment: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
	sub := subjects.New(s, &llm.Fake{})
	h := hseapp.New(s, sub, &llm.Fake{})
	h.Now = c.now
	f := &fixture{DeadlineList: New(s, deadlines.Open(s), h, sub), store: s, clock: c}
	f.Now = c.now
	f.OnChange = func(refs map[string]bool) { f.changes = append(f.changes, refs) }
	return f
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) add(label, due, source, course string, msgID int64) int64 {
	n := deadlines.New{Source: source, Course: course, Label: label, Due: due}
	if source != "ical" {
		n.ChatID, n.MsgIDs = ptr(chat), []int64{msgID}
	}
	return must(f.Deadlines.Add(n, f.Now()))
}

type hrow struct{ id, label, due, course string }

func (f *fixture) hseapp(items, done []hrow) {
	rows := func(raws []hrow) []hseapp.Row {
		out := []hseapp.Row{}
		for _, r := range raws {
			row := hseapp.Row{ID: r.id, Label: r.label, Due: r.due, Title: "Русское " + r.label}
			if r.course != "" {
				row.Course = ptr(r.course)
			}
			out = append(out, row)
		}
		return out
	}
	must(0, f.store.PutJSON(hseapp.ListsKey, hseapp.Lists{FetchedAt: ptr("2026-10-20T12:00:00"), Items: rows(items),
		Done: rows(done)}))
}

func (f *fixture) named() *fixture {
	for _, n := range [][3]string{{"Дискретная математика", "Discrete Math", "DM"},
		{"Безопасность жизнедеятельности", "Life Safety", "Safety"},
		{"Основы российской государственности", "Rus Statehood", "RSF"}} {
		must(f.Subjects.SetManual(n[0], n[1], n[2]))
	}
	return f
}

func (f *fixture) all() []Deadline     { return must(f.All()) }
func (f *fixture) visible() []Deadline { return must(f.Visible()) }

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestSourcesInTheirOrder(t *testing.T) {
	f := setup(t).named()
	f.hseapp([]hrow{{"h1", "DM HW 3", "2026-10-22", "Дискретная математика"},
		{"h2", "Записаться на майнор", "2026-10-30", ""}},
		[]hrow{{"h0", "Safety HW 2", "2026-10-13", "Безопасность жизнедеятельности"}})
	f.add("DM HW 3", "2026-10-22", "tg", "Дискретная математика", 1) // the same as HSE App X's
	f.add("Calc KR-1", "2026-10-23", "tg", "матан", 1)               // a course the dictionary does not know
	mailCalc := f.add("Calc KR-1", "2026-10-23", "mail", "Математический анализ", 1)
	f.add("Calc KR-1", "2026-10-23", "tg", "Математический анализ", 2) // a second post of it
	f.add("Safety HW 2", "2026-10-15", "mail", "", 1)                  // done in HSE App X, the date extended
	midterm := f.add("RSF Midterm", "2026-10-26", "ical", "Основы российской государственности", 1)
	f.add("Old HW", "2026-10-16", "tg", "", 1) // past the three days the table is read for
	moved := f.add("Alg HW 1", "2026-10-21", "tg", "", 1)
	newer := f.add("Alg HW 1", "2026-10-24", "tg", "", 1)
	gone := f.add("Hist essay", "2026-10-25", "tg", "", 1)
	must(f.Deadlines.Move(moved, newer))
	must(f.Deadlines.Drop([]int64{gone}, f.Now()))
	own := must(f.Add("Dentist", D(2026, 10, 21), "personal", ""))

	final := f.all()
	var got []string
	for _, d := range final {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%s", d.Title, d.Source, todo.DueText(d.Due), str(d.Course), d.Ref))
	}
	want := []string{"DM HW 3|hseapp|2026-10-22|DM|hseapp:h1", "Записаться на майнор|hseapp|2026-10-30|<nil>|hseapp:h2",
		fmt.Sprintf("RSF Midterm|ical|2026-10-26|RSF|dl:%d", midterm),
		fmt.Sprintf("Calc KR-1|mail|2026-10-23|<nil>|dl:%d", mailCalc), // mail first, though the chat's came first
		fmt.Sprintf("Alg HW 1|tg|2026-10-24|<nil>|dl:%d", newer), "Dentist|ha|2026-10-21|<nil>|" + own.Key}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if str(final[0].Name) != "Дискретная математика" || str(final[3].Name) != "Математический анализ" {
		t.Fatal("names")
	}
	if got := titles(f.visible()); !slices.Equal(got, []string{"Dentist", "DM HW 3", "Calc KR-1", "Alg HW 1",
		"RSF Midterm", "Записаться на майнор"}) {
		t.Fatal(got)
	}
	items := must(f.Items())
	if items[0] != (Item{UID: "ha-1", Title: "Dentist", Due: "2026-10-21", Category: "personal", Source: "ha"}) {
		t.Fatal(items[0])
	}
	found := must(f.Find(todo.UID(final[0])))
	if items[1].UID != todo.UID(final[0]) || found == nil || !found.Equal(final[0]) {
		t.Fatal("find")
	}
	if must(f.Find("tg-0000000000")) != nil {
		t.Fatal("find none")
	}
}

func TestTheLettersDateStandsForHseapps(t *testing.T) {
	f := setup(t).named()
	f.hseapp([]hrow{{"h3", "Safety HW 3", "2026-11-16", ""}}, nil)
	letter := f.add("Safety HW 3", "2026-11-20", "mail", "Безопасность жизнедеятельности", 1)
	all := f.all()
	if len(all) != 1 || all[0].Source != "mail" || todo.DueText(all[0].Due) != "2026-11-20" ||
		all[0].Ref != fmt.Sprintf("dl:%d", letter) {
		t.Fatal(all)
	}
}

func TestCourseWithoutTheDictionaryIsTheFirstWord(t *testing.T) {
	f := setup(t)
	f.hseapp(nil, []hrow{{"h0", "Calc HW 2", "2026-10-13", "Математический анализ"}})
	f.add("Calc HW 2", "2026-10-21", "tg", "матанализ", 1) // within 14 days of the completed one
	if all := f.all(); len(all) != 0 {
		t.Fatal(all)
	}
}

func TestOverdueRowsLeaveTheList(t *testing.T) {
	f := setup(t)
	f.add("TG yesterday", "2026-10-19", "tg", "", 1)
	f.add("TG two days ago", "2026-10-18", "tg", "", 1)
	f.add("Midterm 3 days ago", "2026-10-17", "ical", "", 1)
	f.hseapp([]hrow{{"h1", "HSE 3 days ago", "2026-10-17", ""}, {"h2", "HSE 4 days ago", "2026-10-16", ""}}, nil)
	var got []string
	for _, item := range must(f.Items()) {
		got = append(got, fmt.Sprintf("%s|%v", item.Title, item.Overdue))
	}
	if !slices.Equal(got, []string{"TG yesterday|true", "HSE 3 days ago|true", "Midterm 3 days ago|true"}) {
		t.Fatal(got)
	}
}

// --- the owner's changes (HA's to-do lists, the web app, Claude) ---------------------------

func refs(keys ...string) map[string]bool {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func sameRefs(a, b map[string]bool) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func pick(t *testing.T, ds []Deadline, ref string) Deadline {
	t.Helper()
	for _, d := range ds {
		if d.Ref == ref {
			return d
		}
	}
	t.Fatal("no " + ref)
	return Deadline{}
}

func TestOwnAndHidden(t *testing.T) {
	f := setup(t)
	calc := f.add("Calc HW 3", "2026-10-21", "tg", "Математический анализ", 1)
	prog := f.add("Prog HW 1", "2026-10-18", "tg", "", 1)
	own := must(f.Add("Dentist", D(2026, 10, 25), "personal", ""))
	if own.Key != "ha-1" || len(f.changes) != 1 || !sameRefs(f.changes[0], refs("ha-1")) {
		t.Fatal(own, f.changes)
	}
	var ve *todo.ValueError
	if _, err := f.Add("  ", D(2026, 10, 25), "personal", ""); !errors.As(err, &ve) ||
		!strings.Contains(err.Error(), "title is empty") {
		t.Fatal(err)
	}
	if _, err := f.Add("Gym", D(2026, 10, 25), "sport", ""); err == nil || !strings.Contains(err.Error(), "category") {
		t.Fatal(err)
	}
	if len(must(f.Todo.Own())) != 1 {
		t.Fatal("own")
	}

	// Done hides it for good, also when it comes back from another chat.
	item := pick(t, f.all(), fmt.Sprintf("dl:%d", calc))
	noErr(t, f.Remove(item, "", ""))
	if !sameRefs(f.changes[len(f.changes)-1], refs()) || !must(f.IsHidden(item)) {
		t.Fatal("hidden")
	}
	rows := must(f.Todo.HiddenRows())
	if len(rows) != 1 || rows[0].UID != todo.UID(item) || rows[0].Source != "tg" || rows[0].Course != nil ||
		rows[0].Title != "Calc HW 3" || rows[0].Due != "2026-10-21" || str(rows[0].Ref) != fmt.Sprintf("dl:%d", calc) ||
		rows[0].Reason != nil || rows[0].Kept != nil {
		t.Fatal(rows)
	}
	f.add("Calc HW 3", "2026-10-21", "tg", "Математический анализ", 9)
	if got := titles(f.all()); !slices.Equal(got, []string{"Prog HW 1", "Dentist"}) {
		t.Fatal(got)
	}

	// Editing one from a source: hidden, an own copy with the new date.
	old := pick(t, f.all(), fmt.Sprintf("dl:%d", prog))
	cp := must(f.Edit(old, nil, D(2026, 10, 29), ""))
	if cp.Source != "ha" || cp.Key != "ha-2" || cp.Title != "Prog HW 1" ||
		!sameRefs(f.changes[len(f.changes)-1], refs("ha-2")) {
		t.Fatal(cp, f.changes)
	}
	edited := must(f.Edit(own, ptr("Dentist 10:00"), time.Time{}, ""))
	if edited.Key != own.Key || !edited.Due.Equal(own.Due) || !sameRefs(f.changes[len(f.changes)-1], refs()) {
		t.Fatal("a rename only")
	}
	if must(f.Edit(edited, nil, time.Time{}, "study")).Category != "study" {
		t.Fatal("study")
	}
	edited = must(f.Edit(edited, nil, time.Time{}, "personal"))
	if edited.Category != "personal" || must(f.Todo.Own())[0].Category != "personal" {
		t.Fatal("personal")
	}
	var got []string
	for _, d := range f.all() {
		got = append(got, d.Title+"|"+d.Source+"|"+todo.DueText(d.Due))
	}
	if !slices.Equal(got, []string{"Dentist 10:00|ha|2026-10-25", "Prog HW 1|ha|2026-10-29"}) {
		t.Fatal(got)
	}

	// «Вернуть»: by the hidden row (the web app) or by the deadline (Claude).
	if !must(f.Unhide(todo.UID(item))) || must(f.Unhide(todo.UID(item))) {
		t.Fatal("unhide")
	}
	if !slices.ContainsFunc(f.all(), func(d Deadline) bool { return d.Ref == fmt.Sprintf("dl:%d", calc) }) {
		t.Fatal("back")
	}
	noErr(t, f.Remove(item, "", ""))
	if !must(f.Restore(item)) || must(f.Restore(item)) || must(f.IsHidden(item)) {
		t.Fatal("restore")
	}
	noErr(t, f.Remove(cp, "", ""))
	noErr(t, f.Remove(edited, "", ""))
	if len(must(f.Todo.Own())) != 0 {
		t.Fatal("own left")
	}
}

func TestARenamedOneStaysHidden(t *testing.T) {
	f := setup(t)
	f.hseapp([]hrow{{"h1", "ДЗ 3 по ДМ", "2026-10-22", "Дискретная математика"}}, nil)
	midterm := f.add("Foundations Midterm", "2026-10-26", "ical", "Foundations of Russian Statehood", 1)
	for _, d := range f.all() {
		noErr(t, f.Remove(d, "", ""))
	}
	f.hseapp([]hrow{{"h1", "DM HW 3", "2026-10-22", "Дискретная математика"}}, nil)
	noErr(t, f.Deadlines.Rename(midterm, "RSF Midterm", ptr("Foundations of Russian Statehood")))
	if len(f.all()) != 0 {
		t.Fatal(f.all())
	}
	var renamed Deadline
	for _, d := range must(f.Unhidden()) {
		if d.Source == "hseapp" {
			renamed = d
		}
	}
	if !must(f.IsHidden(renamed)) || !must(f.Restore(renamed)) || !slices.Equal(titles(f.all()), []string{"DM HW 3"}) {
		t.Fatal("restore")
	}
}

func TestRowsLongPastArePruned(t *testing.T) {
	f := setup(t)
	f.add("Old HW", "2026-10-21", "tg", "", 1)
	all := f.all()
	noErr(t, f.Remove(all[0], "", ""))
	must(f.Add("Old own", D(2026, 10, 21), "study", ""))
	f.clock.moment = f.clock.moment.AddDate(0, 0, 31) // Nov 20: 30 days past is not yet more than 30
	must(f.Add("New own", D(2026, 11, 25), "study", ""))
	if len(must(f.Todo.HiddenRows())) != 1 || len(must(f.Todo.Own())) != 2 {
		t.Fatal("kept")
	}
	f.clock.moment = f.clock.moment.AddDate(0, 0, 1)
	must(f.Add("Newer own", D(2026, 11, 26), "study", "")) // every change cleans up
	if len(must(f.Todo.HiddenRows())) != 0 || !slices.Equal(titles(must(f.Todo.Own())), []string{"New own",
		"Newer own"}) {
		t.Fatal("pruned")
	}
}

func TestPruneAlone(t *testing.T) {
	f := setup(t)
	must(f.Add("Old own", D(2026, 10, 21), "study", ""))
	f.clock.moment = f.clock.moment.AddDate(0, 0, 32)
	noErr(t, f.Prune())
	if len(must(f.Todo.Own())) != 0 {
		t.Fatal("own")
	}
}

func TestAFailedChangeSavesNothing(t *testing.T) {
	f := setup(t)
	f.changes = nil
	_, err := change(f.DeadlineList, func(t *todo.Todo) (bool, error) {
		if _, err := t.Add("x", D(2026, 10, 21), "study", ""); err != nil {
			return false, err
		}
		return false, errors.New("broken")
	})
	if err == nil || len(must(f.Todo.Own())) != 0 || len(f.changes) != 0 {
		t.Fatal(err, f.changes)
	}
}

func TestLatestSurvivesALoop(t *testing.T) {
	f := setup(t)
	a, b := f.add("A", "2026-10-21", "tg", "", 1), f.add("A", "2026-10-22", "tg", "", 1)
	noErr(t, f.store.InTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("update deadlines set moved_by = ? where id = ?", b, a); err != nil {
			return err
		}
		_, err := tx.Exec("update deadlines set moved_by = ? where id = ?", a, b)
		return err
	}))
	latest := must(f.Deadlines.Latest(a))
	if latest == nil || (latest.ID != a && latest.ID != b) || must(f.Deadlines.Latest(999)) != nil {
		t.Fatal(latest)
	}
}
