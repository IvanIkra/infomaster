import datetime as dt
from pathlib import Path

import pytest

from infomaster.collectors.ruz import Change, Diff, parse
from infomaster.llm.client import LLMError
from infomaster.processing.events import event_uid
from infomaster.processing.schedule import type_of
from infomaster.store import Store

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()
CHAT = -1001234567890


def event(**values) -> dict:
    return {"uid": event_uid("tg", CHAT, 1), "source": "tg", "chat_id": CHAT,
            "msg_ids": [1], "course": "Алгебра", "title": "Alg consult", "kind": "consultation",
            "start": "2026-10-23T18:10:00", "end": None, "room": "R205", "required": True, **values}


def add(schedule, now, **values) -> int:
    with schedule.db:
        return schedule.events.add(event(**values), now())


def correction(schedule, now, uid, **values) -> int:
    item = schedule.ruz.by_uid(uid)
    return add(schedule, now, **{"uid": uid, "class_uid": uid, "course": item["subject"], "title": "Discrete Math",
                                 "kind": item["kind"], "start": item["start"], "end": item["end"],
                                 "room": item["room"], **values})


@pytest.fixture
def loaded(schedule):
    schedule.inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    schedule.ruz._sync(parse(FEED))
    return schedule


@pytest.mark.parametrize("kind, of_class, expected", [
    ("Лекция", True, "Lecture"), ("Lecture", True, "Lecture"), ("Проведение семинаров (студенты)", True, "Seminar"),
    ("Практические занятия", True, "Seminar"), ("Ongoing interim assessment", True, "Interim assessment"),
    ("Текущий рубежный контроль", True, "Interim assessment"), ("Экзамен", True, "Exam"),
    ("Консультация", True, "Consultation"), ("consultation", False, "Consultation"), ("talk", False, "Talk"),
    ("Лабораторная работа", True, "Lab"), ("Что-то новое", True, "Class"), ("", False, "Event"),
    ("встреча", False, "Event")])
def test_type_of(kind, of_class, expected):
    assert type_of(kind, of_class) == expected


async def test_class_entries(loaded):
    entries = {e.uid[6]: e for e in loaded.entries()}
    assert entries["1"].title == "Математический анализ"  # not named yet: as the feed has it
    await loaded.after_poll(Diff(first=True))
    entries = {e.uid[6]: e for e in loaded.entries()}
    calc, statehood, dm = entries["1"], entries["3"], entries["4"]
    assert (calc.title, calc.type, calc.location, calc.source, calc.required) == (
        "Calculus", "Seminar", "D208", "ruz", True)
    assert (calc.start, calc.end) == (dt.datetime(2026, 10, 19, 9, 30), dt.datetime(2026, 10, 19, 10, 50))
    assert statehood.location == "https://my.mts-link.ru/j/11111111/22222222222"  # online: the link
    assert dm.location == "https://example.ktalk.ru/abcdef" and dm.type == "Seminar"
    assert entries["8"].cancelled and entries["6"].type == "Interim assessment" and entries["6"].location == ""
    assert [e.start for e in loaded.entries()] == sorted(e.start for e in loaded.entries())


