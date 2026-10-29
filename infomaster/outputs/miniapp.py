"""Mini App on the public port, the only one behind Funnel (§8.1, §8.4).

Every /app/api/ request carries Telegram's initData in `Authorization: tma <initData>`. It is signed with the bot
token, and only the owner's Telegram id gets in (D16).

Screens (D44: four tabs, every change an action on a card): the merged deadlines with the owner's own ones and the
hidden ones (§5.4, §5.5), the timetable as the feeds have it, with the main/optional switch and hiding of the chats'
events and corrections, the owner's own events and corrections of classes (§6.5), the grades and the sheets (§6.7,
§6.3, D14), and the settings: the twin's account with its login and logout and the chats with their settings and the
daily number of images read in them (D12), the course dictionary with manual entries (D3, D33), the HSE App X
login (§5.2, D7), the limits of reading a PDF, the time of the daily summary and its chats (§6.6), the gradebooks:
the name, the sheets and what was read in them, the LLM spending (§7, D40): today, this week and this month by task
and by chat, the cost of each of the last 30 days and the daily limit, which can be changed or raised for today. Tablo
and HA show no spending: this screen is the one place. The general settings: the batches' N, how long news lives, the
prices of the models and the languages of the forum and the Mini App (D41). Every setting is here (D33); only the
secrets of .env are not. The Mini App's texts are the page's own; what the service says here (errors, the reasons of a
failed login, the statuses) comes in the Mini App's language too.

The same port serves /hseapp/callback, where the browser comes back after the HSE login. It has no initData: the
PKCE state of the login link is its protection, and it answers with a short page in the Mini App's language. And
/calendar/<CALENDAR_PUBLIC_SECRET>/main.ics and optional.ics for one's own calendar (see ical_feed, D43).
"""

import calendar
import datetime as dt
import hashlib
import hmac
import html
import json
import time
from functools import partial
from pathlib import Path
from urllib.parse import parse_qsl

from aiohttp import web

from .. import clock
from ..collectors.files import UPLOAD_MAX_SIZE
from ..collectors.grades import sheet_id, short_course
from ..collectors.hseapp import CALLBACK_PATH, HseApp
from ..collectors.inbox import MAILBOX, Inbox
from ..collectors.telegram import LoginError, Twin
from ..config import Env
from ..i18n import LANGUAGES, json_default, reason, tr
from ..llm.budget import SOFT_SHARE, Budget
from ..llm.journal import Journal
from ..processing.grades import Grades, budget
from ..processing.merge import DeadlineList
from ..processing.schedule import Schedule
from ..processing.sheets import SUMMARY_CHARS
from ..processing.subjects import ABBR_CHARS, TITLE_CHARS, SubjectError, Subjects
from ..processing.todo import CATEGORIES, uid
from ..settings import BATCH_HOURS, SHEET_LABEL_CHARS, SPECS, ChatSettings, Settings, SettingsError
from . import ical_feed
from .api import SOURCES_KEY, Sources
from .sheets import SheetTopic
from .summaries import SummaryTopic

STATIC = Path(__file__).with_name("miniapp")
INIT_DATA_TTL = 24 * 3600  # seconds; Telegram signs initData once, when the Mini App opens

ENV_KEY = web.AppKey("env", Env)
BUDGET_KEY = web.AppKey("budget", Budget)
JOURNAL_KEY = web.AppKey("journal", Journal)
INBOX_KEY = web.AppKey("inbox", Inbox)
CHAT_SETTINGS_KEY = web.AppKey("chat_settings", ChatSettings)
SUBJECTS_KEY = web.AppKey("subjects", Subjects)
SCHEDULE_KEY = web.AppKey("schedule", Schedule)
HSEAPP_KEY = web.AppKey("hseapp", HseApp)
DEADLINES_KEY = web.AppKey("deadlines", DeadlineList)
SETTINGS_KEY = web.AppKey("settings", Settings)
SHEETS_KEY = web.AppKey("sheets", SheetTopic)
SUMMARIES_KEY = web.AppKey("summaries", SummaryTopic)
GRADES_KEY = web.AppKey("grades", Grades)
TWIN_KEY = web.AppKey("twin", Twin)
GENERAL = ("batch_hours", "news_ttl_hours", "llm_prices", "forum_language", "miniapp_language")  # of no other screen
GRADE_SETTINGS = {"student": "grades_student", "sheets": "grades_sheets", "window_days": "grades_window_days"}
PDF_LIMITS = ("pdf_pages", "pdf_tokens")
TITLE_LIMIT = 200  # characters of an own deadline's title
SHEET_COURSE_LIMIT, SHEET_NUMBER_LIMIT = 60, 20
GRADE_LABEL_LIMIT = 40
EVENT_TITLE_LIMIT = 120
ROOM_LIMIT = 300  # a room, or a meeting's link
EVENT_FIELDS = ("title", "kind", "start", "end", "room", "required")
KIND_LIMIT = 40  # the kind is the type of the feeds once processing/schedule.py's type_of has read it
CHART_DAYS = 30


def check_init_data(init_data: str, bot_token: str, now: float | None = None) -> dict | None:
    """The Telegram user from valid, fresh initData; None for anything else."""
    fields = dict(parse_qsl(init_data, keep_blank_values=True))
    received = fields.pop("hash", None)
    if not received:
        return None
    check_string = "\n".join(f"{key}={value}" for key, value in sorted(fields.items()))
    secret = hmac.new(b"WebAppData", bot_token.encode(), hashlib.sha256).digest()
    expected = hmac.new(secret, check_string.encode(), hashlib.sha256).hexdigest()
    if not hmac.compare_digest(expected, received):
        return None
    try:
        auth_date = int(fields["auth_date"])
        user = json.loads(fields["user"])
    except (KeyError, ValueError):
        return None
    if (now if now is not None else time.time()) - auth_date > INIT_DATA_TTL:
        return None
    return user if isinstance(user, dict) else None


def language(request: web.Request) -> str:
    """The Mini App's language (D41); English where the app has no settings."""
    settings = request.app.get(SETTINGS_KEY)
    return settings.get("miniapp_language") if settings is not None else "en"


