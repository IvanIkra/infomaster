"""HSE App X deadlines (§5.2, §8.1, §8.4, D7): the port of Tablo's sources/hseapp.py (§5.1, research/hseapp).

The app's own Keycloak client (public, no secret) gives a 3-hour access token and a refresh token whose 21-day window
slides with every refresh. Both live in kv "hseapp.auth" and go nowhere else: not into the status, the log, the forum
or the Mini App. The access token is refreshed when under 10 minutes are left, and at once on a 401; the refresh token
rotates, so every answer is saved at once. A refresh answered 400 or 401 (invalid_grant: three weeks without a
refresh, or a logout) erases the tokens: a login is needed. Two clients refreshing one token chain throw each other
out, so Tablo's own HSE App X must be off before this one runs (`hseapp_enabled` here).

The login is PKCE in a browser, started from the Mini App. The client accepts any http(s) return address, so the
browser comes back to our public port behind Funnel, /hseapp/callback. That route has no initData: the PKCE `state`
is its protection, and only the last 5 login links work (kept in memory: a restart asks for a new link). The
fallback is the app's own ru.hse.hseappx:// address, with the return address pasted by hand in the Mini App.

Every 15 minutes, when `hseapp_enabled` and logged in: POST /v2/deadlines with is_completed false, then true, up to
10 pages each, 60 s per request. ACTIVE ones come from the first, COMPLETED* from the second; the discipline is cut at
the first " (", expires_at becomes a Moscow date. The collector never calls the LLM (§4): `on_poll` hands both lists
to the processing (processing/hseapp.py), which labels them in English and keeps them for the merge. A failed poll
keeps what the last good one gave.

The System topic hears once per change of state: a login is needed (with a button to the Mini App's screen) and the
login is back; and once when the API has failed for 3 hours, and when it works again.
"""

import asyncio
import base64
import datetime as dt
import hashlib
import json
import logging
import secrets
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from urllib.parse import parse_qs, urlencode, urlsplit

import aiohttp

from .. import clock
from ..i18n import Phrase
from ..settings import Settings
from ..store import Store

log = logging.getLogger(__name__)

KEYCLOAK = "https://saml.hse.ru/realms/hse/protocol/openid-connect"
TOKEN_URL = KEYCLOAK + "/token"
AUTH_URL = KEYCLOAK + "/auth"
DEADLINES_URL = "https://api.hseapp.ru/v2/deadlines"
CLIENT_ID = "app-x-android"
APP_REDIRECT_URI = "ru.hse.hseappx://saml.hse.ru/authorize_callback"  # the app's own: the return address is pasted
SCOPE = "profile email"
CALLBACK_PATH = "/hseapp/callback"  # on the public port: the browser comes back here after the login

AUTH_KEY = "hseapp.auth"            # the tokens, a secret: unix seconds for the times, as in Tablo (§9)
TOLD_KEY = "hseapp.login_told"      # the System topic has heard that a login is needed
REFRESH_MARGIN = 10 * 60            # seconds: the access token is refreshed this long before it expires
REFRESH_LIFETIME = 21 * 86400       # seconds, when the answer does not say
POLL_EVERY = 15 * 60                # seconds
TIMEOUT = 60                        # seconds, each request of a poll
LOGIN_TIMEOUT = 30                  # seconds, the exchange of the code
MAX_PAGES = 10
PENDING_LOGINS = 5                  # login links that still work
STALE_AFTER = dt.timedelta(hours=3)  # failing longer than this is told to the System topic (§5.1)
LOGIN_ERROR = "login needed: "      # the status error when Keycloak ended the session


class LoginNeeded(Exception):
    pass


# --- the login ----------------------------------------------------------------------