async def test_corrections_apply_until_ruz_wins(loaded, now):
    uid = "2000004@ruz.hse.ru"  # Discrete Mathematics, Thu Oct 22 16:40, online
    moved = correction(loaded, now, uid, start="2026-10-23T13:00:00", end="2026-10-23T14:20:00", room="R405")
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert (entry.start, entry.location, entry.source) == (dt.datetime(2026, 10, 23, 13, 0), "R405", "tg")
    assert entry.notes == ["Moved from Thu Oct 22 16:40 in a chat"]
    assert entry.link == "https://t.me/c/1234567890/1"

    loaded.events.set_hidden(moved, True)  # the owner says the model was wrong
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert entry.start == dt.datetime(2026, 10, 22, 16, 40) and entry.source == "ruz" and entry.notes == []
    loaded.events.set_hidden(moved, False)

    with loaded.db:
        cancelled = loaded.events.add(event(uid=uid, class_uid=uid, title="Discrete Math", start="2026-10-22T16:40:00",
                                            end="2026-10-22T18:00:00", room="online", cancelled=True, msg_ids=[2]),
                                      now())
        loaded.events.supersede(moved, cancelled)
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert entry.cancelled and entry.notes == ["Cancelled in a chat"] and entry.link.endswith("/2")

    item = loaded.ruz.by_uid(uid)
    await loaded.after_poll(Diff(changes=[Change("new", None, item)]))  # a new class is no change of this one
    assert loaded.events.correction(uid)["id"] == cancelled
    await loaded.after_poll(Diff(changes=[Change("room", item, item)]))
    assert loaded.events.correction(uid) is None and loaded.events.get(moved)["gone_at"] is not None
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert not entry.cancelled and entry.source == "ruz"


def test_moved_back(loaded, now):
    uid = "2000005@ruz.hse.ru"
    item = loaded.ruz.by_uid(uid)
    correction(loaded, now, uid)  # a chat put it back where RUZ has it
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert entry.notes == [] and entry.source == "ruz" and entry.start == dt.datetime.fromisoformat(item["start"])


def test_event_entries(loaded, now):
    consult = add(loaded, now)
    talk = add(loaded, now, uid="talk@x", title="Career fair", kind="event", start="2026-10-24T12:00:00",
               end="2026-10-24T16:00:00", room="", required=False, course="")
    add(loaded, now, uid="hidden@x", title="Club", kind="meeting", hidden=True)
    entries = {e.uid: e for e in loaded.entries()}
    first = entries[loaded.events.get(consult)["uid"]]
    assert (first.title, first.type, first.location, first.required, first.source) == (
        "Alg consult", "Consultation", "R205", True, "tg")
    assert first.end - first.start == dt.timedelta(minutes=80) and first.notes == []  # the course is not named yet
    assert not entries["talk@x"].required and entries["talk@x"].notes == [] and "hidden@x" not in entries
    with loaded.db:
        later = loaded.events.add(event(start="2026-10-24T18:10:00", msg_ids=[3]), now())
        loaded.events.supersede(consult, later)
    loaded.subjects.set_manual("Алгебра", "Algebra", "Alg")
    entries = {e.uid: e for e in loaded.entries()}
    assert entries[loaded.events.get(consult)["uid"]].notes == ["Algebra", "Moved from Fri Oct 23 18:10"]
    assert entries["talk@x"].type == "Event" and loaded.events.get(talk)["uid"] == "talk@x"


def test_versions(loaded, now):
    first = add(loaded, now, msg_ids=[1])
    loaded.events.set_required(first, False)
    loaded.events.set_hidden(first, True)
    with loaded.db:
        moved = loaded.events.add(event(start="2026-10-24T18:10:00", msg_ids=[5], required=True), now())
        assert loaded.events.supersede(first, moved)
        assert not loaded.events.supersede(first, moved)  # not in force any more
    assert loaded.events.current(first)["id"] == moved
    version = loaded.events.get(moved)
    assert (version["required"], version["chosen"], version["hidden"], version["uid"]) == (
        False, True, True, loaded.events.get(first)["uid"])

    with loaded.db:
        edited = loaded.events.add(event(start="2026-10-25T18:10:00", msg_ids=[5]), now())  # message 5 edited
        loaded.events.supersede(moved, edited)
        assert loaded.events.drop([edited], now()) == [first]  # message 5 is gone: the first version is back
    assert loaded.events.by_uid(version["uid"])["id"] == first
    with loaded.db:
        assert loaded.events.drop([first], now()) == []
    assert loaded.events.by_uid(version["uid"]) is None

    fix = correction(loaded, now, "2000005@ruz.hse.ru")
    assert loaded.events.set_required(fix, False)["required"] is True  # a class stays in the main calendar


