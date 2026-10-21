"""Raw chats and messages in SQLite (§9): what the collectors store and the processing reads.

Messages are written once and then only amended: an edit replaces the text and sets `edited_at`, a deletion sets
`deleted_at`. A message waits for its batch until `parsed_at` is set; one edited after that waits again, and its batch
replaces what its earlier text gave. A deleted message is never parsed, and what it gave is withdrawn.

Letters of the university mailbox (collectors/mail.py, §5.2) are messages too, with source "mail": all in one
pseudo chat, `MAILBOX` (no Telegram chat has id 0), which is no row of `chats`. A letter is never edited or deleted,
and its subject is kept for the forum, where it stands instead of a link (§8.3).
"""

import datetime as dt
import json
from collections.abc import Callable

from .. import clock
from ..store import Store

RETENTION = dt.timedelta(days=14)  # р. 29
MAILBOX = 0  # chat_id of the letters

# Ids of private chats and basic groups are unique across the account: their deletions come without a chat.
ACCOUNT_WIDE_IDS = ("user", "bot", "group")

PENDING = "deleted_at is null and (parsed_at is null or edited_at > parsed_at)"

MESSAGE_FIELDS = ("sender_id", "sender_name", "at", "text", "topic_id", "reply_to", "reply_quote", "forward",
                  "grouped_id", "files", "pinned")


