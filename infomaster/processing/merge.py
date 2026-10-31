"""One list of deadlines for every consumer (§5.5, §5.6, §6.4, D4, D8, D10): the port of Tablo's merge.

Sources, the most reliable first: HSE App X (kv "hseapp.deadlines", processing/hseapp.py), the Midterms of RUZ
(source "ical"), then every deadline from mail and every one from the chats as a group of its own (two posts may carry
the same work, and it must be merged), then the owner's own (§5.4). The mail, chat and Midterm rows come from the
`deadlines` table: in force and not older than three days, by date, mail before the chats.

Tablo's rules, unchanged:
- the course a deadline is compared by is the abbr from the dictionary (our rows keep the course as the source names
  it, so it is mapped here), else the first word of the title; the work is the title without that course prefix;
- the same work: equal titles, or the same course and one work inside the other by whole words ("hw 2" is in
  "hw 2 late", not in "hw 20" or "hw 2b"); the same deadline: the same work on the same date;
- merge: a deadline joins unless it is the same as one kept already; one group's items are not compared together;
- done: a deadline from RUZ, mail or the chats leaves when HSE App X has the same work completed within 14 days
  (an extension moves the date in a letter while the app keeps the old one);
- hidden: everything but the own ones leaves when it is the same as a hidden one (ours also when it comes from the
  same row as a hidden one: a rename does not bring it back);
- shown: upcoming by date, then the overdue ones newest first for 3 days (1 for mail and the chats: they cannot tell
  that the work was handed in).

The same list, in the same order, goes to GET /api/deadlines for HA and Tablo, the pinned list of the «Дедлайны» topic
and the Mini App. The owner's changes (HA, the Mini App, the buttons in the forum) go through here too and tell
`on_change` which deadlines they put into the list or moved, so the forum shows them without a message of their own.
"""

import datetime as dt
import re
from collections.abc import Callable, Iterable

from .. import clock
from ..store import Store
from .deadlines import LOOKBACK, Deadlines
from .hseapp import HseAppDeadlines
from .subjects import Subjects, clean
from .todo import Deadline, Todo, uid

DONE_WINDOW_DAYS = 14   # an extension moves the date in a letter, while HSE App X keeps the old one
OVERDUE_DAYS = 3
OVERDUE_DAYS_BY_SOURCE = {"mail": 1, "tg": 1}  # they cannot tell that the work was handed in
NEWS_ORDER = ("mail", "tg")  # more reliable first when duplicates are merged


def _course(d: Deadline) -> str:
    """The course abbreviation, or the first word of the title when there is none ('Safety HW 2')."""
    return (d.course or d.title.split(" ", 1)[0]).casefold()


def _work(d: Deadline) -> str:
    """The title without the course prefix, for comparing: 'Prog Midterm' -> 'midterm'."""
    title = " ".join(d.title.casefold().split())
    course = _course(d)
    if title.startswith(course + " "):
        title = title[len(course) + 1:]
    return title


def same_work(a: Deadline, b: Deadline) -> bool:
    """Same course and same work, whatever the date."""
    if " ".join(a.title.casefold().split()) == " ".join(b.title.casefold().split()):
        return True
    wa, wb = _work(a), _work(b)
    return bool(wa and wb) and _course(a) == _course(b) and (_contains(wa, wb) or _contains(wb, wa))


def _contains(part: str, whole: str) -> bool:
    """Whole words only: 'hw 2' is in 'hw 2 late', not in 'hw 20' or 'hw 2b'."""
    return re.search(rf"(?<!\w){re.escape(part)}(?!\w)", whole) is not None


def same(a: Deadline, b: Deadline) -> bool:
    return a.due == b.due and same_work(a, b)


def merge_deadlines(*sources: Iterable[Deadline]) -> tuple[Deadline, ...]:
    """One list from several sources, most reliable first; a duplicate of a kept deadline is dropped."""
    kept: list[Deadline] = []
    for items in sources:
        kept += [d for d in items if not any(same(d, k) for k in kept)]
    return tuple(kept)


