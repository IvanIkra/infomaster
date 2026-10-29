"""Summaries of chats (§6.6, D17, D20): what was said in a chat over a period, as points, in English or Russian
(D41): the daily one in the forum's language, one asked for with /summary in the language it names, if any.

They are made from the raw messages (`messages`) and never wait for a batch. A period is either asked for (`3h`, `2d`,
`20.10`, `20.10-22.10`; at most 7 days) or runs since the chat's last summary: the messages no summary has taken yet
(`summarized_at`), of the last 7 days at most. Only a summary of the second kind marks its messages, so the daily one
goes on from where the last one ended, and a look back at a period asked for changes nothing.

What goes to the model is what the batches would give it (§10): readable messages, and in a chat with the `important`
filter only the important ones and the replies to them. An image the batches have read (processing/images.py) comes
with what was read on it; a summary never reads an image itself. A period that fits one call is summarized at once; a
longer one is first put into a digest per day (a day too long for one call takes several), and the summary is made
from the digests. Each point links to the message that shows it best.
"""

import datetime as dt
import re
from collections.abc import Callable
from dataclasses import dataclass, field

from .. import clock
from ..collectors.files import Files
from ..collectors.inbox import Inbox
from ..i18n import Phrase
from ..llm.client import Haiku
from ..settings import ChatSettings
from .batches import IMAGE_LIMIT, KINDS, image_size, important, readable, replies_to_important

MAX_DAYS = 7              # D17
MAX_TOKENS = 4_000
CALL_CHARS = 30_000       # of messages in one call: more is split into days
TEXT_LIMIT = 1_500        # characters of one message in the prompt
QUOTE_LIMIT = 200         # characters of a replied message that is not in the prompt
MESSAGE_OVERHEAD = 80     # characters of a message's head line, for splitting
MAX_POINTS = 12
POINT_CHARS = 500         # of one point: a line of the forum's message, which holds 4096
DAY_POINTS = 15


class PeriodError(ValueError):
    """Looks like a period, but is not a usable one; the text (a Phrase) is for the owner."""


@dataclass(frozen=True)
class Period:
    """A period asked for: [start, end), aware. `label` is it as the button data keeps it."""
    start: dt.datetime
    end: dt.datetime
    label: str


@dataclass
class Collected:
    """The messages of one summary: `seen` are all of the period (marked after a summary since the last one),
    `chosen` those the model reads."""
    chat: dict
    start: dt.datetime          # Moscow local, naive
    end: dt.datetime
    seen: list[dict]
    chosen: list[dict]
    fresh: bool                 # since the last summary: its messages get marked
    dates: bool = False         # the period asked was whole days: its span is told by dates
    points: list[dict] = field(default_factory=list)  # {text, msg_id | None}


# --- the period ----------------------------------------------------------------------

UNITS = {"h": "h", "ч": "h", "d": "d", "д": "d"}
DAY = r"(\d{1,2})\.(\d{1,2})"


def parse_period(text: str, now: dt.datetime) -> Period | None:
    """`3h`/`3ч`, `2d`/`2д`, `20.10`, `20.10-22.10`; None when the text is no period. A date later than today is of
    last year."""
    text = text.strip().lower()
    if match := re.fullmatch(r"(\d{1,3})\s*([hчdд])", text):
        number, unit = int(match[1]), UNITS[match[2]]
        hours = number * (24 if unit == "d" else 1)
        if not 1 <= hours <= MAX_DAYS * 24:
            raise PeriodError(Phrase(f"A period is 1 hour to {MAX_DAYS} days.",
                                     f"Промежуток — от 1 часа до {MAX_DAYS} дней."))
        return Period(now - dt.timedelta(hours=hours), now, f"{number}{unit}")
    if match := re.fullmatch(DAY + r"(?:\s*[-–—]\s*" + DAY + r")?", text):
        today = clock.local_day(now)
        first = _date(int(match[1]), int(match[2]), today)
        last = _date(int(match[3]), int(match[4]), today) if match[3] else first
        if last < first:
            raise PeriodError(Phrase("The period ends before it starts.", "Конец промежутка раньше начала."))
        if (last - first).days >= MAX_DAYS:
            raise PeriodError(Phrase(f"A period is {MAX_DAYS} days at most.",
                                     f"Промежуток — не больше {MAX_DAYS} дней."))
        label = f"{first:%d.%m}" + (f"-{last:%d.%m}" if last != first else "")
        return Period(clock.day_start_utc(first), clock.day_start_utc(last + dt.timedelta(days=1)), label)
    return None


