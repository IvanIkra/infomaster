import datetime as dt
import sqlite3

import pytest

from uni_master import tablo
from uni_master.outputs.deadlines import SHOWN_KEY
from uni_master.processing.todo import Deadline, uid

TODAY = dt.date(2026, 10, 20)
SCHEMA = """
create table subjects (name text primary key, title text not null, abbr text not null,
                       manual integer not null default 0);
create table own_deadlines (id integer primary key, title text not null, due text not null, category text not null);
create table hidden_deadlines (uid text primary key, source text not null, course text, title text not null,
                               due text not null);
"""


@pytest.fixture
def tablo_db(tmp_path):
    """Tablo's tablo.db (§9 of sources-overview.md), only the tables the move reads."""
    path = tmp_path / "tablo.db"
    db = sqlite3.connect(path)
    db.executescript(SCHEMA)
    hidden = Deadline("Calc HW 3", dt.date(2026, 10, 22), "study", "tg", "Calc")
    with db:
        db.executemany("insert into own_deadlines(title, due, category) values(?, ?, ?)", [
            ("Dentist", "2026-10-25", "personal"), ("Prog  HW 1b", "2026-10-28", "study"),
            ("Long gone", "2026-08-01", "study"), ("Odd", "2026-10-25", "fun")])
        db.executemany("insert into hidden_deadlines values(?, ?, ?, ?, ?)", [
            (uid(hidden), "tg", "Calc", "Calc HW 3", "2026-10-22"),
            ("hseapp-0000000000", "hseapp", None, "Old HW", "2026-07-01")])
        db.executemany("insert into subjects values(?, ?, ?, ?)", [
            ("Дискретная математика", "Discrete Math", "DM", 1), ("Алгебра", "Algebra", "Alg", 1),
            ("Математический анализ", "Calculus", "Calc", 0), ("История", "History", "TooLongAbbr", 1)])
    db.close()
    return path


def test_import(store, tablo_db):
    with store.db:
        store.db.execute("insert into subjects values('Алгебра', 'Linear Algebra', 'LA', 1)")  # set here: wins
        store.db.execute("insert into subjects values('Дискретная математика', 'Discrete', 'D', 0)")  # Haiku's
    store.put_json(SHOWN_KEY, [{"ref": "dl:1", "title": "Calc HW 3", "due": "2026-10-22", "source": "tg",
                                "course": "Calc"}])
    source = tablo.open_tablo(tablo_db)
    counts = tablo.import_tablo(store, source, TODAY)
    assert counts == {"own": 2, "own_skipped": 2, "hidden": 1, "hidden_skipped": 1, "subjects": 1,
                      "subjects_skipped": 2}
    own = [tuple(row) for row in store.db.execute("select id, title, due, category from own_deadlines")]
    assert own == [(1, "Dentist", "2026-10-25", "personal"), (2, "Prog HW 1b", "2026-10-28", "study")]
    hidden = Deadline("Calc HW 3", dt.date(2026, 10, 22), "study", "tg", "Calc")
    assert [tuple(row) for row in store.db.execute("select uid, ref from hidden_deadlines")] == [(uid(hidden), None)]
    subjects = {row["name"]: (row["title"], row["abbr"], row["manual"])
                for row in store.db.execute("select * from subjects")}
    assert subjects == {"Алгебра": ("Linear Algebra", "LA", 1), "Дискретная математика": ("Discrete Math", "DM", 1)}
    # The topic has shown a list: the copied own deadlines join it quietly.
    assert [e["ref"] for e in store.get_json(SHOWN_KEY)] == ["dl:1", "ha-1", "ha-2"]

    # A second run adds nothing.
    assert tablo.import_tablo(store, source, TODAY) == {"own": 0, "own_skipped": 4, "hidden": 0, "hidden_skipped": 2,
                                                        "subjects": 0, "subjects_skipped": 3}
    assert len(store.get_json(SHOWN_KEY)) == 3


def test_import_nothing_shown(store, tablo_db):
    """The topic has shown nothing yet: its first refresh is quiet anyway, the list is left as it is."""
    tablo.import_tablo(store, tablo.open_tablo(tablo_db), TODAY)
    assert store.get_json(SHOWN_KEY) is None


def test_open_tablo(tmp_path, tablo_db):
    with pytest.raises(tablo.TabloError, match="no such file"):
        tablo.open_tablo(tmp_path / "missing.db")
    other = tmp_path / "other.db"
    sqlite3.connect(other).execute("create table subjects (name text)").connection.close()
    with pytest.raises(tablo.TabloError, match="no hidden_deadlines, own_deadlines"):
        tablo.open_tablo(other)
    source = tablo.open_tablo(tablo_db)
    with pytest.raises(sqlite3.OperationalError):  # read-only: Tablo's file is never changed
        source.execute("delete from own_deadlines")