def respond(request: web.Request, data) -> web.Response:
    """JSON with every Phrase in it (a status error, a letter's subject) in the Mini App's language."""
    return web.json_response(data, dumps=partial(json.dumps, default=json_default(language(request))))


def bad_request(request: web.Request, en: str, ru: str) -> web.HTTPBadRequest:
    return web.HTTPBadRequest(text=tr(language(request), en, ru))


@web.middleware
async def owner_only(request: web.Request, handler):
    if not request.path.startswith("/app/api/"):
        return await handler(request)
    scheme, _, init_data = request.headers.get("Authorization", "").partition(" ")
    env = request.app[ENV_KEY]
    user = check_init_data(init_data, env.bot_token) if scheme == "tma" else None
    if user is None:
        raise web.HTTPUnauthorized(text="open the app from the bot")
    if user.get("id") != env.owner_id:
        raise web.HTTPForbidden(text="not your app")
    request["user"] = user
    return await handler(request)


async def index(request: web.Request) -> web.FileResponse:
    return web.FileResponse(STATIC / "index.html", headers={"Cache-Control": "no-cache"})


async def to_index(request: web.Request) -> web.Response:
    raise web.HTTPFound("/app/")


async def me(request: web.Request) -> web.Response:
    """The stub screen: who is in and what the LLM cost today."""
    today = request.app[JOURNAL_KEY].usage(clock.now_utc())["today"]
    return respond(request, {"first_name": request["user"].get("first_name", ""), "language": language(request),
                             "llm": {**request.app[BUDGET_KEY].status(), "runs_today": today["runs"]}})


async def chats(request: web.Request) -> web.Response:
    """The Chats screen (§8.4): every chat of the twin with its settings, enabled ones first."""
    chat_settings = request.app[CHAT_SETTINGS_KEY]
    listed = [{**chat, "settings": chat_settings.all(chat["id"])} for chat in request.app[INBOX_KEY].chats()]
    listed.sort(key=lambda chat: not chat["settings"]["enabled"])
    sources = request.app[SOURCES_KEY]
    return respond(request, {"twin": sources["telegram"](), "mail": sources.get("mail", dict)(),
                              "images": sources.get("images", dict)(), "chats": listed})


async def update_images(request: web.Request) -> web.Response:
    """{daily}: how many images are read a day in chats with «Картинки» (D12); 0 reads none."""
    values = await _json(request)
    if set(values) != {"daily"}:
        raise web.HTTPBadRequest(text="expected {daily}")
    try:
        daily = request.app[SETTINGS_KEY].set("images_daily", values["daily"])
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return respond(request, request.app[SOURCES_KEY].get("images", lambda: {"daily": daily})())


def _chat_id(request: web.Request) -> int:
    try:
        chat_id = int(request.match_info["chat_id"])
    except ValueError:
        raise web.HTTPNotFound() from None
    if request.app[INBOX_KEY].chat(chat_id) is None:
        raise web.HTTPNotFound(text="no such chat")
    return chat_id


async def _json(request: web.Request) -> dict:
    try:
        values = await request.json()
    except ValueError:
        raise web.HTTPBadRequest(text="expected JSON") from None
    if not isinstance(values, dict):
        raise web.HTTPBadRequest(text="expected a JSON object")
    return values


async def update_chat(request: web.Request) -> web.Response:
    """Change some settings of a chat: {name: value}; all of them are checked before any is saved."""
    chat_id = _chat_id(request)
    values = await _json(request)
    try:
        return respond(request, request.app[CHAT_SETTINGS_KEY].update(chat_id, values))
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None


async def senders(request: web.Request) -> web.Response:
    """Who wrote in the chat, to pick the important people; chosen ones who have not written yet are listed too."""
    chat_id = _chat_id(request)
    listed = request.app[INBOX_KEY].senders(chat_id)
    seen = {sender["id"] for sender in listed}
    listed += [{"id": person, "name": None, "messages": 0}
               for person in request.app[CHAT_SETTINGS_KEY].get(chat_id, "people") if person not in seen]
    return respond(request, listed)


async def subjects(request: web.Request) -> web.Response:
    """The Словарь screen (§8.4): every entry; Haiku's can be overridden by hand, manual ones edited or deleted."""
    dictionary = request.app[SUBJECTS_KEY]
    entries = [{"name": name, "title": s.title, "abbr": s.abbr, "manual": s.manual}
               for name, s in dictionary.all().items()]
    return respond(request, {"subjects": entries, "error": dictionary.error, "title_chars": TITLE_CHARS,
                              "abbr_chars": ABBR_CHARS})


async def put_subject(request: web.Request) -> web.Response:
    """Add or change an entry by hand: {name, title, abbr}; it becomes manual."""
    values = await _json(request)
    try:
        return respond(request, request.app[SUBJECTS_KEY].set_manual(
            *(values.get(key) if isinstance(values.get(key), str) else "" for key in ("name", "title", "abbr"))))
    except SubjectError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None


async def delete_subject(request: web.Request) -> web.Response:
    """Drop an entry: {name}. Haiku names the course again when a source brings it next time."""
    name = (await _json(request)).get("name")
    if not isinstance(name, str) or not request.app[SUBJECTS_KEY].delete(name):
        raise web.HTTPNotFound(text="no such entry")
    return respond(request, {"deleted": name})


def _agenda_state(request: web.Request) -> dict:
    timetable = request.app[SCHEDULE_KEY]
    today = clock.local_day(timetable.now())
    return {"today": today.isoformat(), "ruz": request.app[SOURCES_KEY]["ruz"](),
            "calendar": bool(request.app[ENV_KEY].calendar_secret or request.app[ENV_KEY].calendar_public_secret),
            **timetable.agenda(today)}


async def agenda(request: web.Request) -> web.Response:
    """The Расписание screen: the classes with their corrections and the events, as the feeds have them, from today
    on; every change of the screen answers it anew."""
    return respond(request, _agenda_state(request))


