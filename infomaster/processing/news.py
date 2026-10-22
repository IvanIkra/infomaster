"""Parsed news (§6.1, §8.3, р. 23): one table and one set of rules for the forum and for Tablo.

A live item is neither replaced nor gone nor hidden by the owner and is younger than `news_ttl_hours`. Rank is
`importance + study`, the newer item first among equals. Hiding passes to the item that replaces a hidden one, so an
edit of its message keeps it hidden (the model never sees hidden items, so no other item replaces one). Writing methods
do not commit: call them inside `with store.db`.
"""

import datetime as dt
import json

from .. import clock
from ..store import Store


class News:
    def __init__(self, store: Store):
        self.db = store.db

    def get(self, news_id: int) -> dict | None:
        row = self.db.execute("select * from news where id = ?", (news_id,)).fetchone()
        return _decode(row) if row else None

    def live(self, now: dt.datetime, ttl_hours: int) -> list[dict]:
        """In rank order."""
        rows = self.db.execute(
            "select * from news where replaced_by is null and gone_at is null and not hidden and posted_at >= ? "
            "order by importance + study desc, posted_at desc, id desc",
            (clock.local_iso(now - dt.timedelta(hours=ttl_hours)),))
        return [_decode(row) for row in rows]

    def made_of(self, source: str, chat_id: int, msg_id: int) -> list[dict]:
        """Items in force that one of the message's versions gave."""
        rows = self.db.execute(
            "select * from news where source = ? and chat_id = ? and replaced_by is null and gone_at is null "
            "and exists (select 1 from json_each(news.msg_ids) where value = ?)", (source, chat_id, msg_id))
        return [_decode(row) for row in rows]

    def in_force(self, source: str, chat_id: int, since: dt.datetime) -> list[dict]:
        rows = self.db.execute("select * from news where source = ? and chat_id = ? and replaced_by is null "
                               "and gone_at is null and created_at >= ?", (source, chat_id, clock.local_iso(since)))
        return [_decode(row) for row in rows]

    def add(self, item: dict, at: dt.datetime) -> int:
        """`item`: source, chat_id, msg_ids, study, posted_at (local ISO), full, short, importance."""
        cursor = self.db.execute(
            "insert into news(source, chat_id, msg_ids, study, posted_at, full, short, importance, created_at) "
            "values(?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (item["source"], item["chat_id"], json.dumps(item["msg_ids"]), int(item["study"]), item["posted_at"],
             item["full"], item["short"], item["importance"], clock.local_iso(at)))
        return cursor.lastrowid

    def replace(self, old_id: int, new_id: int) -> None:
        if self.db.execute("update news set replaced_by = ? where id = ? and replaced_by is null and gone_at is null",
                           (new_id, old_id)).rowcount:
            self.db.execute("update news set hidden = 1 where id = ? and exists "
                            "(select 1 from news where id = ? and hidden)", (new_id, old_id))

    def hide(self, news_id: int) -> bool:
        """The owner's «Скрыть» under an alert: off the list and the API for good."""
        return bool(self.db.execute("update news set hidden = 1 where id = ? and not hidden", (news_id,)).rowcount)

    def drop(self, news_ids: list[int], at: dt.datetime) -> None:
        marks = ", ".join("?" * len(news_ids))
        self.db.execute(f"update news set gone_at = ? where gone_at is null and id in ({marks})",
                        (clock.local_iso(at), *news_ids))


def _decode(row) -> dict:
    item = dict(row)
    item["msg_ids"] = json.loads(item["msg_ids"])
    item["study"] = bool(item["study"])
    item["hidden"] = bool(item["hidden"])
    return item
