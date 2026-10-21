"""The RUZ collector against a fake feed: never the real link."""

import datetime as dt
from pathlib import Path

import pytest
from aiohttp import web

from uni_master import clock
from uni_master.collectors import ruz as ruz_module
from uni_master.collectors.ruz import Ruz, parse
from uni_master.outputs.timetable import Timetable

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()


def vevent(uid: str, summary: str, start: str, end: str, kind: str = "Seminar", location: str = "",
           status: str = "") -> str:
    """A VEVENT with Moscow times "2026-10-20 13:00", written in UTC as RUZ does."""
    def utc(text: str) -> str:
        return dt.datetime.fromisoformat(text).replace(tzinfo=clock.TZ).astimezone(dt.UTC).strftime("%Y%m%dT%H%M%SZ")
    lines = ["BEGIN:VEVENT", f"UID:{uid}", f"DTSTART:{utc(start)}", f"DTEND:{utc(end)}", f"SUMMARY:{summary}",
             f"DESCRIPTION:{kind}"]
    lines += [f"LOCATION:{location}"] if location else []
    lines += [f"STATUS:{status}"] if status else []
    return "\r\n".join([*lines, "END:VEVENT"])


def calendar(*events: str) -> bytes:
    return "\r\n".join(["BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN", *events, "END:VCALENDAR", ""]).encode()


def test_parse():
    classes = {c.uid[6]: c for c in parse(FEED)}
    calc = classes["1"]
    assert calc.subject == "Математический анализ"  # a folded "(р ус)" is dropped
    assert (calc.start, calc.end) == (dt.datetime(2026, 10, 19, 9, 30), dt.datetime(2026, 10, 19, 10, 50))
    assert calc.kind == "Проведение семинаров (студенты)"
    assert [classes[k].room for k in "12345"] == ["D208", "R401", "online", "online", "109"]
    assert classes["3"].link == "https://my.mts-link.ru/j/11111111/22222222222"
    assert classes["4"].link == "https://example.ktalk.ru/abcdef"  # from DESCRIPTION, unfolded
    assert classes["6"].room == "" and classes["6"].link is None
    assert classes["8"].cancelled and not classes["9"].cancelled  # kept with its mark, not skipped
    assert [c.start for c in parse(FEED)] == sorted(c.start for c in parse(FEED))


def test_parse_odd_events():
    text = calendar(
        "BEGIN:VEVENT\r\nSUMMARY:Algebra\r\nDTSTART;VALUE=DATE:20261024\r\nEND:VEVENT",           # all day, no UID
        "BEGIN:VEVENT\r\nUID:x\r\nSUMMARY: \r\nDTSTART:20261024T100000Z\r\nEND:VEVENT",           # no subject
        "BEGIN:VEVENT\r\nUID:y\r\nSUMMARY:Algebra\r\nEND:VEVENT",                                  # no start
        "BEGIN:VEVENT\r\nUID:z\r\nSUMMARY:History\r\nDTSTART;TZID=Europe/Moscow:20261024T100000\r\nEND:VEVENT")
    day, history = parse(text)
    assert (day.start, day.end) == (dt.datetime(2026, 10, 24), dt.datetime(2026, 10, 24))
    assert day.uid.startswith("ruz-") and day.uid == parse(text)[0].uid  # stable without a UID
    assert history.start == history.end == dt.datetime(2026, 10, 24, 10, 0)
    with pytest.raises(ValueError):
        parse(b"not a calendar")


@pytest.fixture
async def server(aiohttp_server):
    state = {"if_none_match": [], "status": 200, "body": FEED, "etag": 'W/"v1"'}

    async def feed(request):
        state["if_none_match"].append(request.headers.get("If-None-Match"))
        if state["status"] != 200:
            return web.Response(status=state["status"])
        if request.headers.get("If-None-Match") == state["etag"]:
            return web.Response(status=304)
        return web.Response(body=state["body"], content_type="text/calendar", headers={"ETag": state["etag"]})

    app = web.Application()
    app.router.add_get("/v3/ruz/cal/test", feed)
    test_server = await aiohttp_server(app)
    state["url"] = str(test_server.make_url("/v3/ruz/cal/test"))
    return state


def collector(url, store, now) -> Ruz:
    ruz = Ruz(url, store, now=now)
    ruz.polls, ruz.notices = [], []
    ruz.notify = lambda line: ruz.notices.append(line.ru)  # a Phrase: the forum picks its language

    async def on_poll(diff):
        ruz.polls.append(diff)

    ruz.on_poll = on_poll
    return ruz


async def test_poll_uses_etag(server, store, now):
    ruz = collector(server["url"], store, now)
    first = await ruz.poll()
    assert first.first and first.changes == [] and ruz.count() == 9
    assert ruz.status() == {"ok_at": "2026-10-20T12:00:00", "error": None, "classes": 9}
    again = await ruz.poll()
    assert server["if_none_match"] == [None, 'W/"v1"'] and again.changes == [] and not again.first
    assert len(ruz.polls) == 2  # also after 304: the subjects get another try

    other = collector(server["url"] + "?other", store, now)  # a new link: the old ETag is not sent
    await other.poll()
    assert server["if_none_match"][-1] is None


