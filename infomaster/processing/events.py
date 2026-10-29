"""Events from chats and chat corrections of RUZ classes (§6.1, §6.5, D28).

An event of its own is a consultation, a talk, a meeting: something to attend that is not in the timetable. A
correction is a chat message that moves or cancels a RUZ class: it carries the class's `class_uid` and the class's
UID, so the feed shows the class itself moved or cancelled.

Versions: a move, a cancellation or an edit of the message adds a new row with the same `uid` and points the old one
at it (`replaced_by`); the newest one is in force. Withdrawing a version (its messages deleted or edited into
something else) brings back the newest version before it that is not withdrawn itself, if other messages told that
one; a superseded version whose messages are deleted is withdrawn too, so it never comes back. The owner's choices
(`required` once `chosen`, `hidden`) pass to every newer version.

The owner's own events and corrections come from the Mini App (source `own`, D44). An own correction of a class wins
over the chats' corrections of it; the chats' versions go on under it, so they apply again once it is undone. The
batches never see the own ones: the model is not to move them, nor a chat's message to supersede them.

Writing methods of the processing do not commit: call them inside `with store.db`. The owner's choices commit
themselves.
"""

import datetime as dt
import hashlib
import json
import secrets

from .. import clock
from ..store import Store
from .deadlines import same_label

FIELDS = ("uid", "source", "chat_id", "msg_ids", "class_uid", "course", "title", "kind", "start", "end", "room",
          "required", "chosen", "hidden", "cancelled")
IN_FORCE = "replaced_by is null and gone_at is null"
OWN = "own"  # the source of the owner's own events and corrections


def event_uid(source: str, chat_id: int, msg_id: int, n: int = 0) -> str:
    """Stable UID of an event told by a message (§6.5): a hash of the chat and the message; `n` tells apart
    several events of one message."""
    key = f"{chat_id}:{msg_id}" + (f":{n}" if n else "")
    return f"{source}-{hashlib.sha1(key.encode()).hexdigest()[:16]}@infomaster"


def own_uid() -> str:
    """UID of an own event or correction: no message to hash, it is the same through its versions."""
    return f"{OWN}-{secrets.token_hex(8)}@infomaster"


