"""«Machine» (§8.6, stage 16.6, D65): the web app's screen for how the service runs and how it is set up.

- the sources and their freshness: the twin, the mail, RUZ, HSE App X, the gradebooks, the wiki pages, the batches,
  the images and the LLM limit, each ok, late, failing, off or waiting for a login, and what needs the owner with the
  screen that fixes it (`attention`; the feed's status dot counts it);
- the journal: every change of the owner's in the app and of Claude's through the connector, with its undo;
- the settings, moved over from the Mini App: the twin's login and the chats, HSE App X, the gradebooks and their
  tabs, the batches and news, the reminders, whole PDFs, images, the course dictionary, the LLM limit, the Claude
  connector and the notifications.

The screens read through the owner's actions (actions.py), as Claude does; a change of a setting is the connector's
write tool (`Connector.act`): checked once, journaled with its undo like every other change. What has no tool is the
owner's alone and is not journaled: the twin's and HSE App X's logins, the LLM limit, reading a gradebook now and
revoking a connection of Claude's.
"""

import datetime as dt
import json
from functools import partial

from aiohttp import web

from .. import actions, clock
from ..connector.tools import ToolError, call_action
from ..i18n import json_default, say, tr
from ..library.links import LIBRARY_KEY
from ..outputs.api import SOURCES_KEY

# A source is late when its last good read is older than this many minutes (its poll and some slack).
LATE_MINUTES = {"mail": 30, "ruz": 3 * 60, "hseapp": 60, "grades": 2 * 60, "wiki": 9 * 60}
JOURNAL = 100
SETTING_TOOLS = {"general": "update_general_settings", "reminders": "update_reminders", "images": "update_images",
                 "pdf": "update_pdf_limits", "grades": "update_grade_settings", "notifications": "update_notifications"}