async def test_midterms_follow_their_classes(loaded, now, namer):
    namer.error = LLMError("ANTHROPIC_API_KEY is not set")
    changes = await loaded.after_poll(Diff(first=True))
    assert loaded.subjects.error == "ANTHROPIC_API_KEY is not set"
    midterms = [loaded.deadlines.get(i) for i in changes.deadlines]
    assert [(d["label"], d["due"], d["course"], d["source"]) for d in midterms] == [
        ("Foundations of Russian Statehood Midterm", "2026-10-26", "Foundations of Russian Statehood", "ical"),
        ("Введение в программирование Midterm", "2026-10-29", "Введение в программирование", "ical")]
    assert loaded.deadlines.upcoming(dt.date(2026, 10, 20)) == []  # not for the batch model to move

    namer.error = None
    changes = await loaded.after_poll(Diff())  # a 304: the names get another try
    assert not changes and loaded.subjects.error is None
    assert [loaded.deadlines.get(i)["label"] for i in (midterms[0]["id"], midterms[1]["id"])] == [
        "RSF Midterm", "Prog Midterm"]  # renamed in place: no news

    db = loaded.db
    with db:
        db.execute("update classes set start = '2026-10-27T12:30:00' where uid = '2000006@ruz.hse.ru'")
        db.execute("update classes set cancelled = 1 where uid = '2000007@ruz.hse.ru'")
    changes = loaded.sync_midterms()
    [(old, new)] = changes.moved
    assert old == midterms[0]["id"] and loaded.deadlines.get(new)["due"] == "2026-10-27"
    assert changes.gone == [midterms[1]["id"]] and loaded.deadlines.get(midterms[1]["id"])["gone_at"]

    with db:
        db.execute("update classes set gone_at = '2026-10-20T12:00:00' where uid = '2000006@ruz.hse.ru'")
        db.execute("update classes set cancelled = 0 where uid = '2000007@ruz.hse.ru'")
    changes = loaded.sync_midterms()
    assert changes.gone == [new] and len(changes.deadlines) == 1  # the class is back: its deadline too
    assert not loaded.sync_midterms()


async def test_midterms_follow_chat_corrections(loaded, now):
    await loaded.after_poll(Diff(first=True))
    uid = "2000007@ruz.hse.ru"  # Введение в программирование, Thu Oct 29 10:00-13:00, the interim assessment
    first = loaded.ruz.by_uid(uid)["deadline_id"]
    assert (loaded.deadlines.get(first)["label"], loaded.deadlines.get(first)["due"]) == ("Prog Midterm", "2026-10-29")
    published = []

    async def record(midterms):
        published.append(midterms)

    loaded.on_midterms = record
    moved = correction(loaded, now, uid, title="Intro to Prog", start="2026-11-05T10:00:00", end="2026-11-05T13:00:00")
    [(old, new)] = loaded.sync_midterms().moved  # a chat moved the class: its Midterm goes with it
    assert old == first and (loaded.deadlines.get(new)["label"], loaded.deadlines.get(new)["due"]) == (
        "Prog Midterm", "2026-11-05")
    assert not loaded.sync_midterms()

    assert (await loaded.set_hidden(moved, True))["hidden"]  # the owner: the model was wrong, RUZ's date is back
    [midterms] = published
    [(old, back)] = midterms.moved
    assert old == new and loaded.deadlines.get(back)["due"] == "2026-10-29"
    await loaded.set_hidden(moved, False)
    [(_, again)] = published[1].moved
    assert loaded.deadlines.get(again)["due"] == "2026-11-05"

    with loaded.db:
        cancelled = loaded.events.add(event(uid=uid, class_uid=uid, title="Intro to Prog", start="2026-11-05T10:00:00",
                                            end="2026-11-05T13:00:00", room="R401", cancelled=True, msg_ids=[2]),
                                      now())
        loaded.events.supersede(moved, cancelled)
    changes = loaded.sync_midterms()  # a chat cancelled it: no Midterm
    assert changes.gone == [again] and loaded.deadlines.get(again)["gone_at"] and changes.moved == []
    assert loaded.ruz.by_uid(uid)["deadline_id"] is None and not loaded.sync_midterms()

    await loaded.set_hidden(cancelled, True)  # the cancellation is not applied: the class and its Midterm are back
    [restored] = published[2].deadlines
    assert loaded.deadlines.get(restored)["due"] == "2026-10-29"
    own = add(loaded, now, uid="own@x", msg_ids=[3])
    await loaded.set_hidden(own, True)  # an event of its own has no Midterm
    assert len(published) == 3


