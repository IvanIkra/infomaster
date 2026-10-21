"""Batches (§6.1, р. 19, р. 30): new messages of a chat go to Haiku together, news, deadlines and events come back.

When:
- on schedule: every enabled chat is parsed at the hours of the day that divide by its N (`batch_hours` of the chat,
  else the general one), 0, 3, 6… for N = 3. A slot missed while the service was down or the limit was spent is made
  up at the next hourly tick;
- soon, if the chat is "urgent" and a pinned message, a PDF or a message of a listed person arrives. Messages often
  come in bursts (an album, an announcement in three messages, its correction), so the batch waits until the chat
  has been quiet for `URGENT_QUIET`: every new message of the chat, urgent or not, stretches the wait, but never
  beyond `URGENT_MAX_WAIT` after the trigger.

What goes to the model: messages of the chat that wait for a batch (`Inbox.pending`), except non-study ones older than
`news_ttl_hours` (their news would not be live anyway), messages with nothing to read (a sticker alone, a photo alone
whose image is not read) and, in a chat with the `important` filter, messages that are not important. Those are marked
parsed without a call. Study messages are never skipped for age: they carry deadlines.

Each message is seen once: `parsed_at` is set when the answer is saved. A failed call leaves the messages in the queue.
A message edited after its batch is parsed again, and what it gives replaces what its earlier text gave; a deleted
message withdraws what it gave. Study chats go first, then the mailbox, then the others (р. 30).

Letters of the university mailbox (collectors/mail.py, §5.2) are parsed the same way, with the same prompt and
schema: the mailbox is one more chat (`MAILBOX`), study (every letter is, §6.2), without a filter or urgency, on the
general schedule while mail is on or letters wait. The prompt says it is the student's mailbox and gives each letter's
subject; a letter may be twice as long as a chat message (8000 characters, as Tablo gave them). What letters give has
the source "mail", and its events go to the feeds with X-UNI-SOURCE mail.

Events (§6.5) are consultations, talks, meetings: things to attend that are not in the timetable. The model sees the
upcoming chat events (e...) and, in a study chat, the RUZ classes of the next weeks (c...), so it can point a move or
a cancellation at one of them: a chat event gets a new version with the same UID, a class gets a correction (§6.5,
р. 28). A class counts by the later of its RUZ time and its correction's, so a moved class stays listed until the
move is past. The model also sees the course dictionary (§6.1, §6.2), so it gives the courses as the dictionary names
them and the dictionary's abbreviations in labels and titles. Course names of the saved deadlines and events go to
the dictionary; only names it does not know yet cost a call. After every run the Midterms follow the corrections of
their classes (§4.4).

Sheets (§6.3, р. 14): a PDF of a chat message is downloaded when its batch comes (collectors/files.py), and the model
sees its first page: the text, or for a scan the page itself as a document. The model says which PDFs are sheets, with
their course, number and due date; they become rows of processing/sheets.py in the same transaction, and the forum
gets them after the run.

Images (р. 12): in a chat with «Картинки» on, the image of a message is downloaded when its batch comes and read by
Haiku in a call of its own (processing/images.py), within the daily number of images and while optional tasks run. The
model of the batch sees what was read on it: what it shows and its text.
"""

import asyncio
import datetime as dt
import logging
import re
import time
from collections.abc import Awaitable, Callable, Iterable
from dataclasses import dataclass, field

from .. import clock
from ..collectors.files import Files, NotNow, image_file
from ..collectors.inbox import MAILBOX, Inbox, message_link
from ..llm.budget import BudgetExceeded
from ..llm.client import BadAnswer, Document, Haiku, LLMError
from ..settings import CHAT_SPECS, ChatSettings, Settings
from ..store import Store
from . import pdf
from .deadlines import Deadlines, same_label
from .events import event_uid
from .images import Images
from .news import News
from .schedule import Schedule, local, moved_back
from .sheets import Sheets
from .subjects import TITLE_CHARS

log = logging.getLogger(__name__)

MAX_TOKENS = 16_000
CHUNK_MESSAGES = 40       # an item costs up to ≈250 output tokens, and most chat messages give none
CHUNK_CHARS = 24_000      # of message text in one call
TEXT_LIMIT = 4_000        # characters of one message in the prompt
MAIL_TEXT_LIMIT = 8_000   # of one letter: as much as Tablo gave Haiku (§6.2)
QUOTE_LIMIT = 300         # characters of a replied message that is not in the batch
FIRST_PAGE_LIMIT = 2_000  # characters of a PDF's first page in the prompt
IMAGE_LIMIT = 2_500       # characters of what was read on an image, in the prompt
URGENT_QUIET = 60         # seconds of silence in the chat that end an urgent burst
URGENT_MAX_WAIT = 300     # seconds from the trigger at most, however long the chat keeps writing
TICK_DELAY = 5            # seconds past the hour
RETRACT_WINDOW = dt.timedelta(days=15)  # raw messages live 14 days: older items cannot be checked for deletion

LINE = 28                 # characters in a line of Tablo's news column (§2.1)
FULL_CHARS = 2 * LINE
LABEL_CHARS = 15
EVENT_TITLE_CHARS = TITLE_CHARS  # an event stands in Tablo's Classes column like a subject (§2.1)
READABLE_FILES = {"pdf", "document"}           # a file without text still tells something by its name
IMPORTANT_FILES = {"pdf", "document", "photo", "video"}
URGENT_FILES = {"pdf"}

# Dates, times and words of time in Russian: the `important` filter keeps messages that have them.
DATE = re.compile(
    r"\b\d{1,2}[./]\d{1,2}\b|\b\d{1,2}:\d{2}\b"
    r"|\b\d{1,2}\s+(?:янв|фев|мар|апр|ма[йя]|июн|июл|авг|сен|окт|ноя|дек)"
    r"|\b(?:сегодня|завтра|послезавтра|понедельник|вторник|сред[уаые]|четверг|пятниц|суббот|воскресень|дедлайн|дд\b)",
    re.IGNORECASE)

KINDS = {"user": "private chat", "bot": "bot", "group": "group", "supergroup": "group",
         "forum": "group with topics", "channel": "channel"}

# The mailbox as a chat of the batches: every letter is study, nothing filtered, nothing urgent (§6.2).
MAILBOX_CHAT = {"id": MAILBOX, "title": "University mailbox", "kind": "mail", "username": None, "source": "mail"}
MAILBOX_SETTINGS = {**{name: default for name, (default, _) in CHAT_SPECS.items()}, "enabled": True, "study": True}
MAILBOX_HEADER = ("Chat: the student's university mailbox (@edu.hse.ru, study): letters of teachers, the study office, "
                  "university services and group mailing lists, each with its sender and subject; every letter counts "
                  "as study")


@dataclass
class Changes:
    """What one run gave, for the forum."""
    news: list[int] = field(default_factory=list)              # new items
    deadlines: list[int] = field(default_factory=list)         # new deadlines, Midterms too
    moved: list[tuple[int, int]] = field(default_factory=list)  # (old, new)
    gone: list[int] = field(default_factory=list)              # deadlines withdrawn with their messages or classes
    events: list[int] = field(default_factory=list)            # new events and corrections of classes
    events_moved: list[tuple[int, int]] = field(default_factory=list)  # (old, new): moved, cancelled, edited
    events_gone: list[int] = field(default_factory=list)       # withdrawn with their messages
    sheets: list[int] = field(default_factory=list)            # new sheets, waiting for the forum