class Machine:
    def __init__(self, web_app):
        self.web = web_app  # the WebApp: its language, clock and the connector

    def routes(self) -> list[tuple]:
        return [("GET", "", overview),
                ("GET", "/journal", journal),
                ("GET", "/chats", chats),
                ("PATCH", "/chats/{chat_id}", update_chat),
                ("GET", "/chats/{chat_id}/senders", senders),
                ("POST", "/twin/{step}", twin),
                ("GET", "/hseapp", hseapp),
                ("PATCH", "/hseapp", update_hseapp),
                ("POST", "/hseapp/link", hseapp_link),
                ("POST", "/hseapp/login", hseapp_login),
                ("GET", "/settings", settings),
                ("PATCH", "/settings/{group}", update_settings),
                ("GET", "/courses", courses),
                ("PUT", "/courses", put_course),
                ("POST", "/courses/remove", remove_course),
                ("GET", "/llm", llm),
                ("PATCH", "/llm", update_llm),
                ("POST", "/llm/raise", raise_llm),
                ("GET", "/grades", grades),
                ("POST", "/grades", add_gradebook),
                ("GET", "/grades/{sid}", gradebook),
                ("PATCH", "/grades/{sid}", update_gradebook),
                ("DELETE", "/grades/{sid}", remove_gradebook),
                ("POST", "/grades/{sid}/read", read_gradebook),
                ("PUT", "/grades/{sid}/tabs/{tab}", set_tab),
                ("DELETE", "/grades/{sid}/tabs/{tab}", reset_tab),
                ("GET", "/connector", connector),
                ("DELETE", "/connector/{grant_id}", revoke)]

    def now(self) -> dt.datetime:
        return dt.datetime.fromtimestamp(self.web.seconds(), dt.UTC)

    # --- the sources --------------------------------------------------------------------------------------------

    def _late(self, name: str, ok_at: str | None) -> bool:
        if name not in LATE_MINUTES or not ok_at:
            return False
        local = dt.datetime.fromisoformat(clock.local_iso(self.now()))
        return local - dt.datetime.fromisoformat(ok_at) > dt.timedelta(minutes=LATE_MINUTES[name])

    def sources(self, app: web.Application) -> list[dict]:
        """Each source: {key, state: ok | late | error | login | off | idle, ok_at, error, count}, the order of the
        screen. A source the service runs without is left out."""
        statuses = app.get(SOURCES_KEY) or {}
        status = {name: get() for name, get in statuses.items()}
        listed = []

        def add(key, state, ok_at=None, error=None, count=None):
            if state == "ok" and self._late(key, ok_at):
                state = "late"
            listed.append({"key": key, "state": state, "ok_at": ok_at, "error": error, "count": count})

        if (twin := status.get("telegram")) is not None:
            inbox, chat_settings = app.get(actions.INBOX_KEY), app.get(actions.CHAT_SETTINGS_KEY)
            on = sum(chat_settings.enabled(c["id"]) for c in inbox.chats()) if inbox and chat_settings else None
            add("twin", "login" if not twin.get("account") else "error" if twin.get("error") else "ok",
                twin.get("ok_at"), twin.get("error"), on)
        if (mail := status.get("mail")) is not None:
            add("mail", "off" if not mail.get("configured") else "error" if mail.get("error") else
                "ok" if mail.get("ok_at") else "idle", mail.get("ok_at"), mail.get("error"), mail.get("letters"))
        if (ruz := status.get("ruz")) is not None:
            add("ruz", "error" if ruz.get("error") else "ok" if ruz.get("ok_at") else "idle", ruz.get("ok_at"),
                ruz.get("error"), ruz.get("classes"))
        if (hse := status.get("hseapp")) is not None:
            add("hseapp", "off" if not hse.get("enabled") else "login" if hse.get("login_needed") else
                "error" if hse.get("error") else "ok" if hse.get("ok_at") else "idle", hse.get("ok_at"),
                hse.get("error"), hse.get("items"))
        if (books := status.get("grades")) is not None:
            sheets = (books.get("sheets") or {}).values()
            failing = [s for s in sheets if s.get("error")]
            read = [s["ok_at"] for s in sheets if s.get("ok_at")]
            add("grades", "off" if not sheets else "error" if failing else "ok" if read else "idle",
                min(read) if read else None, failing[0]["error"] if failing else None, len(sheets))
        if (wiki := status.get("wiki")) is not None:
            pages = (wiki.get("pages") or {}).values()
            failing = [p for p in pages if p.get("error")]
            add("wiki", "off" if not pages else "error" if failing else "ok" if wiki.get("ok_at") else "idle",
                wiki.get("ok_at"), failing[0]["error"] if failing else None, len(pages))
        if (batches := status.get("batches")) is not None:
            add("batches", "error" if batches.get("error") else "ok", batches.get("ok_at"), batches.get("error"),
                batches.get("queue"))
        if (images := status.get("images")) is not None:
            add("images", "off" if not images.get("daily") else "error" if images.get("error") else "ok",
                images.get("ok_at"), images.get("error"), images.get("today"))
        if (budget := app.get(actions.BUDGET_KEY)) is not None:
            level = budget.status().get("level")
            add("llm", "error" if level == "hard" else "late" if level not in (None, "ok") else "ok")
        return listed

    def attention(self, app: web.Application) -> list[dict]:
        """What needs the owner: {text, screen} in the app's language; screen is a route of «Machine», or None for
        what only the server's .env and logs fix."""
        lang, found = self.web.lang(), []
        statuses = app.get(SOURCES_KEY) or {}
        status = {name: get() for name, get in statuses.items()}

        def add(en, ru, screen=None):
            found.append({"text": tr(lang, en, ru), "screen": screen})

        twin = status.get("telegram")
        if twin is not None and not twin.get("account"):
            add("The twin is not logged in: no chat is read", "Твинк не вошёл: чаты не читаются", "machine/chats")
        elif twin and twin.get("error"):
            add(f"The twin: {say(twin['error'], lang)}", f"Твинк: {say(twin['error'], lang)}", "machine/chats")
        mail = status.get("mail") or {}
        if mail.get("configured") and mail.get("error"):
            add(f"The mail is not read: {say(mail['error'], lang)}", f"Почта не читается: {say(mail['error'], lang)}")
        hse = status.get("hseapp") or {}
        if hse.get("login_needed"):
            add("HSE App X needs a login", "HSE App X: нужен вход", "machine/hseapp")
        if (ruz := status.get("ruz") or {}).get("error"):
            add(f"RUZ is not read: {say(ruz['error'], lang)}", f"РУЗ не читается: {say(ruz['error'], lang)}")
        for sid, sheet in ((status.get("grades") or {}).get("sheets") or {}).items():
            if sheet.get("error"):
                name = sheet.get("title") or sid
                add(f"A gradebook is not read: {name}", f"Таблица оценок не читается: {name}",
                    f"machine/gradebook?id={sid}")
        for url, page in ((status.get("wiki") or {}).get("pages") or {}).items():
            if page.get("error"):
                name = page.get("title") or url
                add(f"A wiki page is not read: {name}", f"Страница вики не читается: {name}", "machine/processing")
        if (images := status.get("images") or {}).get("error"):
            add(f"Reading images failed: {say(images['error'], lang)}",
                f"Чтение картинок не удалось: {say(images['error'], lang)}", "machine/processing")
        if (budget := app.get(actions.BUDGET_KEY)) is not None:
            level = budget.status().get("level")
            if level == "hard":
                add("The LLM limit is spent: calls wait until midnight", "Лимит LLM исчерпан: вызовы стоят до полуночи",
                    "machine/llm")
            elif level not in (None, "ok"):
                add("Over 80 % of the LLM limit is spent", "Потрачено больше 80 % лимита LLM", "machine/llm")
        env = app.get(actions.ENV_KEY)
        if env is not None and not (env.calendar_secret or env.calendar_public_secret):
            add("The calendar feeds are off: no CALENDAR_SECRET in .env",
                "Ленты календаря выключены: в .env нет CALENDAR_SECRET")
        return found


