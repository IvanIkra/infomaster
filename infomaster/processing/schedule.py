"""The timetable (§6.5): RUZ classes, events from chats and the chat corrections of classes.

One list of entries serves the iCal feeds, the batch prompt and the Mini App:
- a RUZ class is always in the main calendar. A correction in force from a chat applies to it: a cancellation
  makes it cancelled, a move gives it the new time and room with a note. RUZ wins: when RUZ itself changes or
  removes the class, its corrections are gone (`after_poll`);
- an event of its own goes to the main or the optional calendar by `required` (р. 28), unless the owner hid it.

The owner adds events of their own and corrects classes in the Mini App (р. 44): an own correction wins over the chats'
ones until it is undone or RUZ changes the class; a change of an event from a chat hides it and puts an own copy in its
place, so the chat's next message changes only the hidden one (as a changed deadline does, §5.4).

Titles are English: a class takes its subject's title from the dictionary (the feed's name until there is one), an
event has the model's own short title. Types are the words Tablo reads from the first line of DESCRIPTION (§4.2):
Lecture, Seminar, Consultation, Event, Interim assessment, and a few more for kinds that are none of these.

After every RUZ poll the subjects are named (only names not known yet go to Haiku) and each interim assessment keeps
its deadline "<abbr> Midterm" in `deadlines` with source "ical" (§4.4): added, moved, renamed once the dictionary
knows the course, withdrawn when the class is cancelled or removed. It follows the class as the feeds show it, so a
chat's correction in force moves or withdraws it too: the Midterms are synced after every RUZ poll, every batch run,
every hiding of a correction by the owner and every change of the dictionary by hand (its abbr starts the label). The
merge with the other sources is processing/merge.py's.
"""

import datetime as dt
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

from .. import clock
from ..collectors.inbox import Inbox, message_link
from ..collectors.ruz import Diff, Ruz
from ..i18n import Phrase
from ..store import Store
from .deadlines import Deadlines
from .events import OWN, Events, own_uid
from .subjects import Subjects

log = logging.getLogger(__name__)

DEFAULT_LENGTH = dt.timedelta(minutes=80)  # an event whose end nobody said: one HSE class period
CLASS_DAYS = 21           # classes shown to the model, from today on: moves and cancellations come close to the date
AGENDA_DAYS = 28          # the Mini App's timetable, from today on
MIDTERM_KINDS = ("рубежный контроль", "interim assessment")  # §4.4

# Words of a kind, from RUZ (Russian or English) or from the model -> the type; the first match wins.
TYPES = (
    (("рубежный контроль", "interim assessment"), "Interim assessment"),
    (("экзамен", "зачёт", "зачет", "exam"), "Exam"),
    (("консультац", "consultation"), "Consultation"),
    (("лекци", "lecture"), "Lecture"),
    (("семинар", "практическ", "seminar", "practical", "practice", "tutorial"), "Seminar"),
    (("лаборатор", "lab"), "Lab"),
    (("мероприят", "событ", "event"), "Event"),
)


def type_of(kind: str, of_class: bool) -> str:
    """The first line of DESCRIPTION for a kind; an English kind of none of the types keeps its word."""
    folded = (kind or "").casefold()
    for words, name in TYPES:
        if any(word in folded for word in words):
            return name
    if folded.strip() and folded.isascii():
        return folded.strip().capitalize()
    return "Class" if of_class else "Event"


def midterm(kind: str) -> bool:
    folded = (kind or "").casefold()
    return any(word in folded for word in MIDTERM_KINDS)


@dataclass
class Entry:
    """One VEVENT of our feeds."""
    uid: str
    title: str
    start: dt.datetime          # Moscow local, naive
    end: dt.datetime
    type: str
    location: str               # a room or a link
    required: bool              # the main calendar, else the optional one
    cancelled: bool
    source: str                 # ruz, tg, mail, pdf
    link: str | None = None     # the source message
    notes: list[str] = field(default_factory=list)  # DESCRIPTION after the type


@dataclass
class MidtermChanges:
    """What a sync did to the Midterm deadlines, for the Дедлайны topic."""
    deadlines: list[int] = field(default_factory=list)
    moved: list[tuple[int, int]] = field(default_factory=list)
    gone: list[int] = field(default_factory=list)

    def __bool__(self) -> bool:
        return bool(self.deadlines or self.moved or self.gone)


def local(text: str) -> dt.datetime:
    return dt.datetime.fromisoformat(text)


def when(moment: dt.datetime) -> str:
    """'Tue Oct 20 09:30': notes of the feed are English."""
    return f"{moment:%a %b} {moment.day} {moment:%H:%M}"


