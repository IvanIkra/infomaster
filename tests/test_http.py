import datetime as dt
import hashlib
import hmac
import json
import time
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import urlencode

import pytest

from infomaster import clock
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.inbox import Inbox
from infomaster.collectors.ruz import parse
from infomaster.config import ConfigError, Env
from infomaster.outputs import api, miniapp
from infomaster.outputs.ical_feed import CalendarFeed
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings

BOT_TOKEN = "123:secret"
OWNER = 42
FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()
TWIN = {"telegram": lambda: {"ok_at": None, "error": None, "login_needed": False},
        "ruz": lambda: {"ok_at": None, "error": "RUZ_ICAL_URL is not set", "classes": 0, "subjects_error": None}}


def env(**extra) -> Env:
    return Env(api_id=1, api_hash="h", bot_token=BOT_TOKEN, owner_id=OWNER, forum_id=-100, api_token="tok",
               public_url="https://infomaster.example.ts.net", **extra)


def mini(budget, journal, store, schedule, inbox=None, merged=None, sources=None):
    hse = HseApp(store, Settings(store), env().public_url + CALLBACK_PATH)
    return miniapp.create_app(env(), budget, journal, inbox or schedule.inbox, ChatSettings(store), sources or TWIN,
                              schedule.subjects, schedule, hse, merged)


def init_data(user_id: int = OWNER, auth_date: int | None = None, token: str = BOT_TOKEN) -> str:
    """initData signed the way Telegram signs it."""
    fields = {"auth_date": str(auth_date or int(time.time())), "query_id": "AAE",
              "user": json.dumps({"id": user_id, "first_name": "Ivan"}), "signature": "sig"}
    check = "\n".join(f"{k}={v}" for k, v in sorted(fields.items()))
    secret = hmac.new(b"WebAppData", token.encode(), hashlib.sha256).digest()
    return urlencode({**fields, "hash": hmac.new(secret, check.encode(), hashlib.sha256).hexdigest()})


def test_init_data():
    assert miniapp.check_init_data(init_data(), BOT_TOKEN)["id"] == OWNER
    assert miniapp.check_init_data(init_data(token="other:token"), BOT_TOKEN) is None
    assert miniapp.check_init_data(init_data(auth_date=int(time.time()) - 2 * 86400), BOT_TOKEN) is None
    assert miniapp.check_init_data(init_data().replace("Ivan", "Eve"), BOT_TOKEN) is None
    assert miniapp.check_init_data("", BOT_TOKEN) is None


def owner() -> dict:
    return {"Authorization": "tma " + init_data()}


async def test_miniapp(aiohttp_client, budget, journal, store, schedule, merged):
    client = await aiohttp_client(mini(budget, journal, store, schedule, merged=merged))
    assert (await client.get("/app/api/me")).status == 401
    assert (await client.get("/app/api/me", headers={"Authorization": "tma " + init_data(7)})).status == 403
    response = await client.get("/app/api/me", headers={"Authorization": "tma " + init_data()})
    assert response.status == 200
    body = await response.json()
    assert body["first_name"] == "Ivan" and body["llm"]["limit_usd"] == 0.5 and body["language"] == "en"
    page = await client.get("/app/")
    assert page.status == 200 and "telegram-web-app.js" in await page.text()



