"""The journal of what the connector changed (§8.5): every write tool's call with a line about it and the steps that
undo it. «Machine → Claude connector» in the web app lists it with **Undo**: Claude reads messages strangers wrote, and
a change it was talked into must be easy to see and to take back. The web app's changes (§8.6) go through the same
tools and into the same journal (`via` web), so the owner's taps can be undone the same way.

An undo runs its steps through the same tools (connector/tools.py) at the time it is asked for. A step fails when what
it acts on has changed since (a deadline that left its source, an event the chat withdrew): the error is kept and
shown, and the change stays as it is.
"""

import datetime as dt
import json
from collections.abc import Callable

from .. import clock
from ..store import Store

SCHEMA = """
create table if not exists connector_changes (  -- every write through the MCP connector (§8.5)
    id integer primary key,
    at text not null,           -- local time
    grant_id integer,           -- the connection it came through (connector_grants)
    tool text not null,
    arguments text not null,    -- JSON, as the client sent them
    summary text not null,      -- one line for the web app
    undo text,                  -- JSON [{tool, arguments}]; null when it cannot be undone
    note text not null default '',  -- why not, or what an undo does not bring back
    undone_at text,
    undo_error text,            -- the last failed undo
    via text not null default 'claude'  -- claude: through the connector; web: the owner in the web app (§8.6)
);
"""
KEEP = 200  # changes listed and kept
ARGUMENTS_CHARS = 4000


class Changes:
    def __init__(self, store: Store, now: Callable[[], dt.datetime] = clock.now_utc):
        self.db, self.now = store.db, now
        self.db.executescript(SCHEMA)
        with self.db:
            if "via" not in {row["name"] for row in self.db.execute("pragma table_info(connector_changes)")}:
                self.db.execute("alter table connector_changes add column via text not null default 'claude'")

    def record(self, grant_id: int | None, tool: str, arguments: dict, summary: str, undo: list[dict] | None,
               note: str, via: str = "claude") -> int:
        text = json.dumps(arguments, ensure_ascii=False)
        with self.db:
            change_id = self.db.execute(
                "insert into connector_changes(at, grant_id, tool, arguments, summary, undo, note, via) "
                "values(?, ?, ?, ?, ?, ?, ?, ?)",
                (clock.local_iso(self.now()), grant_id, tool, text[:ARGUMENTS_CHARS], summary,
                 json.dumps(undo, ensure_ascii=False) if undo is not None else None, note, via)).lastrowid
            self.db.execute("delete from connector_changes where id <= ?", (change_id - KEEP,))
        return change_id

    def get(self, change_id: int) -> dict | None:
        row = self.db.execute("select * from connector_changes where id = ?", (change_id,)).fetchone()
        return _decode(row) if row else None

    def recent(self, limit: int = 50, via: str | None = None) -> list[dict]:
        """The newest changes first; only those made one way (`via`) when it is given."""
        if via is None:
            rows = self.db.execute("select * from connector_changes order by id desc limit ?", (limit,))
        else:
            rows = self.db.execute("select * from connector_changes where via = ? order by id desc limit ?",
                                   (via, limit))
        return [_decode(row) for row in rows]

    def undone(self, change_id: int, error: str | None) -> None:
        with self.db:
            if error is None:
                self.db.execute("update connector_changes set undone_at = ?, undo_error = null where id = ?",
                                (clock.local_iso(self.now()), change_id))
            else:
                self.db.execute("update connector_changes set undo_error = ? where id = ?", (error, change_id))


def _decode(row) -> dict:
    change = dict(row)
    try:
        change["arguments"] = json.loads(change["arguments"])
    except ValueError:  # cut at ARGUMENTS_CHARS
        change["arguments"] = {"cut": change["arguments"]}
    change["undo"] = json.loads(change["undo"]) if change["undo"] is not None else None
    return change