async def test_failure_keeps_classes_and_is_told(server, store, now):
    ruz = collector(server["url"], store, now)
    await ruz.poll()
    server["status"] = 503
    assert await ruz.poll() is None
    assert ruz.count() == 9 and "503" in ruz.status()["error"] and ruz.notices == []
    now.moment += dt.timedelta(hours=12)
    await ruz.poll()
    await ruz.poll()
    assert len(ruz.notices) == 1
    assert ruz.notices[0].startswith("РУЗ недоступен с 20.10 12:00: HTTP 503 Service Unavailable.")
    server["status"] = 200
    await ruz.poll()
    assert ruz.notices[1] == "РУЗ снова доступен." and ruz.status()["error"] is None
    assert len(ruz.polls) == 2  # failed polls give the processing nothing


async def test_empty_feed_is_a_glitch(server, store, now):
    ruz = collector(server["url"], store, now)
    await ruz.poll()
    server["body"], server["etag"] = calendar(), 'W/"v2"'
    assert await ruz.poll() is None
    assert ruz.count() == 9 and "empty" in ruz.error


async def test_no_link(store, now):
    ruz = Ruz("", store, now=now)
    assert ruz.status() == {"ok_at": None, "error": "RUZ_ICAL_URL is not set", "classes": 0}
    await ruz.run()  # returns at once


async def test_run_polls_every_hour(server, store, now, monkeypatch):
    sleeps = []

    async def sleep(seconds):
        sleeps.append(seconds)
        if len(sleeps) == 2:
            raise RuntimeError("stop")

    monkeypatch.setattr(ruz_module.asyncio, "sleep", sleep)
    ruz = collector(server["url"], store, now)
    with pytest.raises(RuntimeError, match="stop"):
        await ruz.run()
    assert sleeps == [3600, 3600] and len(ruz.polls) == 2


def test_diff(store, now):
    ruz = Ruz("", store, now=now)  # Tue 2026-10-20 12:00
    base = {
        "past": vevent("past", "Calculus", "2026-10-19 09:30", "2026-10-19 10:50"),
        "moved": vevent("moved", "Algebra", "2026-10-21 13:00", "2026-10-21 14:20", "Lecture", "R401"),
        "room": vevent("room", "Discrete Mathematics", "2026-10-22 16:40", "2026-10-22 18:00", "Seminar", "R205"),
        "cancel": vevent("cancel", "Calculus", "2026-10-23 09:30", "2026-10-23 10:50"),
        "removed": vevent("removed", "History", "2026-10-26 11:10", "2026-10-26 12:30"),
        "last": vevent("last", "Algebra", "2026-11-03 13:00", "2026-11-03 14:20"),
    }
    first = ruz._sync(parse(calendar(*base.values())))
    assert first.first and first.changes == [] and ruz.count() == 6

    changed = {
        "moved": vevent("moved", "Algebra", "2026-10-22 11:10", "2026-10-22 12:30", "Lecture", "R401"),
        "room": vevent("room", "Discrete Mathematics", "2026-10-22 16:40", "2026-10-22 18:00", "Seminar", "R510"),
        "cancel": vevent("cancel", "Calculus", "2026-10-23 09:30", "2026-10-23 10:50", status="CANCELLED"),
        "last": base["last"],
        "inside": vevent("inside", "Algebra", "2026-10-28 13:00", "2026-10-28 14:20"),
        "beyond": vevent("beyond", "Algebra", "2026-11-10 13:00", "2026-11-10 14:20"),  # the weeks moving on
    }
    diff = ruz._sync(parse(calendar(*changed.values())))
    assert not diff.first
    assert sorted((c.kind, c.new["uid"]) for c in diff.changes) == [
        ("cancelled", "cancel"), ("moved", "moved"), ("new", "inside"), ("removed", "removed"), ("room", "room")]
    moved = next(c for c in diff.changes if c.kind == "moved")
    assert (moved.old["start"], moved.new["start"]) == ("2026-10-21T13:00:00", "2026-10-22T11:10:00")
    assert ruz.by_uid("past")["gone_at"] is None  # aged out of the feed: kept as it was
    assert ruz.by_uid("removed")["gone_at"] == "2026-10-20T12:00:00"
    assert [c["uid"] for c in ruz.classes(since="2026-10-26")] == ["inside", "last", "beyond"]

    back = ruz._sync(parse(calendar(*{**changed, "cancel": base["cancel"], "removed": base["removed"]}.values())))
    assert sorted((c.kind, c.new["uid"]) for c in back.changes) == [("restored", "cancel"), ("restored", "removed")]


