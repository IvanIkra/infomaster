"""Mini App on the public port, the only one behind Funnel (§8.1, §8.4).

Every /app/api/ request carries Telegram's initData in `Authorization: tma <initData>`. It is signed with the bot
token, and only the owner's Telegram id gets in (р. 16).

Screens: the chats and their settings with the daily number of images read in them (р. 12), the course dictionary
with manual entries (р. 3, р. 33), the timetable's events from chats and corrections of classes with the main/optional
switch and hiding (§6.5), the merged deadlines with the owner's own ones and the hidden ones (§5.4, §5.5: the same
list and the same actions as HA's to-do lists), the HSE App X login (§5.2, р. 7), the sheets with their versions, the
limits of reading them and the joining of a sheet posted as new to an older one (§6.3, р. 14), the summaries: the time
of the daily one and its chats (§6.6), the grades: the name, the sheets and what was read in them (§6.7), the LLM
spending (§7, р. 40): today, this week and this month by task and by chat, the cost of each of the last 30 days and
the daily limit, which can be changed or raised for today. Tablo and HA show no spending: this screen is the one place.

The same port serves /hseapp/callback, where the browser comes back after the HSE login. It has no initData: the
PKCE state of the login link is its protection, and it answers with a short page in Russian.
"""

import calendar
import datetime as dt
import hashlib
import hmac
import html
import json
import time
from pathlib import Path
from urllib.parse import parse_qsl

from aiohttp import web

from .. import clock
from ..collectors.grades import sheet_id
from ..collectors.hseapp import CALLBACK_PATH, HseApp
from ..collectors.inbox import MAILBOX, Inbox
from ..config import Env
from ..llm.budget import SOFT_SHARE, Budget
from ..llm.journal import Journal
from ..processing.grades import Grades
from ..processing.merge import DeadlineList
from ..processing.schedule import Schedule
from ..processing.subjects import ABBR_CHARS, TITLE_CHARS, SubjectError, Subjects
from ..processing.todo import CATEGORIES, uid
from ..settings import SHEET_LABEL_CHARS, SPECS, ChatSettings, Settings, SettingsError
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
GRADE_SETTINGS = {"student": "grades_student", "sheets": "grades_sheets", "window_days": "grades_window_days"}
PDF_LIMITS = ("pdf_pages", "pdf_tokens")
TITLE_LIMIT = 200  # characters of an own deadline's title
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
    return web.json_response({"first_name": request["user"].get("first_name", ""),
                              "llm": {**request.app[BUDGET_KEY].status(), "runs_today": today["runs"]}})


async def chats(request: web.Request) -> web.Response:
    """The Chats screen (§8.4): every chat of the twin with its settings, enabled ones first."""
    chat_settings = request.app[CHAT_SETTINGS_KEY]
    listed = [{**chat, "settings": chat_settings.all(chat["id"])} for chat in request.app[INBOX_KEY].chats()]
    listed.sort(key=lambda chat: not chat["settings"]["enabled"])
    sources = request.app[SOURCES_KEY]
    return web.json_response({"twin": sources["telegram"](), "mail": sources.get("mail", dict)(),
                              "images": sources.get("images", dict)(), "chats": listed})


async def update_images(request: web.Request) -> web.Response:
    """{daily}: how many images are read a day in chats with «Картинки» (р. 12); 0 reads none."""
    values = await _json(request)
    if set(values) != {"daily"}:
        raise web.HTTPBadRequest(text="expected {daily}")
    try:
        daily = request.app[SETTINGS_KEY].set("images_daily", values["daily"])
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return web.json_response(request.app[SOURCES_KEY].get("images", lambda: {"daily": daily})())


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
        return web.json_response(request.app[CHAT_SETTINGS_KEY].update(chat_id, values))
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None


