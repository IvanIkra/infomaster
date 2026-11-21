"""The web app (§8.6, D61, stage 16): a home-screen app on the public port that took the place of the forum and
the web app (D69).

On the public port, next to the connector:
- /web/: the app's page; /web/static/*: its scripts, styles, fonts and icons; /web/sw.js: the service worker, whose
  scope is /web/; /web/manifest.webmanifest;
- /web/api/*: JSON for the app, behind the session cookie (auth.py); what the screens read comes from views.py,
  the «Check» queue from check.py. A request that changes something must come from the app's own origin: the cookie
  is SameSite=Strict already, and the Origin header is checked too.

The owner's changes (stage 16.3) are the connector's write tools (§8.5), called through `Connector.act`: the same
checks, and every change goes into the connector's journal (`via` web) with the steps that undo it.

Notifications (stage 16.4): a device turns them on with its browser's push subscription (push.py); the notifier
(notify.py) sends the deadlines' news, reminders and failures (D69).

News (stage 16.7): the live news in rank order, an item with the message it came from, and «Hide» through the
connector's hide_news.

Shelves (stage 16.8, shelves.py): the problem sheets with their PDFs and versions, the library's raw material by
course and the sorting of its inbox, under /web/api/shelf; the changes are the connector's tools.

Corrections (stage 16.9, fixes.py): a class moved, cancelled or changed, an event changed, hidden or deleted, the
main or the optional calendar, the hidden deadlines and events with «Restore», grades corrected, under /web/api/fix;
the changes are the connector's tools.

«Machine» (stage 16.6, machine.py): the sources and their freshness, the journal of changes with undo, and the Mini
App's settings, under /web/api/machine.

«+» (stage 16.5, quick.py): material for the library at the class going on, one line read by Haiku and confirmed, a
deadline, an event or a grade by hand. Android's «Share» lands on /web/share: the service worker keeps the files and
opens «+» with them; without it, the server only sends the app there.

The client is plain ES modules, no build step: Preact and htm are vendored in static/vendor, the fonts in static/fonts.
"""

import datetime as dt
import json
import time
from collections.abc import Awaitable, Callable
from pathlib import Path
from urllib.parse import urlsplit

from aiohttp import web

from ..config import Env
from ..connector.tools import Context, ToolError
from ..library import LibraryError
from ..library.feed import first_url
from ..llm.client import Haiku
from ..store import Store
from .auth import COOKIE, COOKIE_PATH, SESSION_IDLE, Auth, AuthError, device_name
from .check import Check
from .fixes import FIXES_KEY, Fixes
from .machine import MACHINE_KEY, Machine
from .notify import Notifier
from .push import Push, PushError
from .quick import Quick, QuickError
from .shelves import SHELVES_KEY, Shelves
from .views import Data

STATIC = Path(__file__).parent / "static"
NO_CACHE = {"Cache-Control": "no-cache"}
CHANGES = {"POST", "PUT", "PATCH", "DELETE"}