class Auth:
    """Tokens in kv; the refresh token rotates, so every answer is saved at once."""

    def __init__(self, store: Store, now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.now = store, now
        self._pending: dict[str, tuple[str, str]] = {}  # state -> (PKCE verifier, return address), oldest first
        # One token request at a time: a refresh and a login, or two refreshes, must not overwrite each other.
        self._lock = asyncio.Lock()

    def _seconds(self) -> float:
        return self.now().timestamp()

    @property
    def data(self) -> dict:
        return self.store.get_json(AUTH_KEY) or {}

    @property
    def logged_in(self) -> bool:
        data = self.data
        return bool(data.get("refresh_token")) and data.get("refresh_expires_at", 0) > self._seconds()

    @property
    def ever(self) -> bool:
        """Tokens were saved once (erased ones too): a login needed now means the session ended."""
        return self.store.get_json(AUTH_KEY) is not None

    def save(self, answer: dict) -> None:
        if not isinstance(answer, dict) or not answer.get("access_token") or not answer.get("refresh_token"):
            raise ValueError("token response without tokens")
        now = self._seconds()
        self.store.put_json(AUTH_KEY, {
            "access_token": answer["access_token"],
            "expires_at": now + int(answer.get("expires_in", 300)),
            "refresh_token": answer["refresh_token"],
            "refresh_expires_at": now + int(answer.get("refresh_expires_in", 0) or REFRESH_LIFETIME),
            "refreshed_at": now,
        })

    def forget(self) -> None:
        self.store.put_json(AUTH_KEY, {})

    async def access_token(self, session: aiohttp.ClientSession, force: bool = False) -> str:
        async with self._lock:
            data = self.data
            if not force and data.get("access_token") and \
                    data.get("expires_at", 0) - REFRESH_MARGIN > self._seconds():
                return data["access_token"]
            if not data.get("refresh_token"):
                raise LoginNeeded("not logged in")
            await self._token_request(session, {"grant_type": "refresh_token",
                                                "refresh_token": data["refresh_token"]})
            return self.data["access_token"]

    async def _token_request(self, session: aiohttp.ClientSession, form: dict) -> None:
        """Call under the lock. The body is never logged: it holds the tokens."""
        async with session.post(TOKEN_URL, data={"client_id": CLIENT_ID, **form}) as response:
            body = await response.text()
            if response.status in (400, 401):
                # invalid_grant: the session ended (21 days without a refresh, or a logout).
                error = _json_field(body, "error") or f"HTTP {response.status}"
                if form["grant_type"] == "refresh_token":
                    self.forget()
                raise LoginNeeded(error)
            response.raise_for_status()
        self.save(json.loads(body))

    def login_url(self, redirect_uri: str) -> str:
        verifier = secrets.token_urlsafe(48)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
        state = secrets.token_urlsafe(16)
        kept = list(self._pending.items())[-(PENDING_LOGINS - 1):]
        self._pending = {**dict(kept), state: (verifier, redirect_uri)}
        return AUTH_URL + "?" + urlencode({
            "client_id": CLIENT_ID,
            "redirect_uri": redirect_uri,
            "response_type": "code",
            "scope": SCOPE,
            "state": state,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
        })

    async def finish_login(self, session: aiohttp.ClientSession, returned: str) -> None:
        """`returned`: the return address with ?state=…&code=…, from the callback or pasted by hand. Raises
        ValueError with the reason (a Phrase), for the Mini App and the callback page."""
        query = parse_qs(urlsplit(returned.strip()).query)
        code, state = _first(query, "code"), _first(query, "state")
        if not code:
            if error := _first(query, "error"):
                why = _first(query, "error_description") or error
                raise ValueError(Phrase(f"HSE did not let you in: {why}", f"ВШЭ не пустила: {why}"))
            raise ValueError(Phrase("the address has no code: copy the whole address from the address bar",
                                    "в адресе нет кода: скопируйте адрес из адресной строки целиком"))
        verifier, redirect_uri = self._pending.pop(state, (None, None)) if state else (None, None)
        if not verifier:
            raise ValueError(Phrase("this login link is out of date: open a new one in the Mini App",
                                    "эта ссылка входа устарела: откройте новую в Mini App"))
        try:
            async with self._lock:
                await self._token_request(session, {"grant_type": "authorization_code", "code": code,
                                                    "redirect_uri": redirect_uri, "code_verifier": verifier})
        except LoginNeeded as e:
            raise ValueError(Phrase(f"HSE did not take the code ({e}): log in again",
                                    f"ВШЭ не приняла код ({e}): войдите ещё раз")) from None
        except ValueError:
            raise ValueError(Phrase("the HSE login server gave an unclear answer: log in again",
                                    "сервер входа ВШЭ ответил непонятно: войдите ещё раз")) from None


def _first(query: dict[str, list[str]], name: str) -> str | None:
    return (query.get(name) or [None])[0]


def _json_field(body: str, name: str) -> str | None:
    try:
        value = json.loads(body).get(name)
    except (ValueError, AttributeError):
        return None
    return str(value)[:100] if value else None


# --- the deadlines -----------------------------------------------------------------


@dataclass(frozen=True)
class Raw:
    id: str
    title: str                  # Russian, whitespace collapsed
    discipline: str | None      # cut at the first " (": the course as HSE App X names it
    due: dt.date                # Moscow


def trim_discipline(name) -> str | None:
    """'Дискретная математика (2026/2027 модули: 1,2,3,4) (Сидоров С.С.)' -> 'Дискретная математика'."""
    if not name:
        return None
    return str(name).split(" (")[0].strip() or None


def due_date(expires_at) -> dt.date:
    """ISO UTC -> the Moscow date: 20:59Z is 23:59 of the same day in Moscow."""
    moment = dt.datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=dt.UTC)
    return moment.astimezone(clock.TZ).date()