@dataclass
class Burst:
    """An urgent chat waiting for its batch; monotonic seconds."""
    first: float                # the trigger
    last: float                 # the newest message since

    def ready_at(self) -> float:
        return min(self.last + URGENT_QUIET, self.first + URGENT_MAX_WAIT)


@dataclass
class Context:
    """What the model is shown besides the messages; counted anew before every call."""
    news: list[dict]
    deadlines: list[dict]
    events: list[dict] = field(default_factory=list)
    classes: list[dict] = field(default_factory=list)  # a study chat only: others do not move classes
    courses: dict[str, str] = field(default_factory=dict)  # the dictionary: course name -> abbr


# --- the prompt ---------------------------------------------------------------------

SYSTEM = """\
You read the Telegram chats and channels of a university student (HSE University, Moscow, first year, Software \
Engineering) and pick out what matters. Your news items go to the news column of the student's e-ink desk dashboard \
and to a news list in Telegram; both are read in passing.

Input: new messages of one chat, mostly in Russian, with the chat's name and whether it is a study chat. Each message \
has its id, the sender, the time and its context: a reply, a forward, an album, files, a pin. "listed person" marks \
a sender the student follows (the group head, a teacher): such messages and pinned ones are usually official.
Also given: the course dictionary (course names and their abbreviations), the current news items (ids n...), the \
upcoming deadlines (ids d...) and events (ids e...) already known from all chats, and for a study chat the classes of \
the timetable in the next weeks (ids c...).

A PDF of a message comes with its name and the text of its first page; a scanned one has its first page attached \
as a document instead. An image of a message (a photo, a screenshot) comes as another model read it: what it shows \
and the text on it; treat that text as part of the message.

Output: items. One item is one piece of news: one message, or several that belong together (an album, a question \
and the answer that settles it, an announcement and its correction). Small talk, thanks, jokes, stickers, open \
questions and anything with no fact of its own get no item at all: most messages of a group chat get none.
Every item:
- msg_ids: ids of its messages, the one that tells the news first;
- news: full (at most 2 lines of the column), short (1 line), importance 1-5, replaces;
- deadlines: concrete study tasks with a date, or [];
- events: things to attend at a set time, or [];
- sheets: PDFs of its messages that are sheets, or [].

Style of full and short:
- Telegraphic English, no articles, digits instead of words, the essential fact first.
- Abbreviations: HW, DM, Calc, Alg, Prog, SE, Eng, Hist, KR (контрольная), NIS (научно-исследовательский семинар), \
Course survey (СОП), Mon...Sun, Oct 21, 18:10. Symbols: → · &.
- Never name the chat or the sender: the source is shown separately. No emoji, no links, no hashtags.
- Stay within the character budgets: shorten words rather than cut them.

Importance:
5 - the student must act soon: a deadline, a test, a sign-up or choice with a date, a cancelled or moved class or exam.
4 - other study information (grading formula, results, rooms, materials posted). From city news only what changes \
the student's plans in Moscow today: metro or MCD stations closed, mobile internet off, weather that stops transport.
3 - notable news: Moscow in general, blocked services and apps, other cities.
2 - background: drone and air defence reports, airport delays, fog, minor news, weather small talk.
1 - ads and promos, memes, posts with no content of their own. Still write full and short.
A question or reply of fellow students that adds no new fact is 1-2 and gives no deadlines. Ask: does this change \
what the student does today? If not, it is not 4.

Replacing: when a message repeats or extends a current news item about the same event, set replaces to that item's \
id (n12); for an earlier item of this batch use "m" and its first message id (m1234). The old item disappears, so the \
new full and short must tell the whole story: the newest fact first, the main fact of the old item kept, written \
anew as one phrase, never the old and new texts glued together. Recurring reports on one topic (drones overnight, \
this week) are one running story. Different events on a similar topic are not replaced. Otherwise replaces is "".
A message marked "edited after it was read" was read before: write its item anew from its current text; the service \
replaces its earlier news itself.

Deadlines:
- Only work with a date: homework, tests (KR), projects, sign-ups, choices, forms. Not lectures, not announcements.
- course: the course name exactly as in the course dictionary when the course is there; otherwise its name in Russian \
as the chat or the message gives it, in full if you know it (матан → Математический анализ); "" if the task belongs \
to no course.
- label: "<abbr> <work>" with the dictionary abbreviation ("Calc KR-1", "DM HW 3"); only for a course that is not in \
the dictionary a short English abbreviation of up to 6 letters; without a course just the work ("NIS choice", \
"Course survey"). Within the label budget.
- due: YYYY-MM-DD. Relative dates ("в пятницу", "завтра") count from the message time. Several slots: the earliest.
- A deadline already in the upcoming list with the same date: leave it out.
- The message moves a listed deadline to another date: give the new date and set moves to the listed id ("d12"); \
otherwise moves is "".

Events: something to attend at a set time that is not a regular class of the timetable: a consultation, an extra or \
make-up class, a test or an exam with its time, a talk, a meeting, a fair, an open day.
- course: as for deadlines, or "".
- title: short English within the event title budget, a course named by its abbreviation as in deadline labels: \
"Alg consult", "Calc KR-1", "Career fair".
- start, end: YYYY-MM-DDTHH:MM, Moscow time; end "" when the message does not tell it. Relative dates count from the \
message time.
- room: the room ("R405"), a meeting link, or "".
- kind: one English word in lower case: consultation, exam, test, class, talk, meeting, event.
- required: true when the student has to be there (a consultation before an exam, a test, a make-up class, a \
mandatory meeting); false when it is up to the student (an optional consultation, a talk, a fair, a club).
- moves: the message moves a listed event (e12) or a listed class (c34) to another time or room: its id, with the new \
start, end and room (the title of the listed one is kept); otherwise "".
- cancels: the message cancels a listed event or class: its id, with title and start of the listed one; otherwise "".
- A listed event with the same title and time: leave it out. A regular class is not an event: only its move or \
cancellation is, and only when the message makes clear which class it is.

Sheets: a sheet (листок) is a PDF with tasks for the students to do: homework (ДЗ, ИДЗ), a problem set, a seminar \
sheet, a lab or project assignment, a test or exam variant. Not lecture notes or slides, a textbook, a timetable, a \
grading formula, an order, a form or a list of students. Judge by the file name, the first page and the message.
- msg_id: the message with the PDF;
- course: as for deadlines, or "";
- number: the sheet's number as the sheet or the message gives it ("3", "2.1"), "" if none;
- due: YYYY-MM-DD when the message or the first page tells when to hand it in, else "". Its deadline also goes to \
deadlines as usual.
A message with a sheet usually also gets news ("Calc sheet 3 posted, due Oct 29").

Examples:
(study, posted Sat 2026-10-17 18:50) "1. Матанализ • КР-1 22.10 7 пара/23.10 1 пара. 2. Алгебра • Кр-1 7.11 18:10. \
• ИДЗ-1 (ДД 07.11. 18:10) 3. СОП https://www.hse.ru/evaluation/ (дд 25.10 23:59)"
-> full "Calc KR-1 Oct 22/23 · Alg KR-1 & IDZ-1 Nov 7 18:10", short "Calc & Alg tests: Oct 22, Nov 7", importance 5, \
deadlines: {course "Математический анализ", label "Calc KR-1", due "2026-10-22"}, {course "Алгебра", label \
"Alg KR-1", due "2026-11-07"}, {course "Алгебра", label "Alg IDZ-1", due "2026-11-07"}, {course "", label \
"Course survey", due "2026-10-25"}
(study, posted Mon 2026-10-19; upcoming d7 "DM HW 3" 2026-10-21) "ДЗ 3 по дискретке переносится на пятницу 23:59"
-> full "DM HW 3 moved to Fri Oct 23 23:59", short "DM HW 3 → Fri Oct 23", importance 5, deadlines: {course \
"Дискретная математика", label "DM HW 3", due "2026-10-23", moves "d7"}
(study) 1201 "А семинар завтра будет?", 1202 reply to 1201 from a listed person "Нет, семинар по алгебре переносится \
на четверг, R405", 1203 "спасибо!" -> one item, msg_ids [1202, 1201], full "Alg seminar moved to Thu, R405", short \
"Alg seminar → Thu", importance 5; 1203 gets nothing.
(study, posted Mon 2026-10-19 10:00; listed c34 Tue 2026-10-20 09:30-10:50 "Calculus" Seminar, D208) "Завтрашний \
семинар по матану переносится на четверг 13:00, ауд. R405" -> full "Calc seminar Tue → Thu Oct 22 13:00, R405", short \
"Calc seminar → Thu 13:00", importance 5, events: {course "Математический анализ", title "Calculus", start \
"2026-10-22T13:00", end "2026-10-22T14:20", room "R405", kind "class", required true, moves "c34", cancels ""}
(study, posted Mon 2026-10-19) "Консультация перед экзаменом по алгебре 23.10 в 18:10 в R205" -> full "Alg exam \
consult Fri Oct 23 18:10, R205", short "Alg consult Oct 23 18:10", importance 5, events: {course "Алгебра", title \
"Alg consult", start "2026-10-23T18:10", end "", room "R205", kind "consultation", required true, moves "", \
cancels ""}
(study) "Формула оценивания алгебра 1 семестр" with a PDF -> full "Alg grading formula for term 1 posted", short \
"Alg grading formula posted", importance 4, sheets: []
(study, posted Mon 2026-10-19) 1301 "Новый листок" with PDF "matan_list3.pdf", first page "Листок 3. Ряды. Сдать до \
29.10" -> full "Calc sheet 3 (series) posted, due Thu Oct 29", short "Calc sheet 3 → Oct 29", importance 5, deadlines: \
{course "Математический анализ", label "Calc sheet 3", due "2026-10-29"}, sheets: {msg_id 1301, course \
"Математический анализ", number "3", due "2026-10-29"}
"69 БПЛА, летевших на Москву, были уничтожены за прошедшую ночь" -> full "69 drones downed near Moscow overnight", \
short "69 drones downed overnight", importance 2; then, with that item current as n3, "За неделю в сторону \
Московского региона летели 2730 беспилотников" -> replaces n3, full "69 drones downed overnight, 2730 this week", \
short "Drones: 69 overnight, 2730/week", importance 2
"С 10:00 закрыты станции Таганско-Краснопресненской линии от Китай-города до Выхино." -> full "Line 7 closed \
Kitay-Gorod–Vykhino from 10:00", short "Line 7 partly closed from 10:00", importance 4
"""

