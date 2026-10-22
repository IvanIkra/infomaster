"""Our iCal feeds: what each of them holds, and that Tablo's reader takes them as they are."""

import datetime as dt
import re
from pathlib import Path
from zoneinfo import ZoneInfo

import icalendar
import pytest

from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import Diff, parse
from infomaster.config import Env
from infomaster.outputs import api, miniapp
from infomaster.outputs.ical_feed import CalendarFeed
from infomaster.processing.events import event_uid
from infomaster.settings import ChatSettings, Settings

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()
CHAT = -1001234567890
SECRET = "Zx8-calendar_secret-42"
PUBLIC_SECRET = "Pq3-public_calendar-17"


# --- Tablo's reader (service/tablo/sources/ical.py, §4.2), the same lines -------------------

TABLO_LINK = re.compile(r"https?://")


def tablo_clean(name) -> str:
    name = " ".join(str(name or "").split())
    return re.sub(r"\s*\(рус\)$", "", name).strip()


def tablo_room(location: str, description: str, url: str) -> str:
    location = location.strip()
    if location and not TABLO_LINK.match(location):
        return location.split(",")[0].strip()
    if location or TABLO_LINK.search(description) or url:
        return "online"
    return ""


def tablo_local(value, tz: ZoneInfo) -> dt.datetime:
    if not isinstance(value, dt.datetime):
        value = dt.datetime.combine(value, dt.time())
    if value.tzinfo is not None:
        value = value.astimezone(tz).replace(tzinfo=None)
    return value


def tablo_parse(text: bytes, tz: ZoneInfo) -> list[tuple]:
    events = []
    for ev in icalendar.Calendar.from_ical(text).walk("VEVENT"):
        if str(ev.get("STATUS", "")).upper() == "CANCELLED":
            continue
        subject = tablo_clean(ev.get("SUMMARY"))
        if not subject or ev.get("DTSTART") is None:
            continue
        start = tablo_local(ev.decoded("DTSTART"), tz)
        end = tablo_local(ev.decoded("DTEND"), tz) if ev.get("DTEND") is not None else start
        description = str(ev.get("DESCRIPTION", ""))
        kind = description.split("\n", 1)[0].strip()
        where = tablo_room(str(ev.get("LOCATION", "")), description, str(ev.get("URL", "")))
        events.append((str(ev.get("UID", "")), start, end, subject, where, kind))
    events.sort(key=lambda e: (e[1], e[3]))
    return events


# --- the timetable behind the feeds -----------------------------------------------------

def add_event(schedule, now, **values) -> int:
    event = {"uid": event_uid("tg", CHAT, values.get("msg_ids", [1])[0]), "source": "tg", "chat_id": CHAT,
             "msg_ids": [1], "course": "", "title": "Alg consult", "kind": "consultation",
             "start": "2026-10-23T18:10:00", "end": "2026-10-23T19:30:00", "room": "R205", "required": True, **values}
    with schedule.db:
        return schedule.events.add(event, now())


@pytest.fixture
async def feed(schedule, now):
    schedule.inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    await schedule.after_poll(schedule.ruz._sync(parse(FEED)))
    await schedule.subjects.resolve(["Алгебра"])  # as a batch run does
    add_event(schedule, now, course="Алгебра")
    add_event(schedule, now, msg_ids=[2], title="Career fair", kind="event", start="2026-10-24T12:00:00",
              end="2026-10-24T16:00:00", room="https://events.hse.ru/fair", required=False)
    add_event(schedule, now, msg_ids=[3], title="Club", kind="meeting", start="2026-10-24T18:00:00", end=None,
              room="", required=False, hidden=True)
    add_event(schedule, now, msg_ids=[4], title="Old talk", kind="talk", required=False, cancelled=True,
              start="2026-10-25T15:00:00", end=None, room="")
    item = schedule.ruz.by_uid("2000005@ruz.hse.ru")  # Intro to SE, Fri Oct 23 09:30: moved by a chat
    add_event(schedule, now, msg_ids=[5], uid=item["uid"], class_uid=item["uid"], course=item["subject"],
              title="Intro to SE", kind=item["kind"], start="2026-10-23T11:10:00", end="2026-10-23T12:30:00",
              room="R301")
    item = schedule.ruz.by_uid("2000002@ruz.hse.ru")  # Algebra, Tue Oct 20 13:00: cancelled by a chat
    add_event(schedule, now, msg_ids=[6], uid=item["uid"], class_uid=item["uid"], course=item["subject"],
              title="Algebra", kind=item["kind"], start=item["start"], end=item["end"], room=item["room"],
              cancelled=True)
    return CalendarFeed(schedule, now=now)


