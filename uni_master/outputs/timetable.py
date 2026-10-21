"""The «Расписание» topic (§6.5, §8.3): what changes in the timetable, with a link to the message that told it.

- From the chats and letters, one message each: a new event, its move or cancellation, a move or cancellation of a
  class, and their withdrawal when the message is deleted or edited into something else. Under an event of its own
  the buttons put it into the main or the optional calendar and hide it (a hidden event leaves the feeds); under a
  correction of a class they stop applying it, and the class is back as RUZ has it. Only the owner may press them.
- From RUZ, one message a poll: classes added inside the weeks the feed already covered, moved, changed rooms,
  cancelled, removed, back again. The first poll that brings classes is no news, nor is the first one after the link
  changed: the System topic only hears how many came.

Past things are left out. What the bot posted is kept in `bot_posts`.
"""

import datetime as dt
import html
import logging
import re
from collections.abc import Callable

from telethon import Button

from .. import clock
from ..collectors.inbox import Inbox
from ..collectors.ruz import Change, Diff
from ..processing.batches import Changes
from ..processing.schedule import Schedule, moved_back, type_of
from ..store import Store
from .feed import MESSAGE_LIMIT, WEEKDAYS, remember, source, split
from .forum import Forum

log = logging.getLogger(__name__)

BUTTON = re.compile(rb"^ev:(main|optional|hide|show):(\d+)$")
TOPIC = "schedule"
TYPES_RU = {"Lecture": "лекция", "Seminar": "семинар", "Consultation": "консультация", "Event": "событие",
            "Interim assessment": "рубежный контроль", "Exam": "экзамен", "Lab": "лабораторная", "Class": "занятие"}
ANSWERS = {"main": "Теперь в основном календаре", "optional": "Теперь в необязательном календаре",
           "hide": "Скрыто: в календарях его нет", "show": "Снова в календарях"}
CORRECTION_ANSWERS = {"hide": "Правка не применяется: пара как в РУЗ", "show": "Правка снова применяется"}


def moment(text: str) -> str:
    """'2026-10-22T18:10:00' -> 'чт 22.10 18:10'."""
    when = dt.datetime.fromisoformat(text)
    return f"{WEEKDAYS[when.weekday()]} {when:%d.%m %H:%M}"


def span(start: str, end: str | None) -> str:
    """'чт 22.10 18:10–19:30'; the end is left out when it is unknown or on another day."""
    text = moment(start)
    if end and end[:10] == start[:10] and end > start:
        text += f"–{dt.datetime.fromisoformat(end):%H:%M}"
    return text


def place(room: str | None) -> str:
    if not room:
        return ""
    if re.match(r"https?://", room):
        return f' · <a href="{html.escape(room)}">ссылка</a>'
    return " · онлайн" if room == "online" else f" · {html.escape(room)}"


def kind_ru(kind: str, of_class: bool) -> str:
    """The type in Russian, HTML-escaped: an unknown kind keeps the words the model or the feed gave."""
    name = type_of(kind, of_class)
    return html.escape(TYPES_RU.get(name, name.lower()))


