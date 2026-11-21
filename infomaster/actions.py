"""The owner's actions and readings, shaped as aiohttp handlers (§8.5, §8.6, D69).

They were the Mini App's API; the Mini App is gone (D69) and its handlers stay as the one place of the rules: the
connector's tools call them (connector/tools.py, `call_action`), and so do the web app's screens and «Machine»,
through the tools or directly for what has no tool. An own sheet's upload, its deletion and joining are the shelves'
(web/shelves.py) and the connector's, on SheetPublisher itself. A handler reads the services from the public port's
app by the keys below and answers JSON, a refusal as a 400 or 404 with its text, in the app's language (D41).

What they cover: the merged deadlines with the owner's own ones and the hidden ones (§5.4, §5.5), the timetable as the
feeds have it with the owner's own events and corrections (§6.5), the grades, the sheets' fields (§6.7, §6.3), and the
settings: the twin's account with its login and logout, the chats and their
settings (D12), the course dictionary (D3, D33), the HSE App X login (§5.2, D7), the limits of reading a PDF, the
gradebooks, the LLM spending and its daily limit (§7, D40), the general settings (D41, D52).

The public port (`public_app`) also serves /hseapp/callback, where the browser comes back after the HSE login: the
PKCE state of the login link is its protection, and it answers with a short page in the app's language. And
/calendar/<CALENDAR_PUBLIC_SECRET>/main.ics and optional.ics for one's own calendar (see ical_feed, D43), and the
library's signed links.
"""

import calendar
import datetime as dt
import html
import json
import re
from functools import partial

from aiohttp import web

from . import clock
from .collectors.files import UPLOAD_MAX_SIZE
from .collectors.grades import sheet_id, short_course
from .collectors.hseapp import CALLBACK_PATH, HseApp
from .collectors.inbox import MAILBOX, Inbox, message_link
from .collectors.telegram import LoginError, Twin
from .config import Env
from .i18n import LANGUAGES, json_default, reason, tr
from .library.links import Links
from .library.links import add_routes as add_library_routes
from .llm.budget import SOFT_SHARE, Budget
from .llm.journal import Journal
from .outputs import ical_feed
from .outputs.api import SOURCES_KEY, Sources
from .outputs.reminders import DeadlineReminders
from .outputs.sheets import SheetPublisher
from .processing.grades import Grades, budget
from .processing.merge import DeadlineList
from .processing.schedule import Schedule, message_ref
from .processing.sheets import SUMMARY_CHARS
from .processing.subjects import ABBR_CHARS, TITLE_CHARS, SubjectError, Subjects
from .processing.todo import CATEGORIES, uid
from .settings import BATCH_HOURS, SHEET_LABEL_CHARS, SPECS, ChatSettings, Settings, SettingsError

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
SHEETS_KEY = web.AppKey("sheets", SheetPublisher)
REMINDERS_KEY = web.AppKey("reminders", DeadlineReminders)
GRADES_KEY = web.AppKey("grades", Grades)
TWIN_KEY = web.AppKey("twin", Twin)
GENERAL = ("batch_hours", "news_ttl_hours", "llm_prices", "language",
           "about_me", "wiki_pages")  # of no other group
GRADE_SETTINGS = {"student": "grades_student", "sheets": "grades_sheets", "window_days": "grades_window_days"}
PDF_LIMITS = ("pdf_pages", "pdf_tokens")
TITLE_LIMIT = 200  # characters of an own deadline's title
COURSE_KEY_LIMIT = 200  # an own deadline's course key (the web app, §8.6)
SHEET_COURSE_LIMIT, SHEET_NUMBER_LIMIT = 60, 60  # the number, or a short title instead
GRADE_LABEL_LIMIT = 40
EVENT_TITLE_LIMIT = 120
ROOM_LIMIT = 300  # a room, or a meeting's link
EVENT_FIELDS = ("title", "kind", "start", "end", "room", "required", "cancelled")
NEW_EVENT = {"title", "kind", "start", "end", "room", "required"}  # what a new event needs; cancelled may come too
KIND_LIMIT = 40  # the kind is the type of the feeds once processing/schedule.py's type_of has read it
CHART_DAYS = 30