async def senders(request: web.Request) -> web.Response:
    """Who wrote in the chat, to pick the important people; chosen ones who have not written yet are listed too."""
    chat_id = _chat_id(request)
    listed = request.app[INBOX_KEY].senders(chat_id)
    seen = {sender["id"] for sender in listed}
    listed += [{"id": person, "name": None, "messages": 0}
               for person in request.app[CHAT_SETTINGS_KEY].get(chat_id, "people") if person not in seen]
    return web.json_response(listed)


async def subjects(request: web.Request) -> web.Response:
    """The Словарь screen (§8.4): every entry; Haiku's can be overridden by hand, manual ones edited or deleted."""
    dictionary = request.app[SUBJECTS_KEY]
    entries = [{"name": name, "title": s.title, "abbr": s.abbr, "manual": s.manual}
               for name, s in dictionary.all().items()]
    return web.json_response({"subjects": entries, "error": dictionary.error, "title_chars": TITLE_CHARS,
                              "abbr_chars": ABBR_CHARS})


async def put_subject(request: web.Request) -> web.Response:
    """Add or change an entry by hand: {name, title, abbr}; it becomes manual."""
    values = await _json(request)
    try:
        return web.json_response(request.app[SUBJECTS_KEY].set_manual(
            *(values.get(key) if isinstance(values.get(key), str) else "" for key in ("name", "title", "abbr"))))
    except SubjectError as e:
        raise web.HTTPBadRequest(text=str(e)) from None


async def delete_subject(request: web.Request) -> web.Response:
    """Drop an entry: {name}. Haiku names the course again when a source brings it next time."""
    name = (await _json(request)).get("name")
    if not isinstance(name, str) or not request.app[SUBJECTS_KEY].delete(name):
        raise web.HTTPNotFound(text="no such entry")
    return web.json_response({"deleted": name})


async def schedule(request: web.Request) -> web.Response:
    """The Расписание screen: events from chats and corrections of classes from today on, hidden ones too."""
    timetable = request.app[SCHEDULE_KEY]
    return web.json_response({"ruz": request.app[SOURCES_KEY]["ruz"](),
                              "calendar": bool(request.app[ENV_KEY].calendar_secret),
                              "events": timetable.listing(clock.local_day(timetable.now()))})


async def update_event(request: web.Request) -> web.Response:
    """{required?: bool, hidden?: bool} for the newest version of an event; returns the screen's list anew."""
    try:
        event_id = int(request.match_info["event_id"])
    except ValueError:
        raise web.HTTPNotFound() from None
    values = await _json(request)
    if not values or set(values) - {"required", "hidden"} or \
            any(not isinstance(value, bool) for value in values.values()):
        raise web.HTTPBadRequest(text="expected {required: true|false, hidden: true|false}")
    timetable = request.app[SCHEDULE_KEY]
    events = timetable.events
    event = events.current(event_id)
    if event is None or event["gone_at"] is not None:
        raise web.HTTPNotFound(text="no such event")
    if "required" in values and event["class_uid"] is not None:
        raise web.HTTPBadRequest(text="пара из РУЗ всегда в основном календаре")
    if "required" in values:
        events.set_required(event_id, values["required"])
    if "hidden" in values:  # through the schedule: a correction of an interim assessment moves its Midterm
        await timetable.set_hidden(event_id, values["hidden"])
    return await schedule(request)


# --- deadlines -------------------------------------------------------------------------------

def _deadlines_state(request: web.Request) -> dict:
    """The Дедлайны screen: the merged list as Tablo and HA get it, the own ones, the hidden ones."""
    merged = request.app[DEADLINES_KEY]
    today = merged.today()
    items = [{"uid": uid(d), "title": d.title, "due": d.due.isoformat(), "category": d.category, "source": d.source,
              "course": d.name, "overdue": d.due < today} for d in merged.visible()]
    own = [{"uid": d.key, "title": d.title, "due": d.due.isoformat(), "category": d.category}
           for d in merged.todo.own()]
    hidden = [{"uid": row["uid"], "title": row["title"], "due": row["due"], "source": row["source"]}
              for row in merged.todo.hidden_rows()]
    hseapp = request.app[SOURCES_KEY].get("hseapp", dict)()
    return {"items": items, "own": own, "hidden": hidden, "today": today.isoformat(),
            "hseapp": {"login_needed": bool(hseapp.get("login_needed")), "error": hseapp.get("error")}}


