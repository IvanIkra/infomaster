"""The RUZ timetable in iCal (§5.2, §4.1, §4.2): classes of about five weeks, polled every hour.

The link (`RUZ_ICAL_URL`) is a secret and optional: without it the source says so and everything else works. It never
goes into the status, the log or the System topic: HTTP errors are told by their status, other errors with the link
cut out. A poll sends the saved ETag and takes 304 for "nothing new"; a failed poll keeps the classes it had.

Each VEVENT is read the way Tablo reads it (§4.2): the cleaned SUMMARY is the subject, start and end are Moscow local
time, the kind is the first line of DESCRIPTION, the room is the part of LOCATION before the first comma or "online".
A class cancelled in the feed (STATUS:CANCELLED) is kept with a mark rather than skipped, so our feed can show the
cancellation (§11.2).

`classes` holds the current feed and the past: a class that leaves the feed after it started has aged out of the
five weeks and stays as it was; one that leaves before it started was removed by RUZ (`gone_at`). The status counts
the classes of the last feed that came, not the past ones kept. After each poll, also after 304, `on_poll` gets what
changed; the processing names the subjects and keeps the Midterm deadlines (the collector itself never calls the
LLM, §4). The first poll that brings classes is no news ("first"); an empty feed before it is just nothing yet. A new
link (its hash differs from the one of the last good poll) makes the next poll that brings classes a first one again:
another feed is no list of changes, and its classes the old feed had not would all come as removed and new ones.
"""

import asyncio
import datetime as dt
import hashlib
import logging
import re
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from urllib.parse import urlsplit

import aiohttp
import icalendar

from .. import clock
from ..processing.subjects import clean
from ..store import Store

log = logging.getLogger(__name__)

POLL_EVERY = 3600           # seconds (§4.1)
TIMEOUT = 60                # seconds
STALE_AFTER = dt.timedelta(hours=12)  # failing longer than this is told to the System topic (§4.5)
ETAG_KEY = "ruz.etag"
SIZE_KEY = "ruz.size"       # classes in the last feed that came; a 304 leaves it as it was
HIDDEN_URL = "<RUZ_ICAL_URL>"
LINK = re.compile(r"https?://\S+")
FIELDS = ("subject", "start", "end", "kind", "room", "link", "cancelled")


@dataclass(frozen=True)
class Class:
    uid: str
    subject: str            # cleaned, as the feed names it
    start: dt.datetime      # Moscow local, naive
    end: dt.datetime
    kind: str               # "Лекция", "Seminar", "Ongoing interim assessment"
    room: str               # "R609", "online" or ""
    link: str | None        # the meeting link of an online class
    cancelled: bool = False

    def row(self) -> dict:
        return {"uid": self.uid, "subject": self.subject, "start": self.start.isoformat(timespec="seconds"),
                "end": self.end.isoformat(timespec="seconds"), "kind": self.kind, "room": self.room,
                "link": self.link, "cancelled": self.cancelled}


def room(location: str, description: str, url: str) -> str:
    """'R609, Покровский б-р, д.11' -> 'R609'; a meeting link -> 'online' (Tablo's rule, §4.2)."""
    location = location.strip()
    if location and not LINK.match(location):
        return location.split(",")[0].strip()
    if location or LINK.search(description) or url:
        return "online"
    return ""


def meeting_link(location: str, description: str, url: str) -> str | None:
    for text in (location.strip(), description, url.strip()):
        if found := LINK.search(text):
            return found.group(0)
    return None


def _local(value) -> dt.datetime:
    if not isinstance(value, dt.datetime):  # an all-day event: from midnight
        value = dt.datetime.combine(value, dt.time())
    if value.tzinfo is not None:
        value = value.astimezone(clock.TZ).replace(tzinfo=None)
    return value


