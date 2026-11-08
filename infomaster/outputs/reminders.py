"""Deadline reminders in the «Дедлайны» topic (D47).

Twice a day, Moscow time: on the eve (`reminder_eve`, 20:00 by default) the deadlines due tomorrow, in the morning
(`reminder_day`, 09:00) those due today. One message holds all of a day's deadlines, as the pinned list shows them
(processing/merge.py: hidden and done ones are not there), each with its own button: «Hide» hides it as the buttons
under the topic's other messages do (an own deadline, «Delete», is deleted), «Restore» brings it back. Nothing due:
no message. `deadline_reminders` off: no messages at all.

Each slot is sent once: kv "deadlines.reminded" keeps the day of the last one sent. The very first start takes the
slots that have come as sent, so a new install floods nothing; a slot missed while the service was down is sent when
it starts again, the same day.
"""

import asyncio
import datetime as dt
import html
import json
import logging
import re
from collections.abc import Callable

from telethon import Button

from .. import clock
from ..i18n import Phrase
from ..settings import Settings
from ..store import Store
from .deadlines import TOPIC, DeadlineTopic
from .feed import MESSAGE_LIMIT, remember, split
from .forum import Forum

log = logging.getLogger(__name__)

DONE_KEY = "deadlines.reminded"  # slot name -> the Moscow day of the last reminder sent
SLOTS = (("day", "reminder_day", 0), ("eve", "reminder_eve", 1))  # name, its time setting, days ahead
HEADERS = {"day": Phrase("⏰ <b>Due today</b>", "⏰ <b>Сегодня срок</b>"),
           "eve": Phrase("⏰ <b>Due tomorrow</b>", "⏰ <b>Завтра срок</b>")}
BUTTON = re.compile(rb"^dr:(hide|show):(\d+)$")  # the deadline's place in the message: bot_posts.key holds the refs
BUTTON_TITLE = 32
SLOT_DELAY = 5  # seconds past the slot's minute


class DeadlineReminders:
    def __init__(self, forum: Forum, store: Store, topic: DeadlineTopic, settings: Settings, owner_id: int,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.store, self.db, self.topic, self.settings = forum, store, store.db, topic, settings
        self.owner_id, self.now = owner_id, now
        self._woken = asyncio.Event()

    def reschedule(self) -> None:
        """A time changed: the loop counts its sleep anew."""
        self._woken.set()

    # --- the loop ------------------------------------------------------------------------------

    def start(self) -> None:
        """The slots that have come count as sent on the very first start: a new install floods nothing."""
        done = self.store.get_json(DONE_KEY) or {}
        for name, setting, _ in SLOTS:
            done.setdefault(name, self._slot(setting).date().isoformat())
        self.store.put_json(DONE_KEY, done)

    async def run(self) -> None:
        self.start()
        while True:
            self._woken.clear()
            await self.tick()
            try:
                await asyncio.wait_for(self._woken.wait(), self._seconds_to_next())
            except TimeoutError:
                pass

    async def tick(self) -> None:
        """Send every slot that has come and was not sent; a slot is sent at most once whatever happens."""
        done = self.store.get_json(DONE_KEY) or {}
        for name, setting, ahead in SLOTS:
            day = self._slot(setting).date()
            if day.isoformat() <= done.get(name, ""):
                continue
            done[name] = day.isoformat()
            self.store.put_json(DONE_KEY, done)  # first: a failing post is not sent twice
            if not self.settings.get("deadline_reminders"):
                continue
            try:
                await self.remind(name, day + dt.timedelta(days=ahead))
            except Exception:
                log.exception("the %s deadline reminder failed", name)

    def _slot(self, setting: str) -> dt.datetime:
        """The latest time of the setting that has come, Moscow local."""
        local = self.now().astimezone(clock.TZ).replace(tzinfo=None)
        hour, minute = map(int, self.settings.get(setting).split(":"))
        today = local.replace(hour=hour, minute=minute, second=0, microsecond=0)
        return today if local >= today else today - dt.timedelta(days=1)

    def _seconds_to_next(self) -> float:
        local = self.now().astimezone(clock.TZ).replace(tzinfo=None)
        return min((self._slot(setting) + dt.timedelta(days=1) - local).total_seconds()
                   for _, setting, _ in SLOTS) + SLOT_DELAY

    # --- the message ---------------------------------------------------------------------------

    async def remind(self, name: str, due: dt.date) -> int | None:
        """One message for the deadlines due that day; the message id, or None when nothing is due."""
        due_then = [d for d in self.topic.merged.visible() if d.due == due]
        if not due_then:
            return None
        lang = self.forum.lang
        lines = []
        for d in due_then:
            mark, where = self.topic.where(d)
            line = f"{mark} <b>{html.escape(d.title)}</b>"
            if d.name:
                line += f" · {html.escape(d.name)}"
            lines.append(f"{line} · {where}")
        refs = [d.ref for d in due_then]
        text = split(HEADERS[name].get(lang), lines, MESSAGE_LIMIT)[0]  # a day's deadlines fit one message
        msg_id = await self.forum.post(TOPIC, text, buttons=self._buttons(refs), html=True)
        remember(self.db, TOPIC, msg_id, "deadline_reminder", None, self.now(), key=json.dumps(refs))
        return msg_id

    def _buttons(self, refs: list[str]) -> list[list] | None:
        """A button for each deadline still on the list or hidden: «Hide» («Delete» for an own one) or «Restore»."""
        t, rows = self.forum.tr, []
        for n, ref in enumerate(refs):
            d = self.topic.merged.by_ref(ref)
            if d is None:
                continue
            title = d.title if len(d.title) <= BUTTON_TITLE else d.title[:BUTTON_TITLE - 1] + "…"
            if self.topic.merged.is_hidden(d):
                rows.append([Button.inline(f"{t('Restore', 'Вернуть')}: {title}", f"dr:show:{n}".encode())])
            else:
                action = t("Delete", "Удалить") if d.source == "ha" else t("Hide", "Скрыть")
                rows.append([Button.inline(f"{action}: {title}", f"dr:hide:{n}".encode())])
        return rows or None

    # --- the buttons ---------------------------------------------------------------------------

    async def on_button(self, event) -> None:
        """«Hide», «Delete» or «Restore» of one deadline under a reminder (Telethon's CallbackQuery)."""
        t = self.forum.tr
        if event.sender_id != self.owner_id:
            await event.answer(t("No access", "Нет доступа"), alert=True)
            return
        action, n = event.pattern_match.group(1).decode(), int(event.pattern_match.group(2))
        post = self.db.execute("select key from bot_posts where topic = ? and kind = 'deadline_reminder' "
                               "and msg_id = ?", (TOPIC, event.message_id)).fetchone()
        refs = json.loads(post["key"]) if post is not None and post["key"] else []
        d = self.topic.merged.by_ref(refs[n]) if n < len(refs) else None
        if d is None:
            await event.answer(t("This deadline is off the list", "Этого дедлайна больше нет в списке"), alert=True)
        elif action == "show":
            self.topic.merged.restore(d)
            await event.answer(t("Back on the list", "Снова в списке"))
        elif d.source == "ha":
            self.topic.merged.remove(d)
            await event.answer(t("Deleted", "Удалён"))
        elif self.topic.merged.is_hidden(d):
            await event.answer(t("Hidden already", "Уже скрыт"))
        else:
            self.topic.merged.remove(d)
            await event.answer(t("Hidden: gone from the list, Tablo and HA",
                                 "Скрыт: в списке, в Tablo и в HA его больше нет"))
        await event.edit(buttons=self._buttons(refs))