def events_of(text: bytes) -> dict[str, icalendar.Event]:
    return {str(e["UID"]): e for e in icalendar.Calendar.from_ical(text).walk("VEVENT")}


def by_summary(text: bytes) -> dict[str, icalendar.Event]:
    return {str(e["SUMMARY"]): e for e in icalendar.Calendar.from_ical(text).walk("VEVENT")}


def test_main_and_optional(feed):
    main, optional = by_summary(feed.render("main")), by_summary(feed.render("optional"))
    assert {"Calculus", "Algebra", "Intro to SE", "Alg consult", "Rus Statehood", "Intro to Prog"} <= set(main)
    assert set(optional) == {"Career fair", "Old talk"}  # the hidden club is in no feed
    assert "Club" not in main and str(optional["Career fair"]["STATUS"]) == "CONFIRMED"
    assert str(icalendar.Calendar.from_ical(feed.render("main"))["X-WR-CALNAME"]) == "Study"


def test_tablo_feed(feed):
    events = events_of(feed.render("tablo"))
    assert len(events) == 9 + 3  # the classes, and the events that are not hidden
    fair = next(e for e in events.values() if str(e["SUMMARY"]) == "Career fair")
    assert (str(fair["STATUS"]), str(fair["X-INFOMASTER-OPTIONAL"]), str(fair["X-INFOMASTER-SOURCE"])) == (
        "TENTATIVE", "TRUE", "tg")
    assert str(fair["X-INFOMASTER-LINK"]) == "https://t.me/c/1234567890/2" and str(fair["LOCATION"]) == \
        "https://events.hse.ru/fair"
    assert all("URL" not in e for e in events.values())  # Tablo would take it for an online class

    calc = events["2000001@ruz.hse.ru"]
    assert (str(calc["SUMMARY"]), str(calc["DESCRIPTION"]), str(calc["LOCATION"]), str(calc["STATUS"])) == (
        "Calculus", "Seminar", "D208", "CONFIRMED")
    assert str(calc["X-INFOMASTER-SOURCE"]) == "ruz"
    assert "X-INFOMASTER-LINK" not in calc and "X-INFOMASTER-OPTIONAL" not in calc
    assert calc.decoded("DTSTART") == dt.datetime(2026, 10, 19, 6, 30, tzinfo=dt.UTC)  # UTC, any calendar reads it

    moved = events["2000005@ruz.hse.ru"]
    assert str(moved["DESCRIPTION"]) == "Seminar\nMoved from Fri Oct 23 09:30 in a chat"
    assert (str(moved["LOCATION"]), str(moved["X-INFOMASTER-SOURCE"]), str(moved["X-INFOMASTER-LINK"])) == (
        "R301", "tg", "https://t.me/c/1234567890/5")
    cancelled = events["2000002@ruz.hse.ru"]
    assert str(cancelled["STATUS"]) == "CANCELLED" and str(cancelled["DESCRIPTION"]) == "Lecture\nCancelled in a chat"
    assert str(events["2000008@ruz.hse.ru"]["STATUS"]) == "CANCELLED"  # cancelled in RUZ: kept with its status
    assert str(events["2000006@ruz.hse.ru"]["DESCRIPTION"]) == "Interim assessment"
    consult = next(e for e in events.values() if str(e["SUMMARY"]) == "Alg consult")
    assert str(consult["DESCRIPTION"]) == "Consultation\nAlgebra"


def test_tablo_reads_the_feed(feed):
    """Tablo's ical.py, as it is now, gets the subject, times, kind, room and uid right."""
    read = {uid: rest for uid, *rest in tablo_parse(feed.render("tablo"), ZoneInfo("Europe/Moscow"))}
    assert read["2000001@ruz.hse.ru"] == [dt.datetime(2026, 10, 19, 9, 30), dt.datetime(2026, 10, 19, 10, 50),
                                          "Calculus", "D208", "Seminar"]
    assert read["2000003@ruz.hse.ru"][2:] == ["Rus Statehood", "online", "Lecture"]  # a link in LOCATION
    assert read["2000006@ruz.hse.ru"][2:] == ["Rus Statehood", "", "Interim assessment"]  # Tablo's Midterm (§4.4)
    assert read["2000005@ruz.hse.ru"] == [dt.datetime(2026, 10, 23, 11, 10), dt.datetime(2026, 10, 23, 12, 30),
                                          "Intro to SE", "R301", "Seminar"]  # moved by a chat
    assert "2000002@ruz.hse.ru" not in read and "2000008@ruz.hse.ru" not in read  # cancelled ones are skipped
    consult = read[event_uid("tg", CHAT, 1)]
    assert consult == [dt.datetime(2026, 10, 23, 18, 10), dt.datetime(2026, 10, 23, 19, 30), "Alg consult", "R205",
                       "Consultation"]
    assert read[event_uid("tg", CHAT, 2)][2:] == ["Career fair", "online", "Event"]  # TENTATIVE is read as usual
    assert len(read) == 12 - 3