def language(request: web.Request) -> str:
    """The app's language (D41); English where the app has no settings."""
    settings = request.app.get(SETTINGS_KEY)
    return settings.get("language") if settings is not None else "en"


def respond(request: web.Request, data) -> web.Response:
    """JSON with every Phrase in it (a status error, a letter's subject) in the app's language."""
    return web.json_response(data, dumps=partial(json.dumps, default=json_default(language(request))))


def bad_request(request: web.Request, en: str, ru: str) -> web.HTTPBadRequest:
    return web.HTTPBadRequest(text=tr(language(request), en, ru))


async def chats(request: web.Request) -> web.Response:
    """The chats: every chat of the twin with its settings, enabled ones first."""
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
    """The course dictionary: every entry; Haiku's can be overridden by hand, manual ones edited or deleted."""
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
    """The timetable: the classes with their corrections and the events, as the feeds have them, from today on;
    every change answers it anew."""
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
    """The checked `names` of an event: title, kind, start and end (Moscow), room, required,
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
        day = values.get("start") if isinstance(values.get("start"), str) else ""
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}", day):  # all day: a date, no end (D46)
            if values.get("end"):
                raise bad_request(request, "an all-day event has no end", "у события на весь день нет конца")
            try:
                fields["start"], fields["end"] = dt.date.fromisoformat(day).isoformat(), None
            except ValueError:
                raise bad_request(request, "a date like 2026-10-22", "дата вида 2026-10-22") from None
        else:
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
    """{title, kind, start, end, room, required, cancelled?}: an own event; a start that is a date alone and no end
    make it an all-day one."""
    values = await _json(request)
    request.app[SCHEDULE_KEY].add_event(_event_fields(request, values, NEW_EVENT | ({"cancelled"} & set(values))))
    return respond(request, _agenda_state(request))


async def update_event(request: web.Request) -> web.Response:
    """{required?, hidden?} for the newest version of an event, as the web app's toggles; or some of {title, kind,
    start, end, room, required, cancelled}: the owner's change of an event of its own (not a correction)."""
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
    """{class_uid, start, end, room, cancelled, title?, kind?, required?}: the owner changes a RUZ class as any other
    entry (D49): moves, cancels, renames it, gives it another type or the optional calendar."""
    values = await _json(request)
    fields = _event_fields(request, values, {"start", "end", "room", "cancelled"} |
                           ({"title", "kind", "required"} & set(values)))
    if not isinstance(values.get("class_uid"), str) or await request.app[SCHEDULE_KEY].correct_class(
            values["class_uid"], fields["start"], fields["end"], fields["room"], fields["cancelled"],
            title=fields.get("title"), kind=fields.get("kind"), required=fields.get("required", True)) is None:
        raise web.HTTPNotFound(text="no such class")
    return respond(request, _agenda_state(request))


# --- a source message (D50) -------------------------------------------------------------------

def _message_view(message: dict) -> dict:
    return {"sender": message["sender_name"], "at": message["at"], "text": message["text"],
            "files": [{"kind": f.get("kind"), "name": f.get("name")} for f in message["files"]],
            "forward": (message["forward"] or {}).get("from"), "edited": message["edited_at"] is not None,
            "deleted": message["deleted_at"] is not None}


