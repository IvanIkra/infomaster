"""The «Timetable» topic, «Расписание» in Russian (§6.5, §8.3): what changes in the timetable, with a link to the
message that told it.

- From the chats and letters, one message each: a new event, its move or cancellation, a move or cancellation of a
  class, and their withdrawal when the message is deleted or edited into something else. Under an event of its own
  the buttons put it into the main or the optional calendar and hide it (a hidden event leaves the feeds); under a
  correction of a class they stop applying it, and the class is back as RUZ has it. Only the owner may press them.
- From RUZ, one message a poll: classes added inside the weeks the feed already covered, moved, changed rooms,
  cancelled, removed, back again. The first poll that brings classes is no news, nor is the first one after the link
  changed: the System topic only hears how many came.

Past things are left out. What the bot posted is kept in `bot_posts`. Texts and buttons are in the forum's language
(D41); the titles are the dictionary's, English.
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
from ..i18n import tr
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
ANSWERS = {"main": ("Now in the main calendar", "Теперь в основном календаре"),
           "optional": ("Now in the optional calendar", "Теперь в необязательном календаре"),
           "hide": ("Hidden: it is in no calendar", "Скрыто: в календарях его нет"),
           "show": ("Back in the calendars", "Снова в календарях")}
CORRECTION_ANSWERS = {"hide": ("The correction is off: the class is as RUZ has it",
                               "Правка не применяется: пара как в РУЗ"),
                      "show": ("The correction applies again", "Правка снова применяется")}


def moment(text: str, lang: str) -> str:
    """'2026-10-22T18:10:00' -> 'Thu 22.10 18:10'."""
    when = dt.datetime.fromisoformat(text)
    return f"{WEEKDAYS[lang][when.weekday()]} {when:%d.%m %H:%M}"


def span(start: str, end: str | None, lang: str) -> str:
    """'Thu 22.10 18:10–19:30'; the end is left out when it is unknown or on another day."""
    text = moment(start, lang)
    if end and end[:10] == start[:10] and end > start:
        text += f"–{dt.datetime.fromisoformat(end):%H:%M}"
    return text


def place(room: str | None, lang: str) -> str:
    if not room:
        return ""
    if re.match(r"https?://", room):
        return f' · <a href="{html.escape(room)}">{tr(lang, "link", "ссылка")}</a>'
    return f" · {tr(lang, 'online', 'онлайн')}" if room == "online" else f" · {html.escape(room)}"


def kind_name(kind: str, of_class: bool, lang: str) -> str:
    """The type in lower case, HTML-escaped: an unknown kind keeps the words the model or the feed gave."""
    name = type_of(kind, of_class)
    return html.escape(TYPES_RU.get(name, name.lower()) if lang == "ru" else name.lower())


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
        return source(self.inbox, event, self.forum.lang) if event["chat_id"] is not None else ""

    def _new_text(self, event: dict) -> str:
        lang = self.forum.lang
        lines = [f"🆕 {self._title(event)} — {kind_name(event['kind'], False, lang)}",
                 span(event["start"], event["end"], lang) + place(event["room"], lang)]
        if event["course"]:
            lines.append(html.escape(self.schedule.subjects.title(event["course"])))
        return "\n".join(lines)

    def _class_text(self, correction: dict) -> str:
        lang = self.forum.lang
        item = self.ruz.by_uid(correction["class_uid"])
        what = tr(lang, "Class", "Пара") + f" {self._title(correction)} ({kind_name(item['kind'], True, lang)}"
        if correction["cancelled"]:
            return f"✖️ {what}, {moment(item['start'], lang)}) " + tr(lang, "cancelled", "отменена")
        if moved_back(item, correction):
            back = span(item["start"], item["end"], lang) + place(item["room"], lang)
            return f"↩️ {what}) " + tr(lang, f"as in RUZ again: {back}", f"снова как в РУЗ: {back}")
        moved = f"{moment(item['start'], lang)} → {span(correction['start'], correction['end'], lang)}"
        return f"↪️ {what}) " + tr(lang, f"moved: {moved}", f"перенесена: {moved}") + place(correction["room"], lang)

    def _moved_text(self, old: dict, new: dict) -> str:
        lang = self.forum.lang
        where = place(new["room"], lang)
        if new["cancelled"] and not old["cancelled"]:
            return f"✖️ {self._title(new)} ({moment(old['start'], lang)}) " + tr(lang, "cancelled", "отменено")
        if old["cancelled"] and not new["cancelled"]:
            back = span(new["start"], new["end"], lang) + where
            return f"↩️ {self._title(new)} " + tr(lang, f"is on again: {back}", f"снова в силе: {back}")
        if (old["start"], old["end"]) == (new["start"], new["end"]) and old["room"] != new["room"]:
            elsewhere = tr(lang, "another place", "другое место")
            return f"🚪 {self._title(new)} ({moment(new['start'], lang)}): {elsewhere}{where}"
        moved = f"{moment(old['start'], lang)} → {span(new['start'], new['end'], lang)}{where}"
        return f"↪️ {self._title(new)} " + tr(lang, f"moved: {moved}", f"перенесено: {moved}")

    def _gone_text(self, event: dict) -> str:
        lang = self.forum.lang
        why = tr(lang, "withdrawn: the message was deleted or edited", "снято: сообщение удалено или исправлено")
        if event["class_uid"]:
            item = self.ruz.by_uid(event["class_uid"])
            entry = self.schedule.class_entry(item, self.events.correction(item["uid"]))
            if entry.cancelled:
                now = tr(lang, "the class is cancelled", "пара отменена")
            else:
                now = tr(lang, "the class is ", "пара — ") + span(entry.start.isoformat(), entry.end.isoformat(), lang)
            return "✖️ " + tr(lang, "Change of the class", "Изменение пары") + f" {self._title(event)} {why}; {now}"
        current = self.events.by_uid(event["uid"])  # the version the withdrawn one had moved, back in force
        if current is not None:
            state = tr(lang, "cancelled", "отменено") if current["cancelled"] else \
                span(current["start"], current["end"], lang)
            return "✖️ " + tr(lang, "Change of", "Изменение") + f" {self._title(event)} {why}; " \
                + tr(lang, f"again: {state}", f"снова: {state}")
        return f"✖️ {self._title(event)} ({moment(event['start'], lang)}) {why}"

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
        event_id, hidden, t = current["id"], current["hidden"], self.forum.tr
        if current["class_uid"] is not None:
            return [[Button.inline(t("Apply again", "Применить снова") if hidden else t("Don't apply", "Не применять"),
                                   f"ev:{'show' if hidden else 'hide'}:{event_id}".encode())]]
        hide = Button.inline(t("Back to the calendars", "Вернуть в календари") if hidden else t("Hide", "Скрыть"),
                             f"ev:{'show' if hidden else 'hide'}:{event_id}".encode())
        if current["cancelled"]:
            return [[hide]]
        required = current["required"]
        return [[Button.inline(("✓ " if required else "") + t("Main", "Основной"), f"ev:main:{event_id}".encode()),
                 Button.inline(("" if required else "✓ ") + t("Optional", "Необязательный"),
                               f"ev:optional:{event_id}".encode())],
                [hide]]

    async def on_button(self, event) -> None:
        """A button under a post (Telethon's CallbackQuery); it acts on the newest version of the event."""
        if event.sender_id != self.owner_id:
            await event.answer(self.forum.tr("No access", "Нет доступа"), alert=True)
            return
        action, event_id = event.pattern_match.group(1).decode(), int(event.pattern_match.group(2))
        if action in ("main", "optional"):
            row = self.events.set_required(event_id, action == "main")
        else:  # through the schedule: a correction of an interim assessment moves its Midterm
            row = await self.schedule.set_hidden(event_id, action == "hide")
        if row is None or row["gone_at"] is not None:
            await event.answer(self.forum.tr("This event is gone", "Этого события больше нет"), alert=True)
            await event.edit(buttons=None)
            return
        answer = ANSWERS[action] if row["class_uid"] is None else CORRECTION_ANSWERS.get(action, ("", ""))
        await event.answer(self.forum.tr(*answer))
        await event.edit(buttons=self.buttons(row))

    # --- from RUZ ---------------------------------------------------------------------

    async def publish_ruz(self, diff: Diff) -> None:
        """After a RUZ poll: what RUZ itself changed, in one message (more when it is long)."""
        if diff.first:
            classes = self.ruz.size()
            text = self.forum.tr(("The RUZ link changed" if diff.relinked else "RUZ is connected")
                                 + f": {classes} classes in the timetable.",
                                 ("Ссылка РУЗ сменилась" if diff.relinked else "РУЗ подключён")
                                 + f": в расписании {classes} пар.")
            await self.forum.post("system", text, html=True)
            return
        now = clock.local_iso(self.now())
        lines = [self._ruz_line(change) for change in diff.changes
                 if max(change.new["start"], change.old["start"] if change.old else "") >= now]
        if not lines:
            return
        header = self.forum.tr("<b>RUZ: changes of the timetable</b>", "<b>РУЗ: изменения в расписании</b>")
        for chunk in split(header, lines, MESSAGE_LIMIT):
            remember(self.db, TOPIC, await self.forum.post(TOPIC, chunk, html=True), "ruz", None, self.now())

    def _ruz_line(self, change: Change) -> str:
        new, old, lang = change.new, change.old, self.forum.lang
        what = f"{html.escape(self.schedule.class_title(new))} ({kind_name(new['kind'], True, lang)})"
        match change.kind:
            case "new":
                return f"🆕 {what} — {span(new['start'], new['end'], lang)}{place(new['room'], lang)}"
            case "moved":
                return f"↪️ {what}: {moment(old['start'], lang)} → {span(new['start'], new['end'], lang)}" \
                       f"{place(new['room'], lang)}"
            case "room":
                return f"🚪 {what} {moment(new['start'], lang)}: {html.escape(old['room'] or '—')} → " \
                       f"{html.escape(new['room'] or '—')}"
            case "cancelled":
                return f"✖️ {what} {moment(new['start'], lang)} " + tr(lang, "cancelled", "отменена")
            case "removed":
                return f"✖️ {what} {moment(old['start'], lang)} " + tr(lang, "removed from the timetable",
                                                                         "убрана из расписания")
            case _:
                return f"↩️ {what} {span(new['start'], new['end'], lang)} " + tr(lang, "back in the timetable",
                                                                                  "снова в расписании")
