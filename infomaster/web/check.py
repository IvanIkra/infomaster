"""The «Check» queue of the web app (§8.6, stage 16.3): only what the service is unsure of, one question each.

- an assumed date: a sheet's date the service took from its course's next seminar (§6.3). «Right» confirms it, or the
  owner gives the real one;
- a likely duplicate: two deadlines of one course that the merge (§5.5) kept apart, though they look like one work:
  the same work on dates up to a week apart, or the same number in both titles on such dates («Sheet 12» of the wiki
  and «HW 12» of the chat). «Merge» hides one of them as a duplicate of the other: the one with a real date stays over
  an assumed one, then the more reliable source (HSE App X, RUZ, mail, the chats, the owner's own);
- a deadline without a course: a chat or a letter named none. The owner picks the course, or leaves it outside the
  courses.

A question put aside («They are different», «Outside the courses») is kept by its id (`web_checked`) and not asked
again; its id holds the deadlines' uids, which change with a title or a date, so a changed deadline is asked about
anew. Upcoming deadlines only: an overdue one is past checking.
"""

import datetime as dt
import re

from .. import clock
from ..processing.merge import same_work
from ..processing.todo import Deadline, uid
from ..store import Store
from .views import Data

SCHEMA = """
create table if not exists web_checked (  -- questions of the «Check» queue the owner put aside (§8.6)
    id text primary key,        -- dup:<uid>|<uid>, course:<uid>, assumed:<uid>
    at text not null            -- local time
);
"""
DUPLICATE_DAYS = 7          # a likely duplicate's dates are this close at most
TRUST = ("hseapp", "ical", "mail", "tg", "ha")  # the merge's order: the more reliable source stays
NUMBER = re.compile(r"\d+")


def numbers(title: str) -> set[str]:
    return {n.lstrip("0") or "0" for n in NUMBER.findall(title)}


def likely_same(a: Deadline, b: Deadline) -> bool:
    """Two deadlines of one course that may be one work: see the module's doc. Two of HSE App X or two Midterms are
    never one: those sources list each work once."""
    if a.source == b.source and a.source in ("hseapp", "ical"):
        return False
    if abs((a.due - b.due).days) > DUPLICATE_DAYS:
        return False
    return same_work(a, b) or bool(numbers(a.title) & numbers(b.title))


def stays(a: Deadline, b: Deadline) -> Deadline:
    """Of two duplicates, the one to keep: a real date over an assumed one, then the more reliable source."""
    def rank(d: Deadline) -> tuple[int, int]:
        return int(d.assumed), TRUST.index(d.source) if d.source in TRUST else len(TRUST)
    return a if rank(a) <= rank(b) else b


class Check:
    def __init__(self, data: Data, store: Store):
        self.data, self.db = data, store.db
        self.db.executescript(SCHEMA)

    def put_aside(self) -> set[str]:
        return {row["id"] for row in self.db.execute("select id from web_checked")}

    def items(self, course: str | None = None) -> list[dict]:
        """The open questions, the soonest deadline first; of one course's lens only, when given."""
        data, today = self.data, self.data.now().date()
        aside = self.put_aside()
        ahead = [d for d in data.merged.visible() if d.due >= today]
        keys = {uid(d): data.deadline_key(d) for d in ahead}
        found: list[tuple[dt.date, dict]] = []

        paired: set[str] = set()
        for i, a in enumerate(ahead):
            for b in ahead[i + 1:]:
                ua, ub = uid(a), uid(b)
                if ua in paired or ub in paired or keys[ua] is None or keys[ua] != keys[ub]:
                    continue
                question = "dup:" + "|".join(sorted((ua, ub)))
                if question in aside or not likely_same(a, b):
                    continue
                paired |= {ua, ub}
                keep = stays(a, b)
                first, second = (keep, b if keep is a else a)
                found.append((min(a.due, b.due), {
                    "id": question, "kind": "duplicate", "course": keys[ua], "keep": uid(keep),
                    "items": [data.deadline_item(first, today), data.deadline_item(second, today)]}))
        for d in ahead:
            item_uid = uid(d)
            if d.assumed and f"assumed:{item_uid}" not in aside:
                found.append((d.due, {"id": f"assumed:{item_uid}", "kind": "assumed", "course": keys[item_uid],
                                      "item": data.deadline_item(d, today)}))
            if keys[item_uid] is None and d.category == "study" and d.source in ("tg", "mail", "ical") and \
                    f"course:{item_uid}" not in aside:
                found.append((d.due, {"id": f"course:{item_uid}", "kind": "course", "course": None,
                                      "item": data.deadline_item(d, today)}))
        found.sort(key=lambda pair: pair[0])
        return [question for _, question in found if course is None or question["course"] == course]

    def put_away(self, question: str) -> bool:
        """«They are different», «Outside the courses», or a date left as it is: not asked again. False when there
        is no such question now."""
        if not any(q["id"] == question for q in self.items()):
            return False
        with self.db:
            self.db.execute("insert or ignore into web_checked(id, at) values(?, ?)",
                            (question, clock.local_iso(clock.now_utc())))
        return True

    def ask_again(self, question: str) -> None:
        with self.db:
            self.db.execute("delete from web_checked where id = ?", (question,))