class WebApp:
    def __init__(self, env: Env, store: Store, send_code: Callable[[str], Awaitable[None]] | None = None,
                 lang: Callable[[], str] = lambda: "en", now: Callable[[], float] = time.time,
                 data: Data | None = None, llm: Haiku | None = None):
        parts = urlsplit(env.public_url)
        self.origin = f"{parts.scheme}://{parts.netloc}"
        self.lang, self.seconds = lang, now
        self.data = data  # what the screens read; set by the service, left out by the login's tests
        self.check = Check(data, store) if data is not None else None
        self.quick = Quick(data, llm) if data is not None else None
        self.connector = None  # the service's Connector: the owner's changes are its tools, journaled with undo
        self.auth = Auth(store, parts.hostname or "", self.origin, send_code, lang, now)
        self.push = Push(store, env.public_url, now)
        self.notifier = Notifier(self.push, lang)
        self.machine = Machine(self)
        self.shelves = Shelves(self)
        self.fixes = Fixes(self)

    def attach(self, app: web.Application) -> web.Application:
        app[WEB_KEY] = self
        app.router.add_get("/web", to_index)
        app.router.add_get("/web/", index)
        app.router.add_get("/web/sw.js", service_worker)
        app.router.add_get("/web/manifest.webmanifest", manifest)
        app.router.add_static("/web/static", STATIC)
        app.on_response_prepare.append(revalidate_static)
        app.router.add_post("/web/share", shared)
        api = [("GET", "/me", me),
               ("POST", "/login/code", send_code),
               ("POST", "/login/code/check", check_code),
               ("POST", "/login/passkey/options", login_options),
               ("POST", "/login/passkey", passkey_login),
               ("POST", "/logout", logout),
               ("GET", "/devices", devices),
               ("DELETE", "/sessions/{session_id}", end_session),
               ("POST", "/passkeys/options", passkey_options),
               ("POST", "/passkeys", add_passkey),
               ("DELETE", "/passkeys/{passkey_id}", remove_passkey),
               ("GET", "/feed", feed),
               ("GET", "/week", week),
               ("GET", "/deadline", deadline),
               ("GET", "/class", class_),
               ("GET", "/course", course),
               ("GET", "/news", news),
               ("GET", "/news/{news_id}", news_card),
               ("POST", "/news/{news_id}/hide", hide_news),
               ("GET", "/check", check),
               ("POST", "/check/put-aside", put_aside),
               ("POST", "/deadlines/{action}", deadline_action),
               ("POST", "/changes/{change_id}/undo", undo),
               ("GET", "/push", push_state),
               ("POST", "/push", subscribe),
               ("DELETE", "/push", unsubscribe),
               ("POST", "/push/test", push_test),
               ("GET", "/add", add_state),
               ("POST", "/add/line", add_line),
               ("POST", "/add/files", add_files),
               ("POST", "/add/note", add_note),
               ("POST", "/add/{what}", add_by_hand)]
        api += [(method, "/machine" + path, handler) for method, path, handler in self.machine.routes()]
        api += [(method, "/shelf" + path, handler) for method, path, handler in self.shelves.routes()]
        api += [(method, "/fix" + path, handler) for method, path, handler in self.fixes.routes()]
        app[MACHINE_KEY] = self.machine
        app[SHELVES_KEY] = self.shelves
        app[FIXES_KEY] = self.fixes
        for method, path, handler in api:
            app.router.add_route(method, "/web/api" + path, guarded(handler))
        if self.connector is not None and self.check is not None:
            self.connector.registry.op("_ask_again", self._ask_again)
        return app

    async def _ask_again(self, ctx: Context, args: dict) -> None:
        """The undo of a question put aside: the «Check» queue asks it again."""
        self.check.ask_again(args["id"])


WEB_KEY = web.AppKey("web", WebApp)
SESSION_KEY = web.RequestKey("session", dict)  # the session of the request, put there by guarded
PUBLIC = {"send_code", "check_code", "login_options", "passkey_login"}  # the handlers open without a session


def guarded(handler):
    """The checks every /web/api handler goes through: the origin of a change, then the session unless the
    handler is one of the login steps. Errors come back as JSON {"error": text}."""
    async def wrapper(request: web.Request) -> web.StreamResponse:
        app = request.app[WEB_KEY]
        if request.method in CHANGES and request.headers.get("Origin") != app.origin:
            return error(403, "wrong origin")
        if handler.__name__ not in PUBLIC:
            session = app.auth.session(request.cookies.get(COOKIE))
            if session is None:
                return error(401, "log in")
            request[SESSION_KEY] = session
        try:
            return await handler(request)
        except AuthError as e:
            return error(e.status, e.message)
        except web.HTTPException as e:  # a refusal raised deep down (bad JSON, no such class) comes as JSON too
            if e.status < 400:
                raise
            return error(e.status, e.text if e.text and not e.text.startswith(f"{e.status}:") else e.reason)
    wrapper.__name__ = handler.__name__
    return wrapper


def error(status: int, text: str) -> web.Response:
    return web.json_response({"error": text}, status=status)


