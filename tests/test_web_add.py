"""«+» of the web app (§8.6, stage 16.5): material at the class in progress, one line read by the model and confirmed,
a deadline, an event or a grade by hand; every one journaled with its undo."""

import datetime as dt

import actions_app
import pytest
from aiohttp import FormData
from test_http import TWIN, env
from test_web import FEED, IPHONE, ORIGIN, call, log_in

from infomaster import clock
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import parse
from infomaster.connector import Connector
from infomaster.library import Library
from infomaster.library import links as library_links
from infomaster.llm.budget import BudgetExceeded
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings
from infomaster.web import WebApp
from infomaster.web.quick import Quick
from infomaster.web.views import Data

PNG = b"\x89PNG\r\n\x1a\n" + b"\x00" * 32


class Seconds:
    def __init__(self):
        self.value = 1_800_000_000.0

    def __call__(self):
        return self.value


class Model:
    """Haiku as the tests want it: the answer given, the prompt kept."""

    def __init__(self):
        self.available, self.answer, self.prompts, self.refuse = True, None, [], None

    async def json(self, task, system, prompt, schema, *, max_tokens=8000):
        assert task == "quick" and "data from the student" in system
        self.prompts.append(prompt)
        if self.refuse:
            raise self.refuse
        return self.answer


def at(now, hour, minute=0):
    now.moment = dt.datetime(2026, 10, 20, hour, minute, tzinfo=clock.TZ).astimezone(dt.UTC)


@pytest.fixture
async def adder(aiohttp_client, store, budget, journal, schedule, merged, now, tmp_path):
    """The public port as the service runs it, with the library; Tuesday 20.10.2026, during Algebra (13:00–14:20)."""
    schedule.ruz._sync(parse(FEED))
    schedule.subjects.set_manual("Algebra", "Algebra", "Alg")
    schedule.subjects.set_manual("Discrete Mathematics", "Discrete Mathematics", "DM")
    at(now, 13, 30)
    library = Library(store, tmp_path, now=now)
    library.same_course = schedule.same_course
    sent, seconds, model = [], Seconds(), Model()

    async def send_code(text):
        sent.append(text)

    hse = HseApp(store, Settings(store), env().public_url + CALLBACK_PATH)
    app = actions_app.app(env(), budget, journal, schedule.inbox, ChatSettings(store), TWIN, schedule.subjects,
                             schedule, hse, merged, settings=budget.settings)
    library_links.add_routes(app, library_links.Links(store, library, env().public_url))
    connector = Connector(env(), store, News(store), now=now, seconds=seconds)
    connector.attach(app)
    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "ru", now=seconds,
                    data=Data(schedule, merged, library, lang=lambda: "ru"), llm=model)
    webapp.connector = connector
    client = await aiohttp_client(webapp.attach(app))
    client.token = await log_in(client, sent, seconds)
    client.library, client.model, client.merged = library, model, merged
    return client