def parse(text: str | bytes) -> list[Class]:
    """Classes of a feed by start and subject; raises ValueError on a broken feed."""
    classes = []
    for event in icalendar.Calendar.from_ical(text).walk("VEVENT"):
        subject = clean(event.get("SUMMARY"))
        if not subject or event.get("DTSTART") is None:
            continue
        start = _local(event.decoded("DTSTART"))
        end = _local(event.decoded("DTEND")) if event.get("DTEND") is not None else start
        description = str(event.get("DESCRIPTION", ""))
        location, url = str(event.get("LOCATION", "")), str(event.get("URL", ""))
        uid = str(event.get("UID", "")).strip() or \
            f"ruz-{hashlib.sha1(f'{subject}|{start.isoformat()}'.encode()).hexdigest()[:16]}@uni-master"
        classes.append(Class(uid, subject, start, end, description.split("\n", 1)[0].strip(),
                             room(location, description, url), meeting_link(location, description, url),
                             str(event.get("STATUS", "")).upper() == "CANCELLED"))
    classes.sort(key=lambda c: (c.start, c.subject))
    return classes


@dataclass
class Change:
    """new (inside the weeks the feed covered before), moved, room, cancelled, removed, restored."""
    kind: str
    old: dict | None
    new: dict


@dataclass
class Diff:
    first: bool = False         # the very first poll, or the first after the link changed: nothing to compare with
    changes: list[Change] = field(default_factory=list)
    relinked: bool = False      # first because RUZ_ICAL_URL changed