async def test_miniapp_llm(aiohttp_client, budget, journal, store, schedule, merged, now):
    """The «Расход LLM» screen: periods by task and chat, 30 days, the limit changed and raised."""
    from conftest import spend

    inbox, settings = schedule.inbox, budget.settings
    inbox.put_chat(-1001, "Матан", "supergroup", None, 0)
    spend(journal, 0.20, now(), task="batch", chat_id=-1001)
    spend(journal, 0.05, now(), task="batch", chat_id=0)        # the mailbox
    spend(journal, 0.05, now(), task="subjects")                # no chat
    spend(journal, 0.30, now() - dt.timedelta(days=3), task="image", chat_id=-1001)
    client = await aiohttp_client(miniapp.create_app(env(), budget, journal, inbox, ChatSettings(store), TWIN,
                                                     schedule.subjects, schedule, None, merged, settings))
    assert (await client.get("/app/api/llm")).status == 401
    body = await (await client.get("/app/api/llm", headers=owner())).json()
    today, month = body["periods"]["today"], body["periods"]["month"]
    assert today["cost_usd"] == 0.3 and today["runs"] == 3 and month["cost_usd"] == 0.6
    assert today["by_task"] == [{"task": "batch", "runs": 2, "failed": 0, "cost_usd": 0.25, "tokens": 4},
                                {"task": "subjects", "runs": 1, "failed": 0, "cost_usd": 0.05, "tokens": 2}]
    assert [(c["title"], c["cost_usd"]) for c in today["by_chat"]] == [("Матан", 0.2), ("Mail", 0.05)]
    settings.set("miniapp_language", "ru")  # what the service names itself follows the Mini App's language (D41)
    body = await (await client.get("/app/api/llm", headers=owner())).json()
    assert [c["title"] for c in body["periods"]["today"]["by_chat"]] == ["Матан", "Почта"]
    settings.reset("miniapp_language")
    assert len(body["days"]) == 30 and body["days"][-1] == {"day": "2026-10-20", "runs": 3, "failed": 0,
                                                            "cost_usd": 0.3, "stopped": False}
    assert body["days"][-4]["cost_usd"] == 0.3
    assert body["month_forecast_usd"] == round(0.6 / 20 * 31, 4) and body["daily_limit_usd"] == 0.5

    assert (await client.patch("/app/api/llm", json={"daily_limit_usd": 0}, headers=owner())).status == 400
    assert (await client.patch("/app/api/llm", json={"daily_limit_usd": 1, "x": 1}, headers=owner())).status == 400
    body = await (await client.patch("/app/api/llm", json={"daily_limit_usd": 0.75}, headers=owner())).json()
    assert body["daily_limit_usd"] == 0.75 and body["limit_usd"] == 0.75
    body = await (await client.post("/app/api/llm/raise", headers=owner())).json()
    assert body["limit_usd"] == 1.5 and body["raised_usd"] == 0.75 and body["level"] == "ok"



async def test_miniapp_twin_login(aiohttp_client, budget, journal, store, schedule, merged, tmp_path, monkeypatch):
    """Phone, code, logout from the «Чаты» screen; the twin's errors come back as 400 with their text."""
    from test_telegram import FakeClient

    from infomaster.collectors import telegram

    FakeClient.made, FakeClient.password = [], False
    monkeypatch.setattr(telegram, "TelegramClient", FakeClient)
    twin = telegram.Twin(SimpleNamespace(api_id=1, api_hash="h", owner_id=OWNER), schedule.inbox, ChatSettings(store),
                         tmp_path / "twin", lambda text: None)
    client = await aiohttp_client(miniapp.create_app(env(), budget, journal, schedule.inbox, ChatSettings(store),
                                                     {**TWIN, "telegram": twin.status}, schedule.subjects, schedule,
                                                     None, merged, twin=twin))
    assert (await client.post("/app/api/twin/login", json={"phone": "+79161234567"})).status == 401
    bad = await client.post("/app/api/twin/login", json={"code": "1"}, headers=owner())
    assert bad.status == 400 and await bad.text() == "ask for a code first"
    assert (await client.post("/app/api/twin/login", json={"x": 1}, headers=owner())).status == 400
    body = await (await client.post("/app/api/twin/login", json={"phone": "+79161234567"}, headers=owner())).json()
    assert body["login"] == {"step": "code", "phone": "+7916•••••67"}
    body = await (await client.post("/app/api/twin/login", json={"code": "12345"}, headers=owner())).json()
    assert body["account"]["username"] == "twin_acc" and body["login_needed"] is False
    chats = await (await client.get("/app/api/chats", headers=owner())).json()
    assert chats["twin"]["account"]["name"] == "Twin"
    body = await (await client.post("/app/api/twin/logout", headers=owner())).json()
    assert body["account"] is None and body["login_needed"] is True
    assert (await client.post("/app/api/twin/logout", headers=owner())).status == 400