async def answer(client, method, path, data=None, status=200):
    response = await call(client, method, path, data, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def upload(client, form: FormData, status=200):
    response = await client.post("/web/api/add/files", data=form, headers={
        "Origin": ORIGIN, "User-Agent": IPHONE, "Cookie": f"im_session={client.token}"})
    assert response.status == status, await response.text()
    return await response.json()


async def test_place(adder, now):
    state = await answer(adder, "GET", "/add")
    assert state["current"]["title"] == "Algebra" and state["current"]["course"] == "Alg"
    assert [c["title"] for c in state["classes"]][:2] == ["Algebra", "Математический анализ"]  # the newest first
    assert state["line"] is True and state["library"] is True and state["grades"] is False
    assert {"key": "Alg", "title": "Algebra"} in state["courses"]

    at(now, 15, 40)  # Algebra ended at 14:20: 80 minutes ago, still the class in progress
    assert (await answer(adder, "GET", "/add"))["current"]["title"] == "Algebra"
    at(now, 16, 0)
    assert (await answer(adder, "GET", "/add"))["current"] is None


async def test_files_to_the_class_and_undo(adder):
    state = await answer(adder, "GET", "/add")
    form = FormData()
    form.add_field("class_uid", state["current"]["uid"])
    form.add_field("file", PNG, filename="board.png", content_type="image/png")
    form.add_field("file", b"ID3" + b"\x00" * 64, filename="talk.m4a", content_type="audio/mp4")
    added = await upload(adder, form)
    items = [adder.library.get(item_id) for item_id in added["added"]]
    assert [(i["kind"], i["course"], i["class_uid"], i["source"], i["author"]) for i in items] == [
        ("photo", "Algebra", state["current"]["uid"], "web", "owner"),
        ("audio", "Algebra", state["current"]["uid"], "web", "owner")]
    assert added["change"]["undo"] is True
    await answer(adder, "POST", f"/changes/{added['change']['id']}/undo", {})
    assert all(adder.library.get(item_id) is None for item_id in added["added"])  # removed, back for 30 days

    form = FormData()  # no place: the library's inbox
    form.add_field("file", PNG, filename="x.png", content_type="image/png")
    [item_id] = (await upload(adder, form))["added"]
    assert adder.library.get(item_id)["course"] is None

    form = FormData()
    form.add_field("class_uid", "nothing")
    form.add_field("file", PNG, filename="x.png", content_type="image/png")
    await upload(adder, form, 400)
    form = FormData()
    form.add_field("file", b"", filename="empty.png", content_type="image/png")
    assert "empty" in (await upload(adder, form, 400))["error"]
    form = FormData()
    form.add_field("file", PNG, filename="x.png", content_type="image/png")
    form.add_field("course", "Alg")
    await upload(adder, form, 400)  # the place comes first
    no_file = FormData({"course": "Alg"}, default_to_multipart=True)
    assert (await upload(adder, no_file, 400))["error"] == "Выберите файл."


async def test_notes_and_links(adder):
    note = await answer(adder, "POST", "/add/note", {"text": "Лемма 3 — на зачёте", "course": "Alg"})
    item = adder.library.get(note["added"][0])
    assert (item["kind"], item["course"], item["class_uid"], item["text"]) == ("text", "Algebra", None,
                                                                                "Лемма 3 — на зачёте")
    link = await answer(adder, "POST", "/add/note", {"text": "https://example.com/slides"})
    item = adder.library.get(link["added"][0])
    assert (item["kind"], item["url"], item["course"]) == ("link", "https://example.com/slides", None)
    await answer(adder, "POST", "/add/note", {"text": " "}, 400)
    await answer(adder, "POST", "/add/note", {"text": "x", "course": "Nope"}, 400)


async def test_one_line(adder):
    adder.model.answer = {"kind": "deadline", "title": "Лаба 3, 18:00", "course": "Alg", "due": "2026-10-21",
                          "start": "", "end": "", "room": "", "category": "study",
                          "why": "«До среды» — ближайшая среда."}
    understood = await answer(adder, "POST", "/add/line", {"text": "лаба 3 по алгебре до среды 18:00"})
    assert understood == {"kind": "deadline", "title": "Лаба 3, 18:00", "course": "Alg", "course_title": "Algebra",
                          "due": "2026-10-21", "start": None, "end": None, "room": "", "category": "study",
                          "why": "«До среды» — ближайшая среда.", "line": "лаба 3 по алгебре до среды 18:00"}
    prompt = adder.model.prompts[-1]
    assert "Today: 2026-10-20, Tuesday, 13:30" in prompt and "- Alg: Algebra" in prompt
    assert "2026-10-22 Thu 16:40 DM Seminar" in prompt  # the classes ahead, for «by the seminar»
    assert prompt.endswith("The line:\nлаба 3 по алгебре до среды 18:00")
    assert adder.merged.todo.own() == ()  # nothing saved before the owner confirms

    saved = await answer(adder, "POST", "/add/deadline", {"title": understood["title"], "due": understood["due"],
                                                          "course": "Alg"})
    [own] = adder.merged.todo.own()
    assert (own.title, own.course) == ("Лаба 3, 18:00", "Alg")
    feed = await answer(adder, "GET", "/feed?course=Alg")
    assert [i["title"] for day in feed["days"] for i in day["items"] if i["kind"] == "deadline"] == ["Лаба 3, 18:00"]
    await answer(adder, "POST", f"/changes/{saved['change']['id']}/undo", {})
    assert adder.merged.todo.own() == ()

    # What the model may not decide: an unknown course, a bad date, a deadline without one.
    adder.model.answer = {**adder.model.answer, "course": "Biology", "due": "2026-02-30"}
    odd = await answer(adder, "POST", "/add/line", {"text": "что-то"})
    assert (odd["kind"], odd["course"], odd["due"]) == ("note", None, None)

    adder.model.refuse = BudgetExceeded("spent")
    assert "лимит" in (await answer(adder, "POST", "/add/line", {"text": "x"}, 429))["error"]
    await answer(adder, "POST", "/add/line", {"text": "  "}, 400)
    adder.model.available = False
    assert "Anthropic" in (await answer(adder, "POST", "/add/line", {"text": "x"}, 503))["error"]


def test_checked_event(schedule, merged, now):
    schedule.ruz._sync(parse(FEED))
    quick = Quick(Data(schedule, merged))
    event = quick.checked({"kind": "event", "title": "Consultation", "course": "", "due": "",
                           "start": "2026-10-22T15:00", "end": "2026-10-22T16:00", "room": "R205",
                           "category": "study", "why": ""}, "консультация в четверг 15–16 R205")
    assert (event["kind"], event["start"], event["end"], event["room"]) == \
        ("event", "2026-10-22T15:00", "2026-10-22T16:00", "R205")
    whole_day = quick.checked({**event, "start": "2026-10-22", "end": "2026-10-22T16:00"}, "x")
    assert (whole_day["start"], whole_day["end"]) == ("2026-10-22", None)
    assert quick.checked({**event, "start": ""}, "x")["kind"] == "note"
    assert quick.checked({"kind": "?", "title": ""}, "just a thought")["title"] == "just a thought"


async def test_by_hand(adder, schedule):
    event = await answer(adder, "POST", "/add/event", {"title": "Consultation", "start": "2026-10-22T15:00",
                                                       "end": "2026-10-22T16:00", "room": "R205"})
    assert event["change"]["undo"]
    added = [e for e in schedule.events.in_force_all() if e["title"] == "Consultation"]
    assert len(added) == 1 and added[0]["room"] == "R205"
    await answer(adder, "POST", f"/changes/{event['change']['id']}/undo", {})
    assert not [e for e in schedule.events.in_force_all() if e["title"] == "Consultation"]

    personal = await answer(adder, "POST", "/add/deadline", {"title": "Passport", "due": "2026-10-30",
                                                             "category": "personal"})
    assert personal["change"]["undo"] and adder.merged.todo.own()[0].category == "personal"
    await answer(adder, "POST", "/add/deadline", {"title": "", "due": "2026-10-30"}, 400)
    assert "add_grade" in (await answer(adder, "POST", "/add/grade", {"course": "Alg", "label": "Quiz",
                                                                      "value": 8}, 400))["error"]
    await answer(adder, "POST", "/add/nothing", {}, 404)


async def test_share_without_the_worker(adder):
    response = await adder.post("/web/share", data=FormData({"text": "x"}), allow_redirects=False)
    assert response.status == 303 and response.headers["Location"] == "/web/#/add?shared=lost"