async def test_status_counts_the_last_feed(server, store, now):
    ruz = collector(server["url"], store, now)
    await ruz.poll()
    assert ruz.status()["classes"] == 9
    now.moment = dt.datetime(2026, 11, 4, 12, 0, tzinfo=clock.TZ)  # every class of the feed is past
    server["body"], server["etag"] = calendar(
        vevent("a", "Algebra", "2026-11-05 13:00", "2026-11-05 14:20"),
        vevent("b", "Calculus", "2026-11-06 09:30", "2026-11-06 10:50")), 'W/"v2"'
    await ruz.poll()
    assert ruz.status()["classes"] == 2 and ruz.count() == 11  # the past ones are kept, not counted
    await ruz.poll()  # 304
    assert server["if_none_match"][-1] == 'W/"v2"' and ruz.status()["classes"] == 2


@pytest.mark.parametrize("status", [404, 503])
async def test_errors_never_tell_the_link(aiohttp_server, store, now, caplog, status):
    secret = "/v3/ruz/cal/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

    async def feed(request):
        return web.Response(status=status)

    app = web.Application()
    app.router.add_get(secret, feed)
    test_server = await aiohttp_server(app)
    ruz = collector(str(test_server.make_url(secret)), store, now)
    await ruz.poll()
    now.moment += dt.timedelta(hours=12)
    await ruz.poll()
    assert ruz.status()["error"] == f"HTTP {status} {web.Response(status=status).reason}"
    [notice] = ruz.notices
    assert secret not in notice and "0f1e2d3c" not in ruz.status()["error"]
    assert "0f1e2d3c" not in caplog.text and str(status) in caplog.text


async def test_other_errors_hide_the_link(store, now, caplog):
    ruz = collector("ftp://example.invalid/v3/ruz/cal/0f1e2d3c-secret", store, now)  # refused before connecting
    assert await ruz.poll() is None
    assert ruz.status()["error"] == "NonHttpUrlClientError: <RUZ_ICAL_URL>"
    assert "0f1e2d3c" not in caplog.text
    assert ruz._hide_url("GET /v3/ruz/cal/0f1e2d3c-secret failed") == "GET <RUZ_ICAL_URL> failed"


async def test_empty_feed_before_the_timetable(server, store, now, schedule, forum):
    """Between terms the feed is empty: no 'first poll' every hour, the System topic hears once classes come."""
    ruz = collector(server["url"], store, now)
    schedule.ruz = ruz
    timetable = Timetable(forum, store, schedule.inbox, schedule, 42, now=now)

    async def on_poll(diff):
        ruz.polls.append(diff)
        await timetable.publish_ruz(diff)

    ruz.on_poll = on_poll
    server["body"] = calendar()
    for _ in range(3):
        await ruz.poll()
    assert [diff.first for diff in ruz.polls] == [False] * 3 and forum.messages == {}
    assert ruz.status() == {"ok_at": "2026-10-20T12:00:00", "error": None, "classes": 0}

    server["body"], server["etag"] = FEED, 'W/"v2"'
    await ruz.poll()
    await ruz.poll()
    assert [diff.first for diff in ruz.polls[3:]] == [True, False]
    assert forum.texts("system") == ["РУЗ подключён: в расписании 9 пар."] and forum.texts("schedule") == []


async def test_a_new_link_is_a_first_poll(server, store, now, schedule, forum):
    """RUZ_ICAL_URL changed: another feed, not a list of changes. No «убрана» for every class of the old one."""
    ruz = collector(server["url"], store, now)
    await ruz.poll()
    server["body"], server["etag"] = calendar(
        vevent("other-1@ruz.hse.ru", "Calculus", "2026-10-21 09:30", "2026-10-21 10:50"),
        vevent("2000008@ruz.hse.ru", "Calculus", "2026-10-28 09:30", "2026-10-28 10:50")), 'W/"other"'
    relinked = collector(server["url"] + "?new-link", store, now)
    schedule.ruz = relinked
    timetable = Timetable(forum, store, schedule.inbox, schedule, 42, now=now)

    async def on_poll(diff):
        relinked.polls.append(diff)
        await timetable.publish_ruz(diff)

    relinked.on_poll = on_poll
    diff = await relinked.poll()
    assert diff.first and diff.relinked and server["if_none_match"][-1] is None
    assert forum.texts("system") == ["Ссылка РУЗ сменилась: в расписании 2 пар."] and forum.texts("schedule") == []
    assert [item["uid"] for item in relinked.classes(since="2026-10-20T12:00:00")] == [
        "other-1@ruz.hse.ru", "2000008@ruz.hse.ru"]  # the old feed's classes ahead are gone from the feeds

    again = await relinked.poll()  # the link is the saved one now
    assert not again.first and not again.relinked and server["if_none_match"][-1] == 'W/"other"'
    back = collector(server["url"], store, now)  # and the first link again: one more first poll
    server["etag"] = 'W/"other2"'
    assert (await back.poll()).first


async def test_a_new_link_waits_for_classes(server, store, now):
    """An empty feed behind the new link is no first poll: the next one with classes is."""
    ruz = collector(server["url"], store, now)
    await ruz.poll()
    now.moment += dt.timedelta(days=60)  # nothing of the old feed lies ahead
    server["body"], server["etag"] = calendar(), 'W/"empty"'
    relinked = collector(server["url"] + "?new-link", store, now)
    assert not (await relinked.poll()).first