async def test_miniapp_general(aiohttp_client, budget, journal, store, schedule, merged):
    settings = budget.settings
    client = await aiohttp_client(miniapp.create_app(env(), budget, journal, schedule.inbox, ChatSettings(store), TWIN,
                                                     schedule.subjects, schedule, None, merged, settings))
    body = await (await client.get("/app/api/general", headers=owner())).json()
    assert body["batch_hours"] == 3 and body["news_ttl_hours"] == 24 and 24 in body["batch_choices"]
    prices = body["llm_prices"]
    changed = {**prices, "claude-haiku-5-5": {**prices["claude-haiku-5-5"], "input": 0.2}}
    body = await (await client.patch("/app/api/general", json={"batch_hours": 6, "llm_prices": changed},
                                     headers=owner())).json()
    assert body["batch_hours"] == 6 and settings.get("llm_prices")["claude-haiku-5-5"]["input"] == 0.2
    bad = await client.patch("/app/api/general", json={"batch_hours": 2, "news_ttl_hours": 500}, headers=owner())
    assert bad.status == 400 and settings.get("batch_hours") == 6  # nothing saved from a rejected request
    assert (await client.patch("/app/api/general", json={"pdf_pages": 3}, headers=owner())).status == 400
    assert (await client.patch("/app/api/general", json={}, headers=owner())).status == 400

    # the languages (D41): English by default, each its own
    assert (body["forum_language"], body["miniapp_language"], body["languages"]) == ("en", "en", ["en", "ru"])
    body = await (await client.patch("/app/api/general", json={"miniapp_language": "ru"}, headers=owner())).json()
    assert (body["forum_language"], body["miniapp_language"]) == ("en", "ru")
    assert (await (await client.get("/app/api/me", headers=owner())).json())["language"] == "ru"
    bad = await client.patch("/app/api/general", json={"forum_language": "de"}, headers=owner())
    assert bad.status == 400 and settings.get("forum_language") == "en"


async def test_miniapp_chats(aiohttp_client, budget, journal, store, schedule, merged):
    inbox = Inbox(store)
    inbox.put_chat(-1001, "Матан", "supergroup", None, 10)
    inbox.put_chat(-1002, "Болталка", "group", None, 5)
    inbox.add(-1001, 11, {"sender_id": 7, "sender_name": "Староста", "at": clock.now_utc(), "text": "Дедлайн"})
    client = await aiohttp_client(mini(budget, journal, store, schedule, inbox, merged=merged))
    assert (await client.get("/app/api/chats")).status == 401

    response = await client.patch("/app/api/chats/-1002", json={"enabled": True, "people": [7, 7, 3]},
                                  headers=owner())
    assert (await response.json())["people"] == [3, 7]
    body = await (await client.get("/app/api/chats", headers=owner())).json()
    assert [chat["title"] for chat in body["chats"]] == ["Болталка", "Матан"]  # enabled first
    assert body["chats"][1]["messages"] == 1 and body["twin"]["login_needed"] is False

    bad = await client.patch("/app/api/chats/-1001", json={"study": True, "filter": "some"}, headers=owner())
    assert bad.status == 400
    assert ChatSettings(store).get(-1001, "study") is False  # nothing saved from a rejected request
    assert (await client.patch("/app/api/chats/-5", json={"enabled": True}, headers=owner())).status == 404
    assert (await client.patch("/app/api/chats/-1001", data="[", headers=owner())).status == 400

    senders = await (await client.get("/app/api/chats/-1002/senders", headers=owner())).json()
    assert {s["id"] for s in senders} == {3, 7}  # chosen people are listed before they write
    senders = await (await client.get("/app/api/chats/-1001/senders", headers=owner())).json()
    assert senders == [{"id": 7, "name": "Староста", "messages": 1}]