MACHINE_KEY = web.AppKey("machine", Machine)


def _machine(request: web.Request) -> Machine:
    return request.app[MACHINE_KEY]


def answer(request: web.Request, data) -> web.Response:
    """JSON with every Phrase (a status's error) in the app's language."""
    lang = _machine(request).web.lang()
    return web.json_response(data, dumps=partial(json.dumps, default=json_default(lang)))


def _need(request: web.Request, key):
    if request.app.get(key) is None:
        raise web.HTTPServiceUnavailable(text="this part of infomaster is not running")


async def _body(request: web.Request) -> dict:
    try:
        data = await request.json()
    except (json.JSONDecodeError, UnicodeDecodeError):
        raise web.HTTPBadRequest(text="JSON expected") from None
    if not isinstance(data, dict):
        raise web.HTTPBadRequest(text="a JSON object expected")
    return data


async def _mini(request: web.Request, handler, body=None, query=None, **match_info):
    """An action's answer; its refusal as a 400 with its text."""
    try:
        return await call_action(request.app, handler, body, query, **match_info)
    except ToolError as e:
        raise web.HTTPBadRequest(text=str(e)) from None


async def _act(request: web.Request, tool: str, arguments: dict) -> dict:
    """A setting changed through the connector's tool, journaled: {id, summary, undo, note}."""
    connector = _machine(request).web.connector
    if connector is None or tool not in connector.registry.tools:
        raise web.HTTPServiceUnavailable(text="changes are not wired")
    try:
        change_id, output = await connector.act(tool, arguments)
    except ToolError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return {"id": change_id, "summary": output.summary, "undo": output.undo is not None, "note": output.note}


# --- the overview and the journal ------------------------------------------------------------------------------

async def overview(request: web.Request) -> web.Response:
    machine, app = _machine(request), request.app
    budget = app.get(actions.BUDGET_KEY)
    connector = machine.web.connector
    settings = app.get(actions.SETTINGS_KEY)
    return answer(request, {
        "attention": machine.attention(app), "sources": machine.sources(app),
        "llm": {**budget.status(), "limit_usd": budget.settings.get("llm_daily_limit_usd")} if budget else None,
        "connections": len(connector.oauth.connections()) if connector else None,
        "push": len(machine.web.push.devices()),
        "library": library.usage() if (library := app.get(LIBRARY_KEY)) is not None else None,  # its disk (D69)
        "has": {"twin": app.get(actions.TWIN_KEY) is not None, "settings": settings is not None,
                "grades": app.get(actions.GRADES_KEY) is not None, "reminders": app.get(actions.REMINDERS_KEY)
                is not None, "pdf": app.get(actions.SHEETS_KEY) is not None, "connector": connector is not None,
                "hseapp": app.get(actions.HSEAPP_KEY) is not None and SOURCES_KEY in app},
        "lang": settings.get("language") if settings else machine.web.lang()})