def drop_done(deadlines: Iterable[Deadline], done: Iterable[Deadline]) -> tuple[Deadline, ...]:
    """Hide copies of work completed in HSE App X: same course and work, dates up to 14 days apart."""
    done = tuple(done)
    return tuple(d for d in deadlines if d.source in ("hseapp", "ha") or not any(
        abs((d.due - x.due).days) <= DONE_WINDOW_DAYS and same_work(d, x) for x in done))


def hides(h: Deadline, d: Deadline) -> bool:
    """Tablo's rule, the same deadline; and ours, the same row, so a renamed one stays hidden."""
    return same(d, h) or bool(h.ref) and h.ref == d.ref


def drop_hidden(deadlines: Iterable[Deadline], hidden: Iterable[Deadline]) -> tuple[Deadline, ...]:
    """Hide what was deleted or marked done, also when it comes back from another source.

    The owner's own deadlines are deleted, not hidden: an edited copy of a hidden one stays.
    """
    hidden = tuple(hidden)
    return tuple(d for d in deadlines if d.source == "ha" or not any(hides(h, d) for h in hidden))


def overdue_days(d: Deadline) -> int:
    return OVERDUE_DAYS_BY_SOURCE.get(d.source, OVERDUE_DAYS)


def visible(deadlines: Iterable[Deadline], today: dt.date) -> list[Deadline]:
    """Upcoming by date first, then overdue ones for a few days after the due date, the newest first."""
    deadlines = tuple(deadlines)
    upcoming = sorted((d for d in deadlines if d.due >= today), key=lambda d: d.due)
    overdue = sorted((d for d in deadlines if today - dt.timedelta(days=overdue_days(d)) <= d.due < today),
                     key=lambda d: d.due, reverse=True)
    return upcoming + overdue