async def message(request: web.Request) -> web.Response:
    """?chat=<chat_id>&msg=<msg_id>: a message as the twin read it (a letter for chat 0), with what it answers and its
    own t.me link, for a chat your own account is not in. Kept 14 days (D29): after that only the chat and the link."""
    try:
        chat_id, msg_id = int(request.query["chat"]), int(request.query["msg"])
    except (KeyError, ValueError):
        raise web.HTTPBadRequest(text="expected ?chat=<id>&msg=<id>") from None
    inbox = request.app[INBOX_KEY]
    source = "mail" if chat_id == MAILBOX else "tg"
    chat = inbox.chat(chat_id) if source == "tg" else None
    if source == "tg" and chat is None:
        raise web.HTTPNotFound(text="no such chat")
    stored = inbox.message(chat_id, msg_id, source)
    about = {"id": chat["id"], "title": chat["title"], "kind": chat["kind"]} if chat else None
    body = {"source": source, "chat": about, "link": message_link(chat, msg_id) if chat else None,
            "stored": stored is not None,
            "subject": stored["subject"] if stored else None, "message": _message_view(stored) if stored else None,
            "reply": None}
    if stored and stored["reply_to"]:
        replied = inbox.message(chat_id, stored["reply_to"], source)
        body["reply"] = _message_view(replied) if replied else {"text": stored["reply_quote"] or "", "sender": None}
    elif stored and stored["reply_quote"]:
        body["reply"] = {"text": stored["reply_quote"], "sender": None}
    return respond(request, body)


# --- deadlines -------------------------------------------------------------------------------

def _origin(request: web.Request, d) -> dict:
    """The chat or the letter a deadline of the chats or the mail came from, with the link to its message."""
    row = request.app[DEADLINES_KEY].deadlines.get(int(d.ref[3:])) if d.ref.startswith("dl:") else None
    if row is None or row["source"] not in ("tg", "mail"):
        return {"from": None, "link": None, "message": None}
    timetable = request.app[SCHEDULE_KEY]
    return {"from": timetable.told_by(row), "link": timetable.link(row), "message": message_ref(row)}


def _deadlines_state(request: web.Request) -> dict:
    """The deadlines: the merged list as Tablo and HA get it, the own ones, the hidden ones."""
    merged = request.app[DEADLINES_KEY]
    today = merged.today()
    items = [{"uid": uid(d), "title": d.title, "due": d.due.isoformat(), "category": d.category, "source": d.source,
              "course": d.name, "overdue": d.due < today, "assumed": d.assumed, **_origin(request, d)}
             for d in merged.visible()]
    own = [{"uid": d.key, "title": d.title, "due": d.due.isoformat(), "category": d.category}
           for d in merged.todo.own()]
    hidden = [{"uid": row["uid"], "title": row["title"], "due": row["due"], "source": row["source"]}
              for row in merged.todo.hidden_rows()]
    hseapp = request.app[SOURCES_KEY].get("hseapp", dict)()
    return {"items": items, "own": own, "hidden": hidden, "today": today.isoformat(),
            "hseapp": {"login_needed": bool(hseapp.get("login_needed")), "error": hseapp.get("error")}}


def _own_fields(request: web.Request, values: dict, some: bool) -> dict:
    """title, due and category of an own deadline; every one of them, unless `some`."""
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
    if not some or "category" in values:
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
    """{title, due, category, course?}: a new own deadline; `course`, its key, comes from the web app (§8.6)."""
    values = await _json(request)
    fields = _own_fields(request, values, some=False)
    course = values.get("course")
    if course is not None and (not isinstance(course, str) or len(course) > COURSE_KEY_LIMIT):
        raise web.HTTPBadRequest(text=f"course: up to {COURSE_KEY_LIMIT} characters")
    request.app[DEADLINES_KEY].add(fields["title"], fields["due"], fields["category"], course)
    return respond(request, _deadlines_state(request))


async def edit_deadline(request: web.Request) -> web.Response:
    """{title?, due?, category?}: an own one changes; one from a source is hidden and an own copy takes its place.
    A sheet's assumed date (§6.3) given another date is the sheet's real date: with the title and list as they are,
    the deadline itself moves to it and stays the chats'; the sheet takes the date either way."""
    fields = _own_fields(request, await _json(request), some=True)
    if not fields:
        raise web.HTTPBadRequest(text="expected {title?, due?, category?}")
    merged, d = request.app[DEADLINES_KEY], _listed(request)
    sheet_id = _assumed_sheet(request, d) if "due" in fields else None
    if sheet_id is None or fields.get("title", d.title) != d.title or fields.get("category", d.category) != d.category:
        merged.edit(d, fields.get("title"), fields.get("due"), fields.get("category"))
    if sheet_id is not None:
        await request.app[SHEETS_KEY].edit(sheet_id, {"due": fields["due"].isoformat()})
    return respond(request, _deadlines_state(request))