async def test_a_failing_midterm_post_does_not_stop_hiding(loaded, now):
    await loaded.after_poll(Diff(first=True))

    async def broken(midterms):
        raise RuntimeError("the forum is down")

    loaded.on_midterms = broken
    moved = correction(loaded, now, "2000007@ruz.hse.ru", start="2026-11-05T10:00:00", end="2026-11-05T13:00:00")
    loaded.sync_midterms()
    assert (await loaded.set_hidden(moved, True))["hidden"]
    assert loaded.deadlines.get(loaded.ruz.by_uid("2000007@ruz.hse.ru")["deadline_id"])["due"] == "2026-10-29"


async def test_past_midterm_is_no_news(schedule):
    schedule.ruz._sync(parse(FEED))
    schedule.now.moment += dt.timedelta(days=7)  # Oct 27: the Oct 26 assessment is past
    changes = await schedule.after_poll(Diff())
    assert [schedule.deadlines.get(i)["due"] for i in changes.deadlines] == ["2026-10-29"]


def test_agenda(loaded, now):
    add(loaded, now, uid="past@x", title="Past talk", start="2026-10-19T18:00:00", msg_ids=[7])
    talk = add(loaded, now, end="2026-10-23T19:00:00")
    hidden = add(loaded, now, uid="hidden@x", title="Debate club", start="2026-10-24T12:00:00", msg_ids=[8],
                 hidden=True)
    fix = correction(loaded, now, "2000004@ruz.hse.ru", start="2026-10-21T10:00:00", end="2026-10-21T11:20:00")
    agenda = loaded.agenda(dt.date(2026, 10, 20))
    items = {item["uid"]: item for item in agenda["items"]}
    assert "2000001@ruz.hse.ru" not in items and "past@x" not in items  # Oct 19: past
    moved = items["2000004@ruz.hse.ru"]  # by its corrected time, Wed, with RUZ's own Thu behind it
    assert (moved["start"], moved["source"], moved["event"]["id"], moved["event"]["correction"]) == (
        "2026-10-21T10:00:00", "tg", fix, True)
    assert moved["event"]["class"]["start"] == "2026-10-22T16:40:00" and moved["event"]["chat"] == "Матан"
    told = items[event_uid("tg", CHAT, 1)]
    assert (told["event"]["id"], told["event"]["correction"], told["end"], told["link"]) == (
        talk, False, "2026-10-23T19:00:00", "https://t.me/c/1234567890/1")
    assert items["2000005@ruz.hse.ru"]["event"] is None and "hidden@x" not in items
    assert [(h["id"], h["type"]) for h in agenda["hidden"]] == [(hidden, "Consultation")]
    assert "2000009@ruz.hse.ru" in items  # Nov 3: within four weeks
    assert "2000009@ruz.hse.ru" not in {i["uid"] for i in loaded.agenda(dt.date(2026, 10, 20), days=7)["items"]}


