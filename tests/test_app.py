"""The wiring in the service: a RUZ or HSE App X poll reaches the dictionary, the merged deadlines and Web Push."""

import asyncio
from pathlib import Path
from types import SimpleNamespace

from infomaster.app import Service
from infomaster.collectors.ruz import parse
from infomaster.config import Env
from infomaster.i18n import Alert, Phrase
from infomaster.llm.budget import Level, Notice
from infomaster.outputs import api
from infomaster.processing.batches import Changes

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()


def env() -> Env:
    return Env(api_id=1, api_hash="h", bot_token="1:t", owner_id=42, api_token="tok", public_url="https://x.ts.net")


def pushed(service, now) -> list:
    """The service at Tue 2026-10-20, what goes to Web Push recorded: (kind, what)."""
    told = []
    service.ruz.now = service.schedule.now = service.merged.now = service.hseapp_deadlines.now = now
    service.deadline_watch.now = service.reminders.now = now
    service.deadline_watch.notify = lambda d, was: told.append(("deadline", d.title, was))
    service.web.notifier.send = lambda title, body, *args, **kwargs: told.append(("push", title, body,
                                                                                    kwargs.get("tag")))
    return told


async def test_after_poll(tmp_path, namer, now, aiohttp_client):
    service = Service(env(), tmp_path)  # Telegram clients are made, never connected
    told = pushed(service, now)
    service.subjects.llm = namer
    try:
        await service.deadline_watch.refresh()  # the list was seen before RUZ came
        await service._after_poll(service.ruz._sync(parse(FEED)))
        assert told == []  # the first poll is no news: its Midterms join quietly
        assert [d.title for d in service.merged.visible()] == ["RSF Midterm", "Prog Midterm"]
        assert service._ruz_status() == {"ok_at": None, "error": "RUZ_ICAL_URL is not set", "classes": 9,
                                         "subjects_error": None}
        assert service.calendar.render("main").startswith(b"BEGIN:VCALENDAR")
        assert service.calendar.edit_link("2000001@ruz.hse.ru") == \
            "https://x.ts.net/web/#/c?uid=2000001%40ruz.hse.ru"  # the card in the web app (D69)
        await sources_reach_the_ports(service, aiohttp_client)
        await midterm_follows_its_correction(service, told, now)
        await dictionary_renames_the_midterms_at_once(service, told)

        async def broken():
            raise RuntimeError("the sheets are down")

        service.sheet_publisher.publish = broken
        await service._publish(Changes())  # the sheets failing does not stop the deadlines
    finally:
        service.store.close()


async def sources_reach_the_ports(service, aiohttp_client):
    """run() hands the service's sources to both ports: «Machine» and /api/status read them by name."""
    assert set(service.sources) == {"telegram", "batches", "ruz", "hseapp", "mail", "sheets", "grades",
                                   "images", "wiki"}
    assert service.sources["ruz"]() == service._ruz_status()
    client = await aiohttp_client(api.create_app(service.env, service.budget, service.sources, service.calendar,
                                                 service.merged))
    deadlines = await (await client.get("/api/deadlines", headers={"Authorization": "Bearer tok"})).json()
    assert [item["title"] for item in deadlines["items"]] == ["RSF Midterm", "Prog Midterm"]
    body = await (await client.get("/api/status", headers={"Authorization": "Bearer tok"})).json()
    assert body["ruz"] == service._ruz_status() and {"telegram", "batches", "llm"} <= set(body)
    assert body["hseapp"] == {"enabled": True, "logged_in": False, "ok_at": None, "error": None,
                              "login_needed": True, "items": 0, "done": 0, "labels_error": None}
    assert body["mail"] == {"configured": False, "ok_at": None, "error": "MAIL_USER and MAIL_PASSWORD are not set",
                            "last": None, "letters": 0}
    assert not service.batches.mail  # without the box the schedule does not wait for letters
    assert {key: body["grades"][key] for key in ("configured", "sheets", "new")} == \
        {"configured": False, "sheets": {}, "new": 0}


async def midterm_follows_its_correction(service, told, now):
    """A chat moved the interim assessment: the batch run tells the Midterm's move, the owner's hiding moves it back."""
    uid = "2000007@ruz.hse.ru"
    item = service.ruz.by_uid(uid)
    with service.store.db:
        moved = service.schedule.events.add({
            "uid": uid, "class_uid": uid, "source": "tg", "chat_id": None, "msg_ids": [1], "course": item["subject"],
            "title": "Intro to Prog", "kind": item["kind"], "start": "2026-11-05T10:00:00",
            "end": "2026-11-05T13:00:00", "room": "R401", "required": True}, now())
    service.batches.now = now
    await service.batches.run_chats([])
    assert told[-1] == ("deadline", "Prog Midterm", "2026-10-29")
    await service.schedule.set_hidden(moved, True)
    assert told[-1] == ("deadline", "Prog Midterm", "2026-11-05")