def _own_fields(values: dict, partial: bool) -> dict:
    """title, due and category of an own deadline from the screen; every one of them, unless `partial`."""
    fields = {}
    if not partial or "title" in values:
        title = values.get("title")
        if not isinstance(title, str) or not " ".join(title.split()) or len(title) > TITLE_LIMIT:
            raise web.HTTPBadRequest(text=f"нужно название, до {TITLE_LIMIT} символов")
        fields["title"] = title
    if not partial or "due" in values:
        try:
            fields["due"] = dt.date.fromisoformat(values.get("due") if isinstance(values.get("due"), str) else "")
        except ValueError:
            raise web.HTTPBadRequest(text="срок — дата вида 2026-10-22") from None
    if not partial:
        if values.get("category") not in CATEGORIES:
            raise web.HTTPBadRequest(text="список: учёба (study) или личное (personal)")
        fields["category"] = values["category"]
    return fields


def _listed(request: web.Request):
    """A deadline of the list by uid; an own one also when the merge left it out as a copy of another."""
    merged, item_uid = request.app[DEADLINES_KEY], request.match_info["uid"]
    d = merged.find(item_uid) or next((own for own in merged.todo.own() if own.key == item_uid), None)
    if d is None:
        raise web.HTTPNotFound(text="этого дедлайна уже нет в списке")
    return d


async def deadlines(request: web.Request) -> web.Response:
    return web.json_response(_deadlines_state(request))


async def add_deadline(request: web.Request) -> web.Response:
    """{title, due, category}: a new own deadline."""
    fields = _own_fields(await _json(request), partial=False)
    request.app[DEADLINES_KEY].add(fields["title"], fields["due"], fields["category"])
    return web.json_response(_deadlines_state(request))


async def edit_deadline(request: web.Request) -> web.Response:
    """{title?, due?}: an own one changes; one from a source is hidden and an own copy takes its place (as in HA)."""
    fields = _own_fields(await _json(request), partial=True)
    if not fields:
        raise web.HTTPBadRequest(text="expected {title?, due?}")
    request.app[DEADLINES_KEY].edit(_listed(request), fields.get("title"), fields.get("due"))
    return web.json_response(_deadlines_state(request))


async def delete_deadline(request: web.Request) -> web.Response:
    """An own one is deleted, one from a source is hidden for good (as in HA)."""
    request.app[DEADLINES_KEY].remove(_listed(request))
    return web.json_response(_deadlines_state(request))


async def unhide_deadline(request: web.Request) -> web.Response:
    """«Вернуть»: one hidden row less; the deadline is back if its source still has it."""
    if not request.app[DEADLINES_KEY].unhide(request.match_info["uid"]):
        raise web.HTTPNotFound(text="такого скрытого дедлайна нет")
    return web.json_response(_deadlines_state(request))


# --- sheets -----------------------------------------------------------------------------------

def _sheets_state(request: web.Request) -> dict:
    settings = request.app[SETTINGS_KEY]
    return {"sheets": request.app[SHEETS_KEY].sheets.listing(), "status": request.app[SOURCES_KEY]["sheets"](),
            "limits": {name: settings.get(name) for name in PDF_LIMITS}}


async def sheets(request: web.Request) -> web.Response:
    """The Листочки screen: the sheets with their versions and the limits of reading them."""
    return web.json_response(_sheets_state(request))


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
    return web.json_response(_sheets_state(request))


async def join_sheet(request: web.Request) -> web.Response:
    """{previous: id}: the sheet is a newer version of that one (р. 14); its file replaces the older one's."""
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
        raise web.HTTPBadRequest(text=str(e)) from None
    return web.json_response(_sheets_state(request))