class Timetable:
    def __init__(self, forum: Forum, store: Store, inbox: Inbox, schedule: Schedule, owner_id: int,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.db, self.inbox, self.schedule, self.owner_id, self.now = (
            forum, store.db, inbox, schedule, owner_id, now)
        self.events, self.ruz = schedule.events, schedule.ruz

    # --- from the chats -----------------------------------------------------------------

    async def publish(self, changes: Changes) -> None:
        """After every batch run: the events it gave, moved, cancelled or withdrew."""
        today = clock.local_day(self.now()).isoformat()
        for event_id in changes.events:
            event = self.events.get(event_id)
            if not event["hidden"] and self._when(event) >= today:
                await self._post(event, self._class_text(event) if event["class_uid"] else self._new_text(event))
        for old_id, new_id in changes.events_moved:
            old, new = self.events.get(old_id), self.events.get(new_id)
            if not new["hidden"] and max(old["start"], new["start"]) >= today:
                await self._post(new, self._moved_text(old, new))
        for event_id in changes.events_gone:
            event = self.events.get(event_id)
            if not event["hidden"] and self._when(event) >= today:
                await self._post(event, self._gone_text(event))

    def _when(self, event: dict) -> str:
        """The later of an event's time and, for a correction, its class's time."""
        item = self.ruz.by_uid(event["class_uid"]) if event["class_uid"] else None
        return max(event["start"], item["start"] if item else "")

    def _title(self, event: dict) -> str:
        return f"<b>{html.escape(event['title'])}</b>"

    def _source(self, event: dict) -> str:
        """The message that told it, or the letter's subject."""
        return source(self.inbox, event) if event["chat_id"] is not None else ""

    def _new_text(self, event: dict) -> str:
        lines = [f"🆕 {self._title(event)} — {kind_ru(event['kind'], of_class=False)}",
                 span(event["start"], event["end"]) + place(event["room"])]
        if event["course"]:
            lines.append(html.escape(self.schedule.subjects.title(event["course"])))
        return "\n".join(lines)

    def _class_text(self, correction: dict) -> str:
        item = self.ruz.by_uid(correction["class_uid"])
        what = f"Пара {self._title(correction)} ({kind_ru(item['kind'], of_class=True)}"
        if correction["cancelled"]:
            return f"✖️ {what}, {moment(item['start'])}) отменена"
        if moved_back(item, correction):
            return f"↩️ {what}) снова как в РУЗ: {span(item['start'], item['end'])}{place(item['room'])}"
        return (f"↪️ {what}) перенесена: {moment(item['start'])} → {span(correction['start'], correction['end'])}"
                + place(correction["room"]))

    def _moved_text(self, old: dict, new: dict) -> str:
        if new["cancelled"] and not old["cancelled"]:
            return f"✖️ {self._title(new)} ({moment(old['start'])}) отменено"
        if old["cancelled"] and not new["cancelled"]:
            return f"↩️ {self._title(new)} снова в силе: {span(new['start'], new['end'])}{place(new['room'])}"
        if (old["start"], old["end"]) == (new["start"], new["end"]) and old["room"] != new["room"]:
            return f"🚪 {self._title(new)} ({moment(new['start'])}): другое место{place(new['room'])}"
        return f"↪️ {self._title(new)} перенесено: {moment(old['start'])} → " \
               f"{span(new['start'], new['end'])}{place(new['room'])}"

    def _gone_text(self, event: dict) -> str:
        why = "снято: сообщение удалено или исправлено"
        if event["class_uid"]:
            item = self.ruz.by_uid(event["class_uid"])
            entry = self.schedule.class_entry(item, self.events.correction(item["uid"]))
            now = "пара отменена" if entry.cancelled else \
                f"пара — {span(entry.start.isoformat(), entry.end.isoformat())}"
            return f"✖️ Изменение пары {self._title(event)} {why}; {now}"
        current = self.events.by_uid(event["uid"])  # the version the withdrawn one had moved, back in force
        if current is not None:
            state = "отменено" if current["cancelled"] else span(current["start"], current["end"])
            return f"✖️ Изменение {self._title(event)} {why}; снова: {state}"
        return f"✖️ {self._title(event)} ({moment(event['start'])}) {why}"

    async def _post(self, event: dict, text: str) -> None:
        if link := self._source(event):
            text = f"{text}\n{link}"
        msg_id = await self.forum.post(TOPIC, text, buttons=self.buttons(event), html=True)
        remember(self.db, TOPIC, msg_id, "event", event["id"], self.now())

    # --- the buttons ------------------------------------------------------------------

    def buttons(self, event: dict) -> list[list] | None:
        current = self.events.current(event["id"])
        if current is None or current["gone_at"] is not None:
            return None
        event_id, hidden = current["id"], current["hidden"]
        if current["class_uid"] is not None:
            return [[Button.inline("Применить снова" if hidden else "Не применять",
                                   f"ev:{'show' if hidden else 'hide'}:{event_id}".encode())]]
        hide = Button.inline("Вернуть в календари" if hidden else "Скрыть",
                             f"ev:{'show' if hidden else 'hide'}:{event_id}".encode())
        if current["cancelled"]:
            return [[hide]]
        required = current["required"]
        return [[Button.inline(("✓ " if required else "") + "Основной", f"ev:main:{event_id}".encode()),
                 Button.inline(("" if required else "✓ ") + "Необязательный", f"ev:optional:{event_id}".encode())],
                [hide]]

    async def on_button(self, event) -> None:
        """A button under a post (Telethon's CallbackQuery); it acts on the newest version of the event."""
        if event.sender_id != self.owner_id:
            await event.answer("Нет доступа", alert=True)
            return
        action, event_id = event.pattern_match.group(1).decode(), int(event.pattern_match.group(2))
        if action in ("main", "optional"):
            row = self.events.set_required(event_id, action == "main")
        else:  # through the schedule: a correction of an interim assessment moves its Midterm
            row = await self.schedule.set_hidden(event_id, action == "hide")
        if row is None or row["gone_at"] is not None:
            await event.answer("Этого события больше нет", alert=True)
            await event.edit(buttons=None)
            return
        await event.answer(ANSWERS[action] if row["class_uid"] is None else CORRECTION_ANSWERS.get(action, ""))
        await event.edit(buttons=self.buttons(row))

    # --- from RUZ ---------------------------------------------------------------------

    async def publish_ruz(self, diff: Diff) -> None:
        """After a RUZ poll: what RUZ itself changed, in one message (more when it is long)."""
        if diff.first:
            what = "Ссылка РУЗ сменилась" if diff.relinked else "РУЗ подключён"
            await self.forum.post("system", f"{what}: в расписании {self.ruz.size()} пар.", html=True)
            return
        now = clock.local_iso(self.now())
        lines = [self._ruz_line(change) for change in diff.changes
                 if max(change.new["start"], change.old["start"] if change.old else "") >= now]
        if not lines:
            return
        for chunk in split("<b>РУЗ: изменения в расписании</b>", lines, MESSAGE_LIMIT):
            remember(self.db, TOPIC, await self.forum.post(TOPIC, chunk, html=True), "ruz", None, self.now())

    def _ruz_line(self, change: Change) -> str:
        new, old = change.new, change.old
        what = f"{html.escape(self.schedule.class_title(new))} ({kind_ru(new['kind'], of_class=True)})"
        match change.kind:
            case "new":
                return f"🆕 {what} — {span(new['start'], new['end'])}{place(new['room'])}"
            case "moved":
                return f"↪️ {what}: {moment(old['start'])} → {span(new['start'], new['end'])}{place(new['room'])}"
            case "room":
                return f"🚪 {what} {moment(new['start'])}: {html.escape(old['room'] or '—')} → " \
                       f"{html.escape(new['room'] or '—')}"
            case "cancelled":
                return f"✖️ {what} {moment(new['start'])} отменена"
            case "removed":
                return f"✖️ {what} {moment(old['start'])} убрана из расписания"
            case _:
                return f"↩️ {what} {span(new['start'], new['end'])} снова в расписании"