def test_event_without_end(schedule, now):
    add_event(schedule, now, end=None)
    [event] = events_of(CalendarFeed(schedule, now=now).render("main")).values()
    assert event.decoded("DTEND") - event.decoded("DTSTART") == dt.timedelta(minutes=80)


async def test_routes(aiohttp_client, budget, schedule, now, merged):
    env = Env(api_id=1, api_hash="h", bot_token="1:t", owner_id=42, forum_id=-100, api_token="tok",
              public_url="https://x.ts.net", calendar_secret=SECRET)
    await schedule.after_poll(Diff())
    client = await aiohttp_client(api.create_app(env, budget, {}, CalendarFeed(schedule, now=now), merged))
    response = await client.get(f"/calendar/{SECRET}/tablo.ics")
    assert response.status == 200 and response.content_type == "text/calendar"
    assert (await response.text()).startswith("BEGIN:VCALENDAR")
    for path in (f"/calendar/{SECRET}x/main.ics", "/calendar/wrong/main.ics", f"/calendar/{SECRET}/all.ics",
                 f"/calendar/{SECRET}/main.txt", f"/calendar/{SECRET[:-1]}/main.ics"):
        assert (await client.get(path)).status == 404, path
    assert (await client.get(f"/calendar/{SECRET}/main.ics", headers={"Authorization": "Bearer no"})).status == 200


async def test_public_routes(aiohttp_client, budget, journal, store, schedule, now, merged):
    """Behind Funnel: main and optional by their own secret, never tablo, never by the internal secret."""
    env = Env(api_id=1, api_hash="h", bot_token="1:t", owner_id=42, forum_id=-100, api_token="tok",
              public_url="https://x.ts.net", calendar_secret=SECRET, calendar_public_secret=PUBLIC_SECRET)
    await schedule.after_poll(Diff())
    feed = CalendarFeed(schedule, now=now)
    hse = HseApp(store, Settings(store), env.public_url + CALLBACK_PATH)
    app = miniapp.create_app(env, budget, journal, schedule.inbox, ChatSettings(store), {}, schedule.subjects,
                             schedule, hse, merged, calendar_feed=feed)
    client = await aiohttp_client(app)
    for name in ("main", "optional"):
        response = await client.get(f"/calendar/{PUBLIC_SECRET}/{name}.ics")  # no initData: calendars send none
        assert response.status == 200 and response.content_type == "text/calendar"
        assert (await response.read()) == feed.render(name)
    for path in (f"/calendar/{PUBLIC_SECRET}/tablo.ics", f"/calendar/{SECRET}/main.ics",
                 f"/calendar/{PUBLIC_SECRET[:-1]}/main.ics"):
        assert (await client.get(path)).status == 404, path
    internal = await aiohttp_client(api.create_app(env, budget, {}, feed, merged))
    assert (await internal.get(f"/calendar/{PUBLIC_SECRET}/main.ics")).status == 404
    assert (await internal.get(f"/calendar/{SECRET}/tablo.ics")).status == 200


async def test_public_routes_off(aiohttp_client, budget, journal, store, schedule, now, merged):
    env = Env(api_id=1, api_hash="h", bot_token="1:t", owner_id=42, forum_id=-100, api_token="tok",
              public_url="https://x.ts.net", calendar_secret=SECRET)
    hse = HseApp(store, Settings(store), env.public_url + CALLBACK_PATH)
    client = await aiohttp_client(miniapp.create_app(env, budget, journal, schedule.inbox, ChatSettings(store), {},
                                                     schedule.subjects, schedule, hse, merged,
                                                     calendar_feed=CalendarFeed(schedule, now=now)))
    assert (await client.get(f"/calendar/{SECRET}/main.ics")).status == 404  # no public secret: no feeds here
