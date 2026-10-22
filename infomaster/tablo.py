"""The move from Tablo (stage 10, the checklist of decisions.md): what only Tablo's database holds comes here once.

python -m infomaster import-tablo --tablo-db <tablo.db> copies, from a copy of Tablo's SQLite file (§9 of
sources-overview.md; the service is stopped, the file is opened read-only):
- the own deadlines of HA's to-do lists (`own_deadlines`): those not here yet (the same title, date and list), so a
  second run adds nothing. HA reads the list anew after the switch, so the new ids (ha-<id>) do not matter;
- the hidden ones (`hidden_deadlines`): the uid is made the same way here (source, title, date), so they are kept by
  it; Tablo did not know the row a hidden deadline came from, so it hides by title and date only, as in Tablo;
- the dictionary entries set by hand (`subjects` with `manual`; Tablo keeps them in the table in step with its config):
  one set by hand here already wins, Haiku's answer here gives way.
Rows 30 days past their date are skipped: the next change of the lists would remove them anyway.

The «Дедлайны» topic must not post the copied own deadlines as new ones: when it has shown a list already, they are
added to the list it showed (kv "deadlines.shown"), as the owner's changes are quiet (outputs/deadlines.py).
"""

import datetime as dt
import sqlite3
from pathlib import Path

from .outputs.deadlines import SHOWN_KEY, entry
from .processing.subjects import ABBR_CHARS, MANUAL_TITLE_CHARS, clean
from .processing.todo import CATEGORIES, KEEP_DAYS, OWN_PREFIX, Deadline
from .store import Store


class TabloError(ValueError):
    pass


def open_tablo(path: Path) -> sqlite3.Connection:
    if not path.is_file():
        raise TabloError(f"no such file: {path}")
    db = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    db.row_factory = sqlite3.Row
    tables = {row["name"] for row in db.execute("select name from sqlite_master where type = 'table'")}
    if missing := {"own_deadlines", "hidden_deadlines", "subjects"} - tables:
        db.close()
        raise TabloError(f"not Tablo's database: no {', '.join(sorted(missing))}")
    return db


def import_tablo(store: Store, tablo: sqlite3.Connection, today: dt.date) -> dict[str, int]:
    """Copies what is not here yet; returns how many of each were added and skipped."""
    since = (today - dt.timedelta(days=KEEP_DAYS)).isoformat()
    counts = dict.fromkeys(("own", "own_skipped", "hidden", "hidden_skipped", "subjects", "subjects_skipped"), 0)
    added: list[Deadline] = []
    db = store.db
    with db:
        for row in tablo.execute("select title, due, category from own_deadlines order by due, id"):
            title, due, category = " ".join(str(row["title"]).split()), str(row["due"]), row["category"]
            if (not title or category not in CATEGORIES or not _is_date(due) or due < since
                    or db.execute("select 1 from own_deadlines where title = ? and due = ? and category = ?",
                                  (title, due, category)).fetchone()):
                counts["own_skipped"] += 1
                continue
            cursor = db.execute("insert into own_deadlines(title, due, category) values(?, ?, ?)",
                                (title, due, category))
            key = f"{OWN_PREFIX}{cursor.lastrowid}"
            added.append(Deadline(title, dt.date.fromisoformat(due), category, "ha", key=key, ref=key))
            counts["own"] += 1

        for row in tablo.execute("select uid, source, course, title, due from hidden_deadlines"):
            due = str(row["due"])
            if not _is_date(due) or due < since or not db.execute(
                    "insert or ignore into hidden_deadlines(uid, source, course, title, due) values(?, ?, ?, ?, ?)",
                    (row["uid"], row["source"], row["course"], row["title"], due)).rowcount:
                counts["hidden_skipped"] += 1
            else:
                counts["hidden"] += 1

        for row in tablo.execute("select name, title, abbr from subjects where manual order by name"):
            name, title, abbr = clean(row["name"]), " ".join(str(row["title"]).split()), str(row["abbr"]).strip()
            if (not name or not title or len(title) > MANUAL_TITLE_CHARS or not abbr or len(abbr) > ABBR_CHARS
                    or not db.execute(
                        "insert into subjects(name, title, abbr, manual) values(?, ?, ?, 1) on conflict(name) do "
                        "update set title = excluded.title, abbr = excluded.abbr, manual = 1 where not subjects.manual",
                        (name, title, abbr)).rowcount):
                counts["subjects_skipped"] += 1
            else:
                counts["subjects"] += 1

        shown = store.get_json(SHOWN_KEY)
        if shown is not None and added:  # nothing shown yet: the first refresh is quiet anyway
            store.put_json(SHOWN_KEY, [*shown, *map(entry, added)])
    return counts


def _is_date(value: str) -> bool:
    try:
        dt.date.fromisoformat(value)
    except ValueError:
        return False
    return True