def _moment(request: web.Request, value) -> dt.datetime:
    try:
        moment = dt.datetime.fromisoformat(value if isinstance(value, str) else "")
    except ValueError:
        raise bad_request(request, "a time like 2026-10-22T10:00", "время вида 2026-10-22T10:00") from None
    if moment.tzinfo is not None:
        raise bad_request(request, "a time like 2026-10-22T10:00", "время вида 2026-10-22T10:00")
    return moment


def _event_fields(request: web.Request, values: dict, names: set[str]) -> dict:
    """The checked `names` of an event from the screen: title, kind, start and end (Moscow), room, required,
    cancelled."""
    fields = {}
    if "title" in names:
        title = values.get("title")
        if not isinstance(title, str) or not " ".join(title.split()) or len(title) > EVENT_TITLE_LIMIT:
            raise bad_request(request, f"a title is needed, up to {EVENT_TITLE_LIMIT} characters",
                              f"нужно название, до {EVENT_TITLE_LIMIT} символов")
        fields["title"] = " ".join(title.split())
    if "kind" in names:
        kind = values.get("kind")
        if not isinstance(kind, str) or not kind.strip() or len(kind) > KIND_LIMIT:
            raise web.HTTPBadRequest(text=f"kind: a word up to {KIND_LIMIT} characters, as Event or Consultation")
        fields["kind"] = kind.strip()
    if {"start", "end"} & names:
        start, end = _moment(request, values.get("start")), _moment(request, values.get("end"))
        if end <= start:
            raise bad_request(request, "the end comes after the start", "конец — позже начала")
        fields["start"], fields["end"] = start.isoformat(timespec="seconds"), end.isoformat(timespec="seconds")
    if "room" in names:
        room = values.get("room", "")
        if not isinstance(room, str) or len(room) > ROOM_LIMIT:
            raise web.HTTPBadRequest(text=f"room: up to {ROOM_LIMIT} characters")
        fields["room"] = room.strip()
    for flag in {"required", "cancelled"} & names:
        if not isinstance(values.get(flag), bool):
            raise web.HTTPBadRequest(text=f"{flag}: true or false")
        fields[flag] = values[flag]
    return fields


def _event_id(request: web.Request) -> int:
    try:
        return int(request.match_info["event_id"])
    except ValueError:
        raise web.HTTPNotFound() from None


async def add_event(request: web.Request) -> web.Response:
    """{title, kind, start, end, room, required}: an own event."""
    request.app[SCHEDULE_KEY].add_event(_event_fields(request, await _json(request), set(EVENT_FIELDS)))
    return respond(request, _agenda_state(request))


async def update_event(request: web.Request) -> web.Response:
    """{required?, hidden?} for the newest version of an event, as the forum's buttons; or some of {title, kind,
    start, end, room, required}: the owner's change of an event of its own (not a correction)."""
    event_id = _event_id(request)
    values = await _json(request)
    timetable = request.app[SCHEDULE_KEY]
    event = timetable.events.current(event_id)
    if event is None or event["gone_at"] is not None:
        raise web.HTTPNotFound(text="no such event")
    if values and set(values) <= {"required", "hidden"}:
        if any(not isinstance(value, bool) for value in values.values()):
            raise web.HTTPBadRequest(text="expected {required: true|false, hidden: true|false}")
        if "required" in values and event["class_uid"] is not None:
            raise bad_request(request, "a class of RUZ is always in the main calendar",
                              "пара из РУЗ всегда в основном календаре")
        if "required" in values:
            timetable.events.set_required(event_id, values["required"])
        if "hidden" in values:  # through the schedule: a correction of an interim assessment moves its Midterm
            await timetable.set_hidden(event_id, values["hidden"])
    elif values and set(values) <= set(EVENT_FIELDS):
        if event["class_uid"] is not None:
            raise bad_request(request, "a class is moved or cancelled as a correction",
                              "пару переносят или отменяют правкой")
        timetable.edit_event(event_id, _event_fields(request, values, set(values) | (
            {"start", "end"} if {"start", "end"} & set(values) else set())))
    else:
        raise web.HTTPBadRequest(text="expected {required?, hidden?} or some of " + ", ".join(EVENT_FIELDS))
    return respond(request, _agenda_state(request))


async def delete_event(request: web.Request) -> web.Response:
    """An own event is deleted, an own correction undone; the chats' events are hidden instead."""
    if await request.app[SCHEDULE_KEY].remove_own(_event_id(request)) is None:
        raise web.HTTPNotFound(text="no such own event")
    return respond(request, _agenda_state(request))


async def correct_class(request: web.Request) -> web.Response:
    """{class_uid, start, end, room, cancelled}: the owner moves or cancels a RUZ class."""
    values = await _json(request)
    fields = _event_fields(request, values, {"start", "end", "room", "cancelled"})
    if not isinstance(values.get("class_uid"), str) or await request.app[SCHEDULE_KEY].correct_class(
            values["class_uid"], fields["start"], fields["end"], fields["room"], fields["cancelled"]) is None:
        raise web.HTTPNotFound(text="no such class")
    return respond(request, _agenda_state(request))


# --- deadlines -------------------------------------------------------------------------------

def _origin(request: web.Request, d) -> dict:
    """The chat or the letter a deadline of the chats or the mail came from, with the link to its message."""
    row = request.app[DEADLINES_KEY].deadlines.get(int(d.ref[3:])) if d.ref.startswith("dl:") else None
    if row is None or row["source"] not in ("tg", "mail"):
        return {"from": None, "link": None}
    timetable = request.app[SCHEDULE_KEY]
    return {"from": timetable.told_by(row), "link": timetable.link(row)}


def _deadlines_state(request: web.Request) -> dict:
    """The Дедлайны screen: the merged list as Tablo and HA get it, the own ones, the hidden ones."""
    merged = request.app[DEADLINES_KEY]
    today = merged.today()
    items = [{"uid": uid(d), "title": d.title, "due": d.due.isoformat(), "category": d.category, "source": d.source,
              "course": d.name, "overdue": d.due < today, **_origin(request, d)} for d in merged.visible()]
    own = [{"uid": d.key, "title": d.title, "due": d.due.isoformat(), "category": d.category}
           for d in merged.todo.own()]
    hidden = [{"uid": row["uid"], "title": row["title"], "due": row["due"], "source": row["source"]}
              for row in merged.todo.hidden_rows()]
    hseapp = request.app[SOURCES_KEY].get("hseapp", dict)()
    return {"items": items, "own": own, "hidden": hidden, "today": today.isoformat(),
            "hseapp": {"login_needed": bool(hseapp.get("login_needed")), "error": hseapp.get("error")}}


