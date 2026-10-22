"""Our iCal feeds (§6.5, §8.1, §8.2, р. 2, р. 28, р. 32), on the internal port only.

GET /calendar/<CALENDAR_SECRET>/main.ics, optional.ics and tablo.ics. Calendars send no headers, so the secret in the
path is all the protection: it is compared in constant time, and anything else is 404. Without CALENDAR_SECRET the
routes are off.

- main: the classes and the events one has to attend;
- optional: the events one may attend;
- tablo: both; an optional one is STATUS:TENTATIVE with X-INFOMASTER-OPTIONAL:TRUE. Tablo's ical.py skips only
  CANCELLED, so it reads the feed as it is now and ignores the X- fields until it learns them.

Every event has an English SUMMARY, a DESCRIPTION whose first line is the type (Lecture, Seminar, Consultation, Event,
Interim assessment...), LOCATION with the room or the link, a stable UID, X-INFOMASTER-SOURCE (ruz, tg, mail, pdf) and,
when there is a source message, X-INFOMASTER-LINK. Never URL: Tablo takes an event with URL for an online class (§4.2).
Times are in UTC, which every calendar converts by itself. A cancelled event stays with STATUS:CANCELLED, so a
calendar that has it shows the cancellation.
"""

import datetime as dt
import hmac
from collections.abc import Callable

import icalendar
from aiohttp import web

from .. import clock
from ..processing.schedule import Entry, Schedule

FEEDS = ("main", "optional", "tablo")
NAMES = {"main": "Study", "optional": "Study: optional", "tablo": "Study for Tablo"}
REFRESH = "PT1H"  # RUZ is polled hourly; a calendar that honours the hint asks as often


def utc(local: dt.datetime) -> dt.datetime:
    return local.replace(tzinfo=clock.TZ).astimezone(dt.UTC)


def build(entries: list[Entry], feed: str, stamp: dt.datetime) -> bytes:
    """One of FEEDS as iCalendar text."""
    calendar = icalendar.Calendar()
    calendar.add("prodid", "-//infomaster//timetable//EN")
    calendar.add("version", "2.0")
    calendar.add("calscale", "GREGORIAN")
    calendar.add("method", "PUBLISH")
    calendar.add("x-wr-calname", NAMES[feed])
    calendar.add("x-published-ttl", REFRESH)
    calendar.add("refresh-interval", dt.timedelta(hours=1), parameters={"VALUE": "DURATION"})
    stamp = stamp.astimezone(dt.UTC).replace(microsecond=0)
    for entry in entries:
        if feed == "main" and not entry.required or feed == "optional" and entry.required:
            continue
        optional = feed == "tablo" and not entry.required
        event = icalendar.Event()
        event.add("uid", entry.uid)
        event.add("dtstamp", stamp)
        event.add("dtstart", utc(entry.start))
        if entry.end > entry.start:  # DTEND must be later than DTSTART; without it the event ends where it starts
            event.add("dtend", utc(entry.end))
        event.add("summary", entry.title)
        event.add("description", "\n".join([entry.type, *entry.notes]))
        if entry.location:
            event.add("location", entry.location)
        event.add("status", "CANCELLED" if entry.cancelled else "TENTATIVE" if optional else "CONFIRMED")
        if optional:
            event.add("x-infomaster-optional", "TRUE")
        event.add("x-infomaster-source", entry.source)
        if entry.link:
            event.add("x-infomaster-link", entry.link)
        calendar.add_component(event)
    return calendar.to_ical()


class CalendarFeed:
    def __init__(self, schedule: Schedule, now: Callable[[], dt.datetime] = clock.now_utc):
        self.schedule, self.now = schedule, now

    def render(self, feed: str) -> bytes:
        return build(self.schedule.entries(), feed, self.now())


FEED_KEY = web.AppKey("calendar", CalendarFeed)
SECRET_KEY = web.AppKey("calendar_secret", str)


async def calendar(request: web.Request) -> web.Response:
    secret, given = request.app[SECRET_KEY], request.match_info["secret"]
    if not hmac.compare_digest(given.encode(), secret.encode()) or request.match_info["feed"] not in FEEDS:
        raise web.HTTPNotFound()
    return web.Response(body=request.app[FEED_KEY].render(request.match_info["feed"]), content_type="text/calendar",
                        charset="utf-8", headers={"Cache-Control": "no-cache"})


def add_routes(app: web.Application, secret: str, feed: CalendarFeed) -> None:
    """The feeds on the internal app; nothing without a secret."""
    if not secret:
        return
    app[SECRET_KEY], app[FEED_KEY] = secret, feed
    app.router.add_get("/calendar/{secret}/{feed}.ics", calendar)