def _date(day: int, month: int, today: dt.date) -> dt.date:
    try:
        date = dt.date(today.year, month, day)
    except ValueError:
        raise PeriodError(Phrase(f"No such date: {day:02d}.{month:02d}.", f"Нет такой даты: {day:02d}.{month:02d}.")) \
            from None
    return date.replace(year=date.year - 1) if date > today else date


def span(collected: Collected) -> str:
    """The period of a summary for its header: '21.10, 18:05–21:00', '20.10 14:05 – 22.10 21:00', '20.10–22.10'."""
    start, end = collected.start, collected.end
    if collected.dates:
        last = (end - dt.timedelta(days=1)).date()
        return f"{start:%d.%m}" + (f"–{last:%d.%m}" if last != start.date() else "")
    if start.date() == end.date():
        return f"{start:%d.%m}, {start:%H:%M}–{end:%H:%M}"
    return f"{start:%d.%m %H:%M} – {end:%d.%m %H:%M}"


# --- the prompt ----------------------------------------------------------------------

SYSTEM = f"""\
You summarize a Telegram chat or channel for a university student (HSE University, Moscow, first year, Software \
Engineering) who has not read it. The summary is read in Telegram on a phone.

Input: the chat's name, whether it is a study chat, the period and its messages, mostly in Russian. Each message has \
its id, the sender, the time and its context: a reply, a forward, files, a pin. "listed person" marks a sender the \
student follows (the group head, a teacher): such messages are usually official. An image comes as another model \
read it: what it shows and the text on it.

Output: points, the most important first. One point is one topic of the chat: an announcement, a decision, a question \
and the answer that settled it, a discussion and what came of it, a file posted, a plan. The messages of one topic \
make one point. Small talk, jokes, stickers, thanks and greetings get no point; a chat where nothing of substance \
happened gets no points at all.

Every point:
- text: 1-2 sentences in {{language}}, plain and specific: dates, times, rooms, courses and tasks as the messages \
give them; who said it when it matters (a teacher, the group head). Relative dates ("завтра", "в пятницу") become \
dates counted from the message time: "{{dates}}". No emoji, no links, no hashtags, no Markdown.
- msg_id: the message that shows the point best; the point links to it.

At most {MAX_POINTS} points unless the task says otherwise: leave out the least important rather than squeeze several \
topics into one point. Never add what the messages do not tell; an open question stays a question.
"""

# The words of the prompt in each language a summary may be written in.
PROMPT_WORDS = {"en": {"language": "English", "dates": "on Friday, 23.10"},
                "ru": {"language": "Russian", "dates": "в пятницу, 23.10"}}

PERIOD_TASK = "Task: the summary of the period."
DAY_TASK = (f"Task: the digest of one day of a longer period. Your points will be merged with the other days' into "
            f"one summary, so keep every fact that may matter there, up to {DAY_POINTS} points.")
MERGE_TASK = ("Task: the summary of the whole period from the digests of its days below, each point with its message "
              "id. A later fact overrules an earlier one (a move after an announcement): tell the outcome. msg_id: "
              "one of the ids given with the digests' points.")

SCHEMA = {
    "type": "object",
    "properties": {"points": {"type": "array", "items": {
        "type": "object",
        "properties": {"text": {"type": "string"}, "msg_id": {"type": "integer"}},
        "required": ["text", "msg_id"],
        "additionalProperties": False,
    }}},
    "required": ["points"],
    "additionalProperties": False,
}


def _moment(value: str) -> str:
    return f"{dt.datetime.fromisoformat(value):%a %Y-%m-%d %H:%M}"