async def test_internal_api(aiohttp_client, budget, schedule, merged):
    client = await aiohttp_client(api.create_app(env(), budget, TWIN, CalendarFeed(schedule), merged))
    assert (await client.get("/api/status")).status == 401
    assert (await client.get("/api/status", headers={"Authorization": "Bearer wrong"})).status == 401
    response = await client.get("/api/status", headers={"Authorization": "Bearer tok"})
    body = await response.json()
    assert body["llm"]["level"] == "ok" and body["telegram"]["login_needed"] is False
    assert body["ruz"]["error"] == "RUZ_ICAL_URL is not set" and body["ruz"]["classes"] == 0
    assert (await client.get("/calendar/x/main.ics")).status == 404  # no CALENDAR_SECRET: no feeds



async def test_news_api(aiohttp_client, budget, schedule, merged, store, now):
    """The live news of the «Новости» topic, in rank order; replaced, hidden and old ones are not given."""
    news, settings = News(store), Settings(store)

    def add(full, importance, study, hours_ago):
        posted = clock.local_iso(now() - dt.timedelta(hours=hours_ago))
        with store.db:
            return news.add({"source": "tg", "chat_id": -1001, "msg_ids": [1], "study": study, "posted_at": posted,
                             "full": full, "short": full[:10], "importance": importance}, now())

    chatter, study, old = add("Concert tonight", 3, False, 1), add("Calc KR-1 Oct 22", 4, True, 2), add("Old", 5, 0, 30)
    replaced, hidden = add("Alg test Nov 6", 4, True, 3), add("Party", 5, False, 1)
    with store.db:
        news.replace(replaced, add("Alg test Nov 7", 4, True, 1))
        news.hide(hidden)
    client = await aiohttp_client(api.create_app(env(), budget, TWIN, CalendarFeed(schedule), merged,
                                                 news_list=news, settings=settings, now=now))
    assert (await client.get("/api/news")).status == 401
    body = await (await client.get("/api/news", headers=AUTH)).json()
    assert body["ttl_hours"] == 24 and old not in [item["id"] for item in body["items"]]
    assert [item["full"] for item in body["items"]] == ["Alg test Nov 7", "Calc KR-1 Oct 22", "Concert tonight"]
    assert body["items"][1] == {"id": study, "source": "tg", "study": True,
                                "posted_at": clock.local_iso(now() - dt.timedelta(hours=2)),
                                "full": "Calc KR-1 Oct 22", "short": "Calc KR-1 ", "importance": 4}
    assert chatter == body["items"][2]["id"]


def test_env():
    base = {"TG_API_ID": "1", "TG_API_HASH": "h", "TG_BOT_TOKEN": "t", "TG_OWNER_ID": "42",
            "TG_FORUM_ID": "-1001", "API_TOKEN": "a", "PUBLIC_URL": "https://x.ts.net/"}
    loaded = Env.from_environ(base)
    assert loaded.forum_id == -1001 and loaded.public_url == "https://x.ts.net" and loaded.public_port == 8080
    with pytest.raises(ConfigError, match="TG_BOT_TOKEN"):
        Env.from_environ({**base, "TG_BOT_TOKEN": ""})
    with pytest.raises(ConfigError):
        Env.from_environ({**base, "PUBLIC_URL": "http://x"})
    assert (loaded.ruz_ical_url, loaded.calendar_secret, loaded.calendar_public_secret) == ("", "", "")  # optional
    secret = "s3cr3t-calendar_link"
    loaded = Env.from_environ({**base, "RUZ_ICAL_URL": " https://api.hseapp.ru/v3/ruz/cal/abc ",
                               "CALENDAR_SECRET": secret})
    assert loaded.ruz_ical_url == "https://api.hseapp.ru/v3/ruz/cal/abc" and loaded.calendar_secret == secret
    assert Env.from_environ({**base, "CALENDAR_PUBLIC_SECRET": secret}).calendar_public_secret == secret
    for bad in ({"RUZ_ICAL_URL": "api.hseapp.ru/cal"}, {"CALENDAR_SECRET": "short"},
                {"CALENDAR_SECRET": "with/slash-and-long-enough"}, {"CALENDAR_PUBLIC_SECRET": "short"},
                {"CALENDAR_SECRET": secret, "CALENDAR_PUBLIC_SECRET": secret}):
        with pytest.raises(ConfigError):
            Env.from_environ({**base, **bad})


