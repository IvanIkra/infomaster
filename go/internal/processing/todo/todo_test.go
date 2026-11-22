package todo

import (
	"crypto/sha1"
	"encoding/hex"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/store"
)

func TestUIDVectors(t *testing.T) {
	due := Date(2026, 11, 30)
	for title, want := range map[string]string{
		"Calc HW 5": "tg-fd3159f5b0", "  Calc   hw 5 ": "tg-fd3159f5b0", "ДЗ по Матану": "tg-677ab8864c",
		"Straße essay": "tg-886adf5114", "A B": "tg-4c0ecdd57f", "A\x1fB": "tg-4c0ecdd57f",
		"İstanbul": "tg-45ccda2b47",
	} {
		if got := UID(Deadline{Title: title, Due: due, Category: "study", Source: "tg"}); got != want {
			t.Errorf("%q: %s, want %s", title, got, want)
		}
	}
	if UID(Deadline{Title: "x", Due: due, Source: "ha", Key: "ha-3"}) != "ha-3" {
		t.Fatal("own")
	}
}

func TestOwnAndHidden(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lists := Open(s)
	if _, err := lists.Add(" ", Date(2026, 11, 1), "study", ""); err == nil {
		t.Fatal("an empty title")
	}
	if _, err := lists.Add("x", Date(2026, 11, 1), "work", ""); err == nil {
		t.Fatal("a category")
	}
	own, err := lists.Add(" Buy  books ", Date(2026, 11, 1), "personal", " Alg ")
	if err != nil || own.Title != "Buy books" || own.Key != "ha-1" || *own.Course != "Alg" {
		t.Fatal(own, err)
	}
	title := "Buy more books"
	edited, err := lists.Edit(own, &title, Date(2026, 11, 2), "")
	if err != nil || edited.Category != "personal" || edited.Key != own.Key {
		t.Fatal(edited, err)
	}
	course := "DM"
	source := Deadline{Title: "DM HW 3", Due: Date(2026, 11, 5), Category: "study", Source: "tg", Course: &course,
		Ref: "dl:4"}
	copied, err := lists.Edit(source, nil, Date(2026, 11, 6), "")
	if err != nil || copied.Source != "ha" || *copied.Course != "DM" || copied.Key != "ha-2" {
		t.Fatal(copied, err)
	}
	hidden, err := lists.Hidden()
	if err != nil || len(hidden) != 1 || !hidden[0].Equal(Deadline{Title: "DM HW 3", Due: Date(2026, 11, 5),
		Category: "study", Source: "tg", Course: &course}) || hidden[0].Ref != "dl:4" {
		t.Fatal(hidden, err)
	}
	if err := lists.Remove(source, "bad", ""); err == nil {
		t.Fatal("a reason")
	}
	if ok, _ := lists.Unhide(UID(source)); !ok {
		t.Fatal("unhide")
	}
	if err := lists.Remove(source, "duplicate", "tg-x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := lists.HiddenRows()
	if len(rows) != 1 || *rows[0].Reason != "duplicate" || *rows[0].Kept != "tg-x" {
		t.Fatal(rows)
	}
	if err := lists.Remove(edited, "", ""); err != nil {
		t.Fatal(err)
	}
	if mine, _ := lists.Own(); len(mine) != 1 || mine[0].Key != "ha-2" {
		t.Fatal(mine)
	}
	if err := lists.Prune(Date(2026, 12, 7)); err != nil {
		t.Fatal(err)
	}
	mine, _ := lists.Own()
	rows, _ = lists.HiddenRows()
	if len(mine) != 0 || len(rows) != 0 {
		t.Fatal("pruned")
	}
}

func TestUIDOfMerge(t *testing.T) { // test_merge.py: test_uid
	sum := sha1.Sum([]byte("hseapp|dm hw 5|2026-10-22"))
	expected := "hseapp-" + hex.EncodeToString(sum[:])[:10]
	item := Deadline{Title: "DM  HW 5", Due: Date(2026, 10, 22), Category: "study", Source: "hseapp"}
	other := Deadline{Title: "dm hw 5", Due: Date(2026, 10, 22), Category: "study", Source: "hseapp"}
	if UID(item) != expected || UID(other) != expected {
		t.Fatal(UID(item))
	}
	if UID(Deadline{Title: "Dentist", Due: Date(2026, 10, 25), Category: "personal", Source: "ha", Key: "ha-7"}) != "ha-7" {
		t.Fatal("own")
	}
}