async def body(request: web.Request) -> dict:
    try:
        data = await request.json()
    except (json.JSONDecodeError, UnicodeDecodeError):
        raise web.HTTPBadRequest(text="JSON expected") from None
    if not isinstance(data, dict):
        raise web.HTTPBadRequest(text="a JSON object expected")
    return data


def logged_in(request: web.Request, token: str, data=None) -> web.Response:
    response = web.json_response(data if data is not None else {"ok": True})
    response.set_cookie(COOKIE, token, max_age=SESSION_IDLE, path=COOKIE_PATH, secure=True, httponly=True,
                        samesite="Strict")
    return response


def _device(request: web.Request) -> str:
    return device_name(request.headers.get("User-Agent", ""))


def _id(request: web.Request, name: str) -> int:
    try:
        return int(request.match_info[name])
    except ValueError:
        raise web.HTTPNotFound() from None


# --- pages -------------------------------------------------------------------------------------------------

async def to_index(request: web.Request) -> web.Response:
    raise web.HTTPFound("/web/")


async def revalidate_static(request: web.Request, response: web.StreamResponse) -> None:
    """The app's modules are asked for again on every load (a 304 while unchanged): without it a browser keeps an old
    module by its own guess after a deploy, and the new page runs with it."""
    if request.path.startswith("/web/static/") and "Cache-Control" not in response.headers:
        response.headers["Cache-Control"] = "no-cache"


async def index(request: web.Request) -> web.FileResponse:
    return web.FileResponse(STATIC / "index.html", headers=NO_CACHE)


async def service_worker(request: web.Request) -> web.FileResponse:
    return web.FileResponse(STATIC / "sw.js", headers={**NO_CACHE, "Content-Type": "text/javascript"})


async def manifest(request: web.Request) -> web.FileResponse:
    return web.FileResponse(STATIC / "manifest.webmanifest",
                            headers={**NO_CACHE, "Content-Type": "application/manifest+json"})


# --- the API -----------------------------------------------------------------------------------------------

async def me(request: web.Request) -> web.Response:
    app, session = request.app[WEB_KEY], request[SESSION_KEY]
    return web.json_response({"lang": app.lang(), "session": session["id"], "device": session["device"],
                              "passkeys": len(app.auth.passkeys()), "push": app.push.subscribed(session["id"])})


async def send_code(request: web.Request) -> web.Response:
    await request.app[WEB_KEY].auth.send()
    return web.json_response({"ok": True})


async def check_code(request: web.Request) -> web.Response:
    data = await body(request)
    return logged_in(request, request.app[WEB_KEY].auth.check_code(data.get("code"), _device(request)))


async def login_options(request: web.Request) -> web.Response:
    return web.Response(text=request.app[WEB_KEY].auth.login_options(), content_type="application/json")


async def passkey_login(request: web.Request) -> web.Response:
    data = await body(request)
    return logged_in(request, request.app[WEB_KEY].auth.passkey_login(data.get("credential") or {},
                                                                       _device(request)))


async def logout(request: web.Request) -> web.Response:
    request.app[WEB_KEY].auth.end(request[SESSION_KEY]["id"])
    response = web.json_response({"ok": True})
    response.del_cookie(COOKIE, path=COOKIE_PATH)
    return response


def _devices(request: web.Request) -> dict:
    auth = request.app[WEB_KEY].auth
    return {"current": request[SESSION_KEY]["id"], "sessions": auth.sessions(), "passkeys": auth.passkeys()}


async def devices(request: web.Request) -> web.Response:
    return web.json_response(_devices(request))


async def end_session(request: web.Request) -> web.Response:
    if not request.app[WEB_KEY].auth.end(_id(request, "session_id")):
        return error(404, "no such device")
    return web.json_response(_devices(request))


async def passkey_options(request: web.Request) -> web.Response:
    text = request.app[WEB_KEY].auth.registration_options(request[SESSION_KEY]["id"])
    return web.Response(text=text, content_type="application/json")


