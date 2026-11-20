"""The web app (§8.6, D61, stage 16): a home-screen app on the public port that takes the place of the forum and
the Mini App.

On the public port, next to the Mini App and the connector:
- /web/: the app's page; /web/static/*: its scripts, styles, fonts and icons; /web/sw.js: the service worker, whose
  scope is /web/; /web/manifest.webmanifest;
- /web/api/*: JSON for the app, behind the session cookie (auth.py); what the screens read comes from views.py,
  the «Check» queue from check.py. A request that changes something must come from the app's own origin: the cookie
  is SameSite=Strict already, and the Origin header is checked too.

The owner's changes (stage 16.3) are the connector's write tools (§8.5), called through `Connector.act`: the same
checks, and every change goes into the connector's journal (`via` web) with the steps that undo it.

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
from ..store import Store
from .auth import COOKIE, COOKIE_PATH, SESSION_IDLE, Auth, AuthError, device_name
from .check import Check
from .views import Data

STATIC = Path(__file__).parent / "static"
NO_CACHE = {"Cache-Control": "no-cache"}
CHANGES = {"POST", "PUT", "PATCH", "DELETE"}


class WebApp:
    def __init__(self, env: Env, store: Store, send_code: Callable[[str], Awaitable[None]] | None = None,
                 lang: Callable[[], str] = lambda: "en", now: Callable[[], float] = time.time,
                 data: Data | None = None):
        parts = urlsplit(env.public_url)
        self.origin = f"{parts.scheme}://{parts.netloc}"
        self.lang = lang
        self.data = data  # what the screens read; set by the service, left out by the login's tests
        self.check = Check(data, store) if data is not None else None
        self.connector = None  # the service's Connector: the owner's changes are its tools, journaled with undo
        self.auth = Auth(store, parts.hostname or "", self.origin, send_code, lang, now)

    def attach(self, app: web.Application) -> web.Application:
        app[WEB_KEY] = self
        app.router.add_get("/web", to_index)
        app.router.add_get("/web/", index)
        app.router.add_get("/web/sw.js", service_worker)
        app.router.add_get("/web/manifest.webmanifest", manifest)
        app.router.add_static("/web/static", STATIC)
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
               ("GET", "/check", check),
               ("POST", "/check/put-aside", put_aside),
               ("POST", "/deadlines/{action}", deadline_action),
               ("POST", "/changes/{change_id}/undo", undo)]
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
                              "passkeys": len(app.auth.passkeys())})


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
