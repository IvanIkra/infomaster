"""Deadline reminders (D47), as Web Push (D69).

Twice a day, Moscow time: on the eve (`reminder_eve`, 20:00 by default) the deadlines due tomorrow, in the morning
(`reminder_day`, 09:00) those due today, as the merged list has them (processing/merge.py: hidden and done ones are not
there). One notification holds all of a day's deadlines; nothing due, none. `deadline_reminders` off: none at all.

Each slot is sent once: kv "deadlines.reminded" keeps the day of the last one sent. The very first start takes the
slots that have come as sent, so a new install floods nothing; a slot missed while the service was down is sent when
it starts again, the same day.
"""

import asyncio
import datetime as dt
import logging
from collections.abc import Callable

from .. import clock
from ..processing.merge import DeadlineList
from ..processing.todo import Deadline
from ..settings import Settings
from ..store import Store

log = logging.getLogger(__name__)

DONE_KEY = "deadlines.reminded"  # slot name -> the Moscow day of the last reminder sent
SLOTS = (("day", "reminder_day", 0), ("eve", "reminder_eve", 1))  # name, its time setting, days ahead
SLOT_DELAY = 5  # seconds past the slot's minute


class DeadlineReminders:
    def __init__(self, store: Store, merged: DeadlineList, settings: Settings,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.merged, self.settings, self.now = store, merged, settings, now
        self._woken = asyncio.Event()
        self.notify: Callable[[str, list[Deadline]], None] | None = None  # the web app's notifier (§8.6)

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
            try:
                await self.tick()
            except Exception:  # a broken setting or store: the loop lives on to the next slot
                log.exception("the deadline reminders failed")
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

    # --- the reminder ---------------------------------------------------------------------------

    async def remind(self, name: str, due: dt.date) -> list[Deadline]:
        """The deadlines due that day, told to the notifier; none, nothing is told."""
        due_then = [d for d in self.merged.visible() if d.due == due]
        if due_then and self.notify is not None:
            self.notify(name, due_then)
        return due_then
