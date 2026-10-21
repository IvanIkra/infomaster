"""The «Grades» topic, «Оценки» in Russian (§6.7, §8.3): new and changed grades, one message per sheet after a poll.

A line is a work: its label, the grade on 0-10 (whole without a fraction, else one digit, as on Tablo's screen, §3.7),
the bonus, and in brackets the points as the sheet has them when its scale is not 0-10 and the grade before for a
changed one. The message is headed by the sheet's title, linked to the sheet, and the course's hashtag (#DM), like
the sheets of «Sheets». The words around the grades are in the forum's language (р. 41). Grades wait in grade_events
until posted: a failure of Telegram leaves them for the next poll, as long as they are within grades_window_days.
"""

import asyncio
import datetime as dt
import html
import logging
from collections.abc import Callable
from itertools import groupby

from .. import clock
from ..i18n import tr
from ..processing.grades import Grades
from ..store import Store
from .feed import MESSAGE_LIMIT, remember, split
from .forum import Forum
from .sheets import hashtag

log = logging.getLogger(__name__)

TOPIC = "grades"


def mark(value: float) -> str:
    """8.43 -> '8.4', 9.0 -> '9', 9.96 -> '10'."""
    rounded = round(value, 1)
    return str(int(rounded)) if rounded.is_integer() else f"{rounded:.1f}"


def points(value: float) -> str:
    """The sheet's own numbers: 5.9, 7, 2.5."""
    return f"{round(value, 2):g}"


def line(event: dict, lang: str) -> str:
    text = f"{html.escape(event['label'])} — <b>{mark(event['value'])}</b>"
    if event["bonus"]:
        text += f" {event['bonus']:+g}"
    notes = []
    if event["max"] != 10:
        notes.append(f"{points(event['raw'])} " + tr(lang, "of", "из") + f" {points(event['max'])}")
    if event["previous"] is not None:
        notes.append(tr(lang, "was ", "было ") + mark(event["previous"]))
    return text + (f" ({'; '.join(notes)})" if notes else "")


def header(event: dict, lang: str) -> str:
    name = html.escape(event["title"] or event["course"])
    tag = hashtag(event["course"])
    return f'📊 <b>{tr(lang, "Grades", "Оценки")} · <a href="{html.escape(event["url"])}">{name}</a></b>' \
        + (f" {tag}" if tag else "")


def chunks(events: list[dict], lang: str) -> list[list[dict]]:
    """The events of a sheet in as few messages as fit, each under the sheet's header."""
    result, room = [], 0
    for event in events:
        size = len(line(event, lang)) + 1
        if not result or size > room:
            result.append([])
            room = MESSAGE_LIMIT - len(header(event, lang))
        result[-1].append(event)
        room -= size
    return result


class GradeTopic:
    def __init__(self, forum: Forum, store: Store, grades: Grades, now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.db, self.grades, self.now = forum, store.db, grades, now
        self._lock = asyncio.Lock()

    async def publish(self) -> None:
        """GradeSheets.on_poll: the grades waiting, a message per sheet (more when it is long)."""
        async with self._lock:
            waiting = sorted(self.grades.waiting(), key=lambda e: (e["sheet_id"], e["id"]))
            for _, group in groupby(waiting, key=lambda e: e["sheet_id"]):
                events, lang = list(group), self.forum.lang
                try:
                    for chunk in chunks(events, lang):  # marked chunk by chunk: a failure halfway posts nothing twice
                        text = split(header(chunk[0], lang), [line(event, lang) for event in chunk], MESSAGE_LIMIT)[0]
                        msg_id = await self.forum.post(TOPIC, text, html=True)
                        remember(self.db, TOPIC, msg_id, "grades", None, self.now())
                        self.grades.posted([event["id"] for event in chunk], msg_id)
                except Exception:
                    log.exception("posting the new grades failed, the rest wait for the next poll")
