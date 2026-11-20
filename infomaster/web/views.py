"""What the web app reads (§8.6, stage 16.2): one feed of classes and deadlines, the week, an item's card, a course.

A course is known by its key: the dictionary's abbreviation of its name (§6.2), or the name itself until the
dictionary has one. Classes, events and deadlines all name their course, each in its own way; the key is what they
share, so the course's lens is one filter over all of them.

Deadlines have a date and no time. A study deadline due on a day the course has a class is shown at that class (a
seminar first): it is handed in there. The others close their day.
"""

import datetime as dt
from collections.abc import Callable
from dataclasses import dataclass

from .. import clock
from ..collectors.grades import sheet_id
from ..collectors.inbox import MAILBOX, message_link
from ..i18n import Phrase, say
from ..library import Library
from ..library.links import Links
from ..processing.events import OWN
from ..processing.grades import Grades
from ..processing.merge import DeadlineList
from ..processing.news import News
from ..processing.schedule import NOT_CLASSES, SEMINARS, Entry, Schedule, applies, local
from ..processing.subjects import clean
from ..processing.todo import Deadline, uid

FEED_DAYS = 14          # classes of the feed, from today on; deadlines come however far they are
PAST_COURSE_DAYS = 30   # a course of the lenses had a class this recently, or has one ahead
AHEAD_COURSE_DAYS = 60
QUOTE_CHARS = 700       # of a source message on a card
TWIN_DAYS = 14          # «It is a duplicate…» offers the deadlines this close to the card's
TWINS = 8
SOURCE_NAMES = {"hseapp": Phrase("HSE App X", "HSE App X"), "ical": Phrase("the timetable", "РУЗ"),
                "ha": Phrase("added by you", "добавлен вами")}


