"""OAuth 2.1 of the connector (§8.5): infomaster is its own authorization server, for one person.

What a remote MCP server needs (MCP authorization, revision 2026-07-28, and claude.ai's client):
- protected resource metadata (RFC 9728) at /.well-known/oauth-protected-resource/mcp and at the root path, named
  by `resource_metadata` in the 401 of /mcp;
- authorization server metadata (RFC 8414) at /.well-known/oauth-authorization-server, the issuer being PUBLIC_URL;
- Dynamic Client Registration (RFC 7591) at /oauth/register: claude.ai registers itself when it connects. Only the
  redirect URIs of claude.ai and claude.com and loopback ones (Claude Code) are accepted. Client ID Metadata
  Documents are not advertised, so claude.ai falls back to registration;
- /oauth/authorize with PKCE S256 only, `iss` in the answer (RFC 9207), the `resource` (RFC 8707) checked;
- /oauth/token: the code, then refresh tokens, which rotate. An access token lives an hour;
- /oauth/revoke (RFC 7009).

There is no password: the authorize page asks the bot to send a one-time code to the owner's private chat
(TG_OWNER_ID), and the owner types it in. Tokens and codes are kept as SHA-256 hashes; codes and the open authorize
pages live in memory only (a restart asks to sign in again, nothing more).
"""

import base64
import hashlib
import hmac
import html
import json
import logging
import secrets
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from urllib.parse import urlencode, urlsplit

from aiohttp import web

from ..i18n import tr
from ..store import Store

log = logging.getLogger(__name__)

SCHEMA = """
create table if not exists connector_clients (  -- clients registered for the MCP connector (RFC 7591)
    id text primary key,        -- client_id
    name text not null,
    redirect_uris text not null,    -- JSON
    created_at integer not null     -- unix time
);
create table if not exists connector_grants (   -- connections the owner allowed: one refresh token each
    id integer primary key,
    client_id text not null,
    created_at integer not null,
    used_at integer not null,
    refresh_hash text,          -- SHA-256 of the refresh token
    refresh_until integer,
    previous_hash text,         -- the one it replaced, still good for REFRESH_GRACE
    previous_until integer,
    revoked_at integer
);
create table if not exists connector_tokens (   -- access tokens
    hash text primary key,      -- SHA-256
    grant_id integer not null,
    expires_at integer not null
);
"""

ACCESS_TTL = 3600               # seconds of an access token
REFRESH_TTL = 30 * 86400        # a refresh token unused this long is gone
REFRESH_GRACE = 120             # a rotated refresh token still works this long: two refreshes in flight
CODE_TTL = 300                  # an authorization code
PAGE_TTL = 600                  # an authorize page and the code the bot sent for it
ATTEMPTS = 5                    # wrong codes on one page
RESEND_AFTER = 30               # seconds between codes of one page
CODES_PER_HOUR = 10             # codes the bot sends an hour, all pages together
CLIENTS_MAX = 50
PAGES_MAX = 20                  # open authorize pages kept, the oldest go first
NAME_CHARS = 80
# Where the browser may be sent back with a code: claude.ai's callback (the web, desktop and phone apps) and a
# loopback address on any port (Claude Code).
TRUSTED_HOSTS = ("claude.ai", "claude.com")
LOOPBACK = ("localhost", "127.0.0.1", "::1")


class OAuthError(Exception):
    def __init__(self, error: str, description: str, status: int = 400):
        super().__init__(description)
        self.error, self.description, self.status = error, description, status

    def response(self) -> web.Response:
        return web.json_response({"error": self.error, "error_description": self.description}, status=self.status,
                                 headers=NO_STORE)


NO_STORE = {"Cache-Control": "no-store", "Pragma": "no-cache"}


