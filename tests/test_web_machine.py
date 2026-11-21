"""«Machine» of the web app (§8.6, stage 16.6, D65): the sources and their freshness, what needs attention, the journal
with its undo, the web app's settings changed through the connector's tools, and the notifications' kinds and quiet
hours."""

import asyncio
import datetime as dt

import actions_app
import pytest
from aiohttp import web
from test_http import env
from test_web import FEED, Seconds, call, log_in
from test_web_push import Browser, Service

from infomaster import clock
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import parse
from infomaster.collectors.telegram import LoginError
from infomaster.connector import Connector
from infomaster.i18n import Alert, Phrase
from infomaster.processing.grades import VIEW, Grades, works
from infomaster.processing.news import News
from infomaster.processing.todo import Deadline
from infomaster.settings import ChatSettings, Settings
from infomaster.web import WebApp
from infomaster.web.notify import Notifier, quiet
from infomaster.web.push import Push
from infomaster.web.views import Data

SHEET = "https://docs.google.com/spreadsheets/d/abcDEF123/edit"


class FakeTwin:
    """The twin's login without Telegram: code 12345."""

    def __init__(self):
        self.account, self.pending = None, None

    def status(self):
        return {"ok_at": "2026-10-20T11:58:00" if self.account else None, "error": None,
                "login_needed": not self.account, "account": self.account,
                "login": {"step": "code", "phone": "+7 ••• 42"} if self.pending else None}

    async def send_code(self, phone):
        self.pending = phone

    async def sign_in(self, code=None, password=None):
        if code != "12345":
            raise LoginError(Phrase("the code is wrong", "код неверный"))
        self.pending, self.account = None, {"name": "Test Twin", "username": None}

    async def cancel_login(self):
        self.pending = None

    async def logout(self):
        self.account = None


class Rescheduled:
    def __init__(self):
        self.times = 0

    def reschedule(self):
        self.times += 1


@pytest.fixture
async def machine(aiohttp_client, store, budget, journal, schedule, merged, now):
    """The public port with every part «Machine» sets up: the twin, the sources' statuses, the settings, the
    reminders, the gradebooks and the connector; Tuesday 20.10.2026, noon in Moscow, a logged-in device."""
    schedule.ruz._sync(parse(FEED))
    schedule.subjects.set_manual("Discrete Mathematics", "Discrete Mathematics", "DM")
    sent, seconds = [], Seconds()
    seconds.value = now().timestamp()

    async def send_code(text):
        sent.append(text)

    settings = budget.settings
    twin, reminders = FakeTwin(), Rescheduled()
    grades = Grades(store, settings, None, now=now)
    hse = HseApp(store, settings, env().public_url + CALLBACK_PATH)
    schedule.inbox.put_chat(-1001, "Course chat", "supergroup", None, 0)
    statuses = {
        "telegram": twin.status,
        "mail": lambda: {"configured": True, "ok_at": "2026-10-20T10:00:00", "error": None, "letters": 12},
        "ruz": lambda: {"ok_at": "2026-10-20T11:30:00", "error": None, "classes": 70},
        "hseapp": hse.status,
        "wiki": lambda: {"pages": {"https://wiki.example.org/DM": {"title": "DM", "items": 0, "error": "HTTP 503"}},
                         "ok_at": "2026-10-20T09:00:00"},
        "batches": lambda: {"ok_at": "2026-10-20T11:00:00", "error": None, "queue": 2},
        "grades": lambda: {"configured": True, "sheets": {}},
    }
    app = actions_app.app(env(), budget, journal, schedule.inbox, ChatSettings(store), statuses, schedule.subjects,
                             schedule, hse, merged, settings=settings, grade_list=grades, twin=twin,
                             deadline_reminders=reminders)
    connector = Connector(env(), store, News(store), now=now, seconds=seconds)
    connector.attach(app)
    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "en", now=seconds,
                    data=Data(schedule, merged, lang=lambda: "en"))
    webapp.connector = connector
    client = await aiohttp_client(webapp.attach(app))
    client.token = await log_in(client, sent, seconds)
    client.connector, client.settings, client.twin, client.grades = connector, settings, twin, grades
    client.reminders = reminders
    return client