class Events:
    def __init__(self, store: Store):
        self.db = store.db

    def get(self, event_id: int) -> dict | None:
        row = self.db.execute("select * from events where id = ?", (event_id,)).fetchone()
        return _decode(row) if row else None

    def current(self, event_id: int) -> dict | None:
        """The newest version of an event: a button under an old post acts on it."""
        event = self.get(event_id)
        seen = set()
        while event is not None and event["replaced_by"] is not None and event["id"] not in seen:
            seen.add(event["id"])
            event = self.get(event["replaced_by"])
        return event

    def previous(self, event_id: int) -> dict | None:
        row = self.db.execute("select * from events where replaced_by = ? order by id desc", (event_id,)).fetchone()
        return _decode(row) if row else None

    def in_force_all(self) -> list[dict]:
        rows = self.db.execute(f"select * from events where {IN_FORCE} order by start, id")
        return [_decode(row) for row in rows]

    def upcoming(self, today: dt.date) -> list[dict]:
        """Events of their own in force from today on, by time: the batches show them to the model. The owner's own
        are left out: they are not the model's to move."""
        rows = self.db.execute(f"select * from events where {IN_FORCE} and class_uid is null and start >= ? "
                               "and source != ? order by start, id", (today.isoformat(), OWN))
        return [_decode(row) for row in rows]

    def corrections(self) -> dict[str, dict]:
        """The correction in force of each corrected class, by its uid: the owner's own one, else the chats'
        newest."""
        rows = self.db.execute(f"select * from events where {IN_FORCE} and class_uid is not null "
                               "order by source = ?, id", (OWN,))
        return {row["class_uid"]: _decode(row) for row in rows}

    def correction(self, class_uid: str) -> dict | None:
        """The chats' correction in force of a class: the one a chat's new message supersedes."""
        row = self.db.execute(f"select * from events where {IN_FORCE} and class_uid = ? and source != ? "
                              "order by id desc", (class_uid, OWN)).fetchone()
        return _decode(row) if row else None

    def own_correction(self, class_uid: str) -> dict | None:
        row = self.db.execute(f"select * from events where {IN_FORCE} and class_uid = ? and source = ?",
                              (class_uid, OWN)).fetchone()
        return _decode(row) if row else None

    def find(self, title: str, start: str) -> dict | None:
        """An event of its own in force with the same title and start, from any chat."""
        rows = self.db.execute(f"select * from events where {IN_FORCE} and class_uid is null and start = ?",
                               (start,))
        return next((_decode(row) for row in rows if same_label(row["title"], title)), None)

    def by_uid(self, uid: str) -> dict | None:
        row = self.db.execute(f"select * from events where {IN_FORCE} and uid = ?", (uid,)).fetchone()
        return _decode(row) if row else None

    def made_of(self, source: str, chat_id: int, msg_id: int) -> list[dict]:
        rows = self.db.execute(
            f"select * from events where source = ? and chat_id = ? and {IN_FORCE} "
            "and exists (select 1 from json_each(events.msg_ids) where value = ?)", (source, chat_id, msg_id))
        return [_decode(row) for row in rows]

    def versions(self, source: str, chat_id: int, since: dt.datetime) -> list[dict]:
        """Versions not withdrawn, in force or superseded, by id: those whose messages are deleted are withdrawn
        oldest first, so a superseded one is gone before a newer one could bring it back."""
        rows = self.db.execute("select * from events where source = ? and chat_id = ? and gone_at is null "
                               "and created_at >= ? order by id", (source, chat_id, clock.local_iso(since)))
        return [_decode(row) for row in rows]

    def course_names(self, today: dt.date) -> set[str]:
        """Courses of events in force from today on: the dictionary names them (§6.2)."""
        rows = self.db.execute(f"select distinct course from events where {IN_FORCE} and course is not null "
                               "and start >= ?", (today.isoformat(),))
        return {row["course"] for row in rows}

    # --- writing ----------------------------------------------------------------------

    def add(self, event: dict, at: dt.datetime) -> int:
        """`event`: uid, source, chat_id, msg_ids, class_uid, course, title, kind, start, end, room, required;
        optionally chosen, hidden, cancelled."""
        values = {"chosen": False, "hidden": False, "cancelled": False, "class_uid": None, **event}
        values["msg_ids"] = json.dumps(values["msg_ids"])
        values["course"] = values["course"] or None
        values["end"] = values["end"] or None
        for flag in ("required", "chosen", "hidden", "cancelled"):
            values[flag] = int(bool(values[flag]))
        cursor = self.db.execute(
            f"insert into events({', '.join(FIELDS)}, created_at) values({', '.join('?' * len(FIELDS))}, ?)",
            (*(values[f] for f in FIELDS), clock.local_iso(at)))
        return cursor.lastrowid

    def supersede(self, old_id: int, new_id: int) -> bool:
        """The new row is the next version of the old one in force; the owner's choices pass to it."""
        old = self.get(old_id)
        if old is None or old["replaced_by"] is not None or old["gone_at"] is not None:
            return False
        self.db.execute("update events set replaced_by = ? where id = ?", (new_id, old_id))
        self.db.execute("update events set hidden = ? where id = ?", (int(old["hidden"]), new_id))
        if old["chosen"]:
            self.db.execute("update events set required = ?, chosen = 1 where id = ?", (int(old["required"]), new_id))
        return True

    def drop(self, event_ids: list[int], at: dt.datetime) -> list[int]:
        """Withdraw versions; for one in force, the newest version before it comes back if other messages told it
        and it is not withdrawn itself. Returns the ids of the versions that came back."""
        restored = []
        for event_id in event_ids:
            event = self.get(event_id)
            if event is None or event["gone_at"] is not None:
                continue
            self.db.execute("update events set gone_at = ? where id = ?", (clock.local_iso(at), event_id))
            if event["replaced_by"] is not None:
                continue
            earlier = self.previous(event_id)
            # Past an earlier text of the same message, and past versions withdrawn already (their messages deleted
            # while a newer version was in force).
            while earlier is not None and (_same_messages(earlier, event) or earlier["gone_at"] is not None):
                earlier = self.previous(earlier["id"])
            if earlier is not None:
                self.db.execute("update events set replaced_by = null where id = ?", (earlier["id"],))
                restored.append(earlier["id"])
        return restored

    def withdraw(self, uid: str, at: dt.datetime) -> None:
        """Every version of an own event or correction is gone: none comes back, as with a deleted message."""
        self.db.execute("update events set gone_at = ? where uid = ? and source = ? and gone_at is null",
                        (clock.local_iso(at), uid, OWN))

    def overrule(self, class_uid: str, at: dt.datetime) -> list[int]:
        """RUZ changed the class itself, so it wins (§6.5): every correction of it is gone for good.
        Returns the corrections that were in force."""
        in_force = [row["id"] for row in self.db.execute(
            f"select id from events where class_uid = ? and {IN_FORCE}", (class_uid,))]
        self.db.execute("update events set gone_at = ? where class_uid = ? and gone_at is null",
                        (clock.local_iso(at), class_uid))
        return in_force

    # --- the owner's choices (the forum buttons, the Mini App) ----------------------------

    def set_required(self, event_id: int, required: bool) -> dict | None:
        """Main or optional calendar for the newest version; returns it. A correction stays a class: main."""
        event = self.current(event_id)
        if event is None or event["class_uid"] is not None:
            return event
        with self.db:
            self.db.execute("update events set required = ?, chosen = 1 where id = ?", (int(required), event["id"]))
        return self.get(event["id"])

    def set_hidden(self, event_id: int, hidden: bool) -> dict | None:
        event = self.current(event_id)
        if event is None:
            return None
        with self.db:
            self.db.execute("update events set hidden = ? where id = ?", (int(hidden), event["id"]))
        return self.get(event["id"])


def _same_messages(a: dict, b: dict) -> bool:
    return a["source"] == b["source"] and a["chat_id"] == b["chat_id"] and bool(set(a["msg_ids"]) & set(b["msg_ids"]))


def _decode(row) -> dict:
    event = dict(row)
    event["msg_ids"] = json.loads(event["msg_ids"])
    for flag in ("required", "chosen", "hidden", "cancelled"):
        event[flag] = bool(event[flag])
    return event