class Inbox:
    def __init__(self, store: Store):
        self.db = store.db
        self.wake: Callable[[int, list[int]], None] | None = None  # new or pinned messages: the "urgent" trigger

    # --- chats ------------------------------------------------------------------

    def chat(self, chat_id: int) -> dict | None:
        row = self.db.execute("select * from chats where id = ?", (chat_id,)).fetchone()
        return dict(row) if row else None

    def chats(self) -> list[dict]:
        """With the number of stored messages and the time of the newest one."""
        rows = self.db.execute(
            "select c.*, count(m.id) messages, max(m.at) last_at from chats c "
            "left join messages m on m.source = 'tg' and m.chat_id = c.id group by c.id order by c.title")
        return [dict(row) for row in rows]

    def put_chat(self, chat_id: int, title: str, kind: str, username: str | None, last_id: int) -> bool:
        """Add a chat or refresh its name; returns True if the chat is new. `last_id` is kept for a known chat."""
        with self.db:
            cursor = self.db.execute(
                "insert into chats(id, title, kind, username, last_id, added_at) values(?, ?, ?, ?, ?, ?) "
                "on conflict(id) do nothing",
                (chat_id, title, kind, username, last_id, clock.local_iso(clock.now_utc())))
            if cursor.rowcount:
                return True
            self.db.execute("update chats set title = ?, kind = ?, username = ? where id = ?",
                            (title, kind, username, chat_id))
            return False

    def rename_chat(self, chat_id: int, title: str) -> None:
        with self.db:
            self.db.execute("update chats set title = ? where id = ?", (title, chat_id))

    def advance(self, chat_id: int, msg_id: int) -> None:
        """The newest message seen in a chat, stored or not: a disabled chat moves on too, so enabling it later
        does not pull its history into the queue."""
        with self.db:
            self.db.execute("update chats set last_id = max(last_id, ?) where id = ?", (msg_id, chat_id))

    # --- messages ---------------------------------------------------------------

    def message(self, chat_id: int, msg_id: int, source: str = "tg") -> dict | None:
        row = self.db.execute("select * from messages where source = ? and chat_id = ? and msg_id = ?",
                              (source, chat_id, msg_id)).fetchone()
        return _decode(row) if row else None

    def has(self, chat_id: int, msg_id: int, source: str = "tg") -> bool:
        return self.db.execute("select 1 from messages where source = ? and chat_id = ? and msg_id = ?",
                               (source, chat_id, msg_id)).fetchone() is not None

    def add(self, chat_id: int, msg_id: int, fields: dict, source: str = "tg") -> bool:
        """Store a new message; returns False if it is already stored (the catch-up and live updates overlap)."""
        values = _encode(fields)
        with self.db:
            cursor = self.db.execute(
                f"insert into messages(source, chat_id, msg_id, {', '.join(MESSAGE_FIELDS)}) "
                f"values(?, ?, ?, {', '.join('?' * len(MESSAGE_FIELDS))}) on conflict do nothing",
                (source, chat_id, msg_id, *(values[f] for f in MESSAGE_FIELDS)))
        if cursor.rowcount and self.wake is not None:
            self.wake(chat_id, [msg_id])
        return bool(cursor.rowcount)

    def edit(self, chat_id: int, msg_id: int, text: str, files: list[dict], at: dt.datetime) -> bool:
        """Replace the content; returns False if the message is not stored or nothing in it changed
        (Telegram also sends edits for reactions and link previews)."""
        stored = self.message(chat_id, msg_id)
        if stored is None or (stored["text"] == text and same_files(stored["files"], files)):
            return False
        files_json = json.dumps(files, ensure_ascii=False) if files else None
        with self.db:
            cursor = self.db.execute(
                "update messages set text = ?, files = ?, edited_at = ? where source = 'tg' and chat_id = ? "
                "and msg_id = ? and (text != ? or files is not ?)",
                (text, files_json, clock.local_iso(at), chat_id, msg_id, text, files_json))
        return bool(cursor.rowcount)

    def delete(self, msg_ids: list[int], chat_id: int | None, at: dt.datetime) -> int:
        """Mark messages deleted; without a chat the ids are account-wide (private chats and basic groups)."""
        marks = ", ".join("?" * len(msg_ids))
        if chat_id is None:
            kinds = ", ".join("?" * len(ACCOUNT_WIDE_IDS))
            where = f"chat_id in (select id from chats where kind in ({kinds}))"
            args = ACCOUNT_WIDE_IDS
        else:
            where, args = "chat_id = ?", (chat_id,)
        with self.db:
            cursor = self.db.execute(
                f"update messages set deleted_at = ? where source = 'tg' and deleted_at is null and {where} "
                f"and msg_id in ({marks})", (clock.local_iso(at), *args, *msg_ids))
        return cursor.rowcount

    def pin(self, chat_id: int, msg_ids: list[int], pinned: bool) -> None:
        marks = ", ".join("?" * len(msg_ids))
        with self.db:
            self.db.execute(f"update messages set pinned = ? where source = 'tg' and chat_id = ? "
                            f"and msg_id in ({marks})", (int(pinned), chat_id, *msg_ids))
        if pinned and self.wake is not None:
            self.wake(chat_id, msg_ids)

    def messages(self, chat_id: int, msg_ids: list[int], source: str = "tg") -> dict[int, dict]:
        """Stored ones of these messages, by msg_id."""
        marks = ", ".join("?" * len(msg_ids))
        rows = self.db.execute(f"select * from messages where source = ? and chat_id = ? and msg_id in ({marks})",
                               (source, chat_id, *msg_ids))
        return {row["msg_id"]: _decode(row) for row in rows}

    # --- the queue of the batches -------------------------------------------------

    def pending(self, chat_id: int, source: str = "tg") -> list[dict]:
        """Messages waiting for a batch, oldest first: never parsed, or edited since."""
        rows = self.db.execute(f"select * from messages where source = ? and chat_id = ? and {PENDING} "
                               "order by at, msg_id", (source, chat_id))
        return [_decode(row) for row in rows]

    def pending_counts(self) -> dict[int, int]:
        rows = self.db.execute(f"select chat_id, count(*) n from messages where {PENDING} group by chat_id")
        return {row["chat_id"]: row["n"] for row in rows}

    def mark_parsed(self, chat_id: int, msg_ids: list[int], at: dt.datetime, source: str = "tg") -> None:
        """Call inside `with store.db`, together with saving what the messages gave."""
        marks = ", ".join("?" * len(msg_ids))
        self.db.execute(f"update messages set parsed_at = ? where source = ? and chat_id = ? and msg_id in ({marks})",
                        (clock.local_iso(at), source, chat_id, *msg_ids))

    # --- summaries ----------------------------------------------------------------

    def for_summary(self, chat_id: int, since: dt.datetime, until: dt.datetime, fresh: bool) -> list[dict]:
        """Messages of a chat in [since, until), oldest first, deleted ones aside; `fresh`: only those no summary has
        taken yet (§6.6)."""
        rows = self.db.execute(
            "select * from messages where source = 'tg' and chat_id = ? and deleted_at is null and at >= ? and at < ?"
            + (" and summarized_at is null" if fresh else "") + " order by at, msg_id",
            (chat_id, clock.local_iso(since), clock.local_iso(until)))
        return [_decode(row) for row in rows]

    def mark_summarized(self, chat_id: int, msg_ids: list[int], at: dt.datetime) -> None:
        if not msg_ids:
            return
        marks = ", ".join("?" * len(msg_ids))
        with self.db:
            self.db.execute(f"update messages set summarized_at = ? where source = 'tg' and chat_id = ? "
                            f"and msg_id in ({marks})", (clock.local_iso(at), chat_id, *msg_ids))

    def sent_at(self, at: dt.datetime) -> list[dict]:
        """Stored chat messages sent at this second: what a forward to the bot was forwarded from (its date is the
        original's)."""
        rows = self.db.execute("select * from messages where source = 'tg' and at = ? and deleted_at is null",
                               (clock.local_iso(at),))
        return [_decode(row) for row in rows]

    # --- letters ------------------------------------------------------------------

    def add_letter(self, msg_id: int, fields: dict) -> bool:
        """Store a letter (`fields`: sender_name, at, text, files, subject); False if it is stored already. Call
        inside `with store.db`, together with the mailbox state, so a letter is never stored twice."""
        values = _encode(fields)
        cursor = self.db.execute(
            f"insert into messages(source, chat_id, msg_id, {', '.join(MESSAGE_FIELDS)}, subject) "
            f"values('mail', ?, ?, {', '.join('?' * len(MESSAGE_FIELDS))}, ?) on conflict do nothing",
            (MAILBOX, msg_id, *(values[f] for f in MESSAGE_FIELDS), fields.get("subject") or None))
        return bool(cursor.rowcount)

    def newest_letter(self) -> int:
        """The highest msg_id of the letters stored, 0 for none."""
        row = self.db.execute("select max(msg_id) n from messages where source = 'mail' and chat_id = ?",
                              (MAILBOX,)).fetchone()
        return row["n"] or 0

    def letters(self) -> int:
        """Letters stored: those of the last 14 days."""
        return self.db.execute("select count(*) n from messages where source = 'mail'").fetchone()["n"]

    def subject(self, msg_ids: list[int]) -> str | None:
        """The subject of the first of these letters while it is stored; None after its 14 days or without one."""
        if not msg_ids:
            return None
        row = self.db.execute("select subject from messages where source = 'mail' and chat_id = ? and msg_id = ?",
                              (MAILBOX, msg_ids[0])).fetchone()
        return row["subject"] if row else None

    def senders(self, chat_id: int) -> list[dict]:
        """Who wrote in a chat, most active first: the Mini App picks the important people from them."""
        rows = self.db.execute(
            "select sender_id id, sender_name name, count(*) messages from messages "
            "where source = 'tg' and chat_id = ? and sender_id is not null "
            "group by sender_id order by messages desc, max(at) desc", (chat_id,))
        return [dict(row) for row in rows]

    def prune(self, now: dt.datetime) -> int:
        """Remove raw messages older than 14 days (р. 29)."""
        with self.db:
            cursor = self.db.execute("delete from messages where at < ?", (clock.local_iso(now - RETENTION),))
        return cursor.rowcount