DEADLINE_SCHEMA = {
    "type": "object",
    "properties": {"course": {"type": "string"}, "label": {"type": "string"}, "due": {"type": "string"},
                   "moves": {"type": "string"}},
    "required": ["course", "label", "due", "moves"],
    "additionalProperties": False,
}
EVENT_SCHEMA = {
    "type": "object",
    "properties": {"course": {"type": "string"}, "title": {"type": "string"}, "start": {"type": "string"},
                   "end": {"type": "string"}, "room": {"type": "string"}, "kind": {"type": "string"},
                   "required": {"type": "boolean"}, "moves": {"type": "string"}, "cancels": {"type": "string"}},
    "required": ["course", "title", "start", "end", "room", "kind", "required", "moves", "cancels"],
    "additionalProperties": False,
}
NEWS_SCHEMA = {
    "type": "object",
    "properties": {"full": {"type": "string"}, "short": {"type": "string"},
                   "importance": {"type": "integer", "enum": [1, 2, 3, 4, 5]}, "replaces": {"type": "string"}},
    "required": ["full", "short", "importance", "replaces"],
    "additionalProperties": False,
}
SHEET_SCHEMA = {
    "type": "object",
    "properties": {"msg_id": {"type": "integer"}, "course": {"type": "string"}, "number": {"type": "string"},
                   "due": {"type": "string"}},
    "required": ["msg_id", "course", "number", "due"],
    "additionalProperties": False,
}
SCHEMA = {
    "type": "object",
    "properties": {"items": {"type": "array", "items": {
        "type": "object",
        "properties": {"msg_ids": {"type": "array", "items": {"type": "integer"}}, "news": NEWS_SCHEMA,
                       "deadlines": {"type": "array", "items": DEADLINE_SCHEMA},
                       "events": {"type": "array", "items": EVENT_SCHEMA},
                       "sheets": {"type": "array", "items": SHEET_SCHEMA}},
        "required": ["msg_ids", "news", "deadlines", "events", "sheets"],
        "additionalProperties": False,
    }}},
    "required": ["items"],
    "additionalProperties": False,
}


# --- which messages go to the model ---------------------------------------------------

def readable(message: dict) -> bool:
    """Something to read: a text, a file with a name, a letter's subject."""
    return bool(message["text"].strip()) or any(f["kind"] in READABLE_FILES for f in message["files"]) \
        or bool(message.get("subject"))


def image_size(message: dict) -> int:
    """Characters of what was read on the message's image, in the prompt."""
    image = message.get("image")
    return min(len(image["shows"]) + len(image["text"]), IMAGE_LIMIT) if image else 0


def text_limit(message: dict) -> int:
    return MAIL_TEXT_LIMIT if message["source"] == "mail" else TEXT_LIMIT


def important(message: dict, people: Iterable[int]) -> bool:
    """The `important` filter (§5.1): listed people, pins, files, dates."""
    return (message["sender_id"] in people or message["pinned"]
            or any(f["kind"] in IMPORTANT_FILES for f in message["files"]) or bool(DATE.search(message["text"])))


def replies_to_important(inbox: Inbox, chat_id: int, message: dict, by_id: dict, people: set[int]) -> bool:
    """A reply to an important message (`by_id`: the messages at hand, else the stored ones) also passes the
    `important` filter; for a replied message no longer stored, its quote is checked for dates."""
    if not message["reply_to"]:
        return False
    replied = by_id.get(message["reply_to"]) or inbox.message(chat_id, message["reply_to"])
    if replied is not None:
        return important(replied, people)
    return bool(DATE.search(message["reply_quote"] or ""))


def urgent(message: dict, people: Iterable[int]) -> bool:
    """The `urgent` trigger (р. 19): a pin, a PDF or a listed person."""
    return (message["sender_id"] in people or message["pinned"]
            or any(f["kind"] in URGENT_FILES for f in message["files"]))


# --- checking the answer -------------------------------------------------------------

def _due(value) -> str | None:
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}", value):
        return None
    try:
        return dt.date.fromisoformat(value).isoformat()
    except ValueError:
        return None