async def add_passkey(request: web.Request) -> web.Response:
    data = await body(request)
    credential = data.get("credential")
    request.app[WEB_KEY].auth.add_passkey(request[SESSION_KEY], credential if isinstance(credential, dict) else {})
    return web.json_response(_devices(request))


async def remove_passkey(request: web.Request) -> web.Response:
    if not request.app[WEB_KEY].auth.remove_passkey(_id(request, "passkey_id")):
        return error(404, "no such passkey")
    return web.json_response(_devices(request))


# --- the screens (views.py) --------------------------------------------------------------------------------

def _data(request: web.Request) -> Data:
    data = request.app[WEB_KEY].data
    if data is None:
        raise web.HTTPServiceUnavailable(text="the screens are not wired")
    return data


def _course(request: web.Request) -> str | None:
    return " ".join(request.query.get("course", "").split()) or None


async def feed(request: web.Request) -> web.Response:
    """?course=<key>: the feed, all courses or one course's lens, with the «Check» questions of it."""
    course = _course(request)
    answer = _data(request).feed(course)
    answer["check"] = request.app[WEB_KEY].check.items(course)
    answer["problems"] = len(request.app[WEB_KEY].machine.attention(request.app))  # the status dot
    answer["news"] = _data(request).important_news()  # the mark on the News view
    library = _data(request).library
    answer["inbox"] = library.inbox_count() if library is not None else 0  # the mark on the shelves
    return web.json_response(answer)


async def week(request: web.Request) -> web.Response:
    """?start=<a day of the week, YYYY-MM-DD>&course=<key>: the week of that day, this week without it."""
    data = _data(request)
    try:
        day = dt.date.fromisoformat(request.query["start"]) if "start" in request.query else data.now().date()
    except ValueError:
        return error(400, "start: a date like 2026-11-16")
    return web.json_response(data.week(day, _course(request)))


async def deadline(request: web.Request) -> web.Response:
    card = _data(request).deadline_card(request.query.get("uid", ""))
    return web.json_response(card) if card else error(404, "no such deadline")


async def class_(request: web.Request) -> web.Response:
    card = _data(request).class_card(request.query.get("uid", ""))
    return web.json_response(card) if card else error(404, "no such class")


async def course(request: web.Request) -> web.Response:
    found = _data(request).course(" ".join(request.query.get("key", "").split()))
    return web.json_response(found) if found else error(404, "no such course")


# --- news (stage 16.7) ----------------------------------------------------------------------------------------

async def news(request: web.Request) -> web.Response:
    """The live news in rank order."""
    answer = _data(request).news_list()
    answer["problems"] = len(request.app[WEB_KEY].machine.attention(request.app))  # the status dot
    return web.json_response(answer)


async def news_card(request: web.Request) -> web.Response:
    card = _data(request).news_card(_id(request, "news_id"))
    return web.json_response(card) if card else error(404, "no such news item")


async def hide_news(request: web.Request) -> web.Response:
    """«Hide»: the connector's hide_news, journaled with its undo."""
    try:
        change_id, output = await _connector(request).act("hide_news", {"news_id": _id(request, "news_id")})
    except ToolError as e:
        return error(400, str(e))
    return _change(change_id, output)


# --- the owner's changes (stage 16.3) ------------------------------------------------------------------------

def _connector(request: web.Request):
    connector = request.app[WEB_KEY].connector
    if connector is None:
        raise web.HTTPServiceUnavailable(text="changes are not wired")
    return connector


async def check(request: web.Request) -> web.Response:
    """?course=<key>: the «Check» questions, and the courses a deadline without one may take."""
    data = _data(request)
    return web.json_response({"items": request.app[WEB_KEY].check.items(_course(request)),
                              "courses": data.courses(), "today": data.now().date().isoformat()})


def _text(data: dict, name: str) -> str:
    value = data.get(name)
    if not isinstance(value, str) or not value.strip():
        raise web.HTTPBadRequest(text=f"{name} expected")
    return value.strip()


