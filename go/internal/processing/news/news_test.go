package news

import (
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/store"
)

func TestLiveHideAndReplace(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	all := Open(s)
	add := func(importance int64, study bool, posted string, ids ...int64) int64 {
		id, err := all.Add(New{Source: "tg", ChatID: -1001, MsgIDs: ids, Study: study, PostedAt: posted,
			Full: "full", Short: "short", Importance: importance}, at)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := add(2, true, "2026-10-20T11:00:00", 1)
	b := add(3, false, "2026-10-20T11:30:00", 2)
	c := add(1, false, "2026-10-19T08:00:00", 3) // older than 24 hours
	live, _ := all.Live(at, 24)
	if len(live) != 2 || live[0].ID != b || live[1].ID != a || !live[1].Study { // 3+0 before 2+1: newer first
		t.Fatal(live)
	}
	var stored string
	s.DB.QueryRow("select msg_ids from news where id = ?", a).Scan(&stored)
	if stored != "[1]" {
		t.Fatal(stored)
	}
	if ok, _ := all.Hide(a); !ok {
		t.Fatal("hide")
	}
	if ok, _ := all.Hide(a); ok {
		t.Fatal("hidden already")
	}
	d := add(2, true, "2026-10-20T11:40:00", 1, 4)
	all.Replace(a, d)
	if item, _ := all.Get(d); !item.Hidden {
		t.Fatal("hiding passes to the item that replaces a hidden one")
	}
	if made, _ := all.MadeOf("tg", -1001, 4); len(made) != 1 || made[0].ID != d {
		t.Fatal(made)
	}
	if ok, _ := all.Unhide(a); !ok {
		t.Fatal("unhide")
	}
	if item, _ := all.Get(d); item.Hidden {
		t.Fatal("the replacement shows again")
	}
	all.Drop([]int64{b, c}, at)
	if live, _ := all.Live(at, 24); len(live) != 1 || live[0].ID != d {
		t.Fatal(live)
	}
}