def _own_fields(request: web.Request, values: dict, some: bool) -> dict:
    """title, due and category of an own deadline from the screen; every one of them, unless `some`."""
    fields = {}
    if not some or "title" in values:
        title = values.get("title")
        if not isinstance(title, str) or not " ".join(title.split()) or len(title) > TITLE_LIMIT:
            raise bad_request(request, f"a title is needed, up to {TITLE_LIMIT} characters",
                              f"нужно название, до {TITLE_LIMIT} символов")
        fields["title"] = title
    if not some or "due" in values:
        try:
            fields["due"] = dt.date.fromisoformat(values.get("due") if isinstance(values.get("due"), str) else "")
        except ValueError:
            raise bad_request(request, "the due date is a date like 2026-10-22",
                              "срок — дата вида 2026-10-22") from None
    if not some:
        if values.get("category") not in CATEGORIES:
            raise bad_request(request, "the list: study or personal", "список: учёба (study) или личное (personal)")
        fields["category"] = values["category"]
    return fields


def _listed(request: web.Request):
    """A deadline of the list by uid; an own one also when the merge left it out as a copy of another."""
    merged, item_uid = request.app[DEADLINES_KEY], request.match_info["uid"]
    d = merged.find(item_uid) or next((own for own in merged.todo.own() if own.key == item_uid), None)
    if d is None:
        raise web.HTTPNotFound(text=tr(language(request), "this deadline is off the list already",
                                       "этого дедлайна уже нет в списке"))
    return d


async def deadlines(request: web.Request) -> web.Response:
    return respond(request, _deadlines_state(request))


async def add_deadline(request: web.Request) -> web.Response:
    """{title, due, category}: a new own deadline."""
    fields = _own_fields(request, await _json(request), some=False)
    request.app[DEADLINES_KEY].add(fields["title"], fields["due"], fields["category"])
    return respond(request, _deadlines_state(request))


async def edit_deadline(request: web.Request) -> web.Response:
    """{title?, due?}: an own one changes; one from a source is hidden and an own copy takes its place (as in HA)."""
    fields = _own_fields(request, await _json(request), some=True)
    if not fields:
        raise web.HTTPBadRequest(text="expected {title?, due?}")
    request.app[DEADLINES_KEY].edit(_listed(request), fields.get("title"), fields.get("due"))
    return respond(request, _deadlines_state(request))


async def delete_deadline(request: web.Request) -> web.Response:
    """An own one is deleted, one from a source is hidden for good (as in HA)."""
    request.app[DEADLINES_KEY].remove(_listed(request))
    return respond(request, _deadlines_state(request))


async def unhide_deadline(request: web.Request) -> web.Response:
    """«Вернуть»: one hidden row less; the deadline is back if its source still has it."""
    if not request.app[DEADLINES_KEY].unhide(request.match_info["uid"]):
        raise web.HTTPNotFound(text=tr(language(request), "no such hidden deadline", "такого скрытого дедлайна нет"))
    return respond(request, _deadlines_state(request))


# --- sheets -----------------------------------------------------------------------------------

def _sheets_state(request: web.Request) -> dict:
    settings = request.app[SETTINGS_KEY]
    return {"sheets": request.app[SHEETS_KEY].sheets.listing(), "status": request.app[SOURCES_KEY]["sheets"](),
            "limits": {name: settings.get(name) for name in PDF_LIMITS}}


async def sheets(request: web.Request) -> web.Response:
    """The Листочки screen: the sheets with their versions and the limits of reading them."""
    return respond(request, _sheets_state(request))


async def update_sheets(request: web.Request) -> web.Response:
    """{pdf_pages?, pdf_tokens?}: the limits of reading a PDF whole; both are checked before either is saved."""
    values = await _json(request)
    if not values or set(values) - set(PDF_LIMITS):
        raise web.HTTPBadRequest(text="expected {pdf_pages?, pdf_tokens?}")
    settings = request.app[SETTINGS_KEY]
    try:
        checked = {name: SPECS[name][1](value) for name, value in values.items()}
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    for name, value in checked.items():
        settings.set(name, value)
    return respond(request, _sheets_state(request))


def _sheet_id(request: web.Request) -> int:
    try:
        return int(request.match_info["sheet_id"])
    except ValueError:
        raise web.HTTPNotFound() from None


def _sheet_fields(request: web.Request, values: dict) -> dict:
    """The owner's course, number, due date and summary of a sheet; an empty course or due date clears it."""
    fields = {}
    for name, limit in (("course", SHEET_COURSE_LIMIT), ("number", SHEET_NUMBER_LIMIT),
                        ("summary", SUMMARY_CHARS)):
        if name in values:
            value = values[name]
            if not isinstance(value, str) or len(value) > limit:
                raise web.HTTPBadRequest(text=f"{name}: text up to {limit} characters")
            fields[name] = " ".join(value.split()) if name != "summary" else value.strip()
    for name in ("course", "summary"):
        if name in fields:
            fields[name] = fields[name] or None
    if "due" in values:
        due = values["due"]
        if due in (None, ""):
            fields["due"] = None
        else:
            try:
                fields["due"] = dt.date.fromisoformat(due if isinstance(due, str) else "").isoformat()
            except ValueError:
                raise bad_request(request, "the due date is a date like 2026-10-22",
                                  "срок — дата вида 2026-10-22") from None
    return fields


async def edit_sheet(request: web.Request) -> web.Response:
    """Some of {course, number, due, summary}: the owner's change; the caption of its message follows."""
    values = await _json(request)
    if not values or set(values) - {"course", "number", "due", "summary"}:
        raise web.HTTPBadRequest(text="expected some of course, number, due, summary")
    if await request.app[SHEETS_KEY].edit(_sheet_id(request), _sheet_fields(request, values)) is None:
        raise web.HTTPNotFound(text="no such sheet")
    return respond(request, _sheets_state(request))


