package deadlines

import (
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/store"
)

func TestSameLabel(t *testing.T) {
	if !SameLabel(" Calc  HW\t5 ", "calc hw 5") || SameLabel("Calc HW 5", "Calc HW 6") {
		t.Fatal("same label")
	}
}

func TestMovesAndDrops(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	chat := int64(-1001)
	all := Open(s)
	add := func(label, due string, ids ...int64) int64 {
		id, err := all.Add(New{Source: "tg", ChatID: &chat, MsgIDs: ids, Course: "Calculus", Label: label, Due: due}, at)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := add("Calc HW 5", "2026-10-22", 1)
	if got, _ := all.Find("calc hw  5", "2026-10-22"); got == nil || got.ID != first || *got.Course != "Calculus" {
		t.Fatal(got)
	}
	moved := add("Calc HW 5", "2026-10-24", 2)
	if ok, _ := all.Move(first, moved); !ok {
		t.Fatal("move")
	}
	if latest, _ := all.Latest(first); latest.ID != moved {
		t.Fatal(latest)
	}
	edited := add("Calc HW 5", "2026-10-25", 2) // message 2 edited
	all.Move(moved, edited)
	if back, _ := all.Drop([]int64{edited}, at); len(back) != 1 || back[0] != first {
		t.Fatal(back) // past the earlier text of message 2: the first one is back
	}
	upcoming, _ := all.Upcoming(today, Lookback)
	if len(upcoming) != 1 || upcoming[0].ID != first || upcoming[0].MsgIDs[0] != 1 {
		t.Fatal(upcoming)
	}
	if made, _ := all.MadeOf("tg", chat, 1); len(made) != 1 {
		t.Fatal(made)
	}
	names, _ := all.CourseNames(today)
	if !names["Calculus"] || len(names) != 1 {
		t.Fatal(names)
	}
	sheet := int64(7)
	id, _ := all.Add(New{Source: "tg", ChatID: &chat, MsgIDs: []int64{3}, Label: "Sheet 1", Due: "2026-10-27",
		SheetID: &sheet, Assumed: true}, at)
	row, _ := all.Get(id)
	if !row.Assumed || row.Course != nil {
		t.Fatal(row)
	}
	all.Confirm(id)
	if rows, _ := all.OfSheet(sheet); len(rows) != 1 || rows[0].Assumed {
		t.Fatal(rows)
	}
	empty, _ := all.Add(New{Source: "ical", Label: "Calc Midterm", Due: "2026-10-30"}, at)
	if row, _ := all.Get(empty); len(row.MsgIDs) != 0 || row.ChatID != nil {
		t.Fatal(row)
	}
}
