"""The web app's corrections (§8.6, stage 16.9): a class moved, cancelled and put in the optional calendar, a chat's
correction ignored, an event changed, hidden and deleted, the hidden lists with «Restore», and grades corrected; each
through the connector's tools, with its undo."""

import actions_app
import pytest
from test_grades import URL, item
from test_http import TWIN, add_row, env
from test_web import FEED, call, log_in
from test_web_actions import Seconds

from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import parse
from infomaster.connector import Connector
from infomaster.processing.grades import VIEW, Grades, Item
from infomaster.processing.news import News
from infomaster.processing.todo import uid
from infomaster.settings import ChatSettings
from infomaster.web import WebApp
from infomaster.web.views import Data


@pytest.fixture
async def fixer(aiohttp_client, store, budget, journal, schedule, merged, now):
    """The public port with the timetable of the fixture (Tuesday 20.10.2026, noon), a gradebook, the connector and
    the web app, a logged-in device."""
    schedule.ruz._sync(parse(FEED))
    schedule.subjects.set_manual("Discrete Mathematics", "Discrete Mathematics", "DM")
    schedule.inbox.put_chat(-1001, "DM seminar chat", "supergroup", None, 0)
    sent, seconds = [], Seconds()

    async def send_code(text):
        sent.append(text)

    settings = budget.settings
    grades = Grades(store, settings, None, now=now)
    hse = HseApp(store, settings, env().public_url + CALLBACK_PATH)
    app = actions_app.app(env(), budget, journal, schedule.inbox, ChatSettings(store), TWIN, schedule.subjects,
                             schedule, hse, merged, settings, None, grades)
    connector = Connector(env(), store, News(store), now=now, seconds=seconds)
    connector.attach(app)
    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "en", now=seconds,
                    data=Data(schedule, merged, grades=grades, lang=lambda: "en"))
    webapp.connector = connector
    client = await aiohttp_client(webapp.attach(app))
    client.token = await log_in(client, sent, seconds)
    client.connector, client.grades, client.settings = connector, grades, settings
    return client