async def dictionary_renames_the_midterms_at_once(service, told):
    """A manual entry in the dictionary: the Midterm's label follows now, and a rename is no news."""
    before = len(told)
    service.subjects.set_manual("Foundations of Russian Statehood", "Statehood", "State")
    await asyncio.gather(*service.bot._tasks)
    assert service.deadlines.get(service.ruz.by_uid("2000006@ruz.hse.ru")["deadline_id"])["label"] == \
        "State Midterm"
    assert len(told) == before


async def test_deadlines_wiring(tmp_path, now):
    """The first HSE App X poll joins quietly, later changes are told; the owner's own changes are not."""
    service = Service(env(), tmp_path)
    told = pushed(service, now)
    try:
        assert service.hseapp_deadlines.on_change == service._hseapp_changed
        assert service.merged.on_change == service.deadline_watch.changed
        await service.deadline_watch.refresh()

        def lists(*items):
            service.store.put_json("hseapp.deadlines", {"fetched_at": None, "done": [], "items": [
                {"id": raw_id, "label": label, "due": due, "course": None, "title": label}
                for raw_id, label, due in items]})

        lists(("h1", "DM HW 3", "2026-10-22"), ("h2", "Calc HW 1", "2026-10-23"))
        await service._hseapp_changed(True)
        assert told == []
        lists(("h1", "DM HW 3", "2026-10-22"), ("h2", "Calc HW 1", "2026-10-23"), ("h3", "Eng essay", "2026-10-25"))
        await service._hseapp_changed(False)
        assert told == [("deadline", "Eng essay", None)]

        service.merged.add("Dentist", now().date(), "personal")  # the owner's: no news
        await service.deadline_watch.settle()
        assert len(told) == 1

        with service.store.db:  # what a batch run saved
            service.deadlines.add({"source": "tg", "chat_id": None, "msg_ids": [1], "course": None,
                                   "label": "Alg HW 2", "due": "2026-10-24"}, now())
        await service._publish(Changes())
        assert told[-1] == ("deadline", "Alg HW 2", None)
    finally:
        service.store.close()


async def test_notices_go_to_web_push(tmp_path, now):
    """D69: the sources' failures, the logins they need and the spent LLM limit are pushed; nothing else is."""
    service = Service(env(), tmp_path)
    told = pushed(service, now)
    try:
        assert service.hseapp.callback_url == "https://x.ts.net/hseapp/callback"
        assert service.hseapp.on_poll == service.hseapp_deadlines.after_poll
        service._to_system(Phrase("RUZ is available again.", "РУЗ снова доступен."))  # no failure: not pushed
        service._to_system(Alert("RUZ is unavailable.", "РУЗ недоступен."))
        service.hseapp._tell_login()
        service._twin_notice(Phrase("The twin needs a login.", "Твинку нужен вход."))
        service.budget.notify(Notice(Level.SOFT, 0.4, 0.5))  # the 80 % warning goes quietly
        service.budget.notify(Notice(Level.HARD, 0.5, 0.5))
        assert [(t[0], t[1], t[3]) for t in told] == [
            ("push", "infomaster", None), ("push", "infomaster", "login-hseapp"), ("push", "infomaster", "login-twin"),
            ("push", "The LLM limit is spent", "budget")]
    finally:
        service.store.close()


async def test_watch_follows_the_twins_login_and_logout():
    """A login in «Machine» is watched from then on, a logout is no loss, a lost connection ends the service."""
    loop = asyncio.get_running_loop()
    bot = SimpleNamespace(client=SimpleNamespace(disconnected=loop.create_future()))
    twin = SimpleNamespace(client=None, changed=asyncio.Event())
    service, stop = SimpleNamespace(bot=bot, twin=twin), asyncio.Event()
    watch = asyncio.create_task(Service._watch(service, stop))
    await asyncio.sleep(0)

    first = SimpleNamespace(disconnected=loop.create_future())
    twin.client = first
    twin.changed.set()  # logged in
    await asyncio.sleep(0.01)
    twin.client = None
    twin.changed.set()  # logged out: its client disconnects too, and that is no loss
    first.disconnected.set_result(None)
    await asyncio.sleep(0.01)
    assert not watch.done()

    second = SimpleNamespace(disconnected=loop.create_future())
    twin.client = second
    twin.changed.set()
    await asyncio.sleep(0.01)
    second.disconnected.set_result(None)  # lost for good
    await asyncio.wait_for(watch, 1)

    watch = asyncio.create_task(Service._watch(SimpleNamespace(bot=bot, twin=SimpleNamespace(
        client=None, changed=asyncio.Event())), stop))
    stop.set()
    await asyncio.wait_for(watch, 1)
