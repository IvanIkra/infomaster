// Package tablo is the move from Tablo (stage 10, the checklist of decisions.md): what only Tablo's database holds
// comes here once. The port of infomaster/tablo.py.
//
// `import-tablo --tablo-db <tablo.db>` copies, from a copy of Tablo's SQLite file (§9 of sources-overview.md; the
// service is stopped, the file is opened read-only):
//   - the own deadlines of HA's to-do lists (`own_deadlines`): those not here yet (the same title, date and list), so a
//     second run adds nothing. HA reads the list anew after the switch, so the new ids (ha-<id>) do not matter;
//   - the hidden ones (`hidden_deadlines`): the uid is made the same way here (source, title, date), so they are kept
//     by it; Tablo did not know the row a hidden deadline came from, so it hides by title and date only, as in Tablo;
//   - the dictionary entries set by hand (`subjects` with `manual`; Tablo keeps them in the table in step with its
//     config): one set by hand here already wins, Haiku's answer here gives way.
//
// Rows 30 days past their date are skipped: the next change of the lists would remove them anyway.
//
// The copied own deadlines are no news to push: when a list was seen already, they are added to it (kv
// "deadlines.shown"), as the owner's changes are quiet (outputs/deadlines).
package tablo

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	outputs "github.com/IvanIkra/infomaster/go/internal/outputs/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
	_ "modernc.org/sqlite"
)

// Error is a file that is not Tablo's database.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Open opens Tablo's database read-only, checking it has the tables the move reads.
func Open(path string) (*sql.DB, error) {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, &Error{"no such file: " + path}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("select name from sqlite_master where type = 'table'")
	if err != nil {
		db.Close()
		return nil, err
	}
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		tables[name] = true
	}
	rows.Close()
	var missing []string
	for _, name := range []string{"hidden_deadlines", "own_deadlines", "subjects"} {
		if !tables[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		db.Close()
		return nil, &Error{"not Tablo's database: no " + strings.Join(missing, ", ")}
	}
	return db, nil
}

// Counts are how many of each were added and skipped.
type Counts struct {
	Own             int `json:"own"`
	OwnSkipped      int `json:"own_skipped"`
	Hidden          int `json:"hidden"`
	HiddenSkipped   int `json:"hidden_skipped"`
	Subjects        int `json:"subjects"`
	SubjectsSkipped int `json:"subjects_skipped"`
}

// text is Python's str() of a column value.
func text(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		if v {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}

// isDate is date.fromisoformat for the form Tablo writes, YYYY-MM-DD.
func isDate(value string) bool {
	_, err := todo.ParseDate(value)
	return err == nil
}

func squeeze(s string) string { return strings.Join(pytext.Split(s), " ") }

func readAll(db *sql.DB, query string, columns int) ([][]any, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		values := make([]any, columns)
		pointers := make([]any, columns)
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		out = append(out, values)
	}
	return out, rows.Err()
}

func affected(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// Import copies what is not here yet, in one transaction; `today` is the Moscow date.
func Import(s *store.Store, tablo *sql.DB, today time.Time) (Counts, error) {
	since := todo.DueText(todo.DateOf(today).AddDate(0, 0, -todo.KeepDays))
	var counts Counts
	own, err := readAll(tablo, "select title, due, category from own_deadlines order by due, id", 3)
	if err != nil {
		return counts, err
	}
	hidden, err := readAll(tablo, "select uid, source, course, title, due from hidden_deadlines", 5)
	if err != nil {
		return counts, err
	}
	named, err := readAll(tablo, "select name, title, abbr from subjects where manual order by name", 3)
	if err != nil {
		return counts, err
	}
	err = s.InTx(func(tx *sql.Tx) error {
		counts = Counts{}
		var added []todo.Deadline
		for _, row := range own {
			title, due := squeeze(text(row[0])), text(row[1])
			category, _ := row[2].(string)
			if b, ok := row[2].([]byte); ok {
				category = string(b)
			}
			skip := title == "" || !slices.Contains(todo.Categories, category) || !isDate(due) || due < since
			if !skip {
				var one int
				err := tx.QueryRow("select 1 from own_deadlines where title = ? and due = ? and category = ?",
					title, due, category).Scan(&one)
				switch {
				case err == nil:
					skip = true
				case !errors.Is(err, sql.ErrNoRows):
					return err
				}
			}
			if skip {
				counts.OwnSkipped++
				continue
			}
			result, err := tx.Exec("insert into own_deadlines(title, due, category) values(?, ?, ?)", title, due,
				category)
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			key := todo.OwnPrefix + strconv.FormatInt(id, 10)
			day, _ := todo.ParseDate(due)
			added = append(added, todo.Deadline{Title: title, Due: day, Category: category, Source: "ha", Key: key,
				Ref: key})
			counts.Own++
		}

		for _, row := range hidden {
			due := text(row[4])
			if !isDate(due) || due < since {
				counts.HiddenSkipped++
				continue
			}
			ok, err := affected(tx.Exec("insert or ignore into hidden_deadlines(uid, source, course, title, due) "+
				"values(?, ?, ?, ?, ?)", row[0], row[1], row[2], row[3], due))
			if err != nil {
				return err
			}
			if ok {
				counts.Hidden++
			} else {
				counts.HiddenSkipped++
			}
		}

		for _, row := range named {
			name, title, abbr := subjects.Clean(text(row[0])), squeeze(text(row[1])), pytext.Strip(text(row[2]))
			ok := name != "" && title != "" && pytext.Len(title) <= subjects.ManualTitleChars && abbr != "" &&
				pytext.Len(abbr) <= subjects.AbbrChars
			if ok {
				ok, err = affected(tx.Exec("insert into subjects(name, title, abbr, manual) values(?, ?, ?, 1) "+
					"on conflict(name) do update set title = excluded.title, abbr = excluded.abbr, manual = 1 "+
					"where not subjects.manual", name, title, abbr))
				if err != nil {
					return err
				}
			}
			if ok {
				counts.Subjects++
			} else {
				counts.SubjectsSkipped++
			}
		}

		// kv "deadlines.shown" through the transaction: Store.GetJSON/PutJSON use the one connection it holds.
		var value string
		err := tx.QueryRow("select value from kv where key = ?", outputs.ShownKey).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) || len(added) == 0 { // nothing shown yet: the first refresh is quiet anyway
			return nil
		}
		if err != nil {
			return err
		}
		var shown []outputs.Entry
		if err := pyjson.Unmarshal([]byte(value), &shown); err != nil {
			return err
		}
		if shown == nil { // a stored null is no list seen
			return nil
		}
		for _, d := range added {
			shown = append(shown, outputs.EntryOf(d))
		}
		encoded, err := pyjson.Dumps(shown)
		if err != nil {
			return err
		}
		_, err = tx.Exec("insert into kv(key, value) values(?, ?) on conflict(key) do update set value = excluded.value",
			outputs.ShownKey, encoded)
		return err
	})
	return counts, err
}