async def test_miniapp_subjects(aiohttp_client, budget, journal, store, schedule, merged):
    client = await aiohttp_client(mini(budget, journal, store, schedule, merged=merged))
    subjects = schedule.subjects
    await subjects.resolve(["Алгебра (рус)"])
    assert (await client.get("/app/api/subjects")).status == 401

    body = await (await client.get("/app/api/subjects", headers=owner())).json()
    assert body["subjects"] == [{"name": "Алгебра", "title": "Algebra", "abbr": "Alg", "manual": False}]
    assert (body["error"], body["title_chars"], body["abbr_chars"]) == (None, 13, 6)

    saved = await client.put("/app/api/subjects", json={"name": "Алгебра", "title": "Linear Alg", "abbr": "LA"},
                             headers=owner())
    assert await saved.json() == {"name": "Алгебра", "title": "Linear Alg", "abbr": "LA", "manual": True}
    for bad in ({"name": "", "title": "X", "abbr": "X"}, {"name": "Матан", "title": "Calculus", "abbr": "Calculus"},
                {"name": "Матан", "title": "", "abbr": "C"}, {"name": "Матан", "title": 5, "abbr": "C"}):
        response = await client.put("/app/api/subjects", json=bad, headers=owner())
        assert response.status == 400
    assert "the abbreviation" in await (await client.put("/app/api/subjects", json={
        "name": "Матан", "title": "Calculus", "abbr": "Calculus"}, headers=owner())).text()
    assert (await client.put("/app/api/subjects", data="[", headers=owner())).status == 400

    await subjects.resolve(["Алгебра"])
    assert subjects.get("Алгебра").title == "Linear Alg"  # Haiku is not asked about a manual entry
    deleted = await client.delete("/app/api/subjects", json={"name": "Алгебра"}, headers=owner())
    assert deleted.status == 200 and subjects.get("Алгебра") is None
    assert (await client.delete("/app/api/subjects", json={"name": "Алгебра"}, headers=owner())).status == 404