def _moment(value) -> str | None:
    """'2026-10-23T18:10' -> local ISO with seconds; None for anything else."""
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}", value.strip()):
        return None
    try:
        return dt.datetime.fromisoformat(value.strip()).isoformat(timespec="seconds")
    except ValueError:
        return None


def _ref(value) -> str:
    """e12 or c34; anything else is no reference."""
    value = (value or "").strip() if isinstance(value, str) else ""
    return value if re.fullmatch(r"[ec]\d+", value) else ""


def _event(raw: dict) -> dict | None:
    cancels = _ref(raw.get("cancels"))
    moves = "" if cancels else _ref(raw.get("moves"))
    title, start, end = (raw.get("title") or "").strip(), _moment(raw.get("start")), _moment(raw.get("end"))
    if not cancels and not start:
        return None  # a new event or a move needs its time
    if not (cancels or moves) and not title:
        return None  # an event of its own needs a name; a move or a cancellation keeps the listed one's
    if end is not None and (start is None or end <= start):
        end = None
    required = raw.get("required")
    return {"course": (raw.get("course") or "").strip(), "title": title, "start": start, "end": end,
            "room": (raw.get("room") or "").strip(), "kind": (raw.get("kind") or "").strip().lower() or "event",
            "required": required if isinstance(required, bool) else True, "moves": moves, "cancels": cancels}


def _importance(value) -> int:
    return min(5, max(1, value)) if isinstance(value, int) and not isinstance(value, bool) else 3


def check(answer: dict, msg_ids: set[int], pdfs: set[int] = frozenset()) -> list[dict]:
    """Items of the answer that can be used: known messages, some news, a deadline with a valid date, an event with a
    valid time or a sheet of a message with a PDF read (`pdfs`)."""
    items = []
    for raw in answer.get("items") or []:
        ids = list(dict.fromkeys(i for i in raw.get("msg_ids") or [] if i in msg_ids))
        if not ids:
            continue
        raw_news = raw.get("news") or {}
        full, short = (raw_news.get("full") or "").strip(), (raw_news.get("short") or "").strip()
        news = {"full": full or short, "short": short or full, "importance": _importance(raw_news.get("importance")),
                "replaces": (raw_news.get("replaces") or "").strip()} if full or short else None
        deadlines = []
        for raw_deadline in raw.get("deadlines") or []:
            label, due = (raw_deadline.get("label") or "").strip(), _due(raw_deadline.get("due"))
            if label and due:
                deadlines.append({"course": (raw_deadline.get("course") or "").strip(), "label": label, "due": due,
                                  "moves": (raw_deadline.get("moves") or "").strip()})
        events = [event for raw_event in raw.get("events") or [] if (event := _event(raw_event))]
        sheets = list({raw_sheet["msg_id"]: {
            "msg_id": raw_sheet["msg_id"], "course": (raw_sheet.get("course") or "").strip(),
            "number": " ".join(str(raw_sheet.get("number") or "").split())[:20], "due": _due(raw_sheet.get("due"))}
            for raw_sheet in raw.get("sheets") or [] if raw_sheet.get("msg_id") in pdfs}.values())
        if news or deadlines or events or sheets:
            items.append({"msg_ids": ids, "news": news, "deadlines": deadlines, "events": events, "sheets": sheets})
    return items


def too_long(item: dict) -> list[str]:
    """What does not fit the budgets of Tablo's screen (§2.1); counted in characters until the font comes."""
    reasons = []
    if news := item["news"]:
        if len(news["full"]) > FULL_CHARS:
            reasons.append(f"full has {len(news['full'])} characters, keep it within {FULL_CHARS - 8}")
        if len(news["short"]) > LINE:
            reasons.append(f"short has {len(news['short'])} characters, keep it within {LINE - 3}")
    for deadline in item["deadlines"]:
        if len(deadline["label"]) > LABEL_CHARS:
            reasons.append(f"deadline label \"{deadline['label']}\" has {len(deadline['label'])} characters, "
                           f"keep it within {LABEL_CHARS - 1}")
    for event in item["events"]:
        if not (event["moves"] or event["cancels"]) and len(event["title"]) > EVENT_TITLE_CHARS:
            reasons.append(f"event title \"{event['title']}\" has {len(event['title'])} characters, "
                           f"keep it within {EVENT_TITLE_CHARS}")
    return reasons


def fit(item: dict) -> None:
    """After the one retry: a version that fits stands in for one that does not."""
    if news := item["news"]:
        if len(news["full"]) > FULL_CHARS and len(news["short"]) <= LINE:
            news["full"] = news["short"]
        elif len(news["short"]) > LINE and len(news["full"]) <= LINE:
            news["short"] = news["full"]


# --- the timetable in the prompt -------------------------------------------------------

def _span(start: str, end: str | None) -> str:
    begin = dt.datetime.fromisoformat(start)
    return f"{begin:%a %Y-%m-%d %H:%M}" + (f"-{dt.datetime.fromisoformat(end):%H:%M}" if end else "")


def _event_line(event: dict) -> str:
    facts = [event["kind"], *([event["room"]] if event["room"] else []),
             "required" if event["required"] else "optional", *(["cancelled"] if event["cancelled"] else [])]
    return f"- e{event['id']}: {_span(event['start'], event['end'])} \"{event['title']}\" " + ", ".join(facts)


def _class_line(item: dict) -> str:
    """A class as the model sees it, with what chats did to it already."""
    name = f"\"{item['title']}\"" + (f" ({item['subject']})" if item["subject"] != item["title"] else "")
    line = f"- c{item['id']}: {_span(item['start'], item['end'])} {name} {item['type']}" + \
        (f", {item['room']}" if item["room"] else "")
    correction = item["correction"]
    if item["cancelled"]:
        line += ", cancelled"
    elif correction is not None and correction["cancelled"]:
        line += ", cancelled by a chat message"
    elif correction is not None and not moved_back(item, correction):
        line += f", moved by a chat message to {_span(correction['start'], correction['end'])}" + \
            (f", {correction['room']}" if correction["room"] else "")
    return line


# --- PDFs ----------------------------------------------------------------------------

def _pdfs(messages: list[dict]) -> set[int]:
    """Messages whose PDF was read: only they may be sheets."""
    return {m["msg_id"] for m in messages if (m.get("pdf") or {}).get("sha256")}


def _scanned(messages: list[dict]) -> list[dict]:
    return [m for m in messages if (m.get("pdf") or {}).get("sha256") and pdf.scanned(m["pdf"]["first_page"])]


# --- the service ---------------------------------------------------------------------