def render(message: dict, shown: set[int], people: set[int], quote: str | None) -> list[str]:
    """A message as the model sees it; `shown`: the ids in the prompt, `quote`: the replied message's text when it is
    not among them."""
    head = [str(message["msg_id"]), message["sender_name"] or "unknown sender"]
    if message["sender_id"] in people:
        head.append("listed person")
    head.append(_moment(message["at"]))
    if message["pinned"]:
        head.append("pinned")
    if message["forward"]:
        head.append(f"forwarded from {message['forward'].get('from') or 'unknown'}")
    if message["reply_to"]:
        head.append(f"reply to {message['reply_to']}" if message["reply_to"] in shown else "reply")
    if message["files"]:
        head.append("files: " + ", ".join(f.get("name") or f["kind"] for f in message["files"]))
    lines = ["- " + " · ".join(head)]
    lines += [f"  {line}" for line in message["text"][:TEXT_LIMIT].splitlines() if line.strip()] or ["  (no text)"]
    if image := message.get("image"):
        lines.append(f"  Image, as another model read it: {image['shows'] or 'no description'}")
        lines += [f"  | {line}" for line in image["text"][:IMAGE_LIMIT].splitlines()]
    if quote:
        lines.append(f"  > replied message: «{quote}»")
    return lines


def size(message: dict) -> int:
    return min(len(message["text"]), TEXT_LIMIT) + image_size(message) + MESSAGE_OVERHEAD


def split(messages: list[dict], limit: int) -> list[list[dict]]:
    parts, part, chars = [], [], 0
    for message in messages:
        if part and chars + size(message) > limit:
            parts.append(part)
            part, chars = [], 0
        part.append(message)
        chars += size(message)
    return [*parts, part] if part else parts


def check(answer: dict, msg_ids: set[int], limit: int) -> list[dict]:
    """Points with text; a msg_id that is none of the messages' links nowhere."""
    points = []
    for raw in answer.get("points") or []:
        text = " ".join(str(raw.get("text") or "").split())
        if len(text) > POINT_CHARS:
            text = text[:POINT_CHARS - 1].rstrip() + "…"
        if text:
            msg_id = raw.get("msg_id")
            points.append({"text": text, "msg_id": msg_id if msg_id in msg_ids else None})
    return points[:limit]


# --- the service ---------------------------------------------------------------------