def _change(change_id: int, output) -> web.Response:
    return web.json_response({"change": {"id": change_id, "summary": output.summary,
                                         "undo": output.undo is not None, "note": output.note}})


async def deadline_action(request: web.Request) -> web.Response:
    """POST /deadlines/{action} {uid, ...}: done, not-mine, edit {title?, due?, category?}, confirm (an assumed
    date), merge {keep} (uid is the duplicate that goes), course {course: a key of the lenses}. An own deadline is
    deleted when it is done, not the owner's or a duplicate. The answer: the change, for its undo."""
    data, connector = await body(request), _connector(request)
    action, item_uid = request.match_info["action"], _text(data, "uid")
    merged = _data(request).merged
    d = merged.find(item_uid) or next((own for own in merged.todo.own() if own.key == item_uid), None)
    if d is None:
        return error(404, "this deadline is off the list already")
    own = d.source == "ha"
    if action in ("done", "not-mine"):
        tool, arguments = ("delete_deadline", {"uid": item_uid}) if own else \
            ("hide_deadline", {"uid": item_uid, "reason": action.replace("-", "_")})
    elif action == "merge":
        keep = _text(data, "keep")
        tool, arguments = ("delete_deadline", {"uid": item_uid}) if own else \
            ("hide_deadline", {"uid": item_uid, "reason": "duplicate", "kept": keep})
        if own and keep == item_uid:
            return error(400, "a deadline cannot be a duplicate of itself")
    elif action == "edit":
        tool = "edit_deadline"
        arguments = {"uid": item_uid, **{name: data[name] for name in ("title", "due", "category") if name in data}}
    elif action == "confirm":
        tool, arguments = "confirm_deadline_date", {"uid": item_uid}
    elif action == "course":
        name = _data(request).course_name(_text(data, "course"))
        if name is None:
            return error(400, "no such course")
        tool, arguments = "set_deadline_course", {"uid": item_uid, "course": name}
    else:
        return error(404, "no such action")
    try:
        change_id, output = await connector.act(tool, arguments)
    except ToolError as e:
        return error(400, str(e))
    return _change(change_id, output)


async def put_aside(request: web.Request) -> web.Response:
    """{id}: a «Check» question answered «leave it»: not asked again, until it is undone."""
    question, app = _text(await body(request), "id"), request.app[WEB_KEY]
    connector = _connector(request)
    if not app.check.put_away(question):
        return error(404, "no such question")
    kind = question.split(":", 1)[0]
    summary = {"dup": "Marked two deadlines as different", "course": "Left a deadline outside the courses",
               "assumed": "Left an assumed date as it is"}.get(kind, "Put a question aside")
    undo = [{"tool": "_ask_again", "arguments": {"id": question}}]
    change_id = connector.journal("put_aside", {"id": question}, summary, undo)
    return web.json_response({"change": {"id": change_id, "summary": summary, "undo": True, "note": ""}})


async def undo(request: web.Request) -> web.Response:
    """Undo a change of the journal: the web app's own, or one Claude made through the connector."""
    connector = _connector(request)
    try:
        change = await connector.undo(_id(request, "change_id"))
    except ToolError as e:
        return error(400, str(e))
    return web.json_response({"change": {"id": change["id"], "summary": change["summary"],
                                         "undone_at": change["undone_at"]}})


# --- notifications (stage 16.4) ------------------------------------------------------------------------------

def _push_state(request: web.Request) -> dict:
    push = request.app[WEB_KEY].push
    return {"key": push.vapid.public, "on": push.subscribed(request[SESSION_KEY]["id"]),
            "devices": len(push.devices())}


async def push_state(request: web.Request) -> web.Response:
    """The server's key for the browser's subscription, whether this device gets notifications, how many do."""
    return web.json_response(_push_state(request))


async def subscribe(request: web.Request) -> web.Response:
    """{subscription}: the browser's PushSubscription; this device gets the notifications from now on."""
    data = await body(request)
    try:
        request.app[WEB_KEY].push.subscribe(request[SESSION_KEY]["id"], data.get("subscription"))
    except PushError as e:
        return error(400, str(e))
    return web.json_response(_push_state(request))


