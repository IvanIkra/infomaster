package tablo

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	outputs "github.com/IvanIkra/infomaster/go/internal/outputs/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

var today = todo.Date(2026, 10, 20)

const schema = `
create table subjects (name text primary key, title text not null, abbr text not null,
                       manual integer not null default 0);
create table own_deadlines (id integer primary key, title text not null, due text not null, category text not null);
create table hidden_deadlines (uid text primary key, source text not null, course text, title text not null,
                               due text not null);
`

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func course(s string) *string { return &s }

var hiddenOne = todo.Deadline{Title: "Calc HW 3", Due: todo.Date(2026, 10, 22), Category: "study", Source: "tg",
	Course: course("Calc")}

// tabloDB is Tablo's tablo.db (§9 of sources-overview.md), only the tables the move reads.
func tabloDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tablo.db")
	db := must(sql.Open("sqlite", path))
	defer db.Close()
	for _, q := range []string{schema,
		`insert into own_deadlines(title, due, category) values('Dentist', '2026-10-25', 'personal'),
			('Prog  HW 1b', '2026-10-28', 'study'), ('Long gone', '2026-08-01', 'study'), ('Odd', '2026-10-25', 'fun')`,
		`insert into hidden_deadlines values('` + todo.UID(hiddenOne) + `', 'tg', 'Calc', 'Calc HW 3', '2026-10-22'),
			('hseapp-0000000000', 'hseapp', null, 'Old HW', '2026-07-01')`,
		`insert into subjects values('Дискретная математика', 'Discrete Math', 'DM', 1), ('Алгебра', 'Algebra', 'Alg', 1),
			('Математический анализ', 'Calculus', 'Calc', 0), ('История', 'History', 'TooLongAbbr', 1)`} {
		must(db.Exec(q))
	}
	return path
}

func open(t *testing.T) *store.Store {
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestImport(t *testing.T) {
	s := open(t)
	must(s.DB.Exec("insert into subjects values('Алгебра', 'Linear Algebra', 'LA', 1)"))        // set here: wins
	must(s.DB.Exec("insert into subjects values('Дискретная математика', 'Discrete', 'D', 0)")) // Haiku's
	must(0, s.PutJSON(outputs.ShownKey, []outputs.Entry{{Ref: "dl:1", Title: "Calc HW 3", Due: "2026-10-22",
		Source: "tg", Course: course("Calc")}}))
	source := must(Open(tabloDB(t)))
	defer source.Close()
	counts := must(Import(s, source, today))
	if counts != (Counts{Own: 2, OwnSkipped: 2, Hidden: 1, HiddenSkipped: 1, Subjects: 1, SubjectsSkipped: 2}) {
		t.Fatal(counts)
	}
	var own []string
	rows := must(s.DB.Query("select id || '|' || title || '|' || due || '|' || category from own_deadlines"))
	for rows.Next() {
		var line string
		rows.Scan(&line)
		own = append(own, line)
	}
	rows.Close()
	if !slices.Equal(own, []string{"1|Dentist|2026-10-25|personal", "2|Prog HW 1b|2026-10-28|study"}) {
		t.Fatal(own)
	}
	var uid string
	var ref *string
	var n int
	must(0, s.DB.QueryRow("select uid, ref, (select count(*) from hidden_deadlines) from hidden_deadlines").
		Scan(&uid, &ref, &n))
	if uid != todo.UID(hiddenOne) || ref != nil || n != 1 {
		t.Fatal(uid, ref, n)
	}
	var subjects []string
	rows = must(s.DB.Query("select name || '|' || title || '|' || abbr || '|' || manual from subjects order by name"))
	for rows.Next() {
		var line string
		rows.Scan(&line)
		subjects = append(subjects, line)
	}
	rows.Close()
	if !slices.Equal(subjects, []string{"Алгебра|Linear Algebra|LA|1", "Дискретная математика|Discrete Math|DM|1"}) {
		t.Fatal(subjects)
	}
	// The list was shown: the copied own deadlines join it quietly.
	var shown []outputs.Entry
	must(s.GetJSON(outputs.ShownKey, &shown))
	var refs []string
	for _, e := range shown {
		refs = append(refs, e.Ref)
	}
	if !slices.Equal(refs, []string{"dl:1", "ha-1", "ha-2"}) {
		t.Fatal(refs)
	}

	// A second run adds nothing.
	if counts := must(Import(s, source, today)); counts != (Counts{OwnSkipped: 4, HiddenSkipped: 2,
		SubjectsSkipped: 3}) {
		t.Fatal(counts)
	}
	must(s.GetJSON(outputs.ShownKey, &shown))
	if len(shown) != 3 {
		t.Fatal(shown)
	}
}

func TestImportNothingShown(t *testing.T) {
	s := open(t)
	source := must(Open(tabloDB(t)))
	defer source.Close()
	must(Import(s, source, today))
	if found := must(s.GetJSON(outputs.ShownKey, &[]outputs.Entry{})); found {
		t.Fatal("shown")
	}
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	var e *Error
	if _, err := Open(filepath.Join(dir, "missing.db")); !errors.As(err, &e) ||
		!strings.Contains(err.Error(), "no such file") {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.db")
	db := must(sql.Open("sqlite", other))
	must(db.Exec("create table subjects (name text)"))
	db.Close()
	if _, err := Open(other); !errors.As(err, &e) || err.Error() != "not Tablo's database: no hidden_deadlines, "+
		"own_deadlines" {
		t.Fatal(err)
	}
	source := must(Open(tabloDB(t)))
	defer source.Close()
	if _, err := source.Exec("delete from own_deadlines"); err == nil { // read-only: Tablo's file is never changed
		t.Fatal("written")
	}
}