def digest(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


def s256(verifier: str) -> str:
    return base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()


def redirect_allowed(uri: str) -> bool:
    try:
        parts = urlsplit(uri)
        port = parts.port
    except ValueError:
        return False
    if parts.fragment or parts.username or parts.password:
        return False
    if parts.scheme == "https" and parts.hostname in TRUSTED_HOSTS and port is None:
        return True
    return parts.scheme == "http" and parts.hostname in LOOPBACK


def same_redirect(registered: str, given: str) -> bool:
    """Exact, except the port of a loopback address, which a native client picks per run (RFC 8252, 7.3)."""
    if registered == given:
        return True
    a, b = urlsplit(registered), urlsplit(given)
    return a.scheme == b.scheme == "http" and a.hostname in LOOPBACK and a.hostname == b.hostname and \
        a.path == b.path and a.query == b.query


@dataclass
class Page:
    """An open authorize page: the request it answers and the code the bot sent for it."""
    client_id: str
    client_name: str
    redirect_uri: str
    state: str | None
    challenge: str
    resource: str | None
    opened: float
    code_hash: str | None = None
    sent_at: float = 0
    attempts: int = 0


@dataclass
class Code:
    client_id: str
    redirect_uri: str
    challenge: str
    resource: str | None
    expires: float


@dataclass
class OAuth:
    store: Store
    issuer: str                 # PUBLIC_URL
    send_code: Callable[[str], Awaitable[None]] | None = None  # a text to the owner's private chat with the bot
    lang: Callable[[], str] = lambda: "en"
    now: Callable[[], float] = time.time
    pages: dict[str, Page] = field(default_factory=dict)
    codes: dict[str, Code] = field(default_factory=dict)        # by hash
    spent: dict[str, tuple[int, float]] = field(default_factory=dict)  # a used code's hash -> its grant, until
    sent: list[float] = field(default_factory=list)              # when codes went out, the last hour

    def __post_init__(self):
        self.db = self.store.db
        self.db.executescript(SCHEMA)

    @property
    def resource(self) -> str:
        return self.issuer + "/mcp"

    @property
    def resource_metadata(self) -> str:
        return self.issuer + "/.well-known/oauth-protected-resource/mcp"

    # --- metadata -------------------------------------------------------------------------

    def protected_resource(self) -> dict:
        return {"resource": self.resource, "authorization_servers": [self.issuer],
                "bearer_methods_supported": ["header"], "resource_name": "infomaster"}

    def authorization_server(self) -> dict:
        return {"issuer": self.issuer,
                "authorization_endpoint": self.issuer + "/oauth/authorize",
                "token_endpoint": self.issuer + "/oauth/token",
                "registration_endpoint": self.issuer + "/oauth/register",
                "revocation_endpoint": self.issuer + "/oauth/revoke",
                "response_types_supported": ["code"],
                "grant_types_supported": ["authorization_code", "refresh_token"],
                "code_challenge_methods_supported": ["S256"],
                "token_endpoint_auth_methods_supported": ["none"],
                "revocation_endpoint_auth_methods_supported": ["none"],
                "authorization_response_iss_parameter_supported": True}

    def resource_ok(self, resource: str | None) -> bool:
        """The `resource` of a request names this server, or none is given."""
        if resource is None:
            return True
        canonical = resource.rstrip("/")
        return canonical.lower() in (self.resource.lower(), self.issuer.lower())

    # --- registration -------------------------------------------------------------------------

    def register(self, meta) -> dict:
        if not isinstance(meta, dict):
            raise OAuthError("invalid_client_metadata", "expected a JSON object")
        uris = meta.get("redirect_uris")
        if not isinstance(uris, list) or not uris or not all(isinstance(uri, str) for uri in uris):
            raise OAuthError("invalid_redirect_uri", "redirect_uris: a list of addresses")
        if not all(redirect_allowed(uri) for uri in uris):
            raise OAuthError("invalid_redirect_uri", "only claude.ai's callback and loopback addresses are accepted")
        grants = meta.get("grant_types", ["authorization_code"])
        if not isinstance(grants, list) or "authorization_code" not in grants:
            raise OAuthError("invalid_client_metadata", "grant_types must include authorization_code")
        if meta.get("response_types", ["code"]) != ["code"]:
            raise OAuthError("invalid_client_metadata", "response_types: only code")
        name = meta.get("client_name") if isinstance(meta.get("client_name"), str) else ""
        name = " ".join(name.split())[:NAME_CHARS] or urlsplit(uris[0]).hostname or "client"
        now = int(self.now())
        client_id = secrets.token_urlsafe(16)
        with self.db:
            # Registrations that never led to a connection go after a day; the oldest beyond the cap too.
            self.db.execute("delete from connector_clients where created_at < ? and id not in "
                            "(select client_id from connector_grants where revoked_at is null)", (now - 86400,))
            self.db.execute("delete from connector_clients where id in (select id from connector_clients where id "
                            "not in (select client_id from connector_grants where revoked_at is null) "
                            "order by created_at desc limit -1 offset ?)", (CLIENTS_MAX - 1,))
            self.db.execute("insert into connector_clients(id, name, redirect_uris, created_at) values(?, ?, ?, ?)",
                            (client_id, name, json.dumps(uris), now))
        return {"client_id": client_id, "client_id_issued_at": now, "client_name": name, "redirect_uris": uris,
                "grant_types": ["authorization_code", "refresh_token"], "response_types": ["code"],
                "token_endpoint_auth_method": "none"}

    def client(self, client_id) -> dict | None:
        if not isinstance(client_id, str):
            return None
        row = self.db.execute("select * from connector_clients where id = ?", (client_id,)).fetchone()
        return {**dict(row), "redirect_uris": json.loads(row["redirect_uris"])} if row else None

    # --- the authorize page ------------------------------------------------------------------

    def open_page(self, query) -> tuple[str, Page]:
        """A page for a valid authorization request. OAuthError with status 400 when the client or its redirect
        cannot be trusted (nothing is sent back then); with status 302 for an error to send back to the client."""
        self._expire()
        client = self.client(query.get("client_id"))
        if client is None:
            raise OAuthError("invalid_client", "unknown client: connect again from the client")
        redirect_uri = query.get("redirect_uri")
        if not isinstance(redirect_uri, str) or not any(same_redirect(uri, redirect_uri)
                                                         for uri in client["redirect_uris"]):
            raise OAuthError("invalid_request", "the redirect_uri is not the client's")
        state = query.get("state")
        page = Page(client["id"], client["name"], redirect_uri, state, "", query.get("resource"), self.now())
        if query.get("response_type") != "code":
            raise self._back(page, "unsupported_response_type", "only the code flow")
        challenge = query.get("code_challenge") or ""
        if query.get("code_challenge_method") != "S256" or not 43 <= len(challenge) <= 128:
            raise self._back(page, "invalid_request", "PKCE with S256 is required")
        if not self.resource_ok(page.resource):
            raise self._back(page, "invalid_target", "the resource is not this server")
        page.challenge = challenge
        page_id = secrets.token_urlsafe(24)
        self.pages[page_id] = page
        while len(self.pages) > PAGES_MAX:  # the page is public: anyone may open it, memory is not theirs
            del self.pages[next(iter(self.pages))]
        return page_id, page

    def _back(self, page: Page, error: str, description: str) -> OAuthError:
        failure = OAuthError(error, description, status=302)
        failure.location = self.redirect(page, {"error": error, "error_description": description})
        return failure

    def redirect(self, page: Page, params: dict) -> str:
        params = {**params, **({"state": page.state} if page.state is not None else {}), "iss": self.issuer}
        return page.redirect_uri + ("&" if urlsplit(page.redirect_uri).query else "?") + urlencode(params)

    def page(self, page_id) -> Page | None:
        self._expire()
        return self.pages.get(page_id) if isinstance(page_id, str) else None

    async def send(self, page_id: str) -> str | None:
        """The bot sends a code for the page; the reason it did not, or None."""
        lang, page, now = self.lang(), self.pages[page_id], self.now()
        if now - page.sent_at < RESEND_AFTER:
            return tr(lang, "A code was sent a moment ago: check the chat with the bot.",
                      "Код только что отправлен: посмотрите чат с ботом.")
        self.sent = [at for at in self.sent if now - at < 3600]
        if len(self.sent) >= CODES_PER_HOUR:
            return tr(lang, "Too many codes this hour: try again later.",
                      "Слишком много кодов за час: попробуйте позже.")
        if self.send_code is None:
            return tr(lang, "The bot is not running.", "Бот не запущен.")
        code = f"{secrets.randbelow(10**6):06d}"
        text = tr(lang, f"{code} is the code to connect «{page.client_name}» to infomaster. It reads and changes "
                        "your deadlines, timetable, grades, chats and settings. If you did not ask for it, ignore "
                        "this message.",
                  f"{code} — код, чтобы подключить «{page.client_name}» к infomaster. Он читает и меняет дедлайны, "
                  "расписание, оценки, чаты и настройки. Если вы его не запрашивали, не обращайте внимания.")
        try:
            await self.send_code(text)
        except Exception as e:
            log.warning("the bot could not send the connector's code: %s", e)
            return tr(lang, "The bot could not write to you: open the chat with the bot, press Start and try again.",
                      "Бот не смог вам написать: откройте чат с ботом, нажмите «Старт» и попробуйте снова.")
        page.code_hash, page.sent_at, page.attempts = digest(code), now, 0
        self.sent.append(now)
        return None

    def allow(self, page_id: str, code: str) -> str:
        """The redirect with an authorization code for the right one-time code; OAuthError (status 400) else."""
        lang, page = self.lang(), self.pages[page_id]
        if page.code_hash is None:
            raise OAuthError("access_denied", tr(lang, "Ask for a code first.", "Сначала запросите код."))
        page.attempts += 1
        typed = "".join(code.split()) if isinstance(code, str) else ""
        if not hmac.compare_digest(digest(typed), page.code_hash):
            if page.attempts >= ATTEMPTS:
                page.code_hash = None
                raise OAuthError("access_denied", tr(lang, "Wrong code too many times: ask for a new one.",
                                                     "Слишком много неверных кодов: запросите новый."))
            raise OAuthError("access_denied", tr(lang, "Wrong code.", "Неверный код."))
        del self.pages[page_id]
        auth_code = secrets.token_urlsafe(32)
        self.codes[digest(auth_code)] = Code(page.client_id, page.redirect_uri, page.challenge, page.resource,
                                             self.now() + CODE_TTL)
        log.info("the owner allowed the connector client %s", page.client_name)
        return self.redirect(page, {"code": auth_code})

    def deny(self, page_id: str) -> str:
        page = self.pages.pop(page_id)
        return self.redirect(page, {"error": "access_denied", "error_description": "the owner said no"})

    def _expire(self) -> None:
        now = self.now()
        for key in [key for key, page in self.pages.items() if now - page.opened > PAGE_TTL]:
            del self.pages[key]
        for key in [key for key, code in self.codes.items() if code.expires < now]:
            del self.codes[key]
        for key in [key for key, (_, until) in self.spent.items() if until < now]:
            del self.spent[key]

    # --- tokens -------------------------------------------------------------------------------

    def token(self, form) -> dict:
        grant_type = form.get("grant_type")
        if grant_type == "authorization_code":
            return self._exchange(form)
        if grant_type == "refresh_token":
            return self._refresh(form)
        raise OAuthError("unsupported_grant_type", "authorization_code or refresh_token")

    def _exchange(self, form) -> dict:
        self._expire()
        code_hash = digest(form.get("code") or "")
        code = self.codes.pop(code_hash, None)
        if code is None:
            if code_hash in self.spent:  # a code used twice: the tokens of its first use are not to be trusted
                self.revoke_grant(self.spent.pop(code_hash)[0])
            raise OAuthError("invalid_grant", "the code is unknown, used or expired")
        if form.get("client_id") != code.client_id:
            raise OAuthError("invalid_grant", "the code was issued to another client")
        if form.get("redirect_uri") != code.redirect_uri:
            raise OAuthError("invalid_grant", "redirect_uri differs from the authorization request")
        verifier = form.get("code_verifier") or ""
        if not 43 <= len(verifier) <= 128 or not hmac.compare_digest(s256(verifier), code.challenge):
            raise OAuthError("invalid_grant", "the code_verifier does not match")
        resource = form.get("resource")
        if not self.resource_ok(resource) or (code.resource and resource and
                                              code.resource.rstrip("/") != resource.rstrip("/")):
            raise OAuthError("invalid_target", "the resource is not this server")
        now = int(self.now())
        with self.db:
            grant_id = self.db.execute("insert into connector_grants(client_id, created_at, used_at) values(?, ?, ?)",
                                       (code.client_id, now, now)).lastrowid
        self.spent[code_hash] = (grant_id, self.now() + CODE_TTL)
        return self._issue(grant_id)

    def _refresh(self, form) -> dict:
        token_hash, now = digest(form.get("refresh_token") or ""), int(self.now())
        row = self.db.execute(
            "select * from connector_grants where revoked_at is null and ((refresh_hash = ? and refresh_until > ?) "
            "or (previous_hash = ? and previous_until > ?))", (token_hash, now, token_hash, now)).fetchone()
        if row is None:
            raise OAuthError("invalid_grant", "the refresh token is unknown, revoked or expired")
        if form.get("client_id") not in (None, row["client_id"]):
            raise OAuthError("invalid_grant", "the refresh token was issued to another client")
        if not self.resource_ok(form.get("resource")):
            raise OAuthError("invalid_target", "the resource is not this server")
        return self._issue(row["id"], previous=row["refresh_hash"] if row["refresh_hash"] == token_hash else None)

    def _issue(self, grant_id: int, previous: str | None = None) -> dict:
        """A new access token and a new refresh token; the refresh token used stays good for REFRESH_GRACE."""
        access, refresh, now = secrets.token_urlsafe(32), secrets.token_urlsafe(32), int(self.now())
        with self.db:
            self.db.execute("delete from connector_tokens where expires_at < ?", (now,))
            self.db.execute("insert into connector_tokens(hash, grant_id, expires_at) values(?, ?, ?)",
                            (digest(access), grant_id, now + ACCESS_TTL))
            if previous is not None:
                self.db.execute("update connector_grants set previous_hash = ?, previous_until = ? where id = ?",
                                (previous, now + REFRESH_GRACE, grant_id))
            self.db.execute("update connector_grants set refresh_hash = ?, refresh_until = ?, used_at = ? "
                            "where id = ?", (digest(refresh), now + REFRESH_TTL, now, grant_id))
        return {"access_token": access, "token_type": "Bearer", "expires_in": ACCESS_TTL, "refresh_token": refresh}

    def check(self, token: str) -> int | None:
        """The connection of a valid access token, or None."""
        now = int(self.now())
        row = self.db.execute(
            "select g.id, g.used_at from connector_tokens t join connector_grants g on g.id = t.grant_id "
            "where t.hash = ? and t.expires_at > ? and g.revoked_at is null", (digest(token), now)).fetchone()
        if row is None:
            return None
        if now - row["used_at"] > 60:
            with self.db:
                self.db.execute("update connector_grants set used_at = ? where id = ?", (now, row["id"]))
        return row["id"]

    def revoke(self, token: str) -> None:
        """RFC 7009: an access token goes alone, a refresh token takes its connection with it."""
        token_hash = digest(token)
        row = self.db.execute("select id from connector_grants where refresh_hash = ? or previous_hash = ?",
                              (token_hash, token_hash)).fetchone()
        if row is not None:
            self.revoke_grant(row["id"])
        with self.db:
            self.db.execute("delete from connector_tokens where hash = ?", (token_hash,))

    def revoke_grant(self, grant_id: int) -> bool:
        with self.db:
            done = self.db.execute("update connector_grants set revoked_at = ?, refresh_hash = null, "
                                   "previous_hash = null where id = ? and revoked_at is null",
                                   (int(self.now()), grant_id)).rowcount
            self.db.execute("delete from connector_tokens where grant_id = ?", (grant_id,))
        return bool(done)

    def connections(self) -> list[dict]:
        """For the web app: the connections in force, the newest first."""
        rows = self.db.execute(
            "select g.id, g.created_at, g.used_at, c.name, c.redirect_uris from connector_grants g "
            "left join connector_clients c on c.id = g.client_id where g.revoked_at is null and g.refresh_until > ? "
            "order by g.used_at desc", (int(self.now()),))
        return [{"id": row["id"], "name": row["name"] or "?", "created_at": row["created_at"],
                 "used_at": row["used_at"],
                 "redirect": urlsplit(json.loads(row["redirect_uris"] or '[""]')[0]).hostname} for row in rows]


# --- HTTP ---------------------------------------------------------------------------------------

OAUTH_KEY = web.AppKey("oauth", OAuth)
FRAME_DENY = {"X-Frame-Options": "DENY", "Content-Security-Policy": "frame-ancestors 'none'",
              "Referrer-Policy": "no-referrer", **NO_STORE}


async def protected_resource(request: web.Request) -> web.Response:
    return web.json_response(request.app[OAUTH_KEY].protected_resource())


async def authorization_server(request: web.Request) -> web.Response:
    return web.json_response(request.app[OAUTH_KEY].authorization_server())


async def register(request: web.Request) -> web.Response:
    try:
        meta = await request.json()
    except ValueError:
        return OAuthError("invalid_client_metadata", "expected JSON").response()
    try:
        return web.json_response(request.app[OAUTH_KEY].register(meta), status=201, headers=NO_STORE)
    except OAuthError as e:
        return e.response()


async def _form(request: web.Request) -> dict:
    form = {key: value for key, value in (await request.post()).items() if isinstance(value, str)}
    scheme, _, credentials = request.headers.get("Authorization", "").partition(" ")
    if scheme.lower() == "basic" and "client_id" not in form:  # a client that sends its id the other way
        try:
            form["client_id"] = base64.b64decode(credentials).decode().split(":", 1)[0]
        except ValueError:
            pass
    return form


async def token(request: web.Request) -> web.Response:
    try:
        return web.json_response(request.app[OAUTH_KEY].token(await _form(request)), headers=NO_STORE)
    except OAuthError as e:
        return e.response()


async def revoke(request: web.Request) -> web.Response:
    form = await _form(request)
    if form.get("token"):
        request.app[OAUTH_KEY].revoke(form["token"])
    return web.Response(status=200, headers=NO_STORE)


async def authorize(request: web.Request) -> web.Response:
    oauth = request.app[OAUTH_KEY]
    try:
        page_id, page = oauth.open_page(request.query)
    except OAuthError as e:
        if e.status == 302:
            raise web.HTTPFound(e.location) from None
        return _page(oauth.lang(), None, None, e.description, status=400)
    return _page(oauth.lang(), page_id, page)


async def authorize_form(request: web.Request) -> web.Response:
    oauth = request.app[OAUTH_KEY]
    form = await request.post()
    page_id, action = form.get("page"), form.get("action")
    page = oauth.page(page_id)
    if page is None:
        return _page(oauth.lang(), None, None, tr(oauth.lang(), "This page has expired: connect again from Claude.",
                                                  "Страница устарела: подключитесь заново из Claude."), status=400)
    if action == "deny":
        raise web.HTTPSeeOther(oauth.deny(page_id))
    if action == "send":
        problem = await oauth.send(page_id)
        return _page(oauth.lang(), page_id, page, problem)
    if action == "allow":
        try:
            raise web.HTTPSeeOther(oauth.allow(page_id, form.get("code")))
        except OAuthError as e:
            still = oauth.page(page_id)
            return _page(oauth.lang(), page_id if still else None, still or page, e.description, status=400)
    return _page(oauth.lang(), page_id, page, "?", status=400)


PAGE = """<!doctype html>
<html lang="{lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>infomaster</title>
<style>
  :root {{ color-scheme: light dark; --fg: #111; --bg: #fff; --muted: #666; --accent: #2f6fed; --card: #f2f2f5; }}
  @media (prefers-color-scheme: dark) {{ :root {{ --fg: #eee; --bg: #111; --muted: #aaa; --accent: #6d9cff;
    --card: #1e1e22; }} }}
  body {{ margin: 40px auto; max-width: 480px; padding: 0 16px; font: 17px/1.5 -apple-system, system-ui, sans-serif;
         color: var(--fg); background: var(--bg); }}
  h1 {{ font-size: 22px; }}
  .muted {{ color: var(--muted); font-size: 15px; }}
  .error {{ color: #d33; }}
  .card {{ background: var(--card); border-radius: 12px; padding: 12px 16px; }}
  input {{ font: inherit; font-size: 24px; letter-spacing: 4px; width: 100%; box-sizing: border-box; padding: 10px;
          border-radius: 10px; border: 1px solid var(--muted); background: var(--bg); color: var(--fg); }}
  button {{ font: inherit; padding: 10px 18px; border-radius: 10px; border: 0; margin: 12px 8px 0 0; cursor: pointer; }}
  .primary {{ background: var(--accent); color: #fff; }}
  .plain {{ background: var(--card); color: var(--fg); }}
</style>
</head>
<body>
<h1>{title}</h1>
{body}
</body>
</html>
"""


def _page(lang: str, page_id: str | None, page: Page | None, problem: str | None = None,
          status: int = 200) -> web.Response:
    esc = html.escape
    parts = []
    if page is not None:
        host = urlsplit(page.redirect_uri).hostname or ""
        where = tr(lang, "a program on this computer (a loopback address)",
                   "программу на этом компьютере (loopback)") if host in LOOPBACK else host
        parts.append(f'<p class="card"><b>{esc(page.client_name)}</b> ' + esc(tr(
            lang, "wants to connect to infomaster. It will read and change your deadlines, timetable, grades, news, "
                  "chat messages and settings.",
            "хочет подключиться к infomaster. Он будет читать и менять дедлайны, расписание, оценки, новости, "
            "сообщения чатов и настройки.")) + "</p>")
        back = tr(lang, "After you allow it, the browser goes back to ", "После разрешения браузер вернётся в ")
        parts.append(f'<p class="muted">{esc(back)}<b>{esc(where)}</b>.</p>')
    if problem:
        parts.append(f'<p class="error">{esc(problem)}</p>')
    if page_id is not None and page is not None:
        hidden = f'<input type="hidden" name="page" value="{esc(page_id)}">'
        if page.code_hash is None:
            parts.append(f'<form method="post">{hidden}<p>' + esc(tr(
                lang, "To prove it is you, the bot sends a one-time code to your private chat with it.",
                "Чтобы убедиться, что это вы, бот пришлёт одноразовый код в личный чат с ним.")) +
                '</p><button class="primary" name="action" value="send">' +
                esc(tr(lang, "Send a code to Telegram", "Прислать код в Telegram")) +
                '</button><button class="plain" name="action" value="deny">' + esc(tr(lang, "Cancel", "Отмена")) +
                "</button></form>")
        else:
            parts.append(f'<form method="post">{hidden}<p>' + esc(tr(
                lang, "The bot sent a code to your private chat. Type it here:",
                "Бот прислал код в личный чат. Введите его:")) +
                '</p><input name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="12" autofocus>'
                '<button class="primary" name="action" value="allow">' + esc(tr(lang, "Allow", "Разрешить")) +
                '</button><button class="plain" name="action" value="send">' +
                esc(tr(lang, "New code", "Новый код")) + '</button><button class="plain" name="action" '
                'value="deny">' + esc(tr(lang, "Deny", "Запретить")) + "</button></form>")
    title = tr(lang, "Connect to infomaster", "Подключение к infomaster")
    return web.Response(text=PAGE.format(lang=lang, title=esc(title), body="\n".join(parts)), status=status,
                        content_type="text/html", headers=FRAME_DENY)


def add_routes(app: web.Application, oauth: OAuth) -> None:
    app[OAUTH_KEY] = oauth
    app.router.add_get("/.well-known/oauth-protected-resource", protected_resource)
    app.router.add_get("/.well-known/oauth-protected-resource/mcp", protected_resource)
    app.router.add_get("/.well-known/oauth-authorization-server", authorization_server)
    app.router.add_post("/oauth/register", register)
    app.router.add_get("/oauth/authorize", authorize)
    app.router.add_post("/oauth/authorize", authorize_form)
    app.router.add_post("/oauth/token", token)
    app.router.add_post("/oauth/revoke", revoke)