async def journal(request: web.Request) -> web.Response:
    """The newest changes first: the owner's in the app (via web) and Claude's (via claude)."""
    connector = _machine(request).web.connector
    if connector is None:
        return answer(request, {"changes": []})
    return answer(request, {"changes": [
        {"id": c["id"], "at": c["at"], "tool": c["tool"], "summary": c["summary"], "via": c["via"],
         "undo": c["undo"] is not None, "note": c["note"], "undone_at": c["undone_at"], "undo_error": c["undo_error"]}
        for c in connector.changes.recent(JOURNAL)]})


# --- the twin and the chats ---------------------------------------------------------------------------------

async def chats(request: web.Request) -> web.Response:
    _need(request, SOURCES_KEY)
    return answer(request, await _mini(request, actions.chats))


async def update_chat(request: web.Request) -> web.Response:
    """Some settings of a chat: the connector's update_chat_settings."""
    try:
        chat_id = int(request.match_info["chat_id"])
    except ValueError:
        raise web.HTTPNotFound() from None
    change = await _act(request, "update_chat_settings", {"chat_id": chat_id, **await _body(request)})
    return answer(request, {"settings": request.app[actions.CHAT_SETTINGS_KEY].all(chat_id), "change": change})


async def senders(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.senders, chat_id=request.match_info["chat_id"]))


async def twin(request: web.Request) -> web.Response:
    """POST /twin/login {phone} | {code} | {password}, /twin/cancel, /twin/logout: the twin's status after it."""
    _need(request, actions.TWIN_KEY)
    handler = {"login": actions.twin_login, "cancel": actions.twin_cancel,
               "logout": actions.twin_logout}.get(request.match_info["step"])
    if handler is None:
        raise web.HTTPNotFound(text="no such step")
    data = await _body(request) if handler is actions.twin_login else None
    return answer(request, await _mini(request, handler, data))


# --- HSE App X ----------------------------------------------------------------------------------------------

async def hseapp(request: web.Request) -> web.Response:
    _need(request, SOURCES_KEY)
    return answer(request, await _mini(request, actions.hseapp))


async def update_hseapp(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.update_hseapp, await _body(request)))


async def hseapp_link(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.hseapp_link, await _body(request)))


async def hseapp_login(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.hseapp_login, await _body(request)))


# --- settings ---------------------------------------------------------------------------------------------

def _settings(request: web.Request) -> dict:
    """Every setting of the «Processing» and «Notifications» screens, by the group its tool changes."""
    _need(request, actions.SETTINGS_KEY)
    settings = request.app[actions.SETTINGS_KEY]
    data = {"general": {name: settings.get(name) for name in actions.GENERAL if name != "llm_prices"},
            "batch_choices": list(actions.BATCH_HOURS),
            "images": {"daily": settings.get("images_daily")},
            "notifications": {"kinds": settings.get("push_kinds"), "quiet": settings.get("push_quiet")}}
    if request.app.get(actions.REMINDERS_KEY) is not None:
        data["reminders"] = {key: settings.get(name) for key, name in actions.REMINDER_FIELDS.items()}
    if request.app.get(actions.SHEETS_KEY) is not None:
        data["pdf"] = {name: settings.get(name) for name in actions.PDF_LIMITS}
    statuses = request.app.get(SOURCES_KEY) or {}
    if "wiki" in statuses:
        data["wiki"] = statuses["wiki"]()["pages"]
    if "images" in statuses:
        data["images"]["today"] = statuses["images"]().get("today")
    return data


async def settings(request: web.Request) -> web.Response:
    return answer(request, _settings(request))


async def update_settings(request: web.Request) -> web.Response:
    """PATCH /settings/{group} {name: value}: general, reminders, images, pdf, grades or notifications, through the
    group's connector tool; the settings after it and the change."""
    tool = SETTING_TOOLS.get(request.match_info["group"])
    if tool is None:
        raise web.HTTPNotFound(text="no such group of settings")
    change = await _act(request, tool, await _body(request))
    return answer(request, {"settings": _settings(request), "change": change})


# --- the course dictionary ----------------------------------------------------------------------------------

async def courses(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.subjects))