class Summaries:
    def __init__(self, chat_settings: ChatSettings, inbox: Inbox, llm: Haiku,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.chat_settings, self.inbox, self.llm, self.now = chat_settings, inbox, llm, now
        self.files: Files | None = None  # the readings of images; without it images are files by name

    def daily_chats(self) -> list[int]:
        """Enabled chats with `summary` on: study chats first, then by title."""
        ids = self.chat_settings.flagged_ids("summary") & self.chat_settings.enabled_ids()
        chats = [chat for chat_id in ids if (chat := self.inbox.chat(chat_id)) is not None]
        chats.sort(key=lambda chat: (not self.chat_settings.get(chat["id"], "study"), chat["title"].lower()))
        return [chat["id"] for chat in chats]

    def collect(self, chat_id: int, period: Period | None) -> Collected | None:
        """The messages of a summary; None for a chat that is not stored. Without `period`: since the last summary."""
        chat = self.inbox.chat(chat_id)
        if chat is None:
            return None
        now = self.now()
        start, end = (period.start, period.end) if period else (now - dt.timedelta(days=MAX_DAYS), now)
        seen = self.inbox.for_summary(chat_id, start, end, fresh=period is None)
        settings = self.chat_settings.all(chat_id)
        people = set(settings["people"])
        by_id = {m["msg_id"]: m for m in seen}
        readings = self.files.readings(chat_id, list(by_id)) if self.files is not None else {}
        for message in seen:
            message["image"] = readings.get(message["msg_id"])
        chosen = [m for m in seen if (readable(m) or m["image"]) and (
            settings["filter"] != "important" or important(m, people)
            or replies_to_important(self.inbox, chat_id, m, by_id, people))]
        local_end = end.astimezone(clock.TZ).replace(tzinfo=None)
        if period is None:  # since the first message no summary has taken
            local_start = dt.datetime.fromisoformat(seen[0]["at"]) if seen else local_end
        else:
            local_start = start.astimezone(clock.TZ).replace(tzinfo=None)
        dates = period is not None and local_start.time() == local_end.time() == dt.time()
        return Collected({**chat, "study": settings["study"], "people": people}, local_start, local_end, seen,
                         chosen, fresh=period is None, dates=dates)

    async def summarize(self, collected: Collected, *, asked: bool, lang: str) -> list[dict]:
        """The points of a summary in `lang`, also kept in `collected`. `asked`: by the owner (/summary), which runs
        above 80 % of the LLM limit, unlike the daily one. Raises BudgetExceeded and LLMError (BadAnswer when the
        answer itself is unusable)."""
        messages, system = collected.chosen, SYSTEM.format(**PROMPT_WORDS[lang])
        if not messages:
            collected.points = []
        elif sum(map(size, messages)) <= CALL_CHARS:
            collected.points = await self._ask(collected, system, PERIOD_TASK, messages, MAX_POINTS, asked)
        else:
            days: dict[str, list[dict]] = {}
            for message in messages:
                days.setdefault(message["at"][:10], []).append(message)
            digests = []
            for day, day_messages in days.items():
                points = []
                for part in split(day_messages, CALL_CHARS):
                    points += await self._ask(collected, system, DAY_TASK, part, DAY_POINTS, asked)
                digests.append((day, points))
            collected.points = await self._merge(collected, system, digests, asked)
        return collected.points

    async def _ask(self, collected: Collected, system: str, task: str, messages: list[dict], limit: int,
                   asked: bool) -> list[dict]:
        lines = self._header(collected, task)
        lines += ["", "Messages:"]
        shown = {m["msg_id"] for m in messages}
        chat_id = collected.chat["id"]
        for message in messages:
            quote = None
            if message["reply_to"] and message["reply_to"] not in shown:
                replied = self.inbox.message(chat_id, message["reply_to"])
                quote = " ".join(((replied or {}).get("text") or message["reply_quote"] or "").split())[:QUOTE_LIMIT]
            lines += render(message, shown, collected.chat["people"], quote)
        answer = await self.llm.json("summary", system, "\n".join(lines), SCHEMA, max_tokens=MAX_TOKENS,
                                     chat_id=chat_id, asked=asked)
        return check(answer, shown, limit)

    async def _merge(self, collected: Collected, system: str, digests: list[tuple[str, list[dict]]],
                     asked: bool) -> list[dict]:
        lines = [*self._header(collected, MERGE_TASK), "", "Digests of the days:"]
        ids = set()
        for day, points in digests:
            lines.append(f"{dt.date.fromisoformat(day):%a %Y-%m-%d}:")
            lines += [f"- {point['msg_id'] or 0}: {point['text']}" for point in points] or ["(nothing of substance)"]
            ids |= {point["msg_id"] for point in points if point["msg_id"]}
        answer = await self.llm.json("summary", system, "\n".join(lines), SCHEMA, max_tokens=MAX_TOKENS,
                                     chat_id=collected.chat["id"], asked=asked)
        return check(answer, ids, MAX_POINTS)

    def _header(self, collected: Collected, task: str) -> list[str]:
        chat = collected.chat
        return [f"Today: {self.now().astimezone(clock.TZ):%a %Y-%m-%d %H:%M}",
                f"Chat: \"{chat['title']}\" ({KINDS.get(chat['kind'], chat['kind'])}, "
                f"{'study' if chat['study'] else 'not a study chat'})",
                f"Period: {collected.start:%a %Y-%m-%d %H:%M} — {collected.end:%a %Y-%m-%d %H:%M}",
                task]

    def mark(self, collected: Collected) -> None:
        """A summary since the last one is done: its messages are taken (the daily one goes on after them)."""
        if collected.fresh:
            self.inbox.mark_summarized(collected.chat["id"], [m["msg_id"] for m in collected.seen], self.now())