def moved_back(item: dict, correction: dict) -> bool:
    """A chat moved the class back where RUZ has it: the correction changes nothing."""
    return not correction["cancelled"] and \
        (correction["start"], correction["end"], correction["room"]) == (item["start"], item["end"], item["room"])


def applies(item: dict, correction: dict | None) -> bool:
    """The correction changes the class in the feeds: not hidden by the owner, the class not cancelled by RUZ
    itself, and not a move back to where RUZ has it."""
    return correction is not None and not correction["hidden"] and not item["cancelled"] and \
        not moved_back(item, correction)


def location(room: str, link: str | None) -> str:
    """LOCATION: the room, or the meeting link of an online class (Tablo reads a link as online, §4.2)."""
    return (link or room) if room == "online" else room


class Schedule:
    def __init__(self, store: Store, ruz: Ruz, events: Events, deadlines: Deadlines, subjects: Subjects,
                 inbox: Inbox, now: Callable[[], dt.datetime] = clock.now_utc):
        self.db = store.db
        self.ruz, self.events, self.deadlines, self.subjects, self.inbox, self.now = (
            ruz, events, deadlines, subjects, inbox, now)
        # Midterms moved by the owner's choice (hiding a correction), for the Дедлайны topic; a RUZ poll and a batch
        # run hand theirs over with their own changes.
        self.on_midterms: Callable[[MidtermChanges], Awaitable[None]] | None = None

    # --- entries ----------------------------------------------------------------------

    def entries(self) -> list[Entry]:
        """Every class not removed by RUZ and every event in force that is not hidden, by start."""
        corrections = self.events.corrections()
        entries = [self.class_entry(item, corrections.get(item["uid"])) for item in self.ruz.classes()]
        entries += [self.event_entry(event) for event in self.events.in_force_all()
                    if event["class_uid"] is None and not event["hidden"]]
        return sorted(entries, key=lambda e: (e.start, e.title))

    def class_title(self, item: dict) -> str:
        return self.subjects.title(item["subject"])

    def class_entry(self, item: dict, correction: dict | None = None) -> Entry:
        entry = Entry(uid=item["uid"], title=self.class_title(item), start=local(item["start"]),
                      end=local(item["end"]), type=type_of(item["kind"], of_class=True),
                      location=location(item["room"], item["link"]), required=True, cancelled=item["cancelled"],
                      source="ruz")
        if not applies(item, correction):
            return entry
        entry.source, entry.link = correction["source"], self.link(correction)
        by = "by hand" if correction["source"] == OWN else "in a chat"
        if correction["cancelled"]:
            entry.cancelled = True
            entry.notes.append(f"Cancelled {by}")
        else:
            entry.notes.append(f"Moved from {when(entry.start)} {by}")
            entry.start, entry.end = local(correction["start"]), local(correction["end"])
            if correction["room"] != item["room"]:
                entry.location = correction["room"]
        return entry

    def event_entry(self, event: dict) -> Entry:
        start = local(event["start"])
        entry = Entry(uid=event["uid"], title=event["title"], start=start,
                      end=local(event["end"]) if event["end"] else start + DEFAULT_LENGTH,
                      type=type_of(event["kind"], of_class=False), location=event["room"],
                      required=event["required"], cancelled=event["cancelled"], source=event["source"],
                      link=self.link(event))
        if subject := self.subjects.get(event["course"]):  # a course not named yet stays out: the feed is English
            entry.notes.append(subject.title)
        earlier = self.events.previous(event["id"])
        if earlier is not None and earlier["start"] != event["start"]:
            entry.notes.append(f"Moved from {when(local(earlier['start']))}")
        return entry

    def link(self, event: dict) -> str | None:
        """t.me link to the message that told the event (or a deadline: any row with source, chat_id, msg_ids), where
        the chat has links."""
        chat = self.inbox.chat(event["chat_id"]) if event["chat_id"] is not None else None
        return message_link(chat, event["msg_ids"][0]) if chat and event["msg_ids"] else None

    def told_by(self, event: dict) -> str | Phrase | None:
        """Who told it (an event, or any row with source, chat_id, msg_ids), for the Mini App: the chat's name, or
        the letter's subject (§8.3)."""
        if event["source"] == "mail":
            subject = self.inbox.subject(event["msg_ids"])
            return Phrase(f"letter «{subject}»", f"письмо «{subject}»") if subject else Phrase("letter", "письмо")
        chat = self.inbox.chat(event["chat_id"]) if event["chat_id"] is not None else None
        return chat["title"] if chat else None

    # --- for the batches and the Mini App -------------------------------------------------

    def upcoming_classes(self, today: dt.date) -> list[dict]:
        """Classes of the next weeks with their titles and the correction in force, for the batch prompt. A class
        counts by the later of its RUZ time and its correction's: one moved to a later day stays listed after its
        RUZ day, so an edit of the message can restate the move and a later message can move or cancel it again."""
        corrections = self.events.corrections()
        since, until = today.isoformat(), (today + dt.timedelta(days=CLASS_DAYS)).isoformat()
        listed = []
        for item in self.ruz.classes(until=until):
            correction = corrections.get(item["uid"])
            if max(item["start"], correction["start"] if correction else "") < since:
                continue
            listed.append({**item, "title": self.class_title(item), "type": type_of(item["kind"], of_class=True),
                           "correction": correction})
        return listed

    def agenda(self, today: dt.date, days: int = AGENDA_DAYS) -> dict:
        """The Расписание screen (§8.4): the entries of the feeds from today on, each with the event or correction
        behind it for the owner to change, and the events the owner hid."""
        since, until = today.isoformat(), (today + dt.timedelta(days=days)).isoformat()
        corrections = self.events.corrections()
        events, hidden = {}, []
        for event in self.events.in_force_all():
            if event["class_uid"] is None:
                events[event["uid"]] = event
                if event["hidden"] and event["start"] >= since:
                    hidden.append({"id": event["id"], "title": event["title"], "start": event["start"],
                                   "end": self._end(event), "type": type_of(event["kind"], of_class=False),
                                   "chat": self.told_by(event)})
        classes = {item["uid"]: item for item in self.ruz.classes()}
        items = []
        for entry in self.entries():
            if not since <= entry.start.date().isoformat() < until:
                continue
            item = classes.get(entry.uid)
            event = corrections.get(entry.uid) if item is not None else events.get(entry.uid)
            items.append({"uid": entry.uid, "title": entry.title, "start": entry.start.isoformat(),
                          "end": entry.end.isoformat(), "type": entry.type, "location": entry.location,
                          "required": entry.required, "cancelled": entry.cancelled, "source": entry.source,
                          "link": entry.link, "notes": entry.notes,
                          "event": self._behind(event, item) if event is not None else None})
        return {"items": items, "hidden": hidden}

    def _end(self, event: dict) -> str:
        return event["end"] or (local(event["start"]) + DEFAULT_LENGTH).isoformat()

    def _behind(self, event: dict, item: dict | None) -> dict:
        """The event or correction behind an entry, as the Mini App acts on it; `class` is RUZ's own time."""
        return {"id": event["id"], "correction": item is not None, "own": event["source"] == OWN,
                "hidden": event["hidden"], "cancelled": event["cancelled"], "start": event["start"],
                "end": self._end(event), "room": event["room"], "chat": self.told_by(event), "link": self.link(event),
                "class": {"start": item["start"], "end": item["end"], "room": item["room"]} if item else None}

    # --- the owner's choices ------------------------------------------------------------

    async def set_hidden(self, event_id: int, hidden: bool) -> dict | None:
        """Events.set_hidden for the forum buttons and the Mini App. Hiding a correction, or applying it again,
        moves the class back or forth, and its Midterm with it."""
        row = self.events.set_hidden(event_id, hidden)
        if row is not None and row["class_uid"] is not None:
            await self._follow_midterms()
        return row

    async def _follow_midterms(self) -> None:
        """The Midterms follow the classes as the owner left them; the forum hears of the ones that moved."""
        if (midterms := self.sync_midterms()) and self.on_midterms is not None:
            try:
                await self.on_midterms(midterms)
            except Exception:
                log.exception("posting the Midterm changes failed")

    def add_event(self, fields: dict) -> int:
        """An own event: `fields` are title, kind, start, end, room, required. It is in the feeds as the chats'
        events are."""
        with self.db:
            return self.events.add({**fields, "uid": own_uid(), "source": OWN, "chat_id": None, "msg_ids": [],
                                    "course": None, "chosen": True}, self.now())

    def edit_event(self, event_id: int, fields: dict) -> int | None:
        """The owner's change of an event (some of title, kind, start, end, room, required): an own event gets a new
        version; one from a chat or a letter is hidden and an own copy with the change takes its place. The id of the
        new version; None for no such event."""
        event = self.events.current(event_id)
        if event is None or event["gone_at"] is not None or event["class_uid"] is not None:
            return None
        new = {**{key: event[key] for key in ("title", "kind", "start", "end", "room", "required", "course")},
               **fields, "source": OWN, "chat_id": None, "msg_ids": [], "chosen": True}
        with self.db:
            if event["source"] == OWN:
                new_id = self.events.add({**new, "uid": event["uid"]}, self.now())
                self.events.supersede(event["id"], new_id)
            else:
                new_id = self.events.add({**new, "uid": own_uid()}, self.now())
                self.db.execute("update events set hidden = 1 where id = ?", (event["id"],))
        return new_id

    async def correct_class(self, class_uid: str, start: str, end: str, room: str, cancelled: bool) -> int | None:
        """The owner moves or cancels a RUZ class: a new version of their own correction, or the first one. None for
        no such class."""
        item = self.ruz.by_uid(class_uid)
        if item is None or item["gone_at"] is not None:
            return None
        earlier = self.events.own_correction(class_uid)
        with self.db:
            new_id = self.events.add({"uid": earlier["uid"] if earlier else own_uid(), "source": OWN, "chat_id": None,
                                      "msg_ids": [], "class_uid": class_uid, "course": item["subject"],
                                      "title": self.class_title(item), "kind": item["kind"], "start": start,
                                      "end": end, "room": room, "required": True, "cancelled": cancelled},
                                     self.now())
            if earlier is not None:
                self.events.supersede(earlier["id"], new_id)
        await self._follow_midterms()
        return new_id

    async def remove_own(self, event_id: int) -> dict | None:
        """An own event is deleted, an own correction undone (the chats' one applies again, if there is one); the
        event, or None when it is not an own one in force."""
        event = self.events.current(event_id)
        if event is None or event["gone_at"] is not None or event["source"] != OWN:
            return None
        with self.db:
            self.events.withdraw(event["uid"], self.now())
        if event["class_uid"] is not None:
            await self._follow_midterms()
        return event

    # --- after a RUZ poll -------------------------------------------------------------

    async def after_poll(self, diff: Diff) -> MidtermChanges:
        """Ruz.on_poll: RUZ wins over the chats, the subjects get named, the Midterm deadlines follow the classes."""
        now = self.now()
        with self.db:
            for change in diff.changes:
                if change.kind != "new" and (overruled := self.events.overrule(change.new["uid"], now)):
                    log.info("RUZ changed %s itself: chat corrections %s are gone", change.new["uid"], overruled)
        await self.subjects.try_resolve((item["subject"] for item in self.ruz.classes()), who="ruz")
        return self.sync_midterms()

    def sync_midterms(self) -> MidtermChanges:
        """Each interim assessment's deadline as the feeds show the class: RUZ's own state, then the chat's
        correction in force (a cancellation withdraws it, a move moves it)."""
        changes, now = MidtermChanges(), self.now()
        today = clock.local_day(now).isoformat()
        corrections = self.events.corrections()
        with self.db:
            for item in self.db.execute("select * from classes order by start").fetchall():
                deadline = self.deadlines.get(item["deadline_id"]) if item["deadline_id"] else None
                if deadline is not None and (deadline["moved_by"] or deadline["gone_at"]):
                    deadline = None
                correction = corrections.get(item["uid"])
                applied = applies(item, correction)
                wanted = midterm(item["kind"]) and not item["cancelled"] and item["gone_at"] is None and \
                    not (applied and correction["cancelled"])
                if not wanted:
                    if deadline is not None:
                        self.deadlines.drop([deadline["id"]], now)
                        changes.gone.append(deadline["id"])
                    if item["deadline_id"]:
                        self._link(item["id"], None)
                    continue
                due = (correction["start"] if applied else item["start"])[:10]
                label = f"{self.subjects.abbr(item['subject']) or item['subject']} Midterm"
                if deadline is None:
                    if due >= today:  # a past one is no news
                        new_id = self.deadlines.add({"source": "ical", "chat_id": None, "msg_ids": [],
                                                     "course": item["subject"], "label": label, "due": due}, now)
                        self._link(item["id"], new_id)
                        changes.deadlines.append(new_id)
                elif deadline["due"] != due:
                    new_id = self.deadlines.add({"source": "ical", "chat_id": None, "msg_ids": [],
                                                 "course": item["subject"], "label": label, "due": due}, now)
                    self.deadlines.move(deadline["id"], new_id)
                    self._link(item["id"], new_id)
                    changes.moved.append((deadline["id"], new_id))
                elif (deadline["label"], deadline["course"]) != (label, item["subject"]):
                    self.deadlines.rename(deadline["id"], label, item["subject"])  # the dictionary has named it
        return changes

    def _link(self, class_id: int, deadline_id: int | None) -> None:
        self.db.execute("update classes set deadline_id = ? where id = ?", (deadline_id, class_id))