async def unsubscribe(request: web.Request) -> web.Response:
    request.app[WEB_KEY].push.unsubscribe(request[SESSION_KEY]["id"])
    return web.json_response(_push_state(request))


async def push_test(request: web.Request) -> web.Response:
    """A message to this device only, to see that notifications come."""
    app = request.app[WEB_KEY]
    lang = app.lang()
    sent = await app.push.send({"title": "infomaster",
                                "body": "Notifications work on this device." if lang != "ru"
                                else "Уведомления на этом устройстве работают.", "url": "/web/#/", "tag": "test"},
                               session_id=request[SESSION_KEY]["id"])
    if not sent:
        return error(502, "The push service did not take it." if lang != "ru" else "Служба уведомлений его не приняла.")
    return web.json_response({"sent": sent})


# --- «+» (stage 16.5) ----------------------------------------------------------------------------------------

FIELD_BYTES = 400        # a form field before the files: a class's uid or a course key
CHUNK = 1 << 16
QUICK_ERRORS = {"unavailable": (503, "One line needs the Anthropic key: add it by hand.",
                                "Для одной строки нужен ключ Anthropic: добавьте вручную."),
                "budget": (429, "The daily LLM limit is spent: add it by hand.",
                           "Дневной лимит LLM исчерпан: добавьте вручную."),
                "empty": (400, "Type what to add.", "Напишите, что добавить.")}


def _say(request: web.Request, en: str, ru: str) -> str:
    return ru if request.app[WEB_KEY].lang() == "ru" else en


def _quick(request: web.Request) -> Quick:
    quick = request.app[WEB_KEY].quick
    if quick is None:
        raise web.HTTPServiceUnavailable(text="the screens are not wired")
    return quick


async def shared(request: web.Request) -> web.Response:
    """Android's «Share» before the service worker took over: the files cannot be kept here (no session is sure), so
    the app opens «+» and asks to share again."""
    raise web.HTTPSeeOther("/web/#/add?shared=lost")


async def add_state(request: web.Request) -> web.Response:
    """What «+» shows: the class in progress and the recent ones, the courses, whether one line can be read."""
    data, quick = _data(request), _quick(request)
    connector = request.app[WEB_KEY].connector
    return web.json_response({**quick.place(), "courses": data.courses(), "line": quick.available(),
                              "library": data.library is not None,
                              "grades": connector is not None and "add_grade" in connector.registry.tools})


async def add_line(request: web.Request) -> web.Response:
    """{text}: what Haiku understood of one line; nothing is saved."""
    text = (await body(request)).get("text")
    try:
        return web.json_response(await _quick(request).read(text if isinstance(text, str) else ""))
    except QuickError as e:
        reason = str(e)
        if reason in QUICK_ERRORS:
            status, en, ru = QUICK_ERRORS[reason]
            return error(status, _say(request, en, ru))
        return error(502, _say(request, f"The line was not read: {reason}", f"Строку не разобрали: {reason}"))


def _library(request: web.Request):
    library = _data(request).library
    if library is None:
        raise web.HTTPServiceUnavailable(text="the library is not wired")
    return library


def _placed(request: web.Request, class_uid: str | None, course_key: str | None) -> dict:
    """course and class_uid for the library from the app's class uid or course key; the inbox without either."""
    if class_uid:
        found = _library(request).class_of(class_uid)
        if found is None:
            raise web.HTTPBadRequest(text="no such class")
        return {"course": found["subject"], "class_uid": class_uid}
    if course_key:
        name = _data(request).course_name(course_key)
        if name is None:
            raise web.HTTPBadRequest(text="no such course")
        return {"course": name, "class_uid": None}
    return {"course": None, "class_uid": None}