def _assumed_sheet(request: web.Request, d) -> int | None:
    """The sheet whose assumed date a deadline is, when the sheets are wired."""
    if not d.assumed or not d.ref.startswith("dl:") or request.app.get(SHEETS_KEY) is None:
        return None
    row = request.app[DEADLINES_KEY].deadlines.get(int(d.ref[3:]))
    return row["sheet_id"] if row is not None else None


async def delete_deadline(request: web.Request) -> web.Response:
    """An own one is deleted, one from a source is hidden for good (as in HA)."""
    request.app[DEADLINES_KEY].remove(_listed(request))
    return respond(request, _deadlines_state(request))


async def unhide_deadline(request: web.Request) -> web.Response:
    """«Restore»: one hidden row less; the deadline is back if its source still has it."""
    if not request.app[DEADLINES_KEY].unhide(request.match_info["uid"]):
        raise web.HTTPNotFound(text=tr(language(request), "no such hidden deadline", "такого скрытого дедлайна нет"))
    return respond(request, _deadlines_state(request))


# --- sheets -----------------------------------------------------------------------------------

def _sheets_state(request: web.Request) -> dict:
    settings = request.app[SETTINGS_KEY]
    return {"sheets": request.app[SHEETS_KEY].sheets.listing(), "status": request.app[SOURCES_KEY]["sheets"](),
            "limits": {name: settings.get(name) for name in PDF_LIMITS}}


async def sheets(request: web.Request) -> web.Response:
    """The sheets: with their versions and the limits of reading them."""
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


# --- HSE App X ------------------------------------------------------------------------------

def _hseapp_state(request: web.Request) -> dict:
    """HSE App X: the source's status (never the tokens) and where the browser comes back to."""
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
                    tr(lang, "Go back to infomaster and log in again: Machine → HSE App X.",
                       "Вернитесь в infomaster и войдите ещё раз: Машина → HSE App X."), status=400)
    return page(lang, tr(lang, "Logged in to HSE App X", "Вход в HSE App X выполнен"),
                tr(lang, "The deadlines will be updated within a minute.", "Дедлайны обновятся в течение минуты."),
                tr(lang, "This window can be closed: go back to infomaster.",
                   "Это окно можно закрыть и вернуться в infomaster."))


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


# --- deadline reminders ------------------------------------------------------------------------

REMINDER_FIELDS = {"enabled": "deadline_reminders", "eve": "reminder_eve", "day": "reminder_day"}


def _reminders_state(request: web.Request) -> dict:
    settings = request.app[SETTINGS_KEY]
    return {key: settings.get(name) for key, name in REMINDER_FIELDS.items()}


async def reminders(request: web.Request) -> web.Response:
    """The reminders (D47): on or off, the eve's time and the morning's."""
    return respond(request, _reminders_state(request))


async def update_reminders(request: web.Request) -> web.Response:
    """Some of {enabled, eve: "HH:MM", day: "HH:MM"}; the loop waits for the new times at once."""
    values = await _json(request)
    if not values or not set(values) <= set(REMINDER_FIELDS):
        raise web.HTTPBadRequest(text="expected some of enabled, eve, day")
    settings = request.app[SETTINGS_KEY]
    try:
        for key in REMINDER_FIELDS:  # all checked before any is kept
            if key in values:
                SPECS[REMINDER_FIELDS[key]][1](values[key])
        for key, value in values.items():
            settings.set(REMINDER_FIELDS[key], value)
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    request.app[REMINDERS_KEY].reschedule()
    return respond(request, _reminders_state(request))


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
    """Any of batch_hours, news_ttl_hours, llm_prices, the two languages, about_me and wiki_pages; all are checked
    before any is saved."""
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
    """The LLM spending: today, the week, the month, by task and chat, and the limit."""
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
    """One more daily limit until midnight («Machine → LLM»)."""
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
    """The grades: the name, the sheets with what was read in them, the window of new grades."""
    return respond(request, _grades_state(request))


