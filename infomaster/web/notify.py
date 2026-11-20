"""The notifier (§8.6, stage 16.4): what the forum announces with a sound, as Web Push to the web app's devices.

It is called where the forum is, so the forum can be switched off later (step 16.7) and this stays:
- a new or moved deadline (the «Deadlines» topic's messages that notify; a withdrawn one goes quietly, D55);
- a reminder: the deadlines due tomorrow in the evening, today in the morning (D47);
- a failure of a source or a login it needs (`Alert`: the twin, HSE App X, mail, RUZ, the wiki, a grade sheet);
- the daily LLM limit spent (the 80 % warning goes quietly, as in the forum).

A message names its screen in the app: a deadline's card, the feed, «Machine». A deadline's messages share a tag, so
a move replaces the news of it on the device instead of adding another.

The owner chooses in «Machine → Notifications» (D65) which kinds come (`push_kinds`: deadlines, reminders, failures,
llm) and the quiet hours (`push_quiet`, Moscow time): a message of those hours comes without a sound or vibration.
"""

import asyncio
import datetime as dt
import logging
from collections.abc import Callable, Coroutine, Iterable

from .. import clock
from ..i18n import Alert, Phrase, say, tr
from ..llm.budget import Level, Notice
from ..outputs.feed import day
from ..processing.todo import Deadline, uid
from .push import Push

log = logging.getLogger(__name__)

BODY_CHARS = 400


def quiet(hours: str, moment: dt.datetime) -> bool:
    """Whether Moscow's time of `moment` falls in the quiet hours 'HH:MM-HH:MM' (over midnight when the end is
    earlier); never for ''."""
    if not hours:
        return False
    start, end = hours.split("-")
    now = moment.astimezone(clock.TZ).strftime("%H:%M")
    return start <= now < end if start < end else now >= start or now < end


class Notifier:
    def __init__(self, push: Push, lang: Callable[[], str], now: Callable[[], dt.datetime] = clock.now_utc):
        self.push, self.lang, self.now = push, lang, now
        self.settings = None  # the service's Settings: push_kinds and push_quiet; all kinds, no quiet hours without
        self._tasks: set[asyncio.Task] = set()

    def _spawn(self, coro: Coroutine) -> None:
        try:
            task = asyncio.get_running_loop().create_task(coro)
        except RuntimeError:  # no loop: a test or a script; nothing to send to
            coro.close()
            return
        self._tasks.add(task)
        task.add_done_callback(self._done)

    def _done(self, task: asyncio.Task) -> None:
        self._tasks.discard(task)
        if not task.cancelled() and task.exception() is not None:
            log.error("a push failed", exc_info=task.exception())

    def send(self, title: str, body: str, screen: str = "", tag: str | None = None, urgent: bool = False,
             kind: str | None = None) -> None:
        """One message to every device; `screen` is the app's route after #/ ('' for the feed). A message of a
        `kind` the owner turned off is not sent; in the quiet hours it comes silent."""
        if kind is not None and self.settings is not None and kind not in self.settings.get("push_kinds"):
            return
        body = body if len(body) <= BODY_CHARS else body[:BODY_CHARS].rstrip() + "…"
        message = {"title": title, "body": body, "url": "/web/#/" + screen}
        if tag:
            message["tag"] = tag
        if self.settings is not None and quiet(self.settings.get("push_quiet"), self.now()):
            message["silent"] = True
        self._spawn(self.push.send(message, urgency="high" if urgent else "normal"))

    # --- what the forum announces ---------------------------------------------------------------------------

    def deadline(self, d: Deadline, was: str | None = None) -> None:
        """New (`was` None) or moved from `was` (YYYY-MM-DD)."""
        lang = self.lang()
        when = day(d.due.isoformat(), lang)
        if d.assumed:
            when += " ≈ " + tr(lang, "assumed", "предположительно")
        if was is None:
            title = tr(lang, f"New deadline: {d.title}", f"Новый срок: {d.title}")
            body = " · ".join(filter(None, [when, d.name]))
        else:
            title = tr(lang, f"Deadline moved: {d.title}", f"Срок перенесён: {d.title}")
            body = f"{day(was, lang)} → {when}" + (f" · {d.name}" if d.name else "")
        item_uid = uid(d)
        self.send(title, body, f"d?uid={item_uid}", tag=f"deadline-{item_uid}", kind="deadlines")

    def reminder(self, name: str, deadlines: Iterable[Deadline]) -> None:
        """`name`: day (due today, in the morning) or eve (due tomorrow, in the evening)."""
        deadlines = list(deadlines)
        if not deadlines:
            return
        lang = self.lang()
        title = tr(lang, "Due today", "Сегодня срок") if name == "day" else tr(lang, "Due tomorrow", "Завтра срок")
        body = "\n".join(d.title + (f" · {d.name}" if d.name else "") for d in deadlines)
        self.send(title, body, tag=f"reminder-{name}-{deadlines[0].due.isoformat()}", urgent=True, kind="reminders")

    def system(self, text: str | Phrase, login: str | None = None) -> None:
        """A line of the System topic: it is pushed when the forum notifies of it, a failure or a login needed."""
        if not isinstance(text, Alert):
            return
        self.send("infomaster", say(text, self.lang()), "machine", tag=f"login-{login}" if login else None,
                  urgent=True, kind="failures")

    def budget(self, notice: Notice) -> None:
        if notice.level is not Level.HARD:
            return
        lang = self.lang()
        self.send(tr(lang, "The LLM limit is spent", "Лимит LLM исчерпан"),
                  tr(lang, f"${notice.spent:.2f} of ${notice.limit:.2f} today. Calls wait until midnight Moscow time, "
                           "messages queue up.",
                     f"${notice.spent:.2f} из ${notice.limit:.2f} за день. Вызовы стоят до полуночи по Москве, "
                     "сообщения копятся в очереди."), "machine/llm", tag="budget", kind="llm")