async def add_sheet(request: web.Request) -> web.Response:
    """A multipart form of a PDF (`file`) and course, number, due: an own sheet, posted at once."""
    form = await request.post()
    upload = form.get("file")
    if not isinstance(upload, web.FileField):
        raise bad_request(request, "choose a PDF", "выберите PDF")
    fields = _sheet_fields(request, {name: form[name] for name in ("course", "number", "due") if name in form})
    topic = request.app[SHEETS_KEY]
    try:
        file = await topic.files.keep(upload.file.read(), upload.filename or "sheet.pdf", clock.now_utc())
    except ValueError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    topic.sheets.add_own(file, fields.get("course"), fields.get("number", ""), fields.get("due"), clock.now_utc())
    await topic.publish()
    return respond(request, _sheets_state(request))


async def delete_sheet(request: web.Request) -> web.Response:
    """An own sheet deleted with its messages; the chats' sheets are not the owner's to delete."""
    if not await request.app[SHEETS_KEY].remove(_sheet_id(request)):
        raise web.HTTPNotFound(text="no such own sheet")
    return respond(request, _sheets_state(request))


async def join_sheet(request: web.Request) -> web.Response:
    """{previous: id}: the sheet is a newer version of that one (D14); its file replaces the older one's."""
    try:
        sheet_id = int(request.match_info["sheet_id"])
    except ValueError:
        raise web.HTTPNotFound() from None
    previous = (await _json(request)).get("previous")
    if isinstance(previous, bool) or not isinstance(previous, int):
        raise web.HTTPBadRequest(text="expected {previous: id}")
    try:
        await request.app[SHEETS_KEY].join(sheet_id, previous)
    except ValueError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    return respond(request, _sheets_state(request))


# --- HSE App X ------------------------------------------------------------------------------

def _hseapp_state(request: web.Request) -> dict:
    """The screen's state: the source's status (never the tokens) and where the browser comes back to."""
    return {"status": request.app[SOURCES_KEY]["hseapp"](), "callback": request.app[HSEAPP_KEY].callback_url}


async def hseapp(request: web.Request) -> web.Response:
    return respond(request, _hseapp_state(request))


async def update_hseapp(request: web.Request) -> web.Response:
    """{enabled: bool}: the general setting hseapp_enabled."""
    values = await _json(request)
    if set(values) != {"enabled"} or not isinstance(values["enabled"], bool):
        raise web.HTTPBadRequest(text="expected {enabled: true|false}")
    request.app[HSEAPP_KEY].set_enabled(values["enabled"])
    return respond(request, _hseapp_state(request))


async def hseapp_link(request: web.Request) -> web.Response:
    """{app?: bool} -> {url}: a login link that comes back to our callback, or with `app` to the app's own address
    (its return address is pasted by hand). Each link holds a PKCE state; the last five work."""
    app_redirect = (await _json(request)).get("app", False)
    if not isinstance(app_redirect, bool):
        raise web.HTTPBadRequest(text="expected {app: true|false}")
    return respond(request, {"url": request.app[HSEAPP_KEY].login_link(app=app_redirect)})


async def hseapp_login(request: web.Request) -> web.Response:
    """{url}: the return address pasted by hand, when the browser did not come back by itself."""
    returned = (await _json(request)).get("url")
    if not isinstance(returned, str) or not returned.strip():
        raise bad_request(request, "paste the address the browser came back to",
                          "вставьте адрес, на который вернулся браузер")
    try:
        await request.app[HSEAPP_KEY].login(returned)
    except ValueError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    return respond(request, _hseapp_state(request))


async def hseapp_callback(request: web.Request) -> web.Response:
    """The browser after the HSE login (public, no initData: only the browser that opened the link knows its
    state). The address carries the code: it is not logged (the ports have no access log)."""
    lang = language(request)
    try:
        await request.app[HSEAPP_KEY].login(str(request.rel_url))
    except ValueError as e:
        why = reason(e, lang).rstrip(".")
        return page(lang, tr(lang, "The login failed", "Вход не удался"), f"{why[:1].upper()}{why[1:]}.",
                    tr(lang, "Go back to Telegram and log in again: the Mini App, Settings → HSE App X.",
                       "Вернитесь в Telegram и войдите ещё раз: Mini App, Настройки → HSE App X."), status=400)
    return page(lang, tr(lang, "Logged in to HSE App X", "Вход в HSE App X выполнен"),
                tr(lang, "The deadlines will be updated within a minute.", "Дедлайны обновятся в течение минуты."),
                tr(lang, "This window can be closed: go back to Telegram.",
                   "Это окно можно закрыть и вернуться в Telegram."))


PAGE = """<!doctype html>
<html lang="{lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HSE App X</title>
<style>
  body {{ margin: 48px auto; max-width: 480px; padding: 0 16px; font: 17px/1.5 -apple-system, system-ui, sans-serif;
         color: #111; background: #fff; }}
  @media (prefers-color-scheme: dark) {{ body {{ color: #eee; background: #111; }} }}
  h1 {{ font-size: 22px; }}
</style>
</head>
<body>
<h1>{title}</h1>
<p>{text}</p>
<p>{hint}</p>
</body>
</html>
"""


# --- summaries --------------------------------------------------------------------------------

def _summaries_state(request: web.Request) -> dict:
    topic = request.app[SUMMARIES_KEY]
    return {"time": request.app[SETTINGS_KEY].get("summary_time"), "status": topic.status(),
            "chats": [{"id": chat["id"], "title": chat["title"]}
                      for chat in (topic.inbox.chat(chat_id) for chat_id in topic.summaries.daily_chats())]}


async def summaries(request: web.Request) -> web.Response:
    """The Сводки screen: the time of the daily summary, its chats and how the last one went."""
    return respond(request, _summaries_state(request))