class Batches:
    def __init__(self, store: Store, settings: Settings, chat_settings: ChatSettings, inbox: Inbox, news: News,
                 deadlines: Deadlines, schedule: Schedule, llm: Haiku,
                 now: Callable[[], dt.datetime] = clock.now_utc, monotonic: Callable[[], float] = time.monotonic):
        self.store, self.db = store, store.db
        self.settings, self.chat_settings, self.inbox = settings, chat_settings, inbox
        self.news, self.deadlines, self.llm, self.now, self.monotonic = news, deadlines, llm, now, monotonic
        self.schedule, self.events, self.subjects = schedule, schedule.events, schedule.subjects
        self.publish: Callable[[Changes], Awaitable[None]] | None = None  # after every run, also an empty one
        self.notify: Callable[[str], None] | None = None                  # a line for the System topic
        self.mail = False  # mail is configured: the mailbox takes its slots like an enabled chat
        self.files: Files | None = None    # PDFs of the messages; without it the model reads their names only
        self.sheets: Sheets | None = None  # where the sheets the model finds go
        self.images: Images | None = None  # reads the images of chats with «Картинки»; without it they stay unread
        self.ok_at: str | None = None
        self.error: str | None = None
        self._urgent: dict[int, Burst] = {}
        self._woken = asyncio.Event()

    def status(self) -> dict:
        counts, enabled = self.inbox.pending_counts(), self.chat_settings.enabled_ids() | {MAILBOX}
        return {"ok_at": self.ok_at, "error": self.error,
                "queue": sum(n for chat_id, n in counts.items() if chat_id in enabled)}

    # --- when -----------------------------------------------------------------------

    def wake(self, chat_id: int, msg_ids: list[int]) -> None:
        """Inbox.wake: new or pinned messages. An urgent one in an urgent chat starts a burst; any message of a chat
        in a burst stretches it."""
        now = self.monotonic()
        if (burst := self._urgent.get(chat_id)) is not None:
            burst.last = now
            return
        settings = self.chat_settings.all(chat_id)
        if not (settings["enabled"] and settings["urgent"]):
            return
        if any(message["parsed_at"] is None and message["deleted_at"] is None and urgent(message, settings["people"])
               for message in self.inbox.messages(chat_id, msg_ids).values()):
            self._urgent[chat_id] = Burst(now, now)
            self._woken.set()  # the loop may be asleep until the next tick

    async def run(self) -> None:
        """Forever: the hourly ticks and the urgent chats in between. Slots missed while down are made up at once."""
        await self._guarded(self.tick())
        tick_at = self.monotonic() + self._seconds_to_tick()
        while True:
            self._woken.clear()
            ready_at = min([tick_at, *(burst.ready_at() for burst in self._urgent.values())])
            try:
                await asyncio.wait_for(self._woken.wait(), max(0.0, ready_at - self.monotonic()))
            except TimeoutError:
                pass
            if chats := self._ready_bursts():
                await self._guarded(self.run_chats(self._ordered(chats)))
            if self.monotonic() >= tick_at:
                await self._guarded(self.tick())
                tick_at = self.monotonic() + self._seconds_to_tick()

    def _ready_bursts(self) -> list[int]:
        """Urgent chats whose burst is over; they leave the waiting list."""
        now = self.monotonic()
        ready = [chat_id for chat_id, burst in self._urgent.items() if burst.ready_at() <= now]
        for chat_id in ready:
            del self._urgent[chat_id]
        return ready

    async def tick(self) -> Changes:
        """Parse the chats whose slot has come and publish; the forum also drops news that are no longer live."""
        due = {chat_id: slot for chat_id in self._boxes()
               if (slot := self._slot(chat_id)) > self._done().get(str(chat_id), "")}
        return await self.run_chats(self._ordered(due), slots=due)

    async def run_chats(self, chat_ids: list[int], slots: dict[int, str] | None = None) -> Changes:
        changes = Changes()
        try:
            for chat_id in chat_ids:
                await self.parse_chat(chat_id, changes)
                if slots:
                    self._mark_done(chat_id, slots[chat_id])
            self.ok_at, self.error = clock.local_iso(self.now()), None
        except BudgetExceeded as e:  # the budget tells the System topic itself; the chats stay due
            log.info("batches wait for the LLM limit: %s", e)
        except LLMError as e:
            log.warning("batch failed, the messages stay in the queue: %s", e)
            self.error = str(e)
        # Every run, so names left unnamed while Haiku was down get another try (§6.2); known ones cost nothing.
        today = clock.local_day(self.now())
        names = self.deadlines.course_names(today) | self.events.course_names(today)
        if self.sheets is not None:
            names |= self.sheets.course_names(today)
        await self.subjects.try_resolve(names, who="batches")
        # A correction given, edited or withdrawn in this run moves or withdraws its interim assessment's Midterm.
        midterms = self.schedule.sync_midterms()
        changes.deadlines += midterms.deadlines
        changes.moved += midterms.moved
        changes.gone += midterms.gone
        if self.publish is not None:
            try:
                await self.publish(changes)
            except Exception:
                log.exception("publishing the batch results failed")
        return changes

    def _boxes(self) -> set[int]:
        """What the schedule parses: the enabled chats, and the mailbox while mail is on or letters wait."""
        boxes = self.chat_settings.enabled_ids()
        if self.mail or self.inbox.pending_counts().get(MAILBOX):
            boxes.add(MAILBOX)
        return boxes

    def _seconds_to_tick(self) -> float:
        now = self.now()
        hour = now.replace(minute=0, second=0, microsecond=0)
        return (hour + dt.timedelta(hours=1, seconds=TICK_DELAY) - now).total_seconds()

    def _slot(self, chat_id: int) -> str:
        """The latest hour of the day this chat is parsed at, local ISO."""
        own = None if chat_id == MAILBOX else self.chat_settings.get(chat_id, "batch_hours")
        every = own or self.settings.get("batch_hours")
        local = self.now().astimezone(clock.TZ)
        return clock.local_iso(local.replace(hour=local.hour - local.hour % every, minute=0, second=0, microsecond=0))

    def _done(self) -> dict[str, str]:
        return self.store.get_json("batches.done", {})

    def _mark_done(self, chat_id: int, slot: str) -> None:
        self.store.put_json("batches.done", {**self._done(), str(chat_id): slot})

    def _ordered(self, chat_ids: Iterable[int]) -> list[int]:
        """Study chats, the mailbox, the other chats (р. 30); chats by title."""
        def key(chat_id: int):
            if chat_id == MAILBOX:
                return 1, ""
            chat = self.inbox.chat(chat_id)
            return 0 if self.chat_settings.get(chat_id, "study") else 2, chat["title"] if chat else ""
        return sorted(chat_ids, key=key)

    async def _guarded(self, run: Awaitable) -> None:
        try:
            await run
        except Exception:
            log.exception("batch run failed")

    # --- one chat -------------------------------------------------------------------

    async def parse_chat(self, chat_id: int, changes: Changes) -> None:
        chat, settings = self._chat(chat_id)
        if chat is None:
            return
        self._retract(chat, changes)
        pending = self.inbox.pending(chat_id, chat["source"])
        if not pending:
            return
        chosen, skipped = self._select(chat_id, settings, pending)
        await self._fetch_pdfs(chosen)
        await self._read_images(chat, settings, chosen)  # may raise: then every message stays in the queue
        if waiting := [m["msg_id"] for m in chosen if m["later"]]:
            log.info("messages %s of %s wait for their files: the twin cannot download now", waiting, chat_id)
            chosen = [m for m in chosen if not m["later"]]
        skipped += [m for m in chosen if not (readable(m) or m["image"])]  # a photo alone that was not read
        chosen = [m for m in chosen if readable(m) or m["image"]]
        if skipped:
            with self.db:
                self.inbox.mark_parsed(chat_id, [m["msg_id"] for m in skipped], self.now(), chat["source"])
        for chunk in self._chunks(chosen):
            await self._parse(chat, settings, chunk, changes)

    def _chat(self, chat_id: int) -> tuple[dict | None, dict]:
        """The chat with its `source` and its settings; the mailbox is no row of `chats`."""
        if chat_id == MAILBOX:
            return MAILBOX_CHAT, MAILBOX_SETTINGS
        chat = self.inbox.chat(chat_id)
        return ({**chat, "source": "tg"} if chat is not None else None), self.chat_settings.all(chat_id)

    def _select(self, chat_id: int, settings: dict, pending: list[dict]) -> tuple[list[dict], list[dict]]:
        oldest = clock.local_iso(self.now() - dt.timedelta(hours=self.settings.get("news_ttl_hours")))
        people = set(settings["people"])
        by_id = {m["msg_id"]: m for m in pending}
        chosen, skipped = [], []
        images = settings["images"] and self.images is not None and self.files is not None
        for message in pending:
            keep = (readable(message) or (images and image_file(message) is not None)) and \
                (settings["study"] or message["at"] >= oldest)
            if keep and settings["filter"] == "important":
                keep = important(message, people) or replies_to_important(self.inbox, chat_id, message, by_id, people)
            (chosen if keep else skipped).append(message)
        return chosen, skipped

    async def _fetch_pdfs(self, messages: list[dict]) -> None:
        """The PDF of each message, downloaded now if need be: `message["pdf"]` is its files row or None.
        `message["later"]`: the twin cannot download it now, the message waits for the next run."""
        for message in messages:
            message["pdf"], message["later"] = None, False
            if self.files is not None:
                try:
                    message["pdf"] = await self.files.fetch(message)
                except NotNow:
                    message["later"] = True
                except Exception:  # the batch goes on with the file's name
                    log.exception("fetching the PDF of message %s failed", message["msg_id"])

    async def _read_images(self, chat: dict, settings: dict, messages: list[dict]) -> None:
        """What the image of each message shows, in a chat with «Картинки»: `message["image"]` is {shows, text} or
        None. Raises what Images.read raises when the limit is spent or the model cannot be reached."""
        for message in messages:
            message["image"] = None
            if message["later"] or not (settings["images"] and self.images is not None and self.files is not None
                                        and image_file(message) is not None):
                continue
            try:
                row = await self.files.fetch_image(message)
            except NotNow:
                message["later"] = True
                continue
            except Exception:  # the batch goes on with the message's text
                log.exception("fetching the image of message %s failed", message["msg_id"])
                continue
            if row is not None and row["sha256"]:
                message["image"] = await self.images.read(chat, settings, message, row)

    @staticmethod
    def _chunks(messages: list[dict]) -> list[list[dict]]:
        chunks, chunk, chars = [], [], 0
        for message in messages:
            size = min(len(message["text"]), text_limit(message)) + image_size(message) + \
                min(len((message.get("pdf") or {}).get("first_page") or ""), FIRST_PAGE_LIMIT)
            if chunk and (len(chunk) >= CHUNK_MESSAGES or chars + size > CHUNK_CHARS):
                chunks.append(chunk)
                chunk, chars = [], 0
            chunk.append(message)
            chars += size
        return [*chunks, chunk] if chunk else chunks

    async def _parse(self, chat: dict, settings: dict, chunk: list[dict], changes: Changes) -> None:
        started = self.now()  # an edit during the call is newer than parsed_at and comes back
        today = clock.local_day(started)
        context = Context(self.news.live(started, self.settings.get("news_ttl_hours")), self.deadlines.upcoming(today),
                          self.events.upcoming(today),
                          self.schedule.upcoming_classes(today) if settings["study"] else [],
                          {name: subject.abbr for name, subject in self.subjects.all().items()})
        documents = self._scans(chunk)  # before the prompt: a scan that cannot be attached is not referred to
        try:
            answer = await self.llm.json("batch", SYSTEM, self._prompt(chat, settings, chunk, context), SCHEMA,
                                         max_tokens=MAX_TOKENS, chat_id=chat["id"], documents=documents)
        except BadAnswer as e:
            if len(chunk) > 1:  # find the message that breaks it; the others go through
                half = len(chunk) // 2
                await self._parse(chat, settings, chunk[:half], changes)
                await self._parse(chat, settings, chunk[half:], changes)
            else:
                self._skip(chat, chunk[0], str(e))
            return
        items = check(answer, {m["msg_id"] for m in chunk}, _pdfs(chunk))
        if any(too_long(item) for item in items):
            items = await self._retry(chat, settings, chunk, context, items)
        for item in items:
            fit(item)
        self._save(chat, settings, chunk, items, context, started, changes)

    async def _retry(self, chat: dict, settings: dict, chunk: list[dict], context: Context,
                     items: list[dict]) -> list[dict]:
        """One more call for the items that do not fit, with the reasons (§6.4); on failure the first answer stays."""
        notes = {item["msg_ids"][0]: "; ".join(reasons) for item in items if (reasons := too_long(item))}
        again_ids = {i for item in items if too_long(item) for i in item["msg_ids"]}
        again = [m for m in chunk if m["msg_id"] in again_ids]
        documents = self._scans(again)
        try:
            answer = await self.llm.json("batch", SYSTEM, self._prompt(chat, settings, again, context, notes), SCHEMA,
                                         max_tokens=MAX_TOKENS, chat_id=chat["id"], documents=documents)
        except (BudgetExceeded, LLMError) as e:
            log.info("the retry for length failed, the first answer stays: %s", e)
            return items
        retried = check(answer, again_ids, _pdfs(again))
        result = []
        for item in items:
            if too_long(item):
                twin = next((r for r in retried if set(r["msg_ids"]) & set(item["msg_ids"])), None)
                if twin is not None:
                    retried.remove(twin)
                    item = twin
            result.append(item)
        return result

    def _skip(self, chat: dict, message: dict, error: str) -> None:
        """A message the model cannot answer for: it leaves the queue, and the System topic hears about it."""
        log.warning("message %s of %s skipped: %s", message["msg_id"], chat["id"], error)
        with self.db:
            self.inbox.mark_parsed(chat["id"], [message["msg_id"]], self.now(), chat["source"])
        if self.notify is None:
            return
        if chat["source"] == "mail":
            self.notify(f"Письмо «{message['subject'] or 'без темы'}» ({message['sender_name']}) пропущено разбором: "
                        f"{error}.")
            return
        link = message_link(chat, message["msg_id"])
        self.notify(f"Сообщение из «{chat['title']}» пропущено разбором: {error}." + (f"\n{link}" if link else ""))

    # --- the prompt -------------------------------------------------------------------

    def _prompt(self, chat: dict, settings: dict, messages: list[dict], context: Context,
                notes: dict[int, str] | None = None) -> str:
        now = self.now().astimezone(clock.TZ)
        header = MAILBOX_HEADER if chat["source"] == "mail" else \
            f"Chat: \"{chat['title']}\" ({KINDS.get(chat['kind'], chat['kind'])}, " \
            f"{'study' if settings['study'] else 'not a study chat'})"
        lines = [f"Today: {now:%a %Y-%m-%d %H:%M}",
                 f"Budgets: full at most 2 lines of {LINE} characters; short at most {LINE} characters; "
                 f"deadline label at most {LABEL_CHARS - 1} characters; event title at most {EVENT_TITLE_CHARS} "
                 "characters.",
                 "",
                 header,
                 "",
                 "Course dictionary:"]
        lines += [f'- "{name}" → {abbr}' for name, abbr in context.courses.items()] or ["(empty)"]
        lines += ["", "Current news:"]
        lines += [f"- n{n['id']}: \"{n['full']}\"" for n in context.news] or ["(none)"]
        lines += ["", "Upcoming deadlines:"]
        lines += [f"- d{d['id']}: \"{d['label']}\" {d['due']}" for d in context.deadlines] or ["(none)"]
        lines += ["", "Upcoming events:"]
        lines += [_event_line(e) for e in context.events] or ["(none)"]
        if settings["study"]:
            lines += ["", "Timetable classes:"]
            lines += [_class_line(c) for c in context.classes] or ["(none)"]
        lines += ["", "New messages:"]
        by_id = {m["msg_id"]: m for m in messages}
        album_first: dict[int, int] = {}
        for message in messages:
            if message["grouped_id"]:
                album_first.setdefault(message["grouped_id"], message["msg_id"])
        scans = {message["msg_id"]: n for n, message in enumerate(_scanned(messages), 1)}
        for message in messages:
            lines += self._render(chat, message, by_id, album_first, set(settings["people"]),
                                  (notes or {}).get(message["msg_id"]), scans)
        return "\n".join(lines)

    def _scans(self, messages: list[dict]) -> list[Document]:
        """The first pages of the scanned PDFs, in the order of the prompt. A file that cannot be attached (gone
        meanwhile) leaves the message with its file's name only."""
        documents = []
        for message in _scanned(messages):
            path = self.files.path(message["pdf"]) if self.files is not None else None
            try:
                if path is None:
                    raise pdf.PdfError("the file is gone")
                documents.append(Document(pdf.head(path, 1), 1))
            except pdf.PdfError as e:
                log.warning("first page of %s not attached: %s", message["pdf"]["name"], e)
                message["pdf"] = None
        return documents

    def _render(self, chat: dict, message: dict, by_id: dict, album_first: dict, people: set[int],
                note: str | None, scans: dict[int, int]) -> list[str]:
        chat_id = chat["id"]
        head = [str(message["msg_id"]), message["sender_name"] or "unknown sender"]
        if message["sender_id"] in people:
            head.append("listed person")
        head.append(f"{dt.datetime.fromisoformat(message['at']):%a %Y-%m-%d %H:%M}")
        if message["pinned"]:
            head.append("pinned")
        first = album_first.get(message["grouped_id"])
        if first is not None and first != message["msg_id"]:
            head.append(f"album with {first}")
        if message["forward"]:
            head.append(f"forwarded from {message['forward'].get('from') or 'unknown'}")
        if message["reply_to"]:
            head.append(f"reply to {message['reply_to']}" if message["reply_to"] in by_id else "reply")
        if message["files"]:
            head.append("files: " + ", ".join(f.get("name") or f["kind"] for f in message["files"]))
        lines = ["- " + " · ".join(head)]
        if message["subject"]:
            lines.append(f"  Subject: {' '.join(message['subject'].split())}")
        text = message["text"][:text_limit(message)]
        lines += [f"  {line}" for line in text.splitlines() if line.strip()] or ["  (no text)"]
        if (file := message.get("pdf")) and file["sha256"]:
            about = f"  PDF \"{file['name']}\", {file['pages']} pages"
            if pdf.scanned(file["first_page"]):
                lines.append(f"{about}: a scan, its first page is attached as document {scans[message['msg_id']]}")
            else:
                lines.append(f"{about}, first page:")
                lines += [f"  | {line}" for line in file["first_page"][:FIRST_PAGE_LIMIT].splitlines()]
        if image := message.get("image"):
            lines.append(f"  Image, as another model read it: {image['shows'] or 'no description'}")
            lines += [f"  | {line}" for line in image["text"][:IMAGE_LIMIT].splitlines()]
        if message["reply_to"] and message["reply_to"] not in by_id:
            replied = self.inbox.message(chat_id, message["reply_to"])
            quote = " ".join(((replied or {}).get("text") or message["reply_quote"] or "").split())[:QUOTE_LIMIT]
            if quote:
                lines.append(f"  > replied message: «{quote}»")
        if message["parsed_at"] is not None:
            earlier = ", ".join(f"n{n['id']}" for n in self.news.made_of(chat["source"], chat_id,
                                                                            message["msg_id"]))
            lines.append("  (edited after it was read" + (f"; its earlier news: {earlier}" if earlier else "") + ")")
        if note:
            lines.append(f"  Your previous answer did not fit: {note}. Write it shorter.")
        return lines

    # --- saving -----------------------------------------------------------------------

    def _save(self, chat: dict, settings: dict, chunk: list[dict], items: list[dict], context: Context,
              started: dt.datetime, changes: Changes) -> None:
        chat_id, source, now = chat["id"], chat["source"], self.now()
        by_id = {m["msg_id"]: m for m in chunk}
        known_news = {f"n{n['id']}": n["id"] for n in context.news}
        known_deadlines = {f"d{d['id']}": d["id"] for d in context.deadlines}
        # What edited messages gave before: replaced by their new items, the rest withdrawn.
        edited = [m["msg_id"] for m in chunk if m["parsed_at"] is not None]
        old_news = {i: [n["id"] for n in self.news.made_of(source, chat_id, i)] for i in edited}
        old_deadlines = {d["id"]: d for i in edited for d in self.deadlines.made_of(source, chat_id, i)}
        old_events = {e["id"]: e for i in edited for e in self.events.made_of(source, chat_id, i)}
        settled_news: set[int] = set()
        settled_deadlines: set[int] = set()
        settled_events: set[int] = set()
        batch_news: dict[str, int] = {}  # "m<msg_id>" of this batch -> its news item
        ordinals: dict[int, int] = {}    # first message of an item -> its events so far: their UIDs differ

        with self.db:
            for item in items:
                ids = item["msg_ids"]
                if news := item["news"]:
                    news_id = self.news.add({
                        "source": source, "chat_id": chat_id, "msg_ids": ids, "study": settings["study"],
                        "posted_at": max(by_id[i]["at"] for i in ids), "full": news["full"], "short": news["short"],
                        "importance": news["importance"]}, now)
                    changes.news.append(news_id)
                    target = known_news.get(news["replaces"]) or batch_news.get(news["replaces"])
                    if target is not None:
                        self.news.replace(target, news_id)
                        settled_news.add(target)
                    for old in (n for i in ids for n in old_news.get(i, ())):
                        self.news.replace(old, news_id)
                        settled_news.add(old)
                    batch_news.update({f"m{i}": news_id for i in ids})
                for deadline in item["deadlines"]:
                    same = self.deadlines.find(deadline["label"], deadline["due"])
                    if same is not None:  # known already, maybe from the earlier text of this very message
                        settled_deadlines.add(same["id"])
                        continue
                    new_id = self.deadlines.add({"source": source, "chat_id": chat_id, "msg_ids": ids,
                                                 "course": deadline["course"], "label": deadline["label"],
                                                 "due": deadline["due"]}, now)
                    moved = known_deadlines.get(deadline["moves"])
                    if moved is None:  # an edit that changed the date moves the deadline of the earlier text
                        moved = next((old_id for old_id, old in old_deadlines.items() if old_id not in
                                      settled_deadlines and set(old["msg_ids"]) & set(ids)
                                      and same_label(old["label"], deadline["label"])), None)
                    if moved is not None and self.deadlines.move(moved, new_id):
                        settled_deadlines.add(moved)
                        changes.moved.append((moved, new_id))
                    else:
                        changes.deadlines.append(new_id)
                for event in item["events"]:
                    n = 0
                    if not (event["moves"] or event["cancels"]):
                        n = ordinals[ids[0]] = ordinals.get(ids[0], -1) + 1
                    self._save_event(chat, ids, event, n, context, old_events, settled_events, now, changes)
                for sheet in item["sheets"]:
                    if self.sheets is not None and (sheet_id := self.sheets.add(chat_id, by_id[sheet["msg_id"]],
                                                                                sheet, now)) is not None:
                        changes.sheets.append(sheet_id)
            gone_news = [n for ids in old_news.values() for n in ids if n not in settled_news]
            gone_deadlines = [d for d in old_deadlines if d not in settled_deadlines]
            # A correction of a class the model was not shown (the chat is no longer a study one, the class is past)
            # could not be restated: it stays.
            shown = {c["uid"] for c in context.classes}
            gone_events = [e for e, old in old_events.items() if e not in settled_events
                           and (old["class_uid"] is None or old["class_uid"] in shown)]
            if gone_news:
                self.news.drop(gone_news, now)
            if gone_deadlines:
                self.deadlines.drop(gone_deadlines, now)
                changes.gone.extend(gone_deadlines)
            if gone_events:
                self.events.drop(gone_events, now)
                changes.events_gone.extend(gone_events)
            self.inbox.mark_parsed(chat_id, list(by_id), started, source)
        log.info("batch of %s: %d messages, %d news, %d deadlines, %d events, %d sheets", chat_id, len(chunk),
                 sum(1 for item in items if item["news"]), sum(len(item["deadlines"]) for item in items),
                 sum(len(item["events"]) for item in items), sum(len(item["sheets"]) for item in items))

    def _save_event(self, chat: dict, ids: list[int], event: dict, n: int, context: Context, old_events: dict,
                    settled: set[int], now: dt.datetime, changes: Changes) -> None:
        """One event of the answer: a new version of a listed event, a correction of a listed class, or an event of
        its own. Inside the transaction of _save."""
        told = {"source": chat["source"], "chat_id": chat["id"], "msg_ids": ids}
        ref = event["cancels"] or event["moves"]
        listed = next((e for e in context.events if f"e{e['id']}" == ref), None)
        if listed is not None:
            target = self.events.current(listed["id"])  # another message of this batch may have moved it already
            if target is None or target["gone_at"] is not None:
                return
            new = {**{key: target[key] for key in ("uid", "class_uid", "course", "title", "kind", "start", "end",
                                                   "room", "required", "cancelled")}, **told}
            if event["cancels"]:
                new["cancelled"] = True
            else:
                end = event["end"]
                if end is None and target["end"]:  # the same length as before
                    end = (local(event["start"]) + (local(target["end"]) - local(target["start"]))).isoformat()
                new.update(start=event["start"], end=end, room=event["room"] or target["room"], cancelled=False)
            if all(new[key] == target[key] for key in ("start", "end", "room", "cancelled")):
                settled.add(target["id"])  # told again, nothing new
                return
            new_id = self.events.add(new, now)
            self.events.supersede(target["id"], new_id)
            settled.add(target["id"])
            changes.events_moved.append((target["id"], new_id))
            return

        item = next((c for c in context.classes if f"c{c['id']}" == ref), None)
        if item is not None:
            if item["cancelled"]:
                return  # RUZ has cancelled it itself
            start, end, room = item["start"], item["end"], item["room"]
            earlier = self.events.correction(item["uid"])
            if event["moves"]:
                start, room = event["start"], event["room"] or item["room"]
                end = event["end"] or (local(start) + (local(item["end"]) - local(item["start"]))).isoformat()
                if (start, end, room) == (item["start"], item["end"], item["room"]) and earlier is None:
                    return  # the class where it always is: no move (with a correction in force, it is a move back)
            cancelled = bool(event["cancels"])
            if earlier is not None and (earlier["cancelled"], earlier["start"], earlier["end"], earlier["room"]) == \
                    (cancelled, start, end, room):
                settled.add(earlier["id"])
                return
            new_id = self.events.add({**told, "uid": item["uid"], "class_uid": item["uid"], "course": item["subject"],
                                      "title": item["title"], "kind": item["kind"], "start": start, "end": end,
                                      "room": room, "required": True, "cancelled": cancelled}, now)
            if earlier is not None:
                self.events.supersede(earlier["id"], new_id)
                settled.add(earlier["id"])
            changes.events.append(new_id)
            return

        if event["cancels"] or event["start"] is None or not event["title"]:
            return  # cancels or moves something that was not listed: nothing to apply it to
        uid = event_uid(chat["source"], chat["id"], ids[0], n)
        new = {**told, "uid": uid, "course": event["course"], "title": event["title"], "kind": event["kind"],
               "start": event["start"], "end": event["end"], "room": event["room"], "required": event["required"]}
        earlier = next((e for e in old_events.values() if e["id"] not in settled and e["uid"] == uid), None)
        if earlier is not None:  # the message was edited: its event again, maybe changed
            settled.add(earlier["id"])
            if all(new[key] == earlier[key] for key in ("title", "start", "end", "room")) and not earlier["cancelled"]:
                return
            new_id = self.events.add(new, now)
            self.events.supersede(earlier["id"], new_id)
            changes.events_moved.append((earlier["id"], new_id))
            return
        if (same := self.events.find(event["title"], event["start"])) is not None:
            settled.add(same["id"])  # known already, maybe from the earlier text of this very message
            return
        if self.events.by_uid(uid) is not None:
            return  # another message has moved this event since: this text no longer tells it
        changes.events.append(self.events.add(new, now))

    def _retract(self, chat: dict, changes: Changes) -> None:
        """Items all of whose messages were deleted are withdrawn; an event withdrawn this way brings back the version
        it had moved or cancelled. Superseded versions are withdrawn too, so a newer one withdrawn later never brings
        back a version told only by deleted messages; only those in force are news."""
        if chat["source"] == "mail":
            return  # a letter is never deleted or edited
        chat_id, now = chat["id"], self.now()
        news = self.news.in_force("tg", chat_id, now - RETRACT_WINDOW)
        deadlines = self.deadlines.in_force("tg", chat_id, now - RETRACT_WINDOW)
        events = self.events.versions("tg", chat_id, now - RETRACT_WINDOW)
        msg_ids = sorted({i for row in news + deadlines + events for i in row["msg_ids"]})
        if not msg_ids:
            return
        stored = self.inbox.messages(chat_id, msg_ids)

        def deleted(row: dict) -> bool:
            return all(i in stored and stored[i]["deleted_at"] for i in row["msg_ids"])

        gone_news = [n["id"] for n in news if deleted(n)]
        gone_deadlines = [d["id"] for d in deadlines if deleted(d)]
        gone_events = [e["id"] for e in events if deleted(e)]  # by id: the oldest version first
        with self.db:
            if gone_news:
                self.news.drop(gone_news, now)
            if gone_deadlines:
                self.deadlines.drop(gone_deadlines, now)
            if gone_events:
                self.events.drop(gone_events, now)
        changes.gone.extend(gone_deadlines)
        changes.events_gone.extend(e["id"] for e in events if deleted(e) and e["replaced_by"] is None)