@dataclass
class Data:
    schedule: Schedule
    merged: DeadlineList
    library: Library | None = None
    grades: Grades | None = None
    links: Links | None = None
    lang: Callable[[], str] = lambda: "en"
    news: News | None = None
    ttl_hours: Callable[[], int] = lambda: 24  # how long news lives (the setting news_ttl_hours)

    # --- time and courses ---------------------------------------------------------------------------------

    def now(self) -> dt.datetime:
        """Moscow local time without a zone, as the timetable keeps it."""
        return self.schedule.now().astimezone(clock.TZ).replace(tzinfo=None)

    def key(self, name: str | None) -> str | None:
        """The course key of a name: its abbreviation, or the name until the dictionary has one."""
        if not name or not clean(name):
            return None
        return self.schedule.subjects.abbr(name) or clean(name)

    def title(self, key: str | None) -> str | None:
        """The course's title for a key: the dictionary's, else the key itself."""
        if key is None:
            return None
        found = next((s.title for s in self.schedule.subjects.all().values() if s.abbr == key), None)
        return found or key

    def courses(self) -> list[dict]:
        """The lenses: courses with a class in the last month or the next two, the soonest class first."""
        now = self.now()
        since, until = now - dt.timedelta(days=PAST_COURSE_DAYS), now + dt.timedelta(days=AHEAD_COURSE_DAYS)
        soonest: dict[str, dt.datetime] = {}
        for entry in self.schedule.class_entries():
            if entry.cancelled or not since <= entry.start <= until:
                continue
            key = entry.abbr or clean(entry.title)
            moment = entry.start if entry.start >= now else entry.start + dt.timedelta(days=3650)
            soonest[key] = min(soonest.get(key, moment), moment)
        return [{"key": key, "title": self.title(key)} for key in sorted(soonest, key=lambda k: (soonest[k], k))]

    def _event_keys(self) -> dict[str, str | None]:
        return {e["uid"]: self.key(e["course"]) for e in self.schedule.events.in_force_all() if e["class_uid"] is None}

    def entry_key(self, entry: Entry, events: dict[str, str | None]) -> str | None:
        return (entry.abbr or clean(entry.title)) if entry.of_class else events.get(entry.uid)

    def deadline_key(self, d: Deadline) -> str | None:
        return d.course or self.key(d.name)

    def course_name(self, key: str) -> str | None:
        """A name of the course for a key, as a deadline's row keeps it: the dictionary's, else the timetable's; None
        for no such course."""
        named = next((name for name, s in sorted(self.schedule.subjects.all().items()) if s.abbr == key), None)
        if named is not None:
            return named
        return next((item["subject"] for item in self.schedule.ruz.classes() if self.key(item["subject"]) == key),
                    None)

    # --- items --------------------------------------------------------------------------------------------

    def told(self, value) -> str | None:
        return say(value, self.lang()) if value is not None else None

    def class_item(self, entry: Entry, key: str | None, now: dt.datetime) -> dict:
        return {"kind": "class", "uid": entry.uid, "title": entry.title, "type": entry.type,
                "start": entry.start.isoformat(), "end": entry.end.isoformat(), "all_day": entry.all_day,
                "place": entry.location, "online": entry.location.startswith("http"), "cancelled": entry.cancelled,
                "of_class": entry.of_class, "notes": entry.notes, "course": key, "past": entry.end <= now,
                "due": []}

    def deadline_item(self, d: Deadline, today: dt.date) -> dict:
        key = self.deadline_key(d)
        return {"kind": "deadline", "uid": uid(d), "title": d.title, "due": d.due.isoformat(), "course": key,
                "course_title": self.title(key) if d.course else d.name, "category": d.category, "source": d.source,
                "from": self.source_name(d), "assumed": d.assumed, "overdue": d.due < today, "at_class": None}

    def source_name(self, d: Deadline) -> str | None:
        if d.source in SOURCE_NAMES:
            return self.told(SOURCE_NAMES[d.source])
        row = self.row(d)
        return self.told(self.schedule.told_by(row)) if row else None

    def row(self, d: Deadline) -> dict | None:
        return self.merged.deadlines.get(int(d.ref[3:])) if d.ref.startswith("dl:") and d.ref[3:].isdigit() else None

    def _deadlines(self, course: str | None) -> list[Deadline]:
        return [d for d in self.merged.visible() if course is None or self.deadline_key(d) == course]

    def _entries(self, since: dt.datetime, until: dt.datetime, course: str | None) -> list[tuple[Entry, str | None]]:
        events = self._event_keys()
        listed = []
        for entry in self.schedule.entries():
            if since <= entry.start < until:
                key = self.entry_key(entry, events)
                if course is None or key == course:
                    listed.append((entry, key))
        return listed

    def _days(self, classes: list[dict], deadlines: list[dict], days: list[dt.date]) -> list[dict]:
        """Days with their items in order: the classes by start, each deadline right after the class it is handed
        in at, the rest of the day's deadlines last."""
        by_day = {day.isoformat(): {"date": day.isoformat(), "items": []} for day in days}
        for item in classes:
            by_day[item["start"][:10]]["items"].append(item)
        for d in deadlines:
            day = by_day.get(d["due"])
            if day is None:
                continue
            candidates = [c for c in day["items"] if c["kind"] == "class" and c["course"] == d["course"]
                          and d["category"] == "study" and not c["cancelled"] and not c["all_day"]
                          and c["type"] not in NOT_CLASSES]
            at = next((c for c in candidates if c["type"] in SEMINARS), candidates[0] if candidates else None)
            if at is not None:
                d["at_class"] = {"uid": at["uid"], "start": at["start"], "title": at["title"]}
                at["due"].append({"uid": d["uid"], "title": d["title"]})
            day["items"].append(d)
        for day in by_day.values():
            classes_first = [i for i in day["items"] if i["kind"] == "class"]
            ordered = []
            for item in classes_first:
                ordered.append(item)
                ordered += [d for d in day["items"] if d["kind"] == "deadline" and d["at_class"]
                            and d["at_class"]["uid"] == item["uid"]]
            ordered += [d for d in day["items"] if d["kind"] == "deadline" and not d["at_class"]]
            day["items"] = ordered
        return list(by_day.values())

    # --- screens ------------------------------------------------------------------------------------------

    def feed(self, course: str | None = None, days: int = FEED_DAYS) -> dict:
        """The main screen: the class going on or next today, the overdue deadlines, then every day ahead that
        has a class (within `days`) or a deadline (however far)."""
        now = self.now()
        today = now.date()
        start = dt.datetime.combine(today, dt.time())
        entries = self._entries(start, start + dt.timedelta(days=days), course)
        classes = [self.class_item(entry, key, now) for entry, key in entries]
        deadlines = [self.deadline_item(d, today) for d in self._deadlines(course)]
        overdue = [d for d in deadlines if d["overdue"]]
        ahead = [d for d in deadlines if not d["overdue"]]
        dates = sorted({dt.date.fromisoformat(c["start"][:10]) for c in classes} |
                       {dt.date.fromisoformat(d["due"]) for d in ahead} | {today})
        current = next((c for c in classes if not c["cancelled"] and not c["all_day"] and c["start"][:10] ==
                        today.isoformat() and c["end"] > now.isoformat()), None)
        return {"now": now.isoformat(timespec="minutes"), "today": today.isoformat(), "course": course,
                "courses": self.courses(), "current": current, "overdue": overdue,
                "days": self._days(classes, ahead, dates)}

    def week(self, monday: dt.date, course: str | None = None) -> dict:
        """Seven days from a Monday, past ones too, with their classes and deadlines."""
        monday -= dt.timedelta(days=monday.weekday())
        now = self.now()
        start = dt.datetime.combine(monday, dt.time())
        entries = self._entries(start, start + dt.timedelta(days=7), course)
        classes = [self.class_item(entry, key, now) for entry, key in entries]
        deadlines = [self.deadline_item(d, now.date()) for d in self._deadlines(course)]
        days = [monday + dt.timedelta(days=i) for i in range(7)]
        return {"now": now.isoformat(timespec="minutes"), "today": now.date().isoformat(), "start": monday.isoformat(),
                "course": course, "courses": self.courses(),
                "days": self._days(classes, deadlines, days)}

    def quote(self, ref: tuple[int, int] | None, source: str) -> dict | None:
        """The message an item came from, while it is kept (14 days, D29)."""
        if ref is None:
            return None
        chat_id, msg_id = ref
        stored = self.schedule.inbox.message(chat_id, msg_id, "mail" if chat_id == MAILBOX else source)
        if stored is None:
            return {"chat": chat_id, "msg": msg_id, "text": None}
        text = stored["text"] or ""
        return {"chat": chat_id, "msg": msg_id, "sender": stored["sender_name"], "at": stored["at"],
                "subject": stored.get("subject"),
                "text": text if len(text) <= QUOTE_CHARS else text[:QUOTE_CHARS].rstrip() + "…"}

    def deadline_card(self, item_uid: str) -> dict | None:
        """A deadline with its course, where it is handed in, the message it came from and its history."""
        d = self.merged.find(item_uid) or next((own for own in self.merged.todo.own() if own.key == item_uid), None)
        if d is None:
            return None
        now = self.now()
        item = self.deadline_item(d, now.date())
        day = dt.datetime.combine(d.due, dt.time())
        self._days([self.class_item(e, k, now) for e, k in self._entries(day, day + dt.timedelta(days=1),
                                                                         item["course"])], [item], [d.due])
        row = self.row(d)
        history, quote, link = [], None, None
        if row is not None:
            older, earlier = [], self.merged.db.execute("select * from deadlines where moved_by = ?", (row["id"],))
            chain = [dict(r) for r in earlier.fetchall()]
            while chain:
                older.insert(0, chain[0])
                chain = [dict(r) for r in self.merged.db.execute("select * from deadlines where moved_by = ?",
                                                                 (chain[0]["id"],)).fetchall()]
            rows = older + [row]
            history.append({"at": rows[0]["created_at"], "what": "found", "due": rows[0]["due"],
                            "from": self.told(self.schedule.told_by(rows[0]))})
            for before, after in zip(rows, rows[1:], strict=False):
                history.append({"at": after["created_at"], "what": "moved", "was": before["due"], "due": after["due"],
                                "from": self.told(self.schedule.told_by(after))})
            if row["source"] in ("tg", "mail") and row["chat_id"] is not None and row["msg_ids"]:
                msg_ids = row["msg_ids"] if isinstance(row["msg_ids"], list) else []
                if msg_ids:
                    quote = self.quote((row["chat_id"], msg_ids[0]), row["source"])
                link = self.schedule.link(row)
        elif d.source == "hseapp":
            history.append({"at": None, "what": "hseapp", "due": d.due.isoformat(), "from": "HSE App X"})
        elif d.source == "ha":
            history.append({"at": None, "what": "own", "due": d.due.isoformat(), "from": None})
        return {**item, "quote": quote, "link": link, "history": history, "own": d.source == "ha",
                "twins": self.twins(d, now.date())}

    def twins(self, d: Deadline, today: dt.date) -> list[dict]:
        """What «It is a duplicate…» offers: the other deadlines of the course (of any, for one without a course)
        within two weeks of it, the nearest date first."""
        key = self.deadline_key(d)
        found = [x for x in self.merged.visible() if uid(x) != uid(d) and abs((x.due - d.due).days) <= TWIN_DAYS
                 and (key is None or self.deadline_key(x) == key)]
        found.sort(key=lambda x: (abs((x.due - d.due).days), x.due))
        return [self.deadline_item(x, today) for x in found[:TWINS]]

    def class_card(self, entry_uid: str) -> dict | None:
        """A class or an event with its course, the deadlines handed in at it, the message that changed it, and
        how much material the library has for it."""
        events = self._event_keys()
        entry = next((e for e in self.schedule.entries() if e.uid == entry_uid), None)
        if entry is None:
            return None
        now = self.now()
        key = self.entry_key(entry, events)
        item = self.class_item(entry, key, now)
        day = dt.date.fromisoformat(item["start"][:10])
        deadlines = [self.deadline_item(d, now.date()) for d in self._deadlines(key) if d.due == day] if key else []
        self._days([item], deadlines, [day])
        source = "mail" if entry.message and entry.message[0] == MAILBOX else "tg"
        materials = len(self.library.items(class_uid=entry.uid)) if self.library is not None else 0
        return {**item, "course_title": self.title(key), "source": entry.source, "origin": entry.origin,
                "link": entry.link, "quote": self.quote(entry.message, source), "materials": materials,
                "fix": self.fix(entry)}

    # --- corrections (step 16.9) --------------------------------------------------------------------------

    @staticmethod
    def _span(start: dt.datetime, end: dt.datetime, whole_day: bool) -> dict:
        """start and end as the tools take them: Moscow minutes, or a date and no end for a whole day."""
        if whole_day:
            return {"start": start.date().isoformat(), "end": None}
        return {"start": start.isoformat(timespec="minutes"), "end": end.isoformat(timespec="minutes")}

    def fix(self, entry: Entry) -> dict | None:
        """What stands behind an entry for the owner to change, with every field as it is now: correct_class and
        edit_event reset a field they are not given, so the app sends them all and changes only what was touched.
        A class: RUZ's own time, the owner's correction (`own`, its event id) and the chat's one (`chat`, applied
        unless hidden). An event: its id, whether it is the owner's own or an own copy of a chat's one."""
        events = self.schedule.events
        if entry.of_class:
            item = self.schedule.ruz.by_uid(entry.uid)
            if item is None:
                return None
            own, chat = events.own_correction(entry.uid), events.correction(entry.uid)
            behind = own or (chat if applies(item, chat) else None)
            fields = {**self._span(entry.start, entry.end, entry.all_day),
                      "room": (behind["room"] if behind else item["room"]) or "", "cancelled": entry.cancelled,
                      "kind": own["kind"] if own else item["kind"], "required": entry.required}
            if own and own["title"]:
                fields["title"] = own["title"]
            start, end = local(item["start"]), local(item["end"])
            return {"of": "class", "fields": fields, "own": own["id"] if own else None,
                    "chat": {"id": chat["id"], "hidden": bool(chat["hidden"]),
                             "from": self.told(self.schedule.told_by(chat))} if chat else None,
                    "ruz": {**self._span(start, end, False), "room": item["room"] or "",
                            "cancelled": bool(item["cancelled"])}}
        event = events.by_uid(entry.uid)
        if event is None:
            return None
        return {"of": "event", "id": event["id"], "own": event["source"] == OWN, "copy": bool(event["copy_of"]),
                "required": bool(event["required"]),
                "fields": {"title": event["title"], "kind": event["kind"],
                           **self._span(entry.start, entry.end, entry.all_day), "room": event["room"] or "",
                           "cancelled": bool(event["cancelled"])}}

    def hidden(self) -> dict:
        """What the owner hid, to bring back: the deadlines, the newest due date first, and the events from today
        on (an event an own copy stands in for is not listed: «As it was» on the copy brings it back)."""
        today = self.now().date()
        reasons = {"done": Phrase("done", "сдан"), "not_mine": Phrase("not mine", "не мой"),
                   "duplicate": Phrase("a duplicate", "дубль")}
        deadlines = [{"uid": row["uid"], "title": row["title"], "due": row["due"],
                      "course": self.key(row["course"]) if row["course"] else None,
                      "reason": self.told(reasons[row["reason"]]) if row["reason"] in reasons else None,
                      "from": self.told(SOURCE_NAMES[row["source"]]) if row["source"] in SOURCE_NAMES else None}
                     for row in self.merged.todo.hidden_rows()]
        events = [{**event, "chat": self.told(event["chat"]) if event["chat"] is not None else None}
                  for event in self.schedule.agenda(today, 3650)["hidden"]]
        return {"deadlines": deadlines, "events": events}

    # --- news (step 16.7) ---------------------------------------------------------------------------------

    def news_item(self, item: dict) -> dict:
        """A live item as the news list shows it: where it came from by name, a letter by its subject."""
        inbox = self.schedule.inbox
        if item["source"] == "mail":
            subject = inbox.subject(item["msg_ids"])
            where = {"kind": "mail", "name": subject}
        else:
            chat = inbox.chat(item["chat_id"]) if item["chat_id"] is not None else None
            where = {"kind": chat["kind"] if chat else "chat", "name": chat["title"] if chat else None}
        return {"id": item["id"], "full": item["full"], "short": item["short"], "importance": item["importance"],
                "study": item["study"], "posted_at": item["posted_at"], "from": where}

    def news_list(self) -> dict:
        """The live news in rank order, as Tablo and the forum's list have them."""
        if self.news is None:
            return {"items": [], "ttl_hours": self.ttl_hours()}
        live = self.news.live(self.schedule.now(), self.ttl_hours())
        return {"items": [self.news_item(item) for item in live], "ttl_hours": self.ttl_hours()}

    def important_news(self) -> int:
        """How many live items are of importance 4 or 5: the mark on the News view."""
        if self.news is None:
            return 0
        return sum(item["importance"] >= 4 for item in self.news.live(self.schedule.now(), self.ttl_hours()))

    def news_card(self, news_id: int) -> dict | None:
        """An item with the message it came from, while it is kept, and its link where the chat has one."""
        item = self.news.get(news_id) if self.news is not None else None
        if item is None or item["hidden"]:
            return None
        quote, link = None, None
        if item["chat_id"] is not None and item["msg_ids"]:
            quote = self.quote((item["chat_id"], item["msg_ids"][0]), item["source"])
            chat = self.schedule.inbox.chat(item["chat_id"]) if item["source"] != "mail" else None
            link = message_link(chat, item["msg_ids"][0]) if chat else None
        live = item["replaced_by"] is None and item["gone_at"] is None
        return {**self.news_item(item), "quote": quote, "link": link, "live": live}

    def course(self, key: str) -> dict | None:
        """A course's lens: its title, next class, grades and shelves (knowledge base, sheets, raw material)."""
        names = sorted({item["subject"] for item in self.schedule.ruz.classes() if self.key(item["subject"]) == key})
        known = any(c["key"] == key for c in self.courses()) or names or \
            any(self.deadline_key(d) == key for d in self.merged.visible())
        if not known:
            return None
        now = self.now()
        upcoming = [(e, k) for e, k in self._entries(now, now + dt.timedelta(days=AHEAD_COURSE_DAYS), key)
                    if not e.cancelled]
        nxt = self.class_item(*upcoming[0], now) if upcoming else None
        if nxt is not None:
            day = dt.date.fromisoformat(nxt["start"][:10])
            self._days([nxt], [self.deadline_item(d, now.date()) for d in self._deadlines(key) if d.due == day], [day])
        return {"key": key, "title": self.title(key), "next": nxt, "grades": self._grades(key, names),
                "documents": self._documents(names), "materials": self._materials(names),
                "sheets": self._sheets(key)}

    def _grades(self, key: str, names: list[str]) -> dict:
        works = []
        if self.grades is not None:
            for sheet in self.grades.settings.get("grades_sheets"):
                sid = sheet_id(sheet["url"])
                course = self.grades.view(sid)["course"] or sheet["label"]
                if self.key(course) == key or any(self.schedule.same_course(course, n) for n in names) or \
                        clean(sheet["label"]).casefold() == key.casefold():
                    works += [{"id": i["id"], "label": i["label"], "value": i["value"], "new": i["new"],
                               "edited": i["edited"], "sheet_value": i["sheet_value"], "own": False}
                              for i in self.grades.items(sid)]
            # the grades added by hand: their course is a title, as «+» and the Mini App give it
            works += [{"id": g["id"], "label": g["label"], "value": g["value"], "new": g["new"], "edited": False,
                       "sheet_value": None, "own": True} for g in self.grades.manual()
                      if self.key(g["course"]) == key or any(self.schedule.same_course(g["course"], n) for n in names)]
        values = [w["value"] for w in works if w["value"] is not None]
        return {"count": len(works), "new": sum(w["new"] for w in works),
                "average": round(sum(values) / len(values), 1) if values else None, "works": works}

    def _documents(self, names: list[str]) -> list[dict]:
        if self.library is None or not names:
            return []
        docs = []
        for doc in self.library.documents(names[0]):
            if not doc["version"]:
                continue
            full = self.library.document(doc["id"])
            version = next((v for v in full["versions"] if v["version"] == doc["version"]), None)
            url = None
            if version is not None and self.links is not None:
                part = "source" if doc["kind"] == "cheatsheet" else "pdf"
                version = {**version, "kind": doc["kind"], "title": doc["title"]}
                url = self.links.version(version, part) if self.library.part_path(version, part) else None
            docs.append({"id": doc["id"], "kind": doc["kind"], "title": doc["title"], "day": doc["day"], "url": url})
        order = {"book": 0, "cheatsheet": 1}
        docs.sort(key=lambda d: d["day"] or "", reverse=True)
        docs.sort(key=lambda d: order.get(d["kind"], 2))
        return docs

    def _materials(self, names: list[str]) -> dict:
        if self.library is None or not names:
            return {"count": 0, "new": 0}
        found = next((c for c in self.library.courses() if c["course"] == self.library.course(names[0])), None)
        return {"count": found["items"] if found else 0, "new": found["new"] if found else 0}

    def _sheets(self, key: str) -> dict:
        rows = self.schedule.db.execute("select course from sheets where previous is null and status != 'failed'")
        return {"count": sum(1 for row in rows if self.key(row["course"]) == key)}
