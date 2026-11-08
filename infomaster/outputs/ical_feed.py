"""Our iCal feeds (§6.5, §8.1, §8.2, D2, D28, D32, D43).

On the internal port, for Tablo: GET /calendar/<CALENDAR_SECRET>/main.ics, optional.ics and tablo.ics. On the public
port, behind Funnel, for one's own calendar: GET /calendar/<CALENDAR_PUBLIC_SECRET>/main.ics and optional.ics, never
tablo.ics. Calendars send no headers, so the secret in the path is all the protection: it is compared in constant
time, and anything else is 404. The two secrets are different, so a leaked public link is changed without Tablo; a
port without its secret has no routes.

- main: the classes and the events one has to attend;
- optional: the events one may attend;
- tablo: both; an optional one is STATUS:TENTATIVE with X-INFOMASTER-OPTIONAL:TRUE. Tablo's ical.py skips only
  CANCELLED, so it reads the feed as it is now and ignores the X- fields until it learns them.

Every event has an English SUMMARY, a DESCRIPTION whose first line is the type (Lecture, Seminar, Consultation, Event,
Interim assessment...), LOCATION with the room or the link, a stable UID, X-INFOMASTER-SOURCE (ruz, tg, mail, pdf) and,
when there is a source message, X-INFOMASTER-LINK. Never URL: Tablo takes an event with URL for an online class (§4.2).
Times are in UTC, which every calendar converts by itself; an event told without its time is an all-day one, a DTSTART
date and the next day's DTEND. A cancelled event stays with STATUS:CANCELLED, so a calendar that has it shows the
cancellation.

In main and optional, a class's SUMMARY ends with its type, "Calculus · Seminar", since a calendar app shows the title
alone. tablo.ics keeps the bare title: Tablo takes SUMMARY for the course name.

main and optional also say where an event came from and how to change it, since a calendar app shows the notes and
one link: DESCRIPTION ends with "From: <the chat> <link to the message>" (a letter's subject, RUZ, "you" for an own
one) and "Edit: <link>", and URL is that link, which opens the event's card in the Mini App (Forum.event_link). The
source's link is to the message's page in the Mini App (D50), which your own account sees without being in the chat.
tablo.ics has neither: Tablo takes an event with URL for an online class (§4.2).
"""

import datetime as dt
import hmac
from collections.abc import Callable

import icalendar
from aiohttp import web

from .. import clock
from ..processing.schedule import Entry, Schedule

FEEDS = ("main", "optional", "tablo")
PUBLIC_FEEDS = ("main", "optional")  # tablo.ics with its X- fields is Tablo's, it stays inside
NAMES = {"main": "Study", "optional": "Study: optional", "tablo": "Study for Tablo"}
REFRESH = "PT1H"  # RUZ is polled hourly; a calendar that honours the hint asks as often
MARKS = {"Interim assessment": "Midterm"}  # a type in a class's title, where the word is too long


def utc(local: dt.datetime) -> dt.datetime:
    return local.replace(tzinfo=clock.TZ).astimezone(dt.UTC)


def summary(entry: Entry, feed: str) -> str:
    """The title, with a class's type in one's own calendar ("Calculus · Seminar"); a type not known stays out."""
    if feed == "tablo" or not entry.of_class or entry.type == "Class":
        return entry.title
    return f"{entry.title} · {MARKS.get(entry.type, entry.type)}"


def origin_line(entry: Entry, page: Callable[[int, int], str | None] | None = None) -> str:
    """Where it came from, for one's own calendar: the chat with a link to the message's page (else the message), RUZ,
    the owner."""
    if entry.source == "ruz":
        return "From: RUZ"
    if entry.source == "own":
        return "From: you"
    link = (page(*entry.message) if page and entry.message else None) or entry.link
    return f"From: {entry.origin or entry.source}" + (f" {link}" if link else "")


def build(entries: list[Entry], feed: str, stamp: dt.datetime, edit_link: Callable[[str], str | None] | None = None,
          message_page: Callable[[int, int], str | None] | None = None) -> bytes:
    """One of FEEDS as iCalendar text; `edit_link(uid)` is the link to an entry's card in the Mini App,
    `message_page(chat_id, msg_id)` to a source message's page there."""
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
        if entry.all_day:
            event.add("dtstart", entry.start.date())
            event.add("dtend", entry.start.date() + dt.timedelta(days=1))
        else:
            event.add("dtstart", utc(entry.start))
            if entry.end > entry.start:  # DTEND must be later than DTSTART; without it the event ends where it starts
                event.add("dtend", utc(entry.end))
        event.add("summary", summary(entry, feed))
        lines = [entry.type, *entry.notes]
        link = edit_link(entry.uid) if edit_link and feed != "tablo" else None
        if feed != "tablo":
            lines.append(origin_line(entry, message_page))
        if link:
            lines.append(f"Edit: {link}")
            event.add("url", link)
        event.add("description", "\n".join(lines))
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
        self.edit_link: Callable[[str], str | None] | None = None  # uid -> its card in the Mini App
        self.message_page: Callable[[int, int], str | None] | None = None  # a source message -> its page there

    def render(self, feed: str) -> bytes:
        return build(self.schedule.entries(), feed, self.now(), self.edit_link, self.message_page)


FEED_KEY = web.AppKey("calendar", CalendarFeed)
SECRET_KEY = web.AppKey("calendar_secret", str)
SERVED_KEY = web.AppKey("calendar_feeds", tuple)


async def calendar(request: web.Request) -> web.Response:
    secret, given = request.app[SECRET_KEY], request.match_info["secret"]
    if not hmac.compare_digest(given.encode(), secret.encode()) or \
            request.match_info["feed"] not in request.app[SERVED_KEY]:
        raise web.HTTPNotFound()
    return web.Response(body=request.app[FEED_KEY].render(request.match_info["feed"]), content_type="text/calendar",
                        charset="utf-8", headers={"Cache-Control": "no-cache"})


def add_routes(app: web.Application, secret: str, feed: CalendarFeed, feeds: tuple[str, ...] = FEEDS) -> None:
    """The feeds on the internal app, or PUBLIC_FEEDS on the public one; nothing without a secret."""
    if not secret:
        return
    app[SECRET_KEY], app[FEED_KEY], app[SERVED_KEY] = secret, feed, feeds
    app.router.add_get("/calendar/{secret}/{feed}.ics", calendar)
