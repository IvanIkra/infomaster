"""The wiring in the service: a RUZ or HSE App X poll reaches the dictionary, the merged deadlines and the forum."""

import asyncio
from pathlib import Path
from types import SimpleNamespace

from uni_master.app import Service
from uni_master.collectors.ruz import parse
from uni_master.config import Env
from uni_master.outputs import api
from uni_master.outputs import forum as forum_module
from uni_master.processing.batches import Changes

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()


def env() -> Env:
    return Env(api_id=1, api_hash="h", bot_token="1:t", owner_id=42, forum_id=-100, api_token="tok",
               public_url="https://x.ts.net")


def fake_forum(service, forum, now) -> None:
    service.feed.forum = service.timetable.forum = service.deadline_topic.forum = service.sheet_topic.forum = forum
    service.forum.spawn = forum.spawn
    service.ruz.now = service.schedule.now = service.timetable.now = service.feed.now = now  # Tue 2026-10-20
    service.merged.now = service.deadline_topic.now = service.hseapp_deadlines.now = now


async def test_after_poll(tmp_path, forum, namer, now, aiohttp_client):
    service = Service(env(), tmp_path)  # Telegram clients are made, never connected
    fake_forum(service, forum, now)
    service.subjects.llm = namer
    try:
        await service.deadline_topic.refresh()  # the list was shown before RUZ came
        await service._after_poll(service.ruz._sync(parse(FEED)))
        assert forum.texts("system") == ["РУЗ подключён: в расписании 9 пар."]
        # The first poll is no news: its Midterms join the pinned list without messages of their own.
        assert forum.texts("deadlines") == ["<b>Дедлайны</b>\n\n"
                                            "📅 <b>RSF Midterm</b> — пн 26.10 · Foundations of Russian Statehood\n"
                                            "📅 <b>Prog Midterm</b> — чт 29.10 · Введение в программирование"]
        assert forum.texts("news") == []  # the RUZ task leaves the news list to the batches
        assert service._ruz_status() == {"ok_at": None, "error": "RUZ_ICAL_URL is not set", "classes": 9,
                                         "subjects_error": None}
        assert service.calendar.render("main").startswith(b"BEGIN:VCALENDAR")
        await sources_reach_the_ports(service, aiohttp_client)

        failing, published = [], []

        async def broken(changes):
            failing.append(changes)
            raise RuntimeError("the forum is down")

        async def record(changes):
            published.append(changes)

        await midterm_follows_its_correction(service, forum, now)
        await dictionary_renames_the_midterms_at_once(service, forum)
        service.feed.publish, service.timetable.publish = broken, record
        await service._publish(Changes())  # the news failing does not stop the timetable
        assert len(failing) == len(published) == 1
    finally:
        service.store.close()


async def sources_reach_the_ports(service, aiohttp_client):
    """run() hands the service's sources to both ports: the Mini App's «Расписание» reads "ruz" by name, its
    «HSE App X» reads "hseapp"."""
    assert set(service.sources) == {"telegram", "batches", "ruz", "hseapp", "mail", "sheets", "summaries", "grades",
                                   "images"}
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


async def midterm_follows_its_correction(service, forum, now):
    """A chat moved the interim assessment: the batch run posts the Midterm's move, the owner's hiding moves it back."""
    uid = "2000007@ruz.hse.ru"
    item = service.ruz.by_uid(uid)
    with service.store.db:
        moved = service.schedule.events.add({
            "uid": uid, "class_uid": uid, "source": "tg", "chat_id": None, "msg_ids": [1], "course": item["subject"],
            "title": "Intro to Prog", "kind": item["kind"], "start": "2026-11-05T10:00:00",
            "end": "2026-11-05T13:00:00", "room": "R401", "required": True}, now())
    service.batches.now = now
    await service.batches.run_chats([])
    assert forum.texts("deadlines")[-1] == "↪️ <b>Prog Midterm</b> перенесён: чт 29.10 → чт 05.11"
    await service.schedule.set_hidden(moved, True)
    assert forum.texts("deadlines")[-1] == "↪️ <b>Prog Midterm</b> перенесён: чт 05.11 → чт 29.10"


async def dictionary_renames_the_midterms_at_once(service, forum):
    """A manual entry in the Mini App: the Midterm's label follows now, the list is edited, nothing is posted."""
    before = len(forum.messages)
    service.subjects.set_manual("Foundations of Russian Statehood", "Statehood", "State")
    await forum.settle()
    assert service.deadlines.get(service.ruz.by_uid("2000006@ruz.hse.ru")["deadline_id"])["label"] == \
        "State Midterm"
    assert len(forum.messages) == before and "📅 <b>State Midterm</b> — пн 26.10" in forum.texts("deadlines")[0]


