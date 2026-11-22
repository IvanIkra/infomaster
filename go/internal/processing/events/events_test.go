package events

import (
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/store"
)

func TestUIDVectors(t *testing.T) {
	if UID("tg", -1001234567890, 42, 0) != "tg-ed3de03ad6dbd3b3@infomaster" ||
		UID("tg", -1001234567890, 42, 2) != "tg-690a4aead1368d91@infomaster" ||
		UID("mail", 0, 17, 0) != "mail-de991becd88b2608@infomaster" {
		t.Fatal("uid")
	}
	if a, b := OwnUID(), OwnUID(); a == b || len(a) != len("own-0123456789abcdef@infomaster") {
		t.Fatal(a, b)
	}
}

func TestVersionsAndChoices(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	chat := int64(-1001)
	all := Open(s)
	event := func(start string, ids ...int64) New {
		return New{UID: UID("tg", chat, 1, 0), Source: "tg", ChatID: &chat, MsgIDs: ids, Course: "Algebra",
			Title: "Alg consult", Kind: "consultation", Start: start, Room: "R205", Required: true}
	}
	first, _ := all.Add(event("2026-10-23T18:10:00", 1), at)
	if _, err := all.SetRequired(first, false); err != nil {
		t.Fatal(err)
	}
	if _, err := all.SetHidden(first, true); err != nil {
		t.Fatal(err)
	}
	moved, _ := all.Add(event("2026-10-24T18:10:00", 5), at)
	if ok, _ := all.Supersede(first, moved); !ok {
		t.Fatal("supersede")
	}
	if ok, _ := all.Supersede(first, moved); ok {
		t.Fatal("not in force any more")
	}
	version, _ := all.Get(moved)
	if version.Required || !version.Chosen || !version.Hidden || version.End != nil || *version.Course != "Algebra" {
		t.Fatal(version)
	}
	if current, _ := all.Current(first); current.ID != moved {
		t.Fatal(current)
	}
	edited, _ := all.Add(event("2026-10-25T18:10:00", 5), at) // message 5 edited
	all.Supersede(moved, edited)
	if back, _ := all.Drop([]int64{edited}, at); len(back) != 1 || back[0] != first {
		t.Fatal(back) // message 5 is gone: the first version is back
	}
	if found, _ := all.ByUID(version.UID); found.ID != first {
		t.Fatal(found)
	}
	if found, _ := all.MadeOf("tg", chat, 1); len(found) != 1 {
		t.Fatal(found)
	}
	if found, _ := all.Find("alg  CONSULT", "2026-10-23T18:10:00"); found == nil || found.ID != first {
		t.Fatal(found)
	}
	if back, _ := all.Drop([]int64{first}, at); len(back) != 0 {
		t.Fatal(back)
	}
	if found, _ := all.ByUID(version.UID); found != nil {
		t.Fatal(found)
	}

	class := "c1"
	fix, _ := all.Add(New{UID: class, Source: "tg", ChatID: &chat, MsgIDs: []int64{9}, ClassUID: class, Title: "x",
		Kind: "Seminar", Start: "2026-10-23T09:30:00", Required: true}, at)
	own, _ := all.Add(New{UID: OwnUID(), Source: Own, ClassUID: class, Title: "", Kind: "Seminar",
		Start: "2026-10-23T11:00:00", Required: true}, at)
	if got, _ := all.SetRequired(fix, false); !got.Required {
		t.Fatal("a class stays in the main calendar")
	}
	corrections, _ := all.Corrections()
	if corrections[class].ID != own {
		t.Fatal("the own one wins")
	}
	if c, _ := all.Correction(class); c.ID != fix {
		t.Fatal(c)
	}
	if gone, _ := all.Overrule(class, at); len(gone) != 2 {
		t.Fatal(gone)
	}
	if c, _ := all.OwnCorrection(class); c != nil {
		t.Fatal(c)
	}
}
