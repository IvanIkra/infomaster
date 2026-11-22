package web

// The «Check» queue of the web app (§8.6, stage 16.3): only what the service is unsure of, one question each. The
// port of web/check.py.
//
//   - an assumed date: a sheet's date the service took from its course's next seminar (§6.3). «Right» confirms it, or
//     the owner gives the real one;
//   - a likely duplicate: two deadlines of one course that the merge (§5.5) kept apart, though they look like one
//     work: the same work on dates up to a week apart, or the same number in both titles on such dates («Sheet 12» of
//     the wiki and «HW 12» of the chat). «Merge» hides one of them as a duplicate of the other: the one with a real
//     date stays over an assumed one, then the more reliable source (HSE App X, RUZ, mail, the chats, the owner's
//     own);
//   - a deadline without a course: a chat or a letter named none. The owner picks the course, or leaves it outside
//     the courses.
//
// A question put aside («They are different», «Outside the courses») is kept by its id (`web_checked`) and not asked
// again; its id holds the deadlines' uids, which change with a title or a date, so a changed deadline is asked about
// anew. Upcoming deadlines only: an overdue one is past checking.

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// CheckSchema is the table of the questions put aside.
const CheckSchema = `
create table if not exists web_checked (  -- questions of the «Check» queue the owner put aside (§8.6)
    id text primary key,        -- dup:<uid>|<uid>, course:<uid>, assumed:<uid>
    at text not null            -- local time
);
`

// DuplicateDays is how close a likely duplicate's dates are at most.
const DuplicateDays = 7

// Trust is the merge's order: the more reliable source stays.
var Trust = []string{"hseapp", "ical", "mail", "tg", "ha"}

var number = regexp.MustCompile(`\p{Nd}+`)

func numbers(title string) map[string]bool {
	out := map[string]bool{}
	for _, n := range number.FindAllString(title, -1) {
		n = strings.TrimLeft(n, "0")
		if n == "" {
			n = "0"
		}
		out[n] = true
	}
	return out
}

// LikelySame: two deadlines of one course that may be one work (see the package's doc). Two of HSE App X or two
// Midterms are never one: those sources list each work once.
func LikelySame(a, b todo.Deadline) bool {
	if a.Source == b.Source && (a.Source == "hseapp" || a.Source == "ical") {
		return false
	}
	if dayDistance(a.Due, b.Due) > DuplicateDays {
		return false
	}
	if merge.SameWork(a, b) {
		return true
	}
	other := numbers(b.Title)
	for n := range numbers(a.Title) {
		if other[n] {
			return true
		}
	}
	return false
}

// Stays is the one of two duplicates to keep: a real date over an assumed one, then the more reliable source.
func Stays(a, b todo.Deadline) todo.Deadline {
	rank := func(d todo.Deadline) [2]int {
		assumed := 0
		if d.Assumed {
			assumed = 1
		}
		trust := slices.Index(Trust, d.Source)
		if trust < 0 {
			trust = len(Trust)
		}
		return [2]int{assumed, trust}
	}
	ra, rb := rank(a), rank(b)
	if ra[0] < rb[0] || ra[0] == rb[0] && ra[1] <= rb[1] {
		return a
	}
	return b
}

// Check is the queue.
type Check struct {
	Data  *Data
	Store *store.Store
}

// NewCheck makes the table.
func NewCheck(data *Data, s *store.Store) (*Check, error) {
	if _, err := s.DB.Exec(CheckSchema); err != nil {
		return nil, err
	}
	return &Check{Data: data, Store: s}, nil
}

func (c *Check) putAside() (map[string]bool, error) {
	rows, err := c.Store.DB.Query("select id from web_checked")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Items are the open questions, the soonest deadline first; of one course's lens only, unless course is "".
func (c *Check) Items(course string) ([]httpx.Object, error) {
	data := c.Data
	today := dateOf(data.Now())
	aside, err := c.putAside()
	if err != nil {
		return nil, err
	}
	visible, err := data.Merged.Visible()
	if err != nil {
		return nil, err
	}
	ahead := []todo.Deadline{}
	for _, d := range visible {
		if todo.DueText(d.Due) >= today {
			ahead = append(ahead, d)
		}
	}
	keys := map[string]string{}
	for _, d := range ahead {
		key, err := data.DeadlineKey(d)
		if err != nil {
			return nil, err
		}
		keys[todo.UID(d)] = key
	}
	type question struct {
		due    string
		course string
		object httpx.Object
	}
	found := []question{}
	item := func(d todo.Deadline) (*DeadlineItem, error) { return data.DeadlineItem(d, today) }
	paired := map[string]bool{}
	for i, a := range ahead {
		for _, b := range ahead[i+1:] {
			ua, ub := todo.UID(a), todo.UID(b)
			if paired[ua] || paired[ub] || keys[ua] == "" || keys[ua] != keys[ub] {
				continue
			}
			pair := []string{ua, ub}
			sort.Strings(pair)
			id := "dup:" + strings.Join(pair, "|")
			if aside[id] || !LikelySame(a, b) {
				continue
			}
			paired[ua], paired[ub] = true, true
			keep := Stays(a, b)
			second := a
			if todo.UID(keep) == ua {
				second = b
			}
			first, err := item(keep)
			if err != nil {
				return nil, err
			}
			other, err := item(second)
			if err != nil {
				return nil, err
			}
			due := min(todo.DueText(a.Due), todo.DueText(b.Due))
			found = append(found, question{due, keys[ua], httpx.Object{{Key: "id", Value: id},
				{Key: "kind", Value: "duplicate"}, {Key: "course", Value: keys[ua]}, {Key: "keep", Value: todo.UID(keep)},
				{Key: "items", Value: []*DeadlineItem{first, other}}}})
		}
	}
	for _, d := range ahead {
		uid := todo.UID(d)
		if d.Assumed && !aside["assumed:"+uid] {
			shown, err := item(d)
			if err != nil {
				return nil, err
			}
			found = append(found, question{todo.DueText(d.Due), keys[uid], httpx.Object{{Key: "id", Value: "assumed:" + uid},
				{Key: "kind", Value: "assumed"}, {Key: "course", Value: nullable(keys[uid])}, {Key: "item", Value: shown}}})
		}
		if keys[uid] == "" && d.Category == "study" && (d.Source == "tg" || d.Source == "mail" || d.Source == "ical") &&
			!aside["course:"+uid] {
			shown, err := item(d)
			if err != nil {
				return nil, err
			}
			found = append(found, question{todo.DueText(d.Due), "", httpx.Object{{Key: "id", Value: "course:" + uid},
				{Key: "kind", Value: "course"}, {Key: "course", Value: nil}, {Key: "item", Value: shown}}})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].due < found[j].due })
	out := []httpx.Object{}
	for _, q := range found {
		if course == "" || q.course == course {
			out = append(out, q.object)
		}
	}
	return out, nil
}

// PutAway puts a question aside: «They are different», «Outside the courses», or a date left as it is; it is not
// asked again. False when there is no such question now.
func (c *Check) PutAway(id string) (bool, error) {
	items, err := c.Items("")
	if err != nil {
		return false, err
	}
	for _, q := range items {
		if q.Get("id") == id {
			_, err := c.Store.DB.Exec("insert or ignore into web_checked(id, at) values(?, ?)", id,
				clock.LocalISO(clock.NowUTC()))
			return err == nil, err
		}
	}
	return false, nil
}

// AskAgain is the undo of a question put aside.
func (c *Check) AskAgain(id string) error {
	_, err := c.Store.DB.Exec("delete from web_checked where id = ?", id)
	return err
}
