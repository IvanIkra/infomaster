"""The web app's changes (§8.6, stage 16.3): «Done», «Not mine», editing, merging duplicates, confirming an assumed
date, a course for a deadline without one, the «Check» queue, and the journal with its undo."""

import datetime as dt

import pytest
from aiohttp import web
from test_http import TWIN, add_row, env
from test_web import FEED, IPHONE, call, log_in

from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import parse
from infomaster.connector import Connector
from infomaster.outputs import miniapp
from infomaster.processing.news import News
from infomaster.processing.todo import Deadline, uid
from infomaster.settings import ChatSettings, Settings
from infomaster.web import WebApp
from infomaster.web.check import likely_same, stays
from infomaster.web.views import Data


class Seconds:
    def __init__(self):
        self.value = 1_800_000_000.0

    def __call__(self):
        return self.value


@pytest.fixture
async def actor(aiohttp_client, store, budget, journal, schedule, merged, now):
    """The service's public port as it runs: the Mini App, the connector and the web app on one app; today is
    Tuesday 20.10.2026, a logged-in device."""
    schedule.ruz._sync(parse(FEED))
    schedule.subjects.set_manual("Discrete Mathematics", "Discrete Mathematics", "DM")
    schedule.subjects.set_manual("Algebra", "Algebra", "Alg")
    sent, seconds = [], Seconds()

    async def send_code(text):
        sent.append(text)

    hse = HseApp(store, Settings(store), env().public_url + CALLBACK_PATH)
    app = miniapp.create_app(env(), budget, journal, schedule.inbox, ChatSettings(store), TWIN, schedule.subjects,
                             schedule, hse, merged, budget.settings)
    connector = Connector(env(), store, News(store), now=now, seconds=seconds)
    connector.attach(app)
    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "en", now=seconds,
                    data=Data(schedule, merged, lang=lambda: "en"))
    webapp.connector = connector
    client = await aiohttp_client(webapp.attach(app))
    client.token = await log_in(client, sent, seconds)
    client.connector = connector
    return client