class DeadlineList:
    def __init__(self, store: Store, deadlines: Deadlines, hseapp: HseAppDeadlines, subjects: Subjects,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.db = store.db
        self.deadlines, self.hseapp, self.subjects, self.now = deadlines, hseapp, subjects, now
        self.todo = Todo(store)
        # After a change by the owner: the refs it put into the shown list or moved there (the forum shows them
        # quietly). Called from synchronous code, so it only schedules the work.
        self.on_change: Callable[[set[str]], None] | None = None

    def today(self) -> dt.date:
        return clock.local_day(self.now())

    # --- the sources ----------------------------------------------------------------------

    def _abbrs(self) -> Callable[[str | None], str | None]:
        """Course name -> abbr, from one read of the dictionary."""
        known = {name: subject.abbr for name, subject in self.subjects.all().items()}
        return lambda name: known.get(clean(name)) if name else None

    def _hseapp(self, rows: list[dict], abbr: Callable[[str | None], str | None]) -> tuple[Deadline, ...]:
        return tuple(Deadline(row["label"], dt.date.fromisoformat(row["due"]), "study", "hseapp", abbr(row["course"]),
                              ref=f"hseapp:{row['id']}", name=row["course"]) for row in rows)

    def sources(self) -> tuple[list[tuple[Deadline, ...]], tuple[Deadline, ...]]:
        """The merge's groups in order, and the completed ones of HSE App X."""
        abbr = self._abbrs()
        rows = [Deadline(row["label"], dt.date.fromisoformat(row["due"]), "study", row["source"], abbr(row["course"]),
                         ref=f"dl:{row['id']}", name=row["course"])
                for row in self.deadlines.listed(self.today() - LOOKBACK)]
        ical = tuple(d for d in rows if d.source == "ical")
        news = sorted((d for d in rows if d.source in NEWS_ORDER), key=lambda d: NEWS_ORDER.index(d.source))
        groups = [self._hseapp(self.hseapp.current(), abbr), ical, *((d,) for d in news), self.todo.own()]
        return groups, self._hseapp(self.hseapp.done(), abbr)

    # --- the list ---------------------------------------------------------------------------

    def unhidden(self) -> tuple[Deadline, ...]:
        """Merged and without the work done in HSE App X, the hidden ones still in."""
        groups, done = self.sources()
        return drop_done(merge_deadlines(*groups), done)

    def all(self) -> tuple[Deadline, ...]:
        """The final list (Tablo's hub.deadlines()): merged, without what is done in HSE App X or hidden."""
        return drop_hidden(self.unhidden(), self.todo.hidden())

    def visible(self) -> list[Deadline]:
        """What the screen, HA's to-do lists and the pinned list show, in their order."""
        return visible(self.all(), self.today())

    def items(self) -> list[dict]:
        """GET /api/deadlines: the full list in screen order, past Tablo's 5 rows."""
        today = self.today()
        return [{"uid": uid(d), "title": d.title, "due": d.due.isoformat(), "category": d.category,
                 "source": d.source, "overdue": d.due < today} for d in self.visible()]

    def find(self, item_uid: str) -> Deadline | None:
        """A deadline of the final list by its uid, shown or not."""
        return next((d for d in self.all() if uid(d) == item_uid), None)

    def by_ref(self, ref: str) -> Deadline | None:
        """The deadline a forum post is about, as it is now: one of the chats or RUZ may have moved since (its newest
        version then), a copy from a more reliable source may stand for it, and a hidden one is found too."""
        row = None
        if ref.startswith("dl:") and ref[3:].isdigit():
            row = self.deadlines.latest(int(ref[3:]))
            ref = f"dl:{row['id']}" if row else ref
        listed = self.unhidden()
        found = next((d for d in listed if d.ref == ref), None)
        if found is None and row is not None and row["moved_by"] is None and row["gone_at"] is None:
            told = Deadline(row["label"], dt.date.fromisoformat(row["due"]), "study", row["source"],
                            self._abbrs()(row["course"]))
            found = next((d for d in listed if same(d, told)), None)  # merged away: the copy kept instead
        return found

    def is_hidden(self, d: Deadline) -> bool:
        return d.source != "ha" and any(hides(h, d) for h in self.todo.hidden())

    # --- the owner's changes ----------------------------------------------------------------

    def add(self, title: str, due: dt.date, category: str) -> Deadline:
        """A new own deadline; ValueError for an empty title or an unknown category."""
        return self._change(lambda: self.todo.add(title, due, category))

    def remove(self, d: Deadline) -> None:
        """Done or deleted: an own one is deleted, one from a source is hidden for good."""
        self._change(lambda: self.todo.remove(d))

    def edit(self, d: Deadline, title: str | None, due: dt.date | None, category: str | None = None) -> Deadline:
        """A new title, date or list; one from a source becomes an own copy. ValueError for an empty title."""
        return self._change(lambda: self.todo.edit(d, title, due, category))

    def unhide(self, hidden_uid: str) -> bool:
        """«Вернуть» of the Mini App: one hidden row less."""
        return self._change(lambda: self.todo.unhide(hidden_uid))

    def restore(self, d: Deadline) -> bool:
        """«Вернуть» under a forum post: every hidden row that hides this deadline goes."""
        def unhide() -> bool:
            rows = [row for row in self.todo.hidden_rows() if hides(Deadline(
                row["title"], dt.date.fromisoformat(row["due"]), "study", row["source"], row["course"],
                ref=row["ref"] or ""), d)]
            removed = [self.todo.unhide(row["uid"]) for row in rows]
            return any(removed)
        return self._change(unhide)

    def prune(self) -> None:
        """Own and hidden rows 30 days past their date go: they are off every list."""
        with self.db:
            self.todo.prune(self.today())

    def _change(self, action: Callable):
        """Run an owner's change and clean up the rows long past; tell `on_change` what it brought into the list."""
        before = self._shown()
        with self.db:
            result = action()
            self.todo.prune(self.today())
        after = self._shown()
        if self.on_change is not None:
            self.on_change({ref for ref, due in after.items() if before.get(ref) != due})
        return result

    def _shown(self) -> dict[str, dt.date]:
        return {d.ref: d.due for d in self.visible()}