# --- HSE App X ------------------------------------------------------------------------------

def _hseapp_state(request: web.Request) -> dict:
    """The screen's state: the source's status (never the tokens) and where the browser comes back to."""
    return {"status": request.app[SOURCES_KEY]["hseapp"](), "callback": request.app[HSEAPP_KEY].callback_url}


async def hseapp(request: web.Request) -> web.Response:
    return web.json_response(_hseapp_state(request))


async def update_hseapp(request: web.Request) -> web.Response:
    """{enabled: bool}: the general setting hseapp_enabled."""
    values = await _json(request)
    if set(values) != {"enabled"} or not isinstance(values["enabled"], bool):
        raise web.HTTPBadRequest(text="expected {enabled: true|false}")
    request.app[HSEAPP_KEY].set_enabled(values["enabled"])
    return web.json_response(_hseapp_state(request))


async def hseapp_link(request: web.Request) -> web.Response:
    """{app?: bool} -> {url}: a login link that comes back to our callback, or with `app` to the app's own address
    (its return address is pasted by hand). Each link holds a PKCE state; the last five work."""
    app_redirect = (await _json(request)).get("app", False)
    if not isinstance(app_redirect, bool):
        raise web.HTTPBadRequest(text="expected {app: true|false}")
    return web.json_response({"url": request.app[HSEAPP_KEY].login_link(app=app_redirect)})


async def hseapp_login(request: web.Request) -> web.Response:
    """{url}: the return address pasted by hand, when the browser did not come back by itself."""
    returned = (await _json(request)).get("url")
    if not isinstance(returned, str) or not returned.strip():
        raise web.HTTPBadRequest(text="вставьте адрес, на который вернулся браузер")
    try:
        await request.app[HSEAPP_KEY].login(returned)
    except ValueError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return web.json_response(_hseapp_state(request))


async def hseapp_callback(request: web.Request) -> web.Response:
    """The browser after the HSE login (public, no initData: only the browser that opened the link knows its
    state). The address carries the code: it is not logged (the ports have no access log)."""
    try:
        await request.app[HSEAPP_KEY].login(str(request.rel_url))
    except ValueError as e:
        reason = str(e).rstrip(".")
        return page("Вход не удался", f"{reason[:1].upper()}{reason[1:]}.",
                    "Вернитесь в Telegram и войдите ещё раз: Mini App, экран «HSE App X».", status=400)
    return page("Вход в HSE App X выполнен", "Дедлайны обновятся в течение минуты.",
                "Это окно можно закрыть и вернуться в Telegram.")