def _journal_material(request: web.Request, ids: list[int], what: str) -> dict:
    connector = request.app[WEB_KEY].connector
    summary = f"Added {what} to the library: " + ", ".join(f"#{item_id}" for item_id in ids)
    if connector is None:
        return {"id": None, "summary": summary, "undo": False}
    undo = [{"tool": "remove_material", "arguments": {"id": item_id}} for item_id in ids] \
        if "remove_material" in connector.registry.tools else None
    change_id = connector.journal("add_material", {"ids": ids}, summary, undo)
    return {"id": change_id, "summary": summary, "undo": undo is not None}


async def _form_field(part) -> str:
    data = b""
    while chunk := await part.read_chunk(FIELD_BYTES):
        data += chunk
        if len(data) > FIELD_BYTES:
            raise web.HTTPBadRequest(text=f"{part.name}: up to {FIELD_BYTES} bytes")
    return data.decode(errors="replace").strip()


async def _part_chunks(part):
    while chunk := await part.read_chunk(CHUNK):
        yield chunk


async def add_files(request: web.Request) -> web.Response:
    """Multipart: `class_uid` or `course` (a key) first, then the files; each is written to disk as it comes and
    kept in the library (source web). A refused file stops the upload; the files before it stay, journaled."""
    library = _library(request)
    try:
        reader = await request.multipart()
    except (AssertionError, ValueError, KeyError):
        return error(400, "multipart/form-data expected")
    fields, added = {}, []
    try:
        while (part := await reader.next()) is not None:
            if part.filename is None:
                if part.name not in ("class_uid", "course") or added:
                    return error(400, "class_uid and course come before the files")
                fields[part.name] = await _form_field(part) or None
                continue
            place = _placed(request, fields.get("class_uid"), fields.get("course"))
            added.append(await library.add_stream(_part_chunks(part), name=part.filename,
                                                  media_type=part.headers.get("Content-Type"), source="web",
                                                  author="owner", **place))
    except LibraryError as e:
        if not added:
            return error(400, str(e))
        change = _journal_material(request, added, "files")
        return web.json_response({"added": added, "change": change, "error": str(e)}, status=207)
    if not added:
        return error(400, _say(request, "Choose a file.", "Выберите файл."))
    return web.json_response({"added": added, "change": _journal_material(request, added, "files")})


async def add_note(request: web.Request) -> web.Response:
    """{text, class_uid?, course?}: a note for the library; a line that is only a link is kept as a link."""
    data = await body(request)
    text = data.get("text")
    if not isinstance(text, str) or not text.strip():
        return error(400, _say(request, "Write the note.", "Напишите заметку."))
    text = text.strip()
    place = _placed(request, data.get("class_uid") or None, data.get("course") or None)
    url = first_url(text)
    try:
        if url and url == text:
            item_id = _library(request).add("link", url=url, source="web", author="owner", **place)
        else:
            item_id = _library(request).add("text", text=text, title=" ".join(text.split())[:80], source="web",
                                            author="owner", **place)
    except LibraryError as e:
        return error(400, str(e))
    return web.json_response({"added": [item_id], "change": _journal_material(request, [item_id], "a note")})


async def add_by_hand(request: web.Request) -> web.Response:
    """POST /add/deadline {title, due, category?, course?}, /add/event {title, start, end?, room?, kind?},
    /add/grade {course: a key, label, value}: the connector's tools, journaled with their undo."""
    data, what = await body(request), request.match_info["what"]
    connector = _connector(request)
    if what == "deadline":
        tool = "add_deadline"
        arguments = {"title": data.get("title"), "due": data.get("due"), "category": data.get("category") or "study"}
        if data.get("course"):
            arguments["course"] = data["course"]
    elif what == "event":
        tool = "add_event"
        arguments = {name: data[name] for name in ("title", "start", "end", "room", "kind") if data.get(name)}
    elif what == "grade":
        tool = "add_grade"  # a grade's course is a short label, as a gradebook's: the lens's key is one
        arguments = {"course": data.get("course"), "label": data.get("label"), "value": data.get("value")}
    else:
        return error(404, "no such thing to add")
    try:
        change_id, output = await connector.act(tool, arguments)
    except ToolError as e:
        return error(400, str(e))
    return _change(change_id, output)