async def test_deadlines_wiring(tmp_path, forum, now):
    """The first HSE App X poll joins quietly, later changes are posted; the buttons are the service's."""
    service = Service(env(), tmp_path)
    fake_forum(service, forum, now)
    try:
        assert service.hseapp_deadlines.on_change == service._hseapp_changed
        assert service.merged.on_change == service.deadline_topic.changed
        handlers = [callback for callback, _ in service.forum.client.list_event_handlers()]
        assert service.deadline_topic.on_button in handlers and service.feed.on_button in handlers
        await service.deadline_topic.refresh()

        def lists(*items):
            service.store.put_json("hseapp.deadlines", {"fetched_at": None, "done": [], "items": [
                {"id": raw_id, "label": label, "due": due, "course": None, "title": label}
                for raw_id, label, due in items]})

        lists(("h1", "DM HW 3", "2026-10-22"), ("h2", "Calc HW 1", "2026-10-23"))
        await service._hseapp_changed(True)
        assert forum.texts("deadlines") == ["<b>Дедлайны</b>\n\n🎓 <b>DM HW 3</b> — чт 22.10\n"
                                            "🎓 <b>Calc HW 1</b> — пт 23.10"]
        lists(("h1", "DM HW 3", "2026-10-22"), ("h2", "Calc HW 1", "2026-10-23"), ("h3", "Eng essay", "2026-10-25"))
        await service._hseapp_changed(False)
        assert forum.texts("deadlines")[-1] == "🆕 <b>Eng essay</b> — вс 25.10\nHSE App X"

        service.merged.add("Dentist", now().date(), "personal")  # the owner's: the list only
        await forum.settle()
        assert len(forum.texts("deadlines")) == 2 and "🏠 <b>Dentist</b> — сегодня" in forum.texts("deadlines")[0]

        with service.store.db:  # what a batch run saved
            service.deadlines.add({"source": "tg", "chat_id": None, "msg_ids": [1], "course": None,
                                   "label": "Alg HW 2", "due": "2026-10-24"}, now())
        await service._publish(Changes())
        assert forum.texts("deadlines")[-1] == "🆕 <b>Alg HW 2</b> — сб 24.10"

        # The owner hides it (the Mini App, a button): the list follows, no message.
        [alg] = [d for d in service.merged.visible() if d.title == "Alg HW 2"]
        service.merged.remove(alg)
        await forum.settle()
        assert len(forum.texts("deadlines")) == 3 and "Alg HW 2" not in forum.texts("deadlines")[0]
    finally:
        service.store.close()


async def test_hseapp_wiring(tmp_path):
    """A poll reaches the labels and the lists; a login notice gets the button that opens the bot's chat."""
    service = Service(env(), tmp_path)
    try:
        assert service.hseapp.callback_url == "https://x.ts.net/hseapp/callback"
        assert service.hseapp.on_poll == service.hseapp_deadlines.after_poll
        posts, tasks = [], []

        async def post(topic, text, buttons=None, *, html=False):
            posts.append((topic, text, buttons))

        service.forum.post = post
        service.forum.spawn = lambda coro: tasks.append(asyncio.ensure_future(coro))
        service.forum.username = "example_bot"
        service.hseapp._tell_login()
        await asyncio.gather(*tasks)
        [(topic, text, [button])] = posts
        assert topic == "system" and text.startswith("Нужен вход в HSE App X")
        assert button.type.url == "https://t.me/example_bot?start=hseapp" and button.text == "Войти в HSE App X"
        assert forum_module.start_screen("/start hseapp") == "hseapp" and forum_module.start_screen("/start") is None
        assert forum_module.start_screen("/start other") is None
        assert service.forum.screen_url("hseapp") == "https://x.ts.net/app/?screen=hseapp"

        service.forum.username = None  # before the bot has started: the line goes without the button
        service.hseapp._notify("HSE App X снова доступен.", login=True)
        await asyncio.gather(*tasks)
        assert posts[-1] == ("system", "HSE App X снова доступен.", None)
    finally:
        service.store.close()


async def test_watch_follows_the_twins_login_and_logout():
    """A login in the Mini App is watched from then on, a logout is no loss, a lost connection ends the service."""
    loop = asyncio.get_running_loop()
    bot = SimpleNamespace(client=SimpleNamespace(disconnected=loop.create_future()))
    twin = SimpleNamespace(client=None, changed=asyncio.Event())
    service, stop = SimpleNamespace(forum=bot, twin=twin), asyncio.Event()
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

    watch = asyncio.create_task(Service._watch(SimpleNamespace(forum=bot, twin=SimpleNamespace(
        client=None, changed=asyncio.Event())), stop))
    stop.set()
    await asyncio.wait_for(watch, 1)