async def post(client, path, data, status=200):
    response = await call(client, "POST", path, data, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def get(client, path):
    response = await call(client, "GET", path, token=client.token)
    assert response.status == 200, await response.text()
    return await response.json()


def titles(feed) -> list[str]:
    return [i["title"] for day in feed["days"] for i in day["items"] if i["kind"] == "deadline"]


def item(merged, title):
    return next(d for d in merged.all() if d.title == title)


async def test_done_not_mine_and_undo(actor, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    add_row(merged, "Quiz 2", "2026-10-23", course="Algebra")
    sheet, quiz = uid(item(merged, "Sheet 5")), uid(item(merged, "Quiz 2"))

    done = await post(actor, "/deadlines/done", {"uid": sheet})
    assert done["change"]["undo"] and "as done" in done["change"]["summary"]
    await post(actor, "/deadlines/not-mine", {"uid": quiz})
    assert {(r["title"], r["reason"]) for r in merged.todo.hidden_rows()} == {("Sheet 5", "done"),
                                                                            ("Quiz 2", "not_mine")}
    assert titles(await get(actor, "/feed")) == []  # Tablo and HA see both as hidden too
    assert merged.items() == []

    await post(actor, f"/changes/{done['change']['id']}/undo", {})
    assert titles(await get(actor, "/feed")) == ["Sheet 5"]
    again = await post(actor, f"/changes/{done['change']['id']}/undo", {}, status=400)
    assert "undone already" in again["error"]

    # An own deadline is deleted, and its undo adds it back.
    own = merged.add("Dentist", merged.today().replace(day=24), "personal")
    gone = await post(actor, "/deadlines/done", {"uid": own.key})
    assert merged.todo.own() == ()
    await post(actor, f"/changes/{gone['change']['id']}/undo", {})
    assert [d.title for d in merged.todo.own()] == ["Dentist"]

    # Every change is in the connector's journal as the web app's; the Mini App's connector screen lists Claude's.
    assert {c["via"] for c in actor.connector.changes.recent()} == {"web"}
    assert actor.connector.state()["changes"] == []

    assert (await post(actor, "/deadlines/done", {"uid": "nothing"}, status=404))["error"]
    assert (await post(actor, "/deadlines/later", {"uid": sheet}, status=404))["error"] == "no such action"
    refused = await actor.request("POST", "/web/api/deadlines/done", json={"uid": sheet},
                                  headers={"User-Agent": IPHONE, "Cookie": f"im_session={actor.token}"})
    assert refused.status == 403  # no Origin of the app


async def test_edit(actor, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    sheet = uid(item(merged, "Sheet 5"))
    edited = await post(actor, "/deadlines/edit", {"uid": sheet, "title": "Sheet 5 (both parts)", "due": "2026-10-24"})
    assert [(d.title, d.due.isoformat()) for d in merged.todo.own()] == [("Sheet 5 (both parts)", "2026-10-24")]
    assert titles(await get(actor, "/feed")) == ["Sheet 5 (both parts)"]
    await post(actor, f"/changes/{edited['change']['id']}/undo", {})
    assert titles(await get(actor, "/feed")) == ["Sheet 5"] and merged.todo.own() == ()
    assert "title" in (await post(actor, "/deadlines/edit", {"uid": sheet, "title": " "}, status=400))["error"]


async def test_duplicates(actor, merged):
    add_row(merged, "Sheet 12", "2026-10-26", course="Discrete Mathematics")
    add_row(merged, "HW 12", "2026-10-26", source="mail", course="Discrete Mathematics")
    add_row(merged, "Essay", "2026-10-26", course="Discrete Mathematics")  # no number: another work
    add_row(merged, "Quiz 12", "2026-10-26", course="Algebra")  # another course
    sheet, hw = uid(item(merged, "Sheet 12")), uid(item(merged, "HW 12"))

    [question] = (await get(actor, "/feed"))["check"]
    assert question["kind"] == "duplicate" and question["course"] == "DM"
    assert question["keep"] == hw  # the letter is more reliable than the chat
    assert [i["title"] for i in question["items"]] == ["HW 12", "Sheet 12"]
    assert (await get(actor, "/feed?course=Alg"))["check"] == []

    card = await get(actor, "/deadline?uid=" + sheet)
    assert [t["title"] for t in card["twins"]] == ["HW 12", "Essay"] and card["own"] is False

    merged_change = await post(actor, "/deadlines/merge", {"uid": sheet, "keep": hw})
    [row] = merged.todo.hidden_rows()
    assert (row["title"], row["reason"], row["kept"]) == ("Sheet 12", "duplicate", hw)
    assert (await get(actor, "/check"))["items"] == []
    await post(actor, f"/changes/{merged_change['change']['id']}/undo", {})
    assert [q["id"] for q in (await get(actor, "/check"))["items"]] == [question["id"]]

    # «They are different»: not asked again, until it is undone.
    aside = await post(actor, "/check/put-aside", {"id": question["id"]})
    assert (await get(actor, "/check"))["items"] == []
    await post(actor, f"/changes/{aside['change']['id']}/undo", {})
    assert len((await get(actor, "/check"))["items"]) == 1
    await post(actor, "/check/put-aside", {"id": "dup:x|y"}, status=404)
    await post(actor, "/deadlines/merge", {"uid": sheet, "keep": sheet}, status=400)
    await post(actor, "/deadlines/merge", {"uid": sheet, "keep": "nothing"}, status=400)


def test_likely_same(merged):
    def d(title, due, source="tg", assumed=False):
        return Deadline(title, dt.date.fromisoformat(due), "study", source, "DM", assumed=assumed)

    assert likely_same(d("Sheet 12", "2026-10-26"), d("HW 12", "2026-10-30"))
    assert not likely_same(d("Sheet 12", "2026-10-26"), d("HW 12", "2026-11-03"))  # more than a week apart
    assert not likely_same(d("Sheet 12", "2026-10-26"), d("HW 13", "2026-10-26"))
    assert likely_same(d("Midterm", "2026-10-26"), d("Midterm retake", "2026-10-28"))  # the same work
    assert not likely_same(d("HW 1", "2026-10-26", "hseapp"), d("HW 1 bonus", "2026-10-26", "hseapp"))
    real, guessed = d("Sheet 12", "2026-10-26", "tg"), d("HW 12", "2026-10-26", "mail", assumed=True)
    assert stays(guessed, real) is real  # a real date over an assumed one, then the source


async def test_assumed_date(actor, merged):
    with merged.db:
        merged.deadlines.add({"source": "tg", "chat_id": -1001, "msg_ids": [1], "course": "Discrete Mathematics",
                              "label": "DM sheet 3", "due": "2026-10-22", "assumed": True}, merged.now())
    sheet = uid(item(merged, "DM sheet 3"))
    [question] = (await get(actor, "/check"))["items"]
    assert question == {"id": f"assumed:{sheet}", "kind": "assumed", "course": "DM", "item": question["item"]}
    assert question["item"]["assumed"] is True

    confirmed = await post(actor, "/deadlines/confirm", {"uid": sheet})
    assert item(merged, "DM sheet 3").assumed is False and (await get(actor, "/check"))["items"] == []
    await post(actor, f"/changes/{confirmed['change']['id']}/undo", {})
    assert item(merged, "DM sheet 3").assumed is True
    add_row(merged, "Quiz 2", "2026-10-23", course="Algebra")
    await post(actor, "/deadlines/confirm", {"uid": uid(item(merged, "Quiz 2"))}, status=400)


async def test_course(actor, merged):
    add_row(merged, "AI Challenge", "2026-10-25", course=None)
    challenge = uid(item(merged, "AI Challenge"))
    [question] = (await get(actor, "/check"))["items"]
    assert question["kind"] == "course" and question["id"] == f"course:{challenge}"
    assert [c["key"] for c in (await get(actor, "/check"))["courses"]][:1] == ["Alg"]

    changed = await post(actor, "/deadlines/course", {"uid": challenge, "course": "DM"})
    row = merged.deadlines.get(int(item(merged, "AI Challenge").ref[3:]))
    assert row["course"] == "Discrete Mathematics"
    assert [i["course"] for day in (await get(actor, "/feed?course=DM"))["days"] for i in day["items"]
            if i["kind"] == "deadline"] == ["DM"]
    assert (await get(actor, "/check"))["items"] == []
    await post(actor, f"/changes/{changed['change']['id']}/undo", {})
    assert merged.deadlines.get(row["id"])["course"] is None
    await post(actor, "/deadlines/course", {"uid": challenge, "course": "Nope"}, status=400)

    # «Outside the courses»
    await post(actor, "/check/put-aside", {"id": question["id"]})
    assert (await get(actor, "/check"))["items"] == []


async def test_without_connector(aiohttp_client, store, schedule, merged):
    webapp = WebApp(env(), store, data=Data(schedule, merged))
    client = await aiohttp_client(webapp.attach(web.Application()))
    client.token = "x"
    response = await call(client, "POST", "/deadlines/done", {"uid": "x"})
    assert response.status == 401  # the session first
