"""Deadlines from chats and mail (§6.4) and the Midterms of RUZ (source "ical", §4.4). HSE App X keeps its lists in
kv (processing/hseapp.py), own and hidden ones have their tables (processing/todo.py); processing/merge.py makes one
list of them all (§5.5).

A deadline is in force until a later one moves it (`moved_by`) or its messages are deleted or edited into something
else (`gone_at`); a Midterm follows its class. Only deadlines from chats and mail are shown to the batch model and
moved by it: the others are moved by their own source (§6.5). Writing methods do not commit: call them inside
`with store.db`.
"""

import datetime as dt
import json
import re

from .. import clock
from ..store import Store

LOOKBACK = dt.timedelta(days=3)  # past deadlines still shown to the model and merged (§6.4, §5.3)


def same_label(a: str, b: str) -> bool:
    return re.sub(r"\s+", " ", a).strip().lower() == re.sub(r"\s+", " ", b).strip().lower()


class Deadlines:
    def __init__(self, store: Store):
        self.db = store.db

    def get(self, deadline_id: int) -> dict | None:
        row = self.db.execute("select * from deadlines where id = ?", (deadline_id,)).fetchone()
        return _decode(row) if row else None

    def upcoming(self, today: dt.date) -> list[dict]:
        """From chats and mail, in force and not older than three days, by date."""
        rows = self.db.execute("select * from deadlines where moved_by is null and gone_at is null and due >= ? "
                               "and source in ('tg', 'mail') order by due, id", ((today - LOOKBACK).isoformat(),))
        return [_decode(row) for row in rows]

    def listed(self, since: dt.date) -> list[dict]:
        """Every source of the table, in force and due on `since` or later, by date: the merge reads them (§5.5)."""
        rows = self.db.execute("select * from deadlines where moved_by is null and gone_at is null and due >= ? "
                               "order by due, id", (since.isoformat(),))
        return [_decode(row) for row in rows]

    def latest(self, deadline_id: int) -> dict | None:
        """The newest version of a deadline: the one its moves (`moved_by`) lead to."""
        row, seen = self.get(deadline_id), set()
        while row is not None and row["moved_by"] is not None and row["id"] not in seen:
            seen.add(row["id"])
            newer = self.get(row["moved_by"])
            if newer is None:
                break
            row = newer
        return row

    def find(self, label: str, due: str) -> dict | None:
        """One in force from chats or mail with the same work and date."""
        rows = self.db.execute("select * from deadlines where moved_by is null and gone_at is null and due = ? "
                               "and source in ('tg', 'mail')", (due,))
        return next((_decode(row) for row in rows if same_label(row["label"], label)), None)

    def course_names(self, today: dt.date) -> set[str]:
        """Courses of deadlines in force that are not long past: the dictionary names them (§6.2)."""
        rows = self.db.execute("select distinct course from deadlines where moved_by is null and gone_at is null "
                               "and course is not null and due >= ?", ((today - LOOKBACK).isoformat(),))
        return {row["course"] for row in rows}

    def made_of(self, source: str, chat_id: int, msg_id: int) -> list[dict]:
        """In force from the message's text; those read in the pages of its sheet are the sheet's, not the text's."""
        rows = self.db.execute(
            "select * from deadlines where source = ? and chat_id = ? and moved_by is null and gone_at is null "
            "and sheet_id is null and exists (select 1 from json_each(deadlines.msg_ids) where value = ?)",
            (source, chat_id, msg_id))
        return [_decode(row) for row in rows]

    def in_force(self, source: str, chat_id: int, since: dt.datetime) -> list[dict]:
        rows = self.db.execute("select * from deadlines where source = ? and chat_id = ? and moved_by is null "
                               "and gone_at is null and created_at >= ?", (source, chat_id, clock.local_iso(since)))
        return [_decode(row) for row in rows]

    def add(self, deadline: dict, at: dt.datetime) -> int:
        """`deadline`: source, chat_id, msg_ids, course, label, due; sheet_id for one read in a sheet's pages."""
        cursor = self.db.execute(
            "insert into deadlines(source, chat_id, msg_ids, course, label, due, created_at, sheet_id) "
            "values(?, ?, ?, ?, ?, ?, ?, ?)",
            (deadline["source"], deadline["chat_id"], json.dumps(deadline["msg_ids"]), deadline["course"] or None,
             deadline["label"], deadline["due"], clock.local_iso(at), deadline.get("sheet_id")))
        return cursor.lastrowid

    def move(self, old_id: int, new_id: int) -> bool:
        cursor = self.db.execute("update deadlines set moved_by = ? where id = ? and moved_by is null "
                                 "and gone_at is null", (new_id, old_id))
        return bool(cursor.rowcount)

    def rename(self, deadline_id: int, label: str, course: str | None) -> None:
        self.db.execute("update deadlines set label = ?, course = ? where id = ?", (label, course, deadline_id))

    def drop(self, deadline_ids: list[int], at: dt.datetime) -> None:
        marks = ", ".join("?" * len(deadline_ids))
        self.db.execute(f"update deadlines set gone_at = ? where gone_at is null and id in ({marks})",
                        (clock.local_iso(at), *deadline_ids))


def _decode(row) -> dict:
    deadline = dict(row)
    deadline["msg_ids"] = json.loads(deadline["msg_ids"]) if deadline["msg_ids"] else []
    return deadline
