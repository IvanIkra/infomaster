"""The web app (§8.6, D61, stage 16): a home-screen app on the public port that takes the place of the forum and
the Mini App.

On the public port, next to the Mini App and the connector:
- /web/: the app's page; /web/static/*: its scripts, styles, fonts and icons; /web/sw.js: the service worker, whose
  scope is /web/; /web/manifest.webmanifest;
- /web/api/*: JSON for the app, behind the session cookie (auth.py). A request that changes something must come
  from the app's own origin: the cookie is SameSite=Strict already, and the Origin header is checked too.

The client is plain ES modules, no build step: Preact and htm are vendored in static/vendor, the fonts in static/fonts.
"""

import json
import time
from collections.abc import Awaitable, Callable
from pathlib import Path
from urllib.parse import urlsplit

from aiohttp import web

from ..config import Env
from ..store import Store
from .auth import COOKIE, COOKIE_PATH, SESSION_IDLE, Auth, AuthError, device_name

STATIC = Path(__file__).parent / "static"
NO_CACHE = {"Cache-Control": "no-cache"}
CHANGES = {"POST", "PUT", "PATCH", "DELETE"}


class WebApp:
    def __init__(self, env: Env, store: Store, send_code: Callable[[str], Awaitable[None]] | None = None,
                 lang: Callable[[], str] = lambda: "en", now: Callable[[], float] = time.time):
        parts = urlsplit(env.public_url)
        self.origin = f"{parts.scheme}://{parts.netloc}"
        self.lang = lang
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
               ("DELETE", "/passkeys/{passkey_id}", remove_passkey)]
        for method, path, handler in api:
            app.router.add_route(method, "/web/api" + path, guarded(handler))
        return app


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