async def test_miniapp_agenda(aiohttp_client, budget, journal, store, schedule, now, merged):
    """The Расписание screen: the timetable as the feeds have it; the chats' events switched and hidden, the
    owner's own events added, changed and deleted, classes moved by the owner (D44)."""
    schedule.ruz._sync(parse(FEED))
    client = await aiohttp_client(mini(budget, journal, store, schedule, merged=merged))
    events = schedule.events
    with store.db:
        told = events.add({"uid": "tg-1@infomaster", "source": "tg", "chat_id": None, "msg_ids": [5],
                           "course": "Алгебра", "title": "Alg consult", "kind": "consultation",
                           "start": "2026-10-23T18:10:00", "end": None, "room": "R205", "required": True}, now())
    assert (await client.get("/app/api/agenda")).status == 401

    body = await (await client.get("/app/api/agenda", headers=owner())).json()
    assert body["today"] == "2026-10-20" and body["calendar"] is False and body["ruz"]["error"]
    items = {item["uid"]: item for item in body["items"]}
    told_item = items["tg-1@infomaster"]
    assert (told_item["type"], told_item["end"]) == ("Consultation", "2026-10-23T19:30:00")
    assert items["2000005@ruz.hse.ru"]["event"] is None and "2000001@ruz.hse.ru" not in items  # Oct 19: past

    body = await (await client.patch(f"/app/api/events/{told}", json={"required": False, "hidden": True},
                                     headers=owner())).json()
    assert "tg-1@infomaster" not in {i["uid"] for i in body["items"]} and body["hidden"][0]["id"] == told
    assert events.get(told)["chosen"] and not events.get(told)["required"]

    new = {"title": " Supervisor ", "kind": "Event", "start": "2026-10-24T15:00", "end": "2026-10-24T16:00",
           "room": "R205", "required": True}
    body = await (await client.post("/app/api/events", json=new, headers=owner())).json()
    [own] = [i for i in body["items"] if i["title"] == "Supervisor"]
    assert (own["start"], own["source"], own["event"]["own"]) == ("2026-10-24T15:00:00", "own", True)
    body = await (await client.patch(f"/app/api/events/{own['event']['id']}", json={"title": "Supervisor meeting"},
                                     headers=owner())).json()
    [own] = [i for i in body["items"] if i["source"] == "own"]
    assert own["title"] == "Supervisor meeting"
    response = await client.delete(f"/app/api/events/{own['event']['id']}", headers=owner())
    assert response.status == 200 and all(i["source"] != "own" for i in (await response.json())["items"])
    assert (await client.delete(f"/app/api/events/{told}", headers=owner())).status == 404  # a chat's: hidden only

    fix = {"class_uid": "2000005@ruz.hse.ru", "start": "2026-10-23T15:00", "end": "2026-10-23T16:20", "room": "R510",
           "cancelled": False}
    body = await (await client.post("/app/api/corrections", json=fix, headers=owner())).json()
    moved = next(i for i in body["items"] if i["uid"] == "2000005@ruz.hse.ru")
    assert (moved["start"], moved["location"], moved["event"]["own"], moved["event"]["class"]["start"]) == (
        "2026-10-23T15:00:00", "R510", True, "2026-10-23T09:30:00")
    response = await client.patch(f"/app/api/events/{moved['event']['id']}", json={"title": "x"}, headers=owner())
    assert response.status == 400  # a correction is changed by another correction

    for bad in ({}, {"required": 1}, {"title": ""}, {"hidden": True, "title": "x"}, {"start": "2026-10-24T17:00"},
                {"start": "2026-10-24T17:00", "end": "2026-10-24T16:00"}, {"kind": ""}):
        assert (await client.patch(f"/app/api/events/{told}", json=bad, headers=owner())).status == 400
    for bad in ({**new, "title": "x" * 121}, {**new, "start": "tomorrow"}, {**new, "required": "yes"}):
        assert (await client.post("/app/api/events", json=bad, headers=owner())).status == 400
    assert (await client.post("/app/api/corrections", json={**fix, "class_uid": "nope"}, headers=owner())).status == 404
    assert (await client.patch("/app/api/events/999", json={"hidden": False}, headers=owner())).status == 404
    assert (await client.patch("/app/api/events/x", json={"hidden": False}, headers=owner())).status == 404


async def test_miniapp_hiding_a_correction_moves_its_midterm(aiohttp_client, budget, journal, store, schedule, now,
                                                             merged):
    schedule.inbox.put_chat(-1001, "Матан", "supergroup", None, 0)
    await schedule.after_poll(schedule.ruz._sync(parse(FEED)))
    published = []

    async def record(midterms):
        published.append(midterms)

    schedule.on_midterms = record
    uid = "2000007@ruz.hse.ru"  # the interim assessment, Thu Oct 29
    with store.db:
        moved = schedule.events.add({"uid": uid, "class_uid": uid, "source": "tg", "chat_id": -1001, "msg_ids": [5],
                                     "course": "Введение в программирование", "title": "Intro to Prog",
                                     "kind": "Текущий рубежный контроль", "start": "2026-11-05T10:00:00",
                                     "end": "2026-11-05T13:00:00", "room": "R401", "required": True}, now())
    schedule.sync_midterms()
    client = await aiohttp_client(mini(budget, journal, store, schedule, merged=merged))
    response = await client.patch(f"/app/api/events/{moved}", json={"hidden": True}, headers=owner())
    assert response.status == 200
    [item] = [i for i in (await response.json())["items"] if i["uid"] == uid]
    assert item["event"]["hidden"] and item["start"] == "2026-10-29T10:00:00"
    [(_, back)] = published[0].moved
    assert schedule.deadlines.get(back)["due"] == "2026-10-29"