async def ask(client, method, path, data=None, status=200):
    response = await call(client, method, path, data, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def undo(client, answer):
    await ask(client, "POST", f"/changes/{answer['change']['id']}/undo", {})


def entries(feed) -> dict[str, dict]:
    return {i["title"]: i for day in feed["days"] for i in day["items"] if i["kind"] == "class"}


async def seminar(client) -> dict:
    """The card of Thursday's Discrete Mathematics seminar."""
    found = entries(await ask(client, "GET", "/feed"))["Discrete Mathematics"]
    return await ask(client, "GET", "/class?uid=" + found["uid"])


def chat_event(schedule, **fields) -> int:
    values = {"uid": "tg-ev1@infomaster", "source": "tg", "chat_id": -1001, "msg_ids": [7], "course": None,
              "title": "Consultation", "kind": "Consultation", "start": "2026-10-24T15:00:00",
              "end": "2026-10-24T16:00:00", "room": "R205", "required": True, **fields}
    with schedule.db:
        return schedule.events.add(values, schedule.now())


async def test_class_moved_cancelled_and_back(fixer, schedule):
    card = await seminar(fixer)
    fix = card["fix"]
    assert fix["of"] == "class" and fix["own"] is None and fix["chat"] is None
    assert fix["fields"] == {"start": "2026-10-22T16:40", "end": "2026-10-22T18:00", "room": fix["ruz"]["room"],
                             "cancelled": False, "kind": fix["fields"]["kind"], "required": True}

    # Moved to another room an hour later; the optional calendar stays through a later change of the room.
    moved = await ask(fixer, "POST", "/fix/class", {"uid": card["uid"], **fix["fields"], "start": "2026-10-22T17:40",
                                                    "end": "2026-10-22T19:00", "room": "R101", "required": False})
    assert moved["change"]["undo"]
    card = await seminar(fixer)
    assert card["start"] == "2026-10-22T17:40:00" and card["place"] == "R101"
    assert card["fix"]["own"] is not None and card["fix"]["fields"]["required"] is False
    again = await ask(fixer, "POST", "/fix/class", {"uid": card["uid"], **card["fix"]["fields"], "room": "R102"})
    card = await seminar(fixer)
    assert card["place"] == "R102" and card["start"] == "2026-10-22T17:40:00"
    assert card["fix"]["fields"]["required"] is False
    await undo(fixer, again)
    assert (await seminar(fixer))["place"] == "R101"

    # Cancelled, then «As it was» takes the own correction back; its undo brings the change again.
    card = await seminar(fixer)
    await ask(fixer, "POST", "/fix/class", {"uid": card["uid"], **card["fix"]["fields"], "cancelled": True})
    card = await seminar(fixer)
    assert card["cancelled"]
    back = await ask(fixer, "POST", "/fix/remove", {"id": card["fix"]["own"]})
    card = await seminar(fixer)
    assert not card["cancelled"] and card["start"] == "2026-10-22T16:40:00" and card["fix"]["own"] is None
    await undo(fixer, back)
    assert (await seminar(fixer))["cancelled"]
    assert {c["via"] for c in fixer.connector.changes.recent()} == {"web"}

    refused = await ask(fixer, "POST", "/fix/class", {"uid": card["uid"], "colour": "red"}, status=400)
    assert "expected" in refused["error"]
    assert (await ask(fixer, "POST", "/fix/class", {"uid": "nothing", "start": "x"}, status=404))["error"]


async def test_chat_correction_ignored(fixer, schedule):
    card = await seminar(fixer)
    with schedule.db:
        schedule.events.add({"uid": "tg-c1@infomaster", "source": "tg", "chat_id": -1001, "msg_ids": [3],
                             "class_uid": card["uid"], "course": "Discrete Mathematics", "title": "",
                             "kind": "Seminar", "start": "2026-10-23T10:00:00", "end": "2026-10-23T11:20:00",
                             "room": "R300", "required": True}, schedule.now())
    feed = await ask(fixer, "GET", "/feed")
    card = await ask(fixer, "GET", "/class?uid=" + entries(feed)["Discrete Mathematics"]["uid"])
    assert card["start"] == "2026-10-23T10:00:00" and card["fix"]["chat"]["from"] == "DM seminar chat"
    assert card["fix"]["fields"]["room"] == "R300"
    ignored = await ask(fixer, "POST", "/fix/options", {"id": card["fix"]["chat"]["id"], "hidden": True})
    card = await seminar(fixer)
    assert card["start"] == "2026-10-22T16:40:00" and card["fix"]["chat"]["hidden"]
    assert card["fix"]["fields"]["room"] == card["fix"]["ruz"]["room"]
    await undo(fixer, ignored)
    assert (await seminar(fixer))["start"] == "2026-10-23T10:00:00"


async def test_event_changed_hidden_restored(fixer, schedule):
    chat_event(schedule)
    feed = await ask(fixer, "GET", "/feed")
    event = entries(feed)["Consultation"]
    card = await ask(fixer, "GET", "/class?uid=" + event["uid"])
    fix = card["fix"]
    assert fix["of"] == "event" and not fix["own"] and not fix["copy"] and fix["required"]
    assert fix["fields"] == {"title": "Consultation", "kind": "Consultation", "start": "2026-10-24T15:00",
                             "end": "2026-10-24T16:00", "room": "R205", "cancelled": False}

    # The optional calendar, then hidden: it leaves the feed and waits in the hidden list.
    await ask(fixer, "POST", "/fix/options", {"id": fix["id"], "required": False})
    assert not (await ask(fixer, "GET", "/class?uid=" + event["uid"]))["fix"]["required"]
    await ask(fixer, "POST", "/fix/options", {"id": fix["id"], "hidden": True})
    assert "Consultation" not in entries(await ask(fixer, "GET", "/feed"))
    listed = await ask(fixer, "GET", "/fix/hidden")
    assert [(e["title"], e["chat"]) for e in listed["events"]] == [("Consultation", "DM seminar chat")]
    await ask(fixer, "POST", "/fix/options", {"id": listed["events"][0]["id"], "hidden": False})
    assert "Consultation" in entries(await ask(fixer, "GET", "/feed"))

    # Changed: an own copy stands for the chat's one; «As it was» on the copy brings the chat's one back.
    await ask(fixer, "POST", "/fix/event", {"uid": event["uid"], "room": "R301"})
    copy = entries(await ask(fixer, "GET", "/feed"))["Consultation"]
    assert copy["uid"] != event["uid"] and copy["place"] == "R301"
    card = await ask(fixer, "GET", "/class?uid=" + copy["uid"])
    assert card["fix"]["own"] and card["fix"]["copy"]
    assert (await ask(fixer, "GET", "/fix/hidden"))["events"] == []  # the copy stands for it, not the list
    await ask(fixer, "POST", "/fix/remove", {"id": card["fix"]["id"]})
    assert entries(await ask(fixer, "GET", "/feed"))["Consultation"]["place"] == "R205"

    # A chat's event is hidden, not deleted.
    refused = await ask(fixer, "POST", "/fix/remove", {"id": fix["id"]}, status=400)
    assert "set_event_options" in refused["error"]
    assert (await ask(fixer, "POST", "/fix/event", {"uid": event["uid"]}, status=400))["error"]


async def test_own_event_edited_and_deleted(fixer, schedule):
    await ask(fixer, "POST", "/add/event", {"title": "Supervisor", "start": "2026-10-24T15:00",
                                            "end": "2026-10-24T16:00", "room": "R205"})
    own = entries(await ask(fixer, "GET", "/feed"))["Supervisor"]
    card = await ask(fixer, "GET", "/class?uid=" + own["uid"])
    assert card["fix"]["own"] and not card["fix"]["copy"]
    await ask(fixer, "POST", "/fix/event", {"uid": own["uid"], "start": "2026-10-24T17:00",
                                            "end": "2026-10-24T18:00"})
    assert entries(await ask(fixer, "GET", "/feed"))["Supervisor"]["start"] == "2026-10-24T17:00:00"
    card = await ask(fixer, "GET", "/class?uid=" + own["uid"])
    gone = await ask(fixer, "POST", "/fix/remove", {"id": card["fix"]["id"]})
    assert "Supervisor" not in entries(await ask(fixer, "GET", "/feed"))
    await undo(fixer, gone)
    assert "Supervisor" in entries(await ask(fixer, "GET", "/feed"))


async def test_hidden_deadline_restored(fixer, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    sheet = uid(next(d for d in merged.all() if d.title == "Sheet 5"))
    await ask(fixer, "POST", "/deadlines/not-mine", {"uid": sheet})
    listed = await ask(fixer, "GET", "/fix/hidden")
    assert [(d["title"], d["due"], d["reason"], d["course"]) for d in listed["deadlines"]] == [
        ("Sheet 5", "2026-10-22", "not mine", "DM")]
    restored = await ask(fixer, "POST", "/fix/hidden/deadline", {"uid": sheet})
    assert restored["change"]["undo"]
    assert (await ask(fixer, "GET", "/fix/hidden"))["deadlines"] == []
    await undo(fixer, restored)
    assert len((await ask(fixer, "GET", "/fix/hidden"))["deadlines"]) == 1
    assert (await ask(fixer, "POST", "/fix/hidden/deadline", {"uid": "nothing"}, status=400))["error"]


async def test_grades_corrected(fixer, now):
    fixer.settings.set("grades_sheets", [{"url": URL, "label": "DM"}])
    fixer.grades.record("abc123", "DM", [item("k1", "DM HW 1", 8)])
    own = fixer.grades.add_grade("DM", "Essay", 7)
    fixer.grades.add_grade("Alg", "Quiz", 5)  # another course's
    lens = await ask(fixer, "GET", "/course?key=DM")
    works = {w["label"]: w for w in lens["grades"]["works"]}
    assert set(works) == {"DM HW 1", "Essay"} and lens["grades"]["average"] == 7.5
    assert (works["DM HW 1"]["id"], works["DM HW 1"]["own"], works["Essay"]["id"]) == ("k1", False, f"own-{own}")

    changed = await ask(fixer, "POST", "/fix/grade", {"id": "k1", "value": 9.5})
    assert changed["change"]["undo"]
    work = next(w for w in (await ask(fixer, "GET", "/course?key=DM"))["grades"]["works"] if w["id"] == "k1")
    assert (work["value"], work["edited"], work["sheet_value"]) == (9.5, True, 8)
    reset = await ask(fixer, "POST", "/fix/grade/reset", {"id": "k1"})  # back as the sheet has it
    assert fixer.grades.items("abc123")[0]["value"] == 8
    await undo(fixer, reset)
    assert fixer.grades.items("abc123")[0]["value"] == 9.5

    await ask(fixer, "POST", "/fix/grade", {"id": f"own-{own}", "value": 8, "label": "Essay 1"})
    assert [(g["label"], g["value"]) for g in fixer.grades.manual() if g["id"] == f"own-{own}"] == [("Essay 1", 8)]
    gone = await ask(fixer, "POST", "/fix/grade/reset", {"id": f"own-{own}"})
    assert all(g["label"] != "Essay 1" for g in fixer.grades.manual())
    await undo(fixer, gone)
    assert any(g["label"] == "Essay 1" for g in fixer.grades.manual())
    assert (await ask(fixer, "POST", "/fix/grade", {"id": "k1"}, status=400))["error"]
    assert (await ask(fixer, "POST", "/fix/grade", {"id": "nope", "value": 5}, status=400))["error"]



async def test_grades_with_their_parts(fixer):
    """The lens's tile and the grades' screen read one list: every work laid out, its parts, a zero before any grade
    pending and outside the average, and the tabs the layout skips as summaries."""
    fixer.settings.set("grades_sheets", [{"url": URL, "label": "DM"}])
    cells = lambda *pairs: [{"column": c, "header": h, "value": v} for c, h, v in pairs]  # noqa: E731
    work = lambda col, label, top, raw, bonus=None: {  # noqa: E731
        "column": col, "label": label, "max": top, "bonus_column": bonus, "header": "Score" if bonus else label,
        "raw": raw, "value": round(raw / top * 10, 2)}
    fixer.grades.store.put_json(VIEW + "abc123", {"course": "DM", "tabs": [
        {"tab": "HW-1", "skip": False,
         "columns": cells(("D", "5 / 1a", "1"), ("E", "1b", "0.5"), ("F", "Score", "1.5"), ("G", "Score*", "0")),
         "works": [work("F", "HW 1", 2, 1.5, "G")]},
        {"tab": "HW-2", "skip": False, "columns": cells(("F", "Score", "0"), ("G", "Score*", "0")),
         "works": [{**work("F", "HW 2", 5, 0, "G"), "graded": 0.3}]},  # a third of the group graded: being graded
        {"tab": "Seminars", "skip": False,
         "columns": cells(("B", "Sem 1", "8"), ("C", "Sem 2", "0"), ("D", "Sem 3", "6")),
         "works": [work("B", "Sem 1", 10, 8), work("C", "Sem 2", 10, 0), work("D", "Sem 3", 10, 6)]},
        {"tab": "Totals", "skip": True, "columns": cells(("D", "Midterm", "4.5"))},
        {"tab": "Empty", "skip": True, "columns": []}]})
    fixer.grades.record("abc123", "DM", [
        Item("abc123|HW-1|F|Score", "HW-1", "Score", "DM HW 1", 7.5, 1.5, 2, None, 0),
        Item("abc123|Seminars|B|Sem 1", "Seminars", "Sem 1", "DM Sem 1", 8, 8, 10, None, 1),
        Item("abc123|Seminars|D|Sem 3", "Seminars", "Sem 3", "DM Sem 3", 6, 6, 10, None, 2)])
    grades = (await ask(fixer, "GET", "/course?key=DM"))["grades"]
    works = {w["label"]: w for w in grades["works"]}
    assert list(works) == ["DM HW 1", "HW 2", "DM Sem 1", "Sem 2", "DM Sem 3"]
    # a part is named by its header without a row of numbers only (the maximum of the work above it)
    assert works["DM HW 1"]["parts"] == [{"header": "1a", "value": "1", "note": None},
                                         {"header": "1b", "value": "0.5", "note": None}]
    assert (works["DM HW 1"]["value"], works["DM HW 1"]["bonus"], works["DM HW 1"]["pending"]) == (7.5, "0", False)
    assert works["HW 2"]["pending"] and works["HW 2"]["value"] is None and works["HW 2"]["parts"] == []
    assert works["HW 2"]["checking"] and not works["Sem 2"]["checking"]
    assert works["Sem 2"]["pending"] and works["DM Sem 3"]["parts"] == []
    assert (grades["count"], grades["pending"], grades["average"]) == (3, 2, 7.2)
    assert grades["books"][0]["summaries"] == [{"tab": "Totals", "cells": [
        {"header": "Midterm", "value": "4.5", "note": None, "how": None}]}]
    assert not grades["books"][0]["calc"]  # no formulas: no calculator
    # an owner's change shows over the sheet's grade, and the average follows it
    await ask(fixer, "POST", "/fix/grade", {"id": "abc123|Seminars|D|Sem 3", "value": 9})
    grades = (await ask(fixer, "GET", "/course?key=DM"))["grades"]
    assert next(w for w in grades["works"] if w["id"] == "abc123|Seminars|D|Sem 3")["edited"]
    assert grades["average"] == 8.2