def parse(items: list, completed: bool = False) -> list[Raw]:
    """completed: keep COMPLETED and COMPLETED_LATE instead of ACTIVE."""
    result = []
    for item in items:
        if not isinstance(item, dict):
            continue
        status = str(item.get("completion_status", "ACTIVE")).upper()
        if status.startswith("COMPLETED") != completed or (not completed and status != "ACTIVE"):
            continue
        try:
            due = due_date(item["expires_at"])
        except (KeyError, ValueError, AttributeError, TypeError):
            log.warning("HSE App X deadline without a valid expires_at: %.80s", item.get("title"))
            continue
        title = " ".join(str(item.get("title") or "").split())
        if title:
            result.append(Raw(str(item.get("id") or title), title, trim_discipline(item.get("discipline")), due))
    return result


def describe(error: Exception) -> str:
    """An error for the status and the System topic. HTTP errors by their status: aiohttp's text has the URL."""
    if isinstance(error, aiohttp.ClientResponseError):
        return f"HTTP {error.status} {error.message}".strip()
    return f"{type(error).__name__}: {error}" if str(error) else type(error).__name__


# --- the source ---------------------------------------------------------------------


class HseApp:
    def __init__(self, store: Store, settings: Settings, callback_url: str,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.settings, self.callback_url, self.now = store, settings, callback_url, now
        self.auth = Auth(store, now)
        self.on_poll: Callable[[list[Raw], list[Raw]], Awaitable[None]] | None = None  # active, done
        # A line for the System topic; True asks for the button that opens the Mini App's HSE App X screen.
        self.notify: Callable[[Phrase, bool], None] | None = None
        self.ok_at: str | None = None
        self.error: str | None = None
        self._failing_since: dt.datetime | None = None
        self._told_down = False
        self._wake = asyncio.Event()

    def enabled(self) -> bool:
        return self.settings.get("hseapp_enabled")

    def status(self) -> dict:
        """Never the tokens. The lists and their labels add items, done and labels_error (processing/hseapp.py)."""
        enabled, logged_in = self.enabled(), self.auth.logged_in
        return {"enabled": enabled, "logged_in": logged_in, "ok_at": self.ok_at, "error": self.error,
                "login_needed": enabled and not logged_in}

    # --- the owner's side (the Mini App) ---------------------------------------------------

    def login_link(self, app: bool = False) -> str:
        """The HSE login page; the browser comes back to our callback, or with `app` to the app's own address,
        whose return address is pasted by hand."""
        return self.auth.login_url(APP_REDIRECT_URI if app else self.callback_url)

    async def login(self, returned: str) -> None:
        """Finish a login with the return address; ValueError with the reason (a Phrase)."""
        try:
            async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=LOGIN_TIMEOUT)) as session:
                await self.auth.finish_login(session, returned)
        except (aiohttp.ClientError, TimeoutError) as e:
            what = type(e).__name__
            raise ValueError(Phrase(f"the HSE login server is unavailable ({what}): try again",
                                    f"сервер входа ВШЭ недоступен ({what}): попробуйте ещё раз")) from None
        self.error = None
        log.info("HSE App X: logged in")
        self._tell_login()
        self._wake.set()  # the deadlines now, not in 15 minutes

    def set_enabled(self, enabled: bool) -> None:
        self.settings.set("hseapp_enabled", enabled)
        self._tell_login()
        self._wake.set()

    # --- polling --------------------------------------------------------------------------

    async def run(self) -> None:
        while True:
            try:
                if self.enabled() and self.auth.logged_in:
                    await self.poll()
                self._tell_login()
            except Exception:
                log.exception("HSE App X poll failed")
            await self._nap(POLL_EVERY)

    async def _nap(self, seconds: float) -> None:
        """Until the next poll, or less after a login. Cleared after waking, not before: a login during a poll
        gives one more poll."""
        try:
            await asyncio.wait_for(self._wake.wait(), seconds)
        except TimeoutError:
            pass
        self._wake.clear()

    async def poll(self) -> bool:
        """One fetch of both lists; True when it went through and the processing got them."""
        try:
            async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=TIMEOUT)) as session:
                active = parse(await self._fetch(session))
                done = parse(await self._fetch(session, completed=True), completed=True)
        except LoginNeeded as e:
            self.error = f"{LOGIN_ERROR}{e}"
            log.warning("HSE App X: login needed (%s); log in from the Mini App", e)
            self._tell_login()
            return False
        except (aiohttp.ClientError, TimeoutError, ValueError, OSError) as e:
            self._failed(describe(e))
            return False
        self._recovered()
        log.info("HSE App X: %d deadlines, %d completed", len(active), len(done))
        if self.on_poll is not None:
            try:
                await self.on_poll(active, done)
            except Exception:
                log.exception("processing the HSE App X poll failed")
        return True

    async def _fetch(self, session: aiohttp.ClientSession, completed: bool = False) -> list:
        token = await self.auth.access_token(session)
        items, cursor = [], None
        for _ in range(MAX_PAGES):
            params = {"cursor": cursor} if cursor else None
            for attempt in (0, 1):
                headers = {"Authorization": f"Bearer {token}", "Accept-Language": "ru"}
                async with session.post(DEADLINES_URL, params=params, json={"is_completed": completed},
                                        headers=headers) as response:
                    if response.status != 401 or attempt:
                        response.raise_for_status()
                        page = await response.json(content_type=None)
                        break
                token = await self.auth.access_token(session, force=True)  # revoked early: once more
            if not isinstance(page, dict) or not isinstance(page.get("items") or [], list):
                raise ValueError("unexpected answer of /v2/deadlines")
            items += page.get("items") or []
            cursor = page.get("next_cursor")
            if not cursor:
                break
        return items

    # --- the System topic ---------------------------------------------------------------------

    def _tell_login(self) -> None:
        """One line per change of state: a login is needed (with the button), the login is back. Kept in kv, so a
        restart does not tell it again; turning the source off forgets it silently."""
        enabled, logged_in = self.enabled(), self.auth.logged_in
        told = self.store.get_json(TOLD_KEY, False)
        if enabled and not logged_in and not told:
            self.store.put_json(TOLD_KEY, True)
            if self.auth.ever:
                reason = self.error.removeprefix(LOGIN_ERROR) if (self.error or "").startswith(LOGIN_ERROR) else ""
                why = f" ({reason})" if reason else ""
                text = Phrase(f"The HSE App X login is no longer valid{why}: its deadlines are not updated.\n"
                              "Log in again with the button below or in the Mini App, Settings → HSE App X.",
                              f"Вход в HSE App X больше не действует{why}: дедлайны оттуда не обновляются.\n"
                              "Войти заново — кнопкой ниже или в Mini App, Настройки → HSE App X.")
            else:
                text = Phrase("An HSE App X login is needed: without it no deadlines come from there.\n"
                              "Log in with the button below or in the Mini App, Settings → HSE App X.",
                              "Нужен вход в HSE App X: без него дедлайны оттуда не приходят.\n"
                              "Войти — кнопкой ниже или в Mini App, Настройки → HSE App X.")
            self._notify(text, login=True)
        elif told and (logged_in or not enabled):
            self.store.put_json(TOLD_KEY, False)
            if logged_in:
                self._notify(Phrase("Logged in to HSE App X: the deadlines are updated.",
                                    "Вход в HSE App X выполнен: дедлайны обновляются."))

    def _failed(self, error: str) -> None:
        self.error = error
        log.warning("HSE App X poll failed: %s", error)
        now = self.now()
        self._failing_since = self._failing_since or now
        if not self._told_down and now - self._failing_since >= STALE_AFTER:
            self._told_down = True
            since = self._failing_since.astimezone(clock.TZ)
            self._notify(Phrase(f"HSE App X is unavailable since {since:%d.%m %H:%M}: {error}.\n"
                                "The deadlines have the last list received.",
                                f"HSE App X недоступен с {since:%d.%m %H:%M}: {error}.\n"
                                "В дедлайнах — последний полученный список."))

    def _recovered(self) -> None:
        if self._told_down:
            self._notify(Phrase("HSE App X is available again.", "HSE App X снова доступен."))
        self.ok_at, self.error = clock.local_iso(self.now()), None
        self._failing_since, self._told_down = None, False

    def _notify(self, text: Phrase, login: bool = False) -> None:
        if self.notify is not None:
            self.notify(text, login)