async def update_grades(request: web.Request) -> web.Response:
    """{student?, sheets?: [{url, label, name?}], window_days?}: all checked before any is saved; a sheet removed, or
    all of them for a new name, start again from a baseline. The sheets are read at once."""
    values = await _json(request)
    if not values or set(values) - set(GRADE_SETTINGS):
        raise web.HTTPBadRequest(text="expected {student?, sheets?, window_days?}")
    try:
        request.app[GRADES_KEY].configure({GRADE_SETTINGS[key]: value for key, value in values.items()})
    except SettingsError as e:  # a sheet's link: a Phrase
        raise web.HTTPBadRequest(text=reason(e, language(request))) from None
    return respond(request, _grades_state(request))


def _configured(request: web.Request) -> tuple[list[dict], dict]:
    """The grade sheets as set, and the one of the request by its id; 404 for a sheet not set."""
    sheets = [dict(sheet) for sheet in request.app[GRADES_KEY].settings.get("grades_sheets")]
    found = next((sheet for sheet in sheets if sheet_id(sheet["url"]) == request.match_info["sid"]), None)
    if found is None:
        raise web.HTTPNotFound(text="no such grade sheet")
    return sheets, found


def _gradebook_state(request: web.Request) -> dict:
    """A gradebook (D44): its name and course, how its last read went, and each tab with the own row's
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
    """«Read now»: the gradebooks are read at once, not in 30 minutes."""
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


def public_app(env: Env, budget: Budget, journal: Journal, inbox: Inbox, chat_settings: ChatSettings,
               sources: Sources, dictionary: Subjects, timetable: Schedule, hse: HseApp,
               merged: DeadlineList, *, settings: Settings | None = None,
               sheets: SheetPublisher | None = None, grade_list: Grades | None = None, twin: Twin | None = None,
               calendar_feed: ical_feed.CalendarFeed | None = None,
               deadline_reminders: DeadlineReminders | None = None,
               library_links: Links | None = None) -> web.Application:
    """The public port's app with the services under their keys, for the actions here, the connector and the web app
    (which attach themselves to it); the HSE App X callback; the library's links (/library/<token>) with them; the
    feeds for one's own calendar with the calendar feed."""
    app = web.Application(client_max_size=UPLOAD_MAX_SIZE + 2**20)  # MCP bodies: a source up to 2M chars inline
    app[ENV_KEY], app[BUDGET_KEY], app[JOURNAL_KEY] = env, budget, journal
    app[INBOX_KEY], app[CHAT_SETTINGS_KEY], app[SOURCES_KEY] = inbox, chat_settings, sources
    app[SUBJECTS_KEY], app[SCHEDULE_KEY], app[HSEAPP_KEY], app[DEADLINES_KEY] = dictionary, timetable, hse, merged
    app[SETTINGS_KEY], app[SHEETS_KEY] = settings, sheets
    if twin is not None:
        app[TWIN_KEY] = twin
    if deadline_reminders is not None:
        app[REMINDERS_KEY] = deadline_reminders
    if grade_list is not None:
        app[GRADES_KEY] = grade_list
    if library_links is not None:
        add_library_routes(app, library_links)  # the library and its links under LIBRARY_KEY and LINKS_KEY
    app.router.add_get(CALLBACK_PATH, hseapp_callback)
    if calendar_feed is not None:
        ical_feed.add_routes(app, env.calendar_public_secret, calendar_feed, ical_feed.PUBLIC_FEEDS)
    return app
