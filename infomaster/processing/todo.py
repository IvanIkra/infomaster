"""Own and hidden deadlines (§5.4, D8): the port of Tablo's todo.py, the side HA's to-do lists see.

HA shows every deadline of the merged list in two to-do lists, "Tablo · Study" and "Tablo · Personal", and the Mini
App and the forum buttons act the same way:
- an item added there is the owner's own deadline (`own_deadlines`, uid `ha-<id>`);
- ticking off or deleting an own one deletes it; one from a source is hidden for good (`hidden_deadlines`), even when
  the source sends it again: that is how Haiku's mistakes go away. The row it came from is kept too, so a rename of
  that row (a Midterm named by the dictionary, an HSE App X label once Haiku answers) does not bring it back;
- editing one from a source hides it and keeps an own copy with the new title or date; editing an own one changes it.
Rows 30 days past their date are removed at every change of the lists.

Writing methods do not commit: call them inside `with store.db` (processing/merge.py does).
"""

import datetime as dt
import hashlib
from dataclasses import dataclass, field

from ..store import Store

CATEGORIES = ("study", "personal")
KEEP_DAYS = 30  # past that, an own or a hidden deadline is off every list anyway
OWN_PREFIX = "ha-"


@dataclass(frozen=True)
class Deadline:
    """One deadline of the merge (§2.3): Tablo's model plus what the forum needs to find it again."""
    title: str
    due: dt.date
    category: str               # study, personal
    source: str                 # hseapp, ical, mail, tg, ha
    course: str | None = None   # the abbr from the dictionary: the merge compares by it (§5.5)
    key: str = field(default="", compare=False)  # own ones: "ha-<id>", their uid; the others are keyed by content
    # Which row it is, through renames: dl:<deadlines.id>, hseapp:<id>, ha-<id>. The forum diffs the list by it.
    ref: str = field(default="", compare=False)
    name: str | None = field(default=None, compare=False)  # the course as the source names it, for showing


def uid(d: Deadline) -> str:
    """Stable id of a to-do item: own deadlines by row, the others by source, title and date."""
    if d.key:
        return d.key
    raw = f"{d.source}|{' '.join(d.title.casefold().split())}|{d.due.isoformat()}"
    return f"{d.source}-{hashlib.sha1(raw.encode()).hexdigest()[:10]}"


def check(title: str, due: dt.date, category: str) -> str:
    title = " ".join(str(title).split())
    if not title:
        raise ValueError("title is empty")
    if category not in CATEGORIES:
        raise ValueError(f"category must be one of {', '.join(CATEGORIES)}")
    return title


def own_id(d: Deadline) -> int:
    return int(d.key.removeprefix(OWN_PREFIX))


class Todo:
    def __init__(self, store: Store):
        self.db = store.db

    def own(self) -> tuple[Deadline, ...]:
        rows = self.db.execute("select * from own_deadlines order by due, id").fetchall()
        return tuple(Deadline(r["title"], dt.date.fromisoformat(r["due"]), r["category"], "ha",
                              key=f"{OWN_PREFIX}{r['id']}", ref=f"{OWN_PREFIX}{r['id']}") for r in rows)

    def hidden(self) -> tuple[Deadline, ...]:
        rows = self.db.execute("select * from hidden_deadlines").fetchall()
        return tuple(Deadline(r["title"], dt.date.fromisoformat(r["due"]), "study", r["source"], r["course"],
                              ref=r["ref"] or "") for r in rows)

    def hidden_rows(self) -> list[dict]:
        """For the Mini App: by date, the newest first."""
        return [dict(row) for row in self.db.execute("select * from hidden_deadlines order by due desc, title")]

    def add(self, title: str, due: dt.date, category: str) -> Deadline:
        title = check(title, due, category)
        cursor = self.db.execute("insert into own_deadlines(title, due, category) values(?, ?, ?)",
                                 (title, due.isoformat(), category))
        key = f"{OWN_PREFIX}{cursor.lastrowid}"
        return Deadline(title, due, category, "ha", key=key, ref=key)

    def remove(self, d: Deadline) -> None:
        """Deleted or done: an own deadline goes away, one from a source is hidden."""
        if d.source == "ha":
            self.db.execute("delete from own_deadlines where id = ?", (own_id(d),))
        else:
            self.db.execute(
                "insert or ignore into hidden_deadlines(uid, source, course, title, due, ref) values(?, ?, ?, ?, ?, ?)",
                (uid(d), d.source, d.course, d.title, d.due.isoformat(), d.ref or None))

    def edit(self, d: Deadline, title: str | None, due: dt.date | None) -> Deadline:
        title = check(d.title if title is None else title, due or d.due, d.category)
        due = due or d.due
        if d.source != "ha":
            self.remove(d)
            return self.add(title, due, d.category)
        self.db.execute("update own_deadlines set title = ?, due = ? where id = ?", (title, due.isoformat(), own_id(d)))
        return Deadline(title, due, d.category, "ha", key=d.key, ref=d.key)

    def unhide(self, hidden_uid: str) -> bool:
        """«Вернуть» in the Mini App: the source's deadline is back unless another hidden row matches it."""
        return bool(self.db.execute("delete from hidden_deadlines where uid = ?", (hidden_uid,)).rowcount)

    def prune(self, today: dt.date) -> None:
        since = (today - dt.timedelta(days=KEEP_DAYS)).isoformat()
        self.db.execute("delete from hidden_deadlines where due < ?", (since,))
        self.db.execute("delete from own_deadlines where due < ?", (since,))