async def update_summaries(request: web.Request) -> web.Response:
    """{time: "HH:MM"}: the daily summary's time; the loop waits for the new one at once."""
    values = await _json(request)
    if set(values) != {"time"}:
        raise web.HTTPBadRequest(text="expected {time}")
    try:
        request.app[SETTINGS_KEY].set("summary_time", values["time"])
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    request.app[SUMMARIES_KEY].reschedule()
    return respond(request, _summaries_state(request))


# --- the twin's account ------------------------------------------------------------------------

async def twin_login(request: web.Request) -> web.Response:
    """{phone}: ask for a code; {code}, then {password} if the account has one: sign in. Answers the twin's status."""
    values, twin = await _json(request), request.app[TWIN_KEY]
    try:
        if set(values) == {"phone"}:
            await twin.send_code(str(values["phone"]))
        elif set(values) in ({"code"}, {"password"}):
            await twin.sign_in(code=values.get("code"), password=values.get("password"))
        else:
            raise web.HTTPBadRequest(text="expected {phone}, {code} or {password}")
    except LoginError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    return respond(request, twin.status())


async def twin_cancel(request: web.Request) -> web.Response:
    await request.app[TWIN_KEY].cancel_login()
    return respond(request, request.app[TWIN_KEY].status())


async def twin_logout(request: web.Request) -> web.Response:
    try:
        await request.app[TWIN_KEY].logout()
    except LoginError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    return respond(request, request.app[TWIN_KEY].status())


# --- general settings ---------------------------------------------------------------------------

def _general_state(request: web.Request) -> dict:
    settings = request.app[SETTINGS_KEY]
    return {**{name: settings.get(name) for name in GENERAL}, "batch_choices": list(BATCH_HOURS),
            "languages": list(LANGUAGES)}


async def general(request: web.Request) -> web.Response:
    return respond(request, _general_state(request))


async def update_general(request: web.Request) -> web.Response:
    """Any of batch_hours, news_ttl_hours, llm_prices and the two languages; all are checked before any is
    saved."""
    values = await _json(request)
    if not values or set(values) - set(GENERAL):
        raise web.HTTPBadRequest(text=f"expected some of {', '.join(GENERAL)}")
    try:
        checked = {name: SPECS[name][1](value) for name, value in values.items()}
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    for name, value in checked.items():
        request.app[SETTINGS_KEY].set(name, value)
    return respond(request, _general_state(request))


# --- LLM spending -----------------------------------------------------------------------------

def _llm_state(request: web.Request) -> dict:
    budget, inbox, lang = request.app[BUDGET_KEY], request.app[INBOX_KEY], language(request)
    now = budget.now()  # one clock with the limit

    def chat_title(chat_id: int) -> str:
        if chat_id == MAILBOX:
            return tr(lang, "Mail", "Почта")
        chat = inbox.chat(chat_id)
        return chat["title"] if chat else tr(lang, f"chat {chat_id}", f"чат {chat_id}")

    periods = {}
    for name, totals in request.app[JOURNAL_KEY].usage(now).items():
        by_task, by_chat = totals.pop("by_task"), totals.pop("by_chat")
        periods[name] = {**totals, "by_task": [{"task": task, **row} for task, row in by_task.items()],
                         "by_chat": [{"id": chat_id, "title": chat_title(chat_id), **row}
                                     for chat_id, row in by_chat.items()]}
    today = clock.local_day(now)
    month_days = calendar.monthrange(today.year, today.month)[1]
    stopped = budget.stopped_days()
    days = [{**day, "stopped": day["day"] in stopped}
            for day in request.app[JOURNAL_KEY].daily(now, CHART_DAYS)]
    return {**budget.status(), "daily_limit_usd": budget.settings.get("llm_daily_limit_usd"),
            "soft_share": SOFT_SHARE, "periods": periods, "days": days,
            "month_forecast_usd": round(periods["month"]["cost_usd"] / today.day * month_days, 4)}


async def llm(request: web.Request) -> web.Response:
    """The «Расход LLM» screen."""
    return respond(request, _llm_state(request))


async def update_llm(request: web.Request) -> web.Response:
    """{daily_limit_usd}: the limit of every day; today's raise stays on top of it."""
    values = await _json(request)
    if set(values) != {"daily_limit_usd"}:
        raise web.HTTPBadRequest(text="expected {daily_limit_usd}")
    try:
        request.app[SETTINGS_KEY].set("llm_daily_limit_usd", values["daily_limit_usd"])
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    request.app[BUDGET_KEY].limit_changed()
    return respond(request, _llm_state(request))


async def raise_llm(request: web.Request) -> web.Response:
    """One more daily limit until midnight, as the button in «Система»."""
    budget = request.app[BUDGET_KEY]
    budget.raise_today(budget.settings.get("llm_daily_limit_usd"))
    return respond(request, _llm_state(request))


# --- grades -----------------------------------------------------------------------------------

def _grades_state(request: web.Request) -> dict:
    grades, status = request.app[GRADES_KEY], request.app[SOURCES_KEY].get("grades", dict)()
    read = status.get("sheets") or {}
    sheets = [{"id": sid, "url": sheet["url"], "label": sheet["label"], "name": sheet.get("name", ""),
               **read.get(sid, {}), "grades": grades.items(sid)}
              for sheet in grades.settings.get("grades_sheets") for sid in [sheet_id(sheet["url"])]]
    return {**{key: grades.settings.get(name) for key, name in GRADE_SETTINGS.items() if key != "sheets"},
            "llm": status.get("llm", True), "sheets": sheets, "manual": grades.manual(),
            "label_chars": SHEET_LABEL_CHARS}


async def grade_sheets(request: web.Request) -> web.Response:
    """The Оценки screen: the name, the sheets with what was read in them, the window of new grades."""
    return respond(request, _grades_state(request))


async def update_grades(request: web.Request) -> web.Response:
    """{student?, sheets?: [{url, label, name?}], window_days?}: all checked before any is saved; a sheet removed, or
    all of them for a new name, start again from a baseline. The sheets are read at once."""
    values = await _json(request)
    if not values or set(values) - set(GRADE_SETTINGS):
        raise web.HTTPBadRequest(text="expected {student?, sheets?, window_days?}")
    try:
        request.app[GRADES_KEY].configure({GRADE_SETTINGS[key]: value for key, value in values.items()})
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return respond(request, _grades_state(request))