async def ask(client, method, path, data=None, status=200):
    response = await call(client, method, "/machine" + path, data, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def test_overview(machine, merged):
    seen = await ask(machine, "GET", "")
    states = {s["key"]: s["state"] for s in seen["sources"]}
    assert states == {"twin": "login", "mail": "late", "ruz": "ok", "hseapp": "login", "grades": "off",
                      "wiki": "error", "batches": "ok", "llm": "ok"}  # the mail was read two hours ago
    mail = next(s for s in seen["sources"] if s["key"] == "mail")
    assert mail["ok_at"] == "2026-10-20T10:00:00" and mail["count"] == 12
    assert [(a["text"], a["screen"]) for a in seen["attention"]] == [
        ("The twin is not logged in: no chat is read", "machine/chats"),
        ("HSE App X needs a login", "machine/hseapp"),
        ("A wiki page is not read: DM", "machine/wiki"),
        ("The calendar feeds are off: no CALENDAR_SECRET in .env", None)]
    assert seen["library"] is None  # no library on this port
    assert seen["has"] == {"twin": True, "settings": True, "grades": True, "reminders": True, "pdf": False,
                           "connector": True, "hseapp": True}
    feed = (await call(machine, "GET", "/feed", token=machine.token))
    assert (await feed.json())["problems"] == 4  # the status dot on the feed


async def test_settings_journaled_with_undo(machine):
    settings = await ask(machine, "GET", "/settings")
    assert settings["general"]["news_ttl_hours"] == 24 and "llm_prices" not in settings["general"]
    assert settings["reminders"] == {"enabled": True, "eve": "20:00", "day": "09:00"} and "pdf" not in settings

    changed = await ask(machine, "PATCH", "/settings/general", {"news_ttl_hours": 48, "about_me": "Group 1"})
    assert changed["settings"]["general"]["news_ttl_hours"] == 48 and changed["change"]["undo"]
    await ask(machine, "PATCH", "/settings/reminders", {"eve": "21:30"})
    assert machine.settings.get("reminder_eve") == "21:30" and machine.reminders.times == 1

    bad = await ask(machine, "PATCH", "/settings/general", {"news_ttl_hours": 0}, status=400)
    assert "at least 1" in bad["error"]
    assert (await ask(machine, "PATCH", "/settings/llm", {"x": 1}, status=404))["error"]

    journal = (await ask(machine, "GET", "/journal"))["changes"]
    assert [(c["tool"], c["via"]) for c in journal] == [("update_reminders", "web"), ("update_general_settings", "web")]
    await call(machine, "POST", f"/changes/{changed['change']['id']}/undo", {}, token=machine.token)
    assert machine.settings.get("news_ttl_hours") == 24 and machine.settings.get("about_me") == ""
    assert (await ask(machine, "GET", "/journal"))["changes"][1]["undone_at"]

    # Claude's changes through the connector are in the same journal, with its own mark.
    machine.connector.changes.record(None, "add_deadline", {}, "Added a deadline", None, "", "claude")
    assert (await ask(machine, "GET", "/journal"))["changes"][0]["via"] == "claude"

    refused = await machine.request("PATCH", "/web/api/machine/settings/general", json={"news_ttl_hours": 12},
                                    headers={"Cookie": f"im_session={machine.token}"})
    assert refused.status == 403  # no Origin of the app


async def test_twin_and_chats(machine):
    chats = await ask(machine, "GET", "/chats")
    assert chats["twin"]["account"] is None and [c["title"] for c in chats["chats"]] == ["Course chat"]

    assert (await ask(machine, "POST", "/twin/login", {"phone": "+70000000000"}))["login"]["step"] == "code"
    wrong = await ask(machine, "POST", "/twin/login", {"code": "1"}, status=400)
    assert wrong["error"] == "the code is wrong"
    assert (await ask(machine, "POST", "/twin/login", {"code": "12345"}))["account"]["name"] == "Test Twin"
    assert (await ask(machine, "POST", "/twin/sideways", {}, status=404))["error"] == "no such step"
    states = {s["key"]: s["state"] for s in (await ask(machine, "GET", ""))["sources"]}
    assert states["twin"] == "ok"

    on = await ask(machine, "PATCH", "/chats/-1001", {"enabled": True, "study": True})
    assert on["settings"]["enabled"] and on["settings"]["study"] and on["change"]["undo"]
    assert (await ask(machine, "PATCH", "/chats/-1001", {"filter": "some"}, status=400))["error"]
    assert (await ask(machine, "PATCH", "/chats/-5", {"enabled": True}, status=400))["error"].startswith("no such")
    assert await ask(machine, "GET", "/chats/-1001/senders") == []
    await call(machine, "POST", f"/changes/{on['change']['id']}/undo", {}, token=machine.token)
    assert not (await ask(machine, "GET", "/chats"))["chats"][0]["settings"]["enabled"]

    assert (await ask(machine, "POST", "/twin/logout", {}))["account"] is None


async def test_dictionary_and_llm(machine, budget):
    put = await ask(machine, "PUT", "/courses", {"name": "Algebra", "title": "Algebra", "abbr": "Alg"})
    assert {s["name"] for s in put["subjects"]} == {"Discrete Mathematics", "Algebra"} and put["change"]["undo"]
    removed = await ask(machine, "POST", "/courses/remove", {"name": "Algebra"})
    assert [s["name"] for s in removed["subjects"]] == ["Discrete Mathematics"]
    await call(machine, "POST", f"/changes/{removed['change']['id']}/undo", {}, token=machine.token)
    assert "Algebra" in {s["name"] for s in (await ask(machine, "GET", "/courses"))["subjects"]}

    llm = await ask(machine, "GET", "/llm")
    assert llm["daily_limit_usd"] == 0.5 and len(llm["days"]) == 30
    assert (await ask(machine, "PATCH", "/llm", {"daily_limit_usd": 1.5}))["daily_limit_usd"] == 1.5
    assert (await ask(machine, "PATCH", "/llm", {"daily_limit_usd": 0}, status=400))["error"]
    assert (await ask(machine, "POST", "/llm/raise", {}))["limit_usd"] == 3.0  # one more limit until midnight


async def test_gradebooks(machine, store):
    await ask(machine, "PATCH", "/settings/grades", {"student": "Doe Jane"})
    added = await ask(machine, "POST", "/grades", {"url": SHEET, "label": "DM"})
    assert [(s["id"], s["label"]) for s in added["sheets"]] == [("abcDEF123", "DM")] and added["change"]["undo"]
    assert (await ask(machine, "POST", "/grades", {"url": "https://example.org/x"}, status=400))["error"]

    cells = [{"column": "C", "header": "HW 1", "value": "8"}, {"column": "D", "header": "Test", "value": "14"}]
    store.put_json(VIEW + "abcDEF123", {"course": "DM", "tabs": [
        {"tab": "Group 1", "own_row": 7, "skip": False, "reason": None, "by": "haiku", "header_rows": [1],
         "columns": cells,
         "works": works([{"column": "C", "label": "HW 1", "max": 10, "bonus_column": None}], cells)}]})
    book = await ask(machine, "GET", "/grades/abcDEF123")
    assert book["student"] == "Doe Jane" and [t["tab"] for t in book["tabs"]] == ["Group 1"]

    layout = {"skip": False, "works": [{"column": "C", "label": "HW 1", "max": 10, "bonus_column": None},
                                       {"column": "D", "label": "Test 1", "max": 20, "bonus_column": None}]}
    laid = await ask(machine, "PUT", "/grades/abcDEF123/tabs/Group%201", layout)
    [tab] = laid["tabs"]
    assert tab["by"] == "you" and [(w["label"], w["value"]) for w in tab["works"]] == [("HW 1", 8.0), ("Test 1", 7.0)]
    bad = {"skip": False, "works": [{"column": "Z", "label": "X", "max": 10, "bonus_column": None}]}
    assert (await ask(machine, "PUT", "/grades/abcDEF123/tabs/Group%201", bad, status=400))["error"]
    reset = await ask(machine, "DELETE", "/grades/abcDEF123/tabs/Group%201")
    assert reset["tabs"][0]["by"] == "haiku" and reset["change"]["summary"]

    renamed = await ask(machine, "PATCH", "/grades/abcDEF123", {"name": "Discrete grades"})
    assert renamed["name"] == "Discrete grades"
    assert (await ask(machine, "POST", "/grades/abcDEF123/read", {}))["id"] == "abcDEF123"
    gone = await ask(machine, "DELETE", "/grades/abcDEF123")
    assert gone["sheets"] == [] and gone["change"]["note"]  # its grades are forgotten, the undo cannot bring them


async def test_connector_and_hseapp(machine):
    state = await ask(machine, "GET", "/connector")
    assert state["url"].endswith("/mcp") and state["connections"] == [] and "changes" not in state
    assert (await ask(machine, "DELETE", "/connector/7", status=404))["error"] == "no such connection"

    hse = await ask(machine, "GET", "/hseapp")
    assert hse["status"]["login_needed"] and hse["callback"].endswith(CALLBACK_PATH)
    link = await ask(machine, "POST", "/hseapp/link", {"app": False})
    assert link["url"].startswith("https://")
    assert (await ask(machine, "PATCH", "/hseapp", {"enabled": False}))["status"]["enabled"] is False
    assert (await ask(machine, "POST", "/hseapp/login", {"url": " "}, status=400))["error"]


async def test_notification_settings(machine):
    assert (await ask(machine, "GET", "/settings"))["notifications"] == {
        "kinds": ["deadlines", "reminders", "failures", "llm"], "quiet": ""}
    changed = await ask(machine, "PATCH", "/settings/notifications", {"kinds": ["llm", "deadlines"],
                                                                      "quiet": "23:00-8:00"})
    assert changed["settings"]["notifications"] == {"kinds": ["deadlines", "llm"], "quiet": "23:00-08:00"}
    assert (await ask(machine, "PATCH", "/settings/notifications", {"quiet": "9:00-9:00"}, status=400))["error"]
    assert (await ask(machine, "PATCH", "/settings/notifications", {"kinds": ["news"]}, status=400))["error"]
    await call(machine, "POST", f"/changes/{changed['change']['id']}/undo", {}, token=machine.token)
    assert machine.settings.get("push_kinds") == ["deadlines", "reminders", "failures", "llm"]
    assert machine.settings.get("push_quiet") == ""


def test_quiet_hours():
    at = lambda hm: dt.datetime.fromisoformat(f"2026-10-20T{hm}").replace(tzinfo=clock.TZ)  # noqa: E731
    assert not quiet("", at("03:00"))
    assert quiet("23:00-08:00", at("23:00")) and quiet("23:00-08:00", at("02:30"))
    assert not quiet("23:00-08:00", at("08:00")) and not quiet("23:00-08:00", at("12:00"))
    assert quiet("13:00-14:00", at("13:30")) and not quiet("13:00-14:00", at("14:00"))


async def test_notifier_kinds_and_quiet(store):
    service, seconds = Service(), Seconds()
    push = Push(store, "https://infomaster.example.ts.net", seconds, post=service)
    store.db.execute("create table if not exists web_sessions (id integer primary key)")
    store.db.execute("insert into web_sessions(id) values(1)")
    browser = Browser()
    push.subscribe(1, browser.subscription)
    moment = dt.datetime(2026, 10, 20, 23, 30, tzinfo=clock.TZ)
    notifier = Notifier(push, lambda: "en", now=lambda: moment)
    notifier.settings = settings = Settings(store)
    settings.set("push_kinds", ["deadlines", "failures"])
    settings.set("push_quiet", "23:00-07:00")

    d = Deadline("Calc HW 5", dt.date(2026, 10, 22), "study", "tg", "Calc")
    notifier.deadline(d)
    notifier.reminder("eve", [d])  # reminders are off
    notifier.system(Alert("RUZ is unavailable.", "РУЗ недоступен."))
    await asyncio.gather(*notifier._tasks)
    new, failure = [browser.read(body) for _, body, _ in service.sent]
    assert new["title"] == "New deadline: Calc HW 5" and new["silent"] is True  # the quiet hours: no sound
    assert failure["url"] == "/web/#/machine" and failure["silent"] is True

    service.sent.clear()
    moment = dt.datetime(2026, 10, 21, 9, 0, tzinfo=clock.TZ)
    notifier.now = lambda: moment
    notifier.deadline(d, was="2026-10-24")
    await asyncio.gather(*notifier._tasks)
    [moved] = [browser.read(body) for _, body, _ in service.sent]
    assert "silent" not in moved


async def test_without_parts(aiohttp_client, store):
    """The web app alone (the login's tests): «Machine» answers what it can and refuses the rest."""
    sent, seconds = [], Seconds()

    async def send_code(text):
        sent.append(text)

    client = await aiohttp_client(WebApp(env(), store, send_code=send_code, now=seconds).attach(web.Application()))
    client.token = await log_in(client, sent, seconds)
    seen = await ask(client, "GET", "")
    assert seen["sources"] == [] and seen["attention"] == [] and seen["llm"] is None
    assert (await ask(client, "GET", "/journal"))["changes"] == []
    assert (await ask(client, "GET", "/settings", status=503))["error"]
    assert (await ask(client, "PATCH", "/settings/general", {"batch_hours": 6}, status=503))["error"]