async def put_course(request: web.Request) -> web.Response:
    """{name, title, abbr}: a manual entry (the connector's set_course)."""
    data = await _body(request)
    change = await _act(request, "set_course", {key: data.get(key) for key in ("name", "title", "abbr")})
    return answer(request, {**await _mini(request, actions.subjects), "change": change})


async def remove_course(request: web.Request) -> web.Response:
    """{name}: the entry dropped; Haiku names the course again when a source brings it."""
    change = await _act(request, "delete_course", {"name": (await _body(request)).get("name")})
    return answer(request, {**await _mini(request, actions.subjects), "change": change})


# --- the LLM limit ------------------------------------------------------------------------------------------

async def llm(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.llm))


async def update_llm(request: web.Request) -> web.Response:
    _need(request, actions.SETTINGS_KEY)
    return answer(request, await _mini(request, actions.update_llm, await _body(request)))


async def raise_llm(request: web.Request) -> web.Response:
    return answer(request, await _mini(request, actions.raise_llm))


# --- the gradebooks -----------------------------------------------------------------------------------------

async def grades(request: web.Request) -> web.Response:
    _need(request, actions.GRADES_KEY)
    return answer(request, await _mini(request, actions.grade_sheets))


async def add_gradebook(request: web.Request) -> web.Response:
    """{url, label?, name?}: the connector's add_gradebook."""
    data = await _body(request)
    change = await _act(request, "add_gradebook", {key: data[key] for key in ("url", "label", "name") if data.get(key)
                                                   or key == "url"})
    return answer(request, {**await _mini(request, actions.grade_sheets), "change": change})


async def gradebook(request: web.Request) -> web.Response:
    _need(request, actions.GRADES_KEY)
    return answer(request, await _mini(request, actions.gradebook, sid=request.match_info["sid"]))


async def update_gradebook(request: web.Request) -> web.Response:
    """{name?, label?}"""
    sid = request.match_info["sid"]
    change = await _act(request, "update_gradebook", {"sheet_id": sid, **await _body(request)})
    return answer(request, {**await _mini(request, actions.gradebook, sid=sid), "change": change})


async def remove_gradebook(request: web.Request) -> web.Response:
    change = await _act(request, "remove_gradebook", {"sheet_id": request.match_info["sid"]})
    return answer(request, {**await _mini(request, actions.grade_sheets), "change": change})


async def read_gradebook(request: web.Request) -> web.Response:
    """The gradebooks read at once instead of in 30 minutes; nothing to journal."""
    _need(request, actions.GRADES_KEY)
    return answer(request, await _mini(request, actions.read_gradebook, sid=request.match_info["sid"]))


async def set_tab(request: web.Request) -> web.Response:
    """{skip, works: [{column, label, max, bonus_column}]}: the owner's layout of a tab, over Haiku's."""
    sid, tab = request.match_info["sid"], request.match_info["tab"]
    data = await _body(request)
    change = await _act(request, "set_tab_layout", {"sheet_id": sid, "tab": tab, "skip": data.get("skip"),
                                                    "works": data.get("works")})
    return answer(request, {**await _mini(request, actions.gradebook, sid=sid), "change": change})


async def reset_tab(request: web.Request) -> web.Response:
    sid, tab = request.match_info["sid"], request.match_info["tab"]
    change = await _act(request, "reset_tab_layout", {"sheet_id": sid, "tab": tab})
    return answer(request, {**await _mini(request, actions.gradebook, sid=sid), "change": change})


# --- the Claude connector -----------------------------------------------------------------------------------

def _connector_state(request: web.Request) -> dict:
    connector = _machine(request).web.connector
    if connector is None:
        raise web.HTTPServiceUnavailable(text="the connector is not running")
    state = connector.state()
    state.pop("changes")  # the journal has them, with the app's own
    return state


async def connector(request: web.Request) -> web.Response:
    return answer(request, _connector_state(request))


async def revoke(request: web.Request) -> web.Response:
    connector_ = _machine(request).web.connector
    if connector_ is None:
        raise web.HTTPServiceUnavailable(text="the connector is not running")
    try:
        grant_id = int(request.match_info["grant_id"])
    except ValueError:
        raise web.HTTPNotFound() from None
    if not connector_.oauth.revoke_grant(grant_id):
        raise web.HTTPNotFound(text="no such connection")
    return answer(request, _connector_state(request))