# --- deadlines: HA's to-do lists and the Mini App ------------------------------------------------

AUTH = {"Authorization": "Bearer tok"}


def add_row(merged, label, due, source="tg", course="Математический анализ"):
    with merged.db:
        return merged.deadlines.add({"source": source, "chat_id": -1001, "msg_ids": [1], "course": course,
                                     "label": label, "due": due}, merged.now())


async def test_deadlines_api(aiohttp_client, budget, schedule, merged):
    """The merged list for Tablo, as Tablo's route gave it; nothing changes it through the API (D34)."""
    client = await aiohttp_client(api.create_app(env(), budget, TWIN, CalendarFeed(schedule), merged))
    today = merged.today()
    add_row(merged, "Calc HW 3", (today + dt.timedelta(days=1)).isoformat(), "mail")
    merged.hseapp.store.put_json("hseapp.deadlines", {"fetched_at": None, "done": [], "items": [
        {"id": "p1", "label": "Prog HW 1", "due": (today - dt.timedelta(days=2)).isoformat(), "course": None,
         "title": "ДЗ 1 по программированию"}]})
    assert (await client.get("/api/deadlines")).status == 401
    resp = await client.get("/api/deadlines", headers=AUTH)
    items = (await resp.json())["items"]
    assert [(i["title"], i["source"], i["overdue"]) for i in items] == [
        ("Calc HW 3", "mail", False), ("Prog HW 1", "hseapp", True)]
    assert set(items[0]) == {"uid", "title", "due", "category", "source", "overdue"} and items[0]["category"] == "study"

    body = {"title": "Dentist", "due": (today + dt.timedelta(days=5)).isoformat(), "category": "personal"}
    assert (await client.post("/api/deadlines", json=body, headers=AUTH)).status == 405
    assert (await client.patch(f"/api/deadlines/{items[0]['uid']}", json={"done": True}, headers=AUTH)).status == 404
    assert (await client.delete(f"/api/deadlines/{items[0]['uid']}", headers=AUTH)).status == 404
    assert {d.title for d in merged.all()} == {"Calc HW 3", "Prog HW 1"} and merged.changes == []