def _configured(request: web.Request) -> tuple[list[dict], dict]:
    """The grade sheets as set, and the one of the request by its id; 404 for a sheet not set."""
    sheets = [dict(sheet) for sheet in request.app[GRADES_KEY].settings.get("grades_sheets")]
    found = next((sheet for sheet in sheets if sheet_id(sheet["url"]) == request.match_info["sid"]), None)
    if found is None:
        raise web.HTTPNotFound(text="no such grade sheet")
    return sheets, found


def _gradebook_state(request: web.Request) -> dict:
    """A gradebook's screen (D44): its name and course, how its last read went, and each tab with the own row's
    cells, the works laid out in them, and whose the layout is."""
    grades, (_, sheet) = request.app[GRADES_KEY], _configured(request)
    sid = request.match_info["sid"]
    read = (request.app[SOURCES_KEY].get("grades", dict)().get("sheets") or {}).get(sid, {})
    view = grades.view(sid)
    course = view["course"] or short_course(sheet["label"], read.get("title"), sid)
    return {"id": sid, "url": sheet["url"], "label": sheet["label"], "name": sheet.get("name", ""),
            "title": read.get("title"), "ok_at": read.get("ok_at"), "error": read.get("error"),
            "student": grades.settings.get("grades_student"), "course": course, "label_chars": SHEET_LABEL_CHARS,
            "label_budget": budget(course), "items": len(grades.items(sid)), "tabs": view["tabs"]}


async def gradebook(request: web.Request) -> web.Response:
    return respond(request, _gradebook_state(request))


async def update_gradebook(request: web.Request) -> web.Response:
    """{name?, label?}: the owner's name of a gradebook and the course in front of its grades."""
    values = await _json(request)
    if not values or set(values) - {"name", "label"}:
        raise web.HTTPBadRequest(text="expected {name?, label?}")
    sheets, sheet = _configured(request)
    sheet.update(values)
    try:
        request.app[GRADES_KEY].configure({"grades_sheets": sheets})
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return respond(request, _gradebook_state(request))


async def read_gradebook(request: web.Request) -> web.Response:
    """«Прочитать сейчас»: the gradebooks are read at once, not in 30 minutes."""
    _configured(request)
    grades = request.app[GRADES_KEY]
    if grades.wake is not None:
        grades.wake()
    return respond(request, _gradebook_state(request))


def _layout_items(values: dict) -> tuple[bool, list[dict]]:
    """{skip, works: [{column, label, max, bonus_column}]} of the owner's layout of a tab."""
    listed = values.get("works")
    if set(values) != {"skip", "works"} or not isinstance(values["skip"], bool) or not isinstance(listed, list):
        raise web.HTTPBadRequest(text="expected {skip, works: [{column, label, max, bonus_column}]}")
    items, columns = [], set()
    for work in listed:
        if not isinstance(work, dict) or set(work) != {"column", "label", "max", "bonus_column"} or \
                not isinstance(work["column"], str) or not isinstance(work["label"], str) or \
                isinstance(work["max"], bool) or not isinstance(work["max"], int | float) or work["max"] <= 0 or \
                not isinstance(work["bonus_column"], str | None) or work["column"] in columns:
            raise web.HTTPBadRequest(text="a work: {column, label, max > 0, bonus_column or null}, each column once")
        columns.add(work["column"])
        items.append({**work, "label": " ".join(work["label"].split())})
    return values["skip"], items


async def set_layout(request: web.Request) -> web.Response:
    """The owner's layout of a tab: it stands over Haiku's; the gradebook is read again at once."""
    _configured(request)
    skip, items = _layout_items(await _json(request))
    try:
        found = request.app[GRADES_KEY].set_layout(request.match_info["sid"], request.match_info["tab"], skip, items)
    except ValueError as e:
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    if found is None:
        raise web.HTTPNotFound(text="no such tab")
    return respond(request, _gradebook_state(request))


async def clear_layout(request: web.Request) -> web.Response:
    """The tab given back to Haiku."""
    _configured(request)
    if request.app[GRADES_KEY].clear_layout(request.match_info["sid"], request.match_info["tab"]) is None:
        raise web.HTTPNotFound(text="no such tab")
    return respond(request, _gradebook_state(request))


def _grade_fields(request: web.Request, values: dict, names: set[str]) -> dict:
    fields = {}
    for name, limit in (("course", SHEET_LABEL_CHARS), ("label", GRADE_LABEL_LIMIT)):
        if name in names:
            value = values.get(name)
            if not isinstance(value, str) or not " ".join(value.split()) or len(value) > limit:
                raise bad_request(request, f"{name}: up to {limit} characters", f"{name}: до {limit} символов")
            fields[name] = " ".join(value.split())
    value = values.get("value")
    if isinstance(value, bool) or not isinstance(value, int | float) or not 0 <= value <= 10:
        raise bad_request(request, "a grade from 0 to 10", "оценка от 0 до 10")
    fields["value"] = round(float(value), 2)
    return fields


async def add_grade(request: web.Request) -> web.Response:
    """{course, label, value}: a grade added by hand."""
    values = await _json(request)
    if set(values) != {"course", "label", "value"}:
        raise web.HTTPBadRequest(text="expected {course, label, value}")
    fields = _grade_fields(request, values, {"course", "label"})
    request.app[GRADES_KEY].add_grade(fields["course"], fields["label"], fields["value"])
    return respond(request, _grades_state(request))


async def change_grade(request: web.Request) -> web.Response:
    """{value, label?, course?}: a grade added by hand changed, or a sheet's grade changed by the owner."""
    values = await _json(request)
    if "value" not in values or set(values) - {"value", "label", "course"}:
        raise web.HTTPBadRequest(text="expected {value, label?, course?}")
    fields = _grade_fields(request, values, set(values) & {"label", "course"})
    if not request.app[GRADES_KEY].change_grade(request.match_info["grade"], fields.get("label"), fields["value"],
                                                fields.get("course")):
        raise web.HTTPNotFound(text="no such grade")
    return respond(request, _grades_state(request))