# A photo as messages stored before stage 9 have it: without its size, and a link preview's picture counted as one.
LEGACY_PHOTO = {"kind": "photo"}


def same_files(stored: list[dict], files: list[dict]) -> bool:
    """The files of an edit are those stored. A legacy photo is the same as a photo of any size, or as nothing (it was
    a link preview, no longer counted): an edit of such a message is no new content. Gone with the 14 days of the
    messages of stage 8."""
    if stored == files:
        return True
    return stored == [LEGACY_PHOTO] and (not files or (len(files) == 1 and files[0]["kind"] == "photo"))


def message_link(chat: dict, msg_id: int) -> str | None:
    """t.me link to a message; private chats and basic groups have none."""
    if chat.get("username"):
        return f"https://t.me/{chat['username']}/{msg_id}"
    if chat["kind"] in ("supergroup", "forum", "channel"):
        return f"https://t.me/c/{-chat['id'] - 10**12}/{msg_id}"  # marked -100XXXXXXXXXX -> XXXXXXXXXX
    return None


def _encode(fields: dict) -> dict:
    values = {f: fields.get(f) for f in MESSAGE_FIELDS}
    values["at"] = clock.local_iso(fields["at"])
    values["forward"] = json.dumps(fields["forward"], ensure_ascii=False) if fields.get("forward") else None
    values["files"] = json.dumps(fields["files"], ensure_ascii=False) if fields.get("files") else None
    values["pinned"] = int(bool(fields.get("pinned")))
    return values


def _decode(row) -> dict:
    message = dict(row)
    message["forward"] = json.loads(message["forward"]) if message["forward"] else None
    message["files"] = json.loads(message["files"]) if message["files"] else []
    message["pinned"] = bool(message["pinned"])
    return message