class Ruz:
    def __init__(self, url: str, store: Store, now: Callable[[], dt.datetime] = clock.now_utc):
        self.url, self.store, self.db, self.now = url, store, store.db, now
        self.on_poll: Callable[[Diff], Awaitable[None]] | None = None  # after every good poll, also a 304
        self.notify: Callable[[str], None] | None = None               # a line for the System topic
        self.ok_at: str | None = None
        self.error: str | None = None if url else "RUZ_ICAL_URL is not set"
        self._failing_since: dt.datetime | None = None
        self._told = False

    def status(self) -> dict:
        return {"ok_at": self.ok_at, "error": self.error, "classes": self.size()}

    # --- reading ----------------------------------------------------------------------

    def by_uid(self, uid: str) -> dict | None:
        row = self.db.execute("select * from classes where uid = ?", (uid,)).fetchone()
        return _decode(row) if row else None

    def classes(self, since: str = "", until: str = "9999") -> list[dict]:
        """Classes not removed by RUZ that start in [since, until), local ISO bounds; by start."""
        rows = self.db.execute("select * from classes where gone_at is null and start >= ? and start < ? "
                               "order by start, subject", (since, until))
        return [_decode(row) for row in rows]

    def count(self) -> int:
        """Every class kept, the past ones too: with none, the saved ETag is not sent."""
        return self.db.execute("select count(*) n from classes where gone_at is null").fetchone()["n"]

    def size(self) -> int:
        """Classes in the last feed that came: the status and the System topic. The past ones kept are not
        counted, so the number does not grow over a semester; a legitimately empty feed counts 0."""
        return self.store.get_json(SIZE_KEY, 0)

    # --- polling ----------------------------------------------------------------------

    async def run(self) -> None:
        if not self.url:
            log.info("RUZ_ICAL_URL is not set: no classes from RUZ")
            return
        while True:
            try:
                await self.poll()
            except Exception:
                log.exception("RUZ poll failed")
            await asyncio.sleep(POLL_EVERY)

    async def poll(self) -> Diff | None:
        """One fetch; the diff, or None when the fetch failed (the classes stay as they were)."""
        saved = self.store.get_json(ETAG_KEY) or {}
        relinked = bool(saved.get("url")) and saved["url"] != _digest(self.url)
        etag = saved.get("etag") if not relinked and self.count() else None
        try:
            async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=TIMEOUT)) as session:
                async with session.get(self.url, headers={"If-None-Match": etag} if etag else {}) as response:
                    if response.status == 304:
                        body = None
                    else:
                        response.raise_for_status()
                        body, etag = await response.read(), response.headers.get("ETag")
            classes = None if body is None else parse(body)
            if classes is not None and not classes and self.classes(since=clock.local_iso(self.now())):
                raise ValueError("the feed came empty while classes lie ahead")  # a glitch, not a holiday
        except aiohttp.ClientResponseError as e:  # its text carries the request URL: the status says enough
            self._failed(f"HTTP {e.status} {e.message}".strip())
            return None
        except (aiohttp.ClientError, TimeoutError, ValueError, OSError) as e:
            self._failed(self._hide_url(f"{type(e).__name__}: {e}"))  # InvalidURL and others print the link
            return None

        diff = Diff() if classes is None else self._sync(classes, relinked)
        self.store.put_json(ETAG_KEY, {"url": _digest(self.url), "etag": etag})
        self._recovered()
        if classes is not None:
            log.info("RUZ: %d classes in the feed, %d changes", len(classes), len(diff.changes))
        if self.on_poll is not None:
            try:
                await self.on_poll(diff)
            except Exception:
                log.exception("processing the RUZ poll failed")
        return diff

    def _hide_url(self, text: str) -> str:
        """The link, and its path where the secret is, cut out of an error's text."""
        path = urlsplit(self.url).path
        for secret in (self.url, path if len(path) > 1 else ""):
            if secret:
                text = text.replace(secret, HIDDEN_URL)
        return text

    def _failed(self, error: str) -> None:
        self.error = error
        log.warning("RUZ poll failed: %s", error)
        now = self.now()
        self._failing_since = self._failing_since or now
        if not self._told and now - self._failing_since >= STALE_AFTER:
            self._told = True
            if self.notify is not None:
                since = self._failing_since.astimezone(clock.TZ)
                self.notify(f"РУЗ недоступен с {since:%d.%m %H:%M}: {error}.\n"
                            "В ленте и в Tablo — последнее полученное расписание.")

    def _recovered(self) -> None:
        if self._told and self.notify is not None:
            self.notify("РУЗ снова доступен.")
        self.ok_at, self.error = clock.local_iso(self.now()), None
        self._failing_since, self._told = None, False

    def _sync(self, classes: list[Class], relinked: bool = False) -> Diff:
        """Bring `classes` to the feed and tell what changed; `relinked`: the link is not the one of the last poll."""
        now = clock.local_iso(self.now())
        stored = {row["uid"]: _decode(row) for row in self.db.execute("select * from classes")}
        # An empty feed with nothing stored (between terms, before the timetable is out) stores nothing either: it is
        # no first poll, or every hourly poll would be one. The first feed with classes is.
        first = (not stored or relinked) and bool(classes)
        diff = Diff(first=first, relinked=first and bool(stored))
        # New classes past the end of what the feed covered are just the weeks moving on: no news.
        horizon = max((row["start"] for row in stored.values() if row["gone_at"] is None), default="")
        seen: set[str] = set()
        with self.db:
            for item in classes:
                if item.uid in seen:
                    continue
                seen.add(item.uid)
                new = item.row()
                old = stored.get(item.uid)
                if old is None:
                    self.db.execute(
                        f"insert into classes(uid, {', '.join(FIELDS)}, seen_at) "
                        f"values(?, {', '.join('?' * len(FIELDS))}, ?)",
                        (item.uid, *(int(new[f]) if f == "cancelled" else new[f] for f in FIELDS), now))
                    if not diff.first and new["start"] <= horizon:
                        diff.changes.append(Change("new", None, self.by_uid(item.uid)))
                    continue
                self.db.execute(f"update classes set {', '.join(f'{f} = ?' for f in FIELDS)}, seen_at = ?, "
                                "gone_at = null where uid = ?",
                                (*(int(new[f]) if f == "cancelled" else new[f] for f in FIELDS), now, item.uid))
                if kind := _change(old, new):
                    diff.changes.append(Change(kind, old, self.by_uid(item.uid)))
            for uid, old in stored.items():
                if uid not in seen and old["gone_at"] is None and old["start"] >= now:
                    self.db.execute("update classes set gone_at = ? where uid = ?", (now, uid))
                    diff.changes.append(Change("removed", old, self.by_uid(uid)))
        self.store.put_json(SIZE_KEY, len(seen))
        return diff


def _change(old: dict, new: dict) -> str | None:
    if old["gone_at"] is not None:
        return "restored"
    if old["cancelled"] != new["cancelled"]:
        return "cancelled" if new["cancelled"] else "restored"
    if (old["start"], old["end"]) != (new["start"], new["end"]):
        return "moved"
    if old["room"] != new["room"]:
        return "room"
    return None


def _digest(url: str) -> str:
    """The saved ETag belongs to one link; the link itself is a secret and is not stored."""
    return hashlib.sha256(url.encode()).hexdigest()[:16]


def _decode(row) -> dict:
    item = dict(row)
    item["cancelled"] = bool(item["cancelled"])
    return item