async def test_miniapp_deadlines(aiohttp_client, budget, journal, store, schedule, merged):
    sources = {**TWIN, "hseapp": lambda: {"login_needed": True, "error": None, "enabled": True}}
    client = await aiohttp_client(mini(budget, journal, store, schedule, merged=merged, sources=sources))
    assert (await client.get("/app/api/deadlines")).status == 401
    schedule.inbox.put_chat(-1001, "Матан", "group", None, 0)
    row = add_row(merged, "Calc HW 3", "2026-10-21")
    letter = add_row(merged, "Phil essay", "2026-10-22", "mail", "Философия")
    body = await (await client.get("/app/api/deadlines", headers=owner())).json()
    assert body["hseapp"] == {"login_needed": True, "error": None} and body["today"] == "2026-10-20"
    item, essay = body["items"]
    assert item == {"uid": merged.items()[0]["uid"], "title": "Calc HW 3", "due": "2026-10-21", "category": "study",
                    "source": "tg", "course": "Математический анализ", "overdue": False, "from": "Матан",
                    "link": None}  # where the chat and the letter it came from are known (D44)
    assert (essay["from"], essay["link"]) == ("letter", None)
    with merged.db:
        merged.deadlines.drop([letter], merged.now())
    assert body["own"] == [] and body["hidden"] == []

    for bad in ({"title": "", "due": "2026-10-25", "category": "study"},
                {"title": "Gym", "due": "25.10", "category": "study"},
                {"title": "Gym", "due": "2026-10-25", "category": "sport"},
                {"title": "x" * 201, "due": "2026-10-25", "category": "study"}):
        assert (await client.post("/app/api/deadlines", json=bad, headers=owner())).status == 400
    body = await (await client.post("/app/api/deadlines", json={"title": "Gym", "due": "2026-10-25",
                                                                 "category": "personal"}, headers=owner())).json()
    assert body["own"] == [{"uid": "ha-1", "title": "Gym", "due": "2026-10-25", "category": "personal"}]
    assert [i["title"] for i in body["items"]] == ["Calc HW 3", "Gym"]

    body = await (await client.patch("/app/api/deadlines/ha-1", json={"due": "2026-10-26"}, headers=owner())).json()
    assert body["own"][0]["due"] == "2026-10-26"
    body = await (await client.patch("/app/api/deadlines/ha-1", json={"category": "study"}, headers=owner())).json()
    assert body["own"][0]["category"] == "study"
    body = await (await client.patch("/app/api/deadlines/ha-1", json={"category": "personal"}, headers=owner())).json()
    assert body["own"][0]["category"] == "personal"
    assert (await client.patch("/app/api/deadlines/ha-1", json={"category": "sport"}, headers=owner())).status == 400
    assert (await client.patch("/app/api/deadlines/ha-1", json={}, headers=owner())).status == 400
    assert (await client.patch("/app/api/deadlines/ha-9", json={"title": "x"}, headers=owner())).status == 404

    body = await (await client.delete(f"/app/api/deadlines/{item['uid']}", headers=owner())).json()
    assert body["hidden"] == [{"uid": item["uid"], "title": "Calc HW 3", "due": "2026-10-21", "source": "tg"}]
    assert [i["title"] for i in body["items"]] == ["Gym"]
    body = await (await client.delete(f"/app/api/hidden/{item['uid']}", headers=owner())).json()
    assert body["hidden"] == [] and [i["title"] for i in body["items"]] == ["Calc HW 3", "Gym"]
    assert (await client.delete(f"/app/api/hidden/{item['uid']}", headers=owner())).status == 404

    # An own one the merge leaves out as a copy of another is still the owner's to change.
    body = await (await client.post("/app/api/deadlines", json={"title": "Calc HW 3", "due": "2026-10-21",
                                                                 "category": "study"}, headers=owner())).json()
    assert len(body["own"]) == 2 and [i["source"] for i in body["items"]] == ["tg", "ha"]
    body = await (await client.delete("/app/api/deadlines/ha-2", headers=owner())).json()
    assert [own["uid"] for own in body["own"]] == ["ha-1"] and row


async def test_miniapp_summaries(aiohttp_client, budget, journal, store, schedule, merged, forum, now):
    from infomaster.outputs.summaries import SummaryTopic
    from infomaster.processing.summaries import Summaries

    inbox, chat_settings, settings = schedule.inbox, ChatSettings(store), Settings(store)
    inbox.put_chat(-1001, "Матан", "supergroup", None, 0)
    chat_settings.update(-1001, {"enabled": True, "summary": True})
    topic = SummaryTopic(forum, store, inbox, chat_settings, Summaries(chat_settings, inbox, None, now=now), settings,
                         OWNER, -100, now=now)
    hse = HseApp(store, settings, env().public_url + CALLBACK_PATH)
    client = await aiohttp_client(miniapp.create_app(env(), budget, journal, inbox, chat_settings, TWIN,
                                                     schedule.subjects, schedule, hse, merged, settings,
                                                     summary_topic=topic))
    response = await client.get("/app/api/summaries", headers=owner())
    state = await response.json()
    assert state["time"] == "21:00" and state["chats"] == [{"id": -1001, "title": "Матан"}]

    response = await client.patch("/app/api/summaries", json={"time": "9:15"}, headers=owner())
    assert (await response.json())["time"] == "09:15" and settings.get("summary_time") == "09:15"
    assert topic._woken.is_set()  # the loop counts its sleep anew
    response = await client.patch("/app/api/summaries", json={"time": "25:00"}, headers=owner())
    assert response.status == 400 and settings.get("summary_time") == "09:15"
    response = await client.patch("/app/api/summaries", json={"time": "21:00", "x": 1}, headers=owner())
    assert response.status == 400
    assert (await client.get("/app/api/summaries")).status == 401