async def test_own_events_and_corrections(loaded, now):
    """The owner's own events and corrections (D44)."""
    talk = add(loaded, now)
    own = loaded.add_event({"title": "Supervisor", "kind": "Event", "start": "2026-10-24T15:00:00",
                            "end": "2026-10-24T16:00:00", "room": "R205", "required": False})
    [item] = [i for i in loaded.agenda(dt.date(2026, 10, 20))["items"] if i["title"] == "Supervisor"]
    assert (item["source"], item["event"]["own"], item["required"]) == ("own", True, False)
    newer = loaded.edit_event(own, {"start": "2026-10-24T17:00:00", "end": "2026-10-24T18:00:00"})
    assert loaded.events.get(own)["replaced_by"] == newer
    assert loaded.events.get(newer)["uid"] == loaded.events.get(own)["uid"]

    # A chat's event changed: hidden, an own copy in its place; the batches see neither the copy nor any own event.
    copy = loaded.edit_event(talk, {"title": "Alg consult, moved", "start": "2026-10-23T19:00:00",
                                    "end": "2026-10-23T20:00:00"})
    assert loaded.events.get(talk)["hidden"] and loaded.events.get(talk)["replaced_by"] is None
    assert (loaded.events.get(copy)["source"], loaded.events.get(copy)["course"]) == ("own", "Алгебра")
    assert [e["id"] for e in loaded.events.upcoming(dt.date(2026, 10, 20))] == [talk]

    # The owner's move of a class wins over the chats' corrections, a later one too; undone, the chats' newest applies.
    uid = "2000005@ruz.hse.ru"
    chats = correction(loaded, now, uid, start="2026-10-23T11:10:00", end="2026-10-23T12:30:00")
    mine = await loaded.correct_class(uid, "2026-10-23T15:00:00", "2026-10-23T16:20:00", "R510", False)
    later = correction(loaded, now, uid, msg_ids=[9], start="2026-10-23T13:00:00", end="2026-10-23T14:20:00")
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert (entry.start, entry.location, entry.source) == (dt.datetime(2026, 10, 23, 15), "R510", "own")
    assert entry.notes == ["Moved from Fri Oct 23 09:30 by hand"]
    assert loaded.events.correction(uid)["id"] == later and chats  # what a chat's next message supersedes
    again = await loaded.correct_class(uid, "2026-10-23T15:00:00", "2026-10-23T16:20:00", "R510", True)
    assert loaded.events.get(mine)["replaced_by"] == again
    assert (await loaded.remove_own(again))["id"] == again
    [entry] = [e for e in loaded.entries() if e.uid == uid]
    assert entry.start == dt.datetime(2026, 10, 23, 13) and not entry.cancelled

    # Deleted, an own event does not come back with an older version; the chats' ones are hidden, never deleted.
    assert (await loaded.remove_own(newer))["id"] == newer
    assert all(e["title"] != "Supervisor" for e in loaded.events.in_force_all())
    assert await loaded.remove_own(talk) is None and loaded.edit_event(later, {"title": "x"}) is None
    assert await loaded.correct_class("x@ruz.hse.ru", "2026-10-23T15:00:00", "2026-10-23T16:00:00", "", False) is None


async def test_own_correction_moves_the_midterm(loaded):
    await loaded.after_poll(Diff(first=True))
    published = []

    async def record(midterms):
        published.append(midterms)

    loaded.on_midterms = record
    uid = "2000007@ruz.hse.ru"  # the interim assessment, Thu Oct 29
    mine = await loaded.correct_class(uid, "2026-11-05T10:00:00", "2026-11-05T13:00:00", "R401", False)
    [(_, moved)] = published[0].moved
    assert loaded.deadlines.get(moved)["due"] == "2026-11-05"
    await loaded.remove_own(mine)
    [(_, back)] = published[1].moved
    assert loaded.deadlines.get(back)["due"] == "2026-10-29"


def test_new_tables_join_an_older_database(tmp_path):
    path = tmp_path / "infomaster.db"
    store = Store(path)
    with store.db:
        store.db.execute("insert into deadlines(source, label, due, created_at) values('tg', 'DM HW 3', "
                         "'2026-10-21', '2026-10-20T12:00:00')")
        for table in ("subjects", "classes", "events"):
            store.db.execute(f"drop table {table}")  # as stage 3 left it
    store.close()
    store = Store(path)
    assert store.db.execute("select count(*) n from deadlines").fetchone()["n"] == 1
    assert {row["name"] for row in store.db.execute("select name from sqlite_master where type = 'table'")} >= {
        "subjects", "classes", "events", "deadlines", "messages"}