PAGE = """<!doctype html>
<html lang="ru">
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
    return web.json_response(_summaries_state(request))


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
    return web.json_response(_summaries_state(request))


# --- LLM spending -----------------------------------------------------------------------------

def _llm_state(request: web.Request) -> dict:
    budget, inbox = request.app[BUDGET_KEY], request.app[INBOX_KEY]
    now = budget.now()  # one clock with the limit

    def chat_title(chat_id: int) -> str:
        if chat_id == MAILBOX:
            return "Почта"
        chat = inbox.chat(chat_id)
        return chat["title"] if chat else f"чат {chat_id}"

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
    return web.json_response(_llm_state(request))


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
    return web.json_response(_llm_state(request))


async def raise_llm(request: web.Request) -> web.Response:
    """One more daily limit until midnight, as the button in «Система»."""
    budget = request.app[BUDGET_KEY]
    budget.raise_today(budget.settings.get("llm_daily_limit_usd"))
    return web.json_response(_llm_state(request))


# --- grades -----------------------------------------------------------------------------------

def _grades_state(request: web.Request) -> dict:
    grades, status = request.app[GRADES_KEY], request.app[SOURCES_KEY].get("grades", dict)()
    read = status.get("sheets") or {}
    sheets = [{"id": sid, "url": sheet["url"], "label": sheet["label"], **read.get(sid, {}),
               "grades": grades.items(sid)}
              for sheet in grades.settings.get("grades_sheets") for sid in [sheet_id(sheet["url"])]]
    return {**{key: grades.settings.get(name) for key, name in GRADE_SETTINGS.items() if key != "sheets"},
            "llm": status.get("llm", True), "sheets": sheets, "label_chars": SHEET_LABEL_CHARS}


async def grade_sheets(request: web.Request) -> web.Response:
    """The Оценки screen: the name, the sheets with what was read in them, the window of new grades."""
    return web.json_response(_grades_state(request))


async def update_grades(request: web.Request) -> web.Response:
    """{student?, sheets?: [{url, label}], window_days?}: all checked before any is saved; a sheet removed, or all of
    them for a new name, start again from a baseline. The sheets are read at once."""
    values = await _json(request)
    if not values or set(values) - set(GRADE_SETTINGS):
        raise web.HTTPBadRequest(text="expected {student?, sheets?, window_days?}")
    try:
        request.app[GRADES_KEY].configure({GRADE_SETTINGS[key]: value for key, value in values.items()})
    except SettingsError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return web.json_response(_grades_state(request))


def page(title: str, text: str, hint: str, status: int = 200) -> web.Response:
    body = PAGE.format(title=html.escape(title), text=html.escape(text), hint=html.escape(hint))
    return web.Response(text=body, status=status, content_type="text/html",
                        headers={"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"})


def create_app(env: Env, budget: Budget, journal: Journal, inbox: Inbox, chat_settings: ChatSettings,
               sources: Sources, dictionary: Subjects, timetable: Schedule, hse: HseApp,
               merged: DeadlineList, settings: Settings | None = None,
               sheet_topic: SheetTopic | None = None, summary_topic: SummaryTopic | None = None,
               grade_list: Grades | None = None) -> web.Application:
    """The Mini App's port; without the settings and the sheets' topic the Листочки screen has no API, without the
    settings and the summaries' topic the Сводки screen has none, without the grades the Оценки screen has none."""
    app = web.Application(middlewares=[owner_only])
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
    app.router.add_get("/app/api/llm", llm)
    app.router.add_post("/app/api/llm/raise", raise_llm)
    if settings is not None:
        app.router.add_patch("/app/api/images", update_images)
        app.router.add_patch("/app/api/llm", update_llm)
    app.router.add_get("/app/api/subjects", subjects)
    app.router.add_put("/app/api/subjects", put_subject)
    app.router.add_delete("/app/api/subjects", delete_subject)
    app.router.add_get("/app/api/schedule", schedule)
    app.router.add_patch("/app/api/events/{event_id}", update_event)
    app.router.add_get("/app/api/deadlines", deadlines)
    app.router.add_post("/app/api/deadlines", add_deadline)
    app.router.add_patch("/app/api/deadlines/{uid}", edit_deadline)
    app.router.add_delete("/app/api/deadlines/{uid}", delete_deadline)
    app.router.add_delete("/app/api/hidden/{uid}", unhide_deadline)
    if settings is not None and sheet_topic is not None:
        app.router.add_get("/app/api/sheets", sheets)
        app.router.add_patch("/app/api/sheets", update_sheets)
        app.router.add_post("/app/api/sheets/{sheet_id}/join", join_sheet)
    if settings is not None and summary_topic is not None:
        app.router.add_get("/app/api/summaries", summaries)
        app.router.add_patch("/app/api/summaries", update_summaries)
    if grade_list is not None:
        app[GRADES_KEY] = grade_list
        app.router.add_get("/app/api/grades", grade_sheets)
        app.router.add_patch("/app/api/grades", update_grades)
    app.router.add_get("/app/api/hseapp", hseapp)
    app.router.add_patch("/app/api/hseapp", update_hseapp)
    app.router.add_post("/app/api/hseapp/link", hseapp_link)
    app.router.add_post("/app/api/hseapp/login", hseapp_login)
    app.router.add_get(CALLBACK_PATH, hseapp_callback)
    return app