async def drop_grade(request: web.Request) -> web.Response:
    """A grade added by hand deleted, or a sheet's grade back as the sheet has it."""
    if not request.app[GRADES_KEY].drop_grade(request.match_info["grade"]):
        raise web.HTTPNotFound(text="no such grade")
    return respond(request, _grades_state(request))


def page(lang: str, title: str, text: str, hint: str, status: int = 200) -> web.Response:
    body = PAGE.format(lang=lang, title=html.escape(title), text=html.escape(text), hint=html.escape(hint))
    return web.Response(text=body, status=status, content_type="text/html",
                        headers={"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"})


def create_app(env: Env, budget: Budget, journal: Journal, inbox: Inbox, chat_settings: ChatSettings,
               sources: Sources, dictionary: Subjects, timetable: Schedule, hse: HseApp,
               merged: DeadlineList, settings: Settings | None = None,
               sheet_topic: SheetTopic | None = None, summary_topic: SummaryTopic | None = None,
               grade_list: Grades | None = None, twin: Twin | None = None,
               calendar_feed: ical_feed.CalendarFeed | None = None) -> web.Application:
    """The Mini App's port; without the settings and the sheets' topic the Листочки screen has no API, without the
    settings and the summaries' topic the Сводки screen has none, without the grades the Оценки screen has none,
    without the calendar feed there are no public feeds."""
    app = web.Application(middlewares=[owner_only], client_max_size=UPLOAD_MAX_SIZE + 2**20)  # an own sheet's PDF
    app[ENV_KEY], app[BUDGET_KEY], app[JOURNAL_KEY] = env, budget, journal
    app[INBOX_KEY], app[CHAT_SETTINGS_KEY], app[SOURCES_KEY] = inbox, chat_settings, sources
    app[SUBJECTS_KEY], app[SCHEDULE_KEY], app[HSEAPP_KEY], app[DEADLINES_KEY] = dictionary, timetable, hse, merged
    app[SETTINGS_KEY], app[SHEETS_KEY], app[SUMMARIES_KEY] = settings, sheet_topic, summary_topic
    app.router.add_get("/app", to_index)
    app.router.add_get("/app/", index)
    app.router.add_get("/app/api/me", me)
    app.router.add_get("/app/api/chats", chats)
    app.router.add_patch("/app/api/chats/{chat_id}", update_chat)
    app.router.add_get("/app/api/chats/{chat_id}/senders", senders)
    if twin is not None:
        app[TWIN_KEY] = twin
        app.router.add_post("/app/api/twin/login", twin_login)
        app.router.add_post("/app/api/twin/cancel", twin_cancel)
        app.router.add_post("/app/api/twin/logout", twin_logout)
    app.router.add_get("/app/api/llm", llm)
    app.router.add_post("/app/api/llm/raise", raise_llm)
    if settings is not None:
        app.router.add_patch("/app/api/images", update_images)
        app.router.add_patch("/app/api/llm", update_llm)
        app.router.add_get("/app/api/general", general)
        app.router.add_patch("/app/api/general", update_general)
    app.router.add_get("/app/api/subjects", subjects)
    app.router.add_put("/app/api/subjects", put_subject)
    app.router.add_delete("/app/api/subjects", delete_subject)
    app.router.add_get("/app/api/agenda", agenda)
    app.router.add_post("/app/api/events", add_event)
    app.router.add_patch("/app/api/events/{event_id}", update_event)
    app.router.add_delete("/app/api/events/{event_id}", delete_event)
    app.router.add_post("/app/api/corrections", correct_class)
    app.router.add_get("/app/api/deadlines", deadlines)
    app.router.add_post("/app/api/deadlines", add_deadline)
    app.router.add_patch("/app/api/deadlines/{uid}", edit_deadline)
    app.router.add_delete("/app/api/deadlines/{uid}", delete_deadline)
    app.router.add_delete("/app/api/hidden/{uid}", unhide_deadline)
    if settings is not None and sheet_topic is not None:
        app.router.add_get("/app/api/sheets", sheets)
        app.router.add_patch("/app/api/sheets", update_sheets)
        app.router.add_post("/app/api/sheets", add_sheet)
        app.router.add_patch("/app/api/sheets/{sheet_id}", edit_sheet)
        app.router.add_delete("/app/api/sheets/{sheet_id}", delete_sheet)
        app.router.add_post("/app/api/sheets/{sheet_id}/join", join_sheet)
    if settings is not None and summary_topic is not None:
        app.router.add_get("/app/api/summaries", summaries)
        app.router.add_patch("/app/api/summaries", update_summaries)
    if grade_list is not None:
        app[GRADES_KEY] = grade_list
        app.router.add_get("/app/api/grades", grade_sheets)
        app.router.add_patch("/app/api/grades", update_grades)
        app.router.add_post("/app/api/grades/items", add_grade)
        app.router.add_patch("/app/api/grades/items/{grade}", change_grade)
        app.router.add_delete("/app/api/grades/items/{grade}", drop_grade)
        app.router.add_get("/app/api/grades/sheets/{sid}", gradebook)
        app.router.add_patch("/app/api/grades/sheets/{sid}", update_gradebook)
        app.router.add_post("/app/api/grades/sheets/{sid}/read", read_gradebook)
        app.router.add_put("/app/api/grades/sheets/{sid}/tabs/{tab}", set_layout)
        app.router.add_delete("/app/api/grades/sheets/{sid}/tabs/{tab}", clear_layout)
    app.router.add_get("/app/api/hseapp", hseapp)
    app.router.add_patch("/app/api/hseapp", update_hseapp)
    app.router.add_post("/app/api/hseapp/link", hseapp_link)
    app.router.add_post("/app/api/hseapp/login", hseapp_login)
    app.router.add_get(CALLBACK_PATH, hseapp_callback)
    if calendar_feed is not None:
        ical_feed.add_routes(app, env.calendar_public_secret, calendar_feed, ical_feed.PUBLIC_FEEDS)
    return app
