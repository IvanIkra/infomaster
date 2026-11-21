"""Who may open the web app (§8.6, D61): the one owner, with a session per device.

- The first login on a device is a one-time code the bot sends to the owner's private chat, as the connector's
  sign-in does (§8.5). A logged-in device may then add a passkey (WebAuthn), and its later logins use that.
- A session is a random token in an HttpOnly, Secure, SameSite=Strict cookie on /web; the database keeps its SHA-256
  only. A session unused for SESSION_IDLE is gone, and any one can be ended from another device.
- The code, its attempts and the WebAuthn challenges live in memory: a restart only asks for a new code.

Passkeys are discoverable (the login asks for no name) and need user verification (Face ID, a fingerprint, a PIN).
They are bound to the host of PUBLIC_URL: if the Funnel name changes, the code from the bot still works.
"""

import base64
import hashlib
import hmac
import json
import logging
import secrets
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

import webauthn
from webauthn.helpers import structs

from ..i18n import tr
from ..store import Store

log = logging.getLogger(__name__)

SCHEMA = """
create table if not exists web_sessions (   -- devices logged in to the web app (D61)
    id integer primary key,
    token_hash text not null unique,        -- SHA-256 of the cookie's token
    device text not null,                   -- from the User-Agent, for the list of devices
    created_at integer not null,            -- unix seconds
    used_at integer not null
);
create table if not exists web_passkeys (   -- the owner's WebAuthn credentials (D61)
    id integer primary key,
    credential_id text not null unique,     -- base64url
    public_key blob not null,               -- COSE
    sign_count integer not null,
    transports text,                        -- JSON list, a hint for the browser
    device text not null,                   -- where it was added
    created_at integer not null,
    used_at integer
);
"""

COOKIE = "im_session"
COOKIE_PATH = "/web"
SESSION_IDLE = 90 * 86400   # a session unused this long is gone
TOUCH_EVERY = 3600          # used_at is written at most this often
CODE_TTL = 600              # the code from the bot
ATTEMPTS = 3                # wrong codes before a new one is needed
RESEND_AFTER = 30           # seconds between two codes
CODES_PER_HOUR = 6
CODES_PER_DAY = 12          # with ATTEMPTS: 36 guesses a day of a million, and each code is a message to the owner
CHALLENGE_TTL = 300         # a WebAuthn ceremony
CHALLENGES_MAX = 20         # open ceremonies kept: the login's options are public, memory is not theirs
USER_ID = b"infomaster-owner"  # the one user, the same on every device: an authenticator keeps one passkey for it


class AuthError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status, self.message = status, message


def digest(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def device_name(user_agent: str) -> str:
    """'iPhone · Safari' from a User-Agent: enough to tell one's own devices apart in the list."""
    ua = user_agent or ""
    system = next((name for key, name in (("iPhone", "iPhone"), ("iPad", "iPad"), ("Android", "Android"),
                                          ("Macintosh", "Mac"), ("Windows", "Windows"), ("CrOS", "ChromeOS"),
                                          ("Linux", "Linux")) if key in ua), None)
    browser = next((name for key, name in (("YaBrowser", "Yandex"), ("Edg/", "Edge"), ("Firefox", "Firefox"),
                                           ("FxiOS", "Firefox"), ("CriOS", "Chrome"), ("Chrome", "Chrome"),
                                           ("Safari", "Safari")) if key in ua), None)
    return " · ".join(part for part in (system, browser) if part) or "?"


def _challenge_of(credential) -> str | None:
    """The challenge a credential answers, from its clientDataJSON; None when it has none."""
    try:
        data = credential["response"]["clientDataJSON"]
        client_data = json.loads(base64.urlsafe_b64decode(data + "=" * (-len(data) % 4)))
        challenge = client_data["challenge"]
    except (KeyError, TypeError, ValueError):
        return None
    return challenge if isinstance(challenge, str) else None


@dataclass
class Auth:
    store: Store
    rp_id: str                  # the host of PUBLIC_URL
    origin: str                 # scheme://host[:port] of PUBLIC_URL
    send_code: Callable[[str], Awaitable[None]] | None = None  # a text to the owner's private chat with the bot
    lang: Callable[[], str] = lambda: "en"
    now: Callable[[], float] = time.time
    code_hash: str | None = None
    code_sent: float = 0
    attempts: int = 0
    sent: list[float] = field(default_factory=list)        # when codes went out, the last hour
    # challenge (base64url) -> (kind, the session that asked to register or None, expires)
    challenges: dict[str, tuple[str, int | None, float]] = field(default_factory=dict)

    def __post_init__(self):
        self.db = self.store.db
        self.db.executescript(SCHEMA)

    def _error(self, status: int, en: str, ru: str) -> AuthError:
        return AuthError(status, tr(self.lang(), en, ru))

    # --- the code from the bot ---------------------------------------------------------------------------

    async def send(self) -> None:
        """The bot sends a new code; AuthError with the reason it did not."""
        now = self.now()
        if now - self.code_sent < RESEND_AFTER:
            raise self._error(429, "A code was sent a moment ago: check the chat with the bot.",
                              "Код только что отправлен: посмотрите чат с ботом.")
        self.sent = [at for at in self.sent if now - at < 86400]
        if len([at for at in self.sent if now - at < 3600]) >= CODES_PER_HOUR or len(self.sent) >= CODES_PER_DAY:
            raise self._error(429, "Too many codes for now: try again later, or log in with a passkey.",
                              "Слишком много кодов: попробуйте позже или войдите по ключу доступа.")
        if self.send_code is None:
            raise self._error(503, "The bot is not running.", "Бот не запущен.")
        # Counted before the bot is awaited: requests at the same moment cannot all pass the limits.
        self.code_hash, self.code_sent = None, now
        self.sent.append(now)
        code = f"{secrets.randbelow(10**6):06d}"
        text = tr(self.lang(), f"{code} is the code to log in to the infomaster app. If you did not ask for it, "
                               "ignore this message.",
                  f"{code} — код для входа в приложение infomaster. Если вы его не запрашивали, не обращайте "
                  "внимания.")
        try:
            await self.send_code(text)
        except Exception as e:
            log.warning("the bot could not send the web app's code: %s", e)
            raise self._error(502, "The bot could not write to you: open the chat with the bot, press Start and "
                                   "try again.",
                              "Бот не смог вам написать: откройте чат с ботом, нажмите «Старт» и попробуйте "
                              "снова.") from None
        self.code_hash, self.attempts = digest(code), 0

    def check_code(self, code, device: str) -> str:
        """A new session's token for the right code; AuthError else. A code works once."""
        if self.code_hash is None or self.now() - self.code_sent > CODE_TTL:
            self.code_hash = None
            raise self._error(400, "Ask for a code first.", "Сначала запросите код.")
        self.attempts += 1
        typed = "".join(code.split()) if isinstance(code, str) else ""
        if not hmac.compare_digest(digest(typed), self.code_hash):
            if self.attempts >= ATTEMPTS:
                self.code_hash = None
                raise self._error(400, "Wrong code too many times: ask for a new one.",
                                  "Слишком много неверных кодов: запросите новый.")
            raise self._error(400, "Wrong code.", "Неверный код.")
        self.code_hash = None
        log.info("the web app: a login by the bot's code on %s", device)
        return self.start(device)

    # --- sessions ----------------------------------------------------------------------------------------

    def start(self, device: str) -> str:
        token, now = secrets.token_urlsafe(32), int(self.now())
        with self.db:
            self.db.execute("insert into web_sessions(token_hash, device, created_at, used_at) values(?, ?, ?, ?)",
                            (digest(token), device, now, now))
        return token

    def session(self, token: str | None) -> dict | None:
        """The session of a cookie's token, its use noted; None for an unknown or idle one."""
        if not token:
            return None
        now = int(self.now())
        with self.db:
            self.db.execute("delete from web_sessions where used_at < ?", (now - SESSION_IDLE,))
        row = self.db.execute("select * from web_sessions where token_hash = ?", (digest(token),)).fetchone()
        if row is None:
            return None
        if now - row["used_at"] >= TOUCH_EVERY:
            with self.db:
                self.db.execute("update web_sessions set used_at = ? where id = ?", (now, row["id"]))
        return dict(row)

    def end(self, session_id: int) -> bool:
        with self.db:
            return self.db.execute("delete from web_sessions where id = ?", (session_id,)).rowcount > 0

    def sessions(self) -> list[dict]:
        return [{"id": r["id"], "device": r["device"], "created_at": r["created_at"], "used_at": r["used_at"]}
                for r in self.db.execute("select * from web_sessions order by used_at desc")]

    # --- passkeys ----------------------------------------------------------------------------------------

    def passkeys(self) -> list[dict]:
        return [{"id": r["id"], "device": r["device"], "created_at": r["created_at"], "used_at": r["used_at"]}
                for r in self.db.execute("select * from web_passkeys order by created_at")]

    def remove_passkey(self, passkey_id: int) -> bool:
        with self.db:
            return self.db.execute("delete from web_passkeys where id = ?", (passkey_id,)).rowcount > 0

    def _remember(self, challenge: bytes, kind: str, session_id: int | None) -> None:
        now = self.now()
        self.challenges = {c: v for c, v in self.challenges.items() if v[2] > now}
        self.challenges[b64url(challenge)] = (kind, session_id, now + CHALLENGE_TTL)
        while len(self.challenges) > CHALLENGES_MAX:  # the oldest go first
            del self.challenges[next(iter(self.challenges))]

    def _take(self, credential, kind: str, session_id: int | None) -> bytes:
        """The challenge a credential answers, once, if this service gave it for this ceremony."""
        challenge = _challenge_of(credential) if isinstance(credential, dict) else None
        entry = self.challenges.pop(challenge, None) if challenge else None
        if entry is None or entry[0] != kind or entry[1] != session_id or entry[2] < self.now():
            raise self._error(400, "The passkey's request has expired: try again.",
                              "Запрос ключа устарел: попробуйте снова.")
        return base64.urlsafe_b64decode(challenge + "=" * (-len(challenge) % 4))

    def registration_options(self, session_id: int) -> str:
        """JSON options for navigator.credentials.create() on a logged-in device."""
        options = webauthn.generate_registration_options(
            rp_id=self.rp_id, rp_name="infomaster", user_id=USER_ID, user_name="infomaster",
            user_display_name="infomaster",
            authenticator_selection=structs.AuthenticatorSelectionCriteria(
                resident_key=structs.ResidentKeyRequirement.REQUIRED,
                user_verification=structs.UserVerificationRequirement.REQUIRED),
            exclude_credentials=[structs.PublicKeyCredentialDescriptor(id=webauthn.base64url_to_bytes(r[0]))
                                 for r in self.db.execute("select credential_id from web_passkeys")])
        self._remember(options.challenge, "register", session_id)
        return webauthn.options_to_json(options)

    def add_passkey(self, session: dict, credential) -> dict:
        challenge = self._take(credential, "register", session["id"])
        try:
            verified = webauthn.verify_registration_response(
                credential=credential, expected_challenge=challenge, expected_rp_id=self.rp_id,
                expected_origin=self.origin, require_user_verification=True)
        except Exception as e:
            log.info("a passkey was not added: %s", e)
            raise self._error(400, "The passkey was not accepted.", "Ключ не принят.") from None
        transports = credential.get("response", {}).get("transports")
        with self.db:
            cursor = self.db.execute(
                "insert into web_passkeys(credential_id, public_key, sign_count, transports, device, created_at) "
                "values(?, ?, ?, ?, ?, ?) on conflict(credential_id) do nothing",
                (b64url(verified.credential_id), verified.credential_public_key, verified.sign_count,
                 json.dumps(transports) if isinstance(transports, list) else None, session["device"],
                 int(self.now())))
        log.info("the web app: a passkey added on %s", session["device"])
        return {"id": cursor.lastrowid}

    def login_options(self) -> str:
        """JSON options for navigator.credentials.get(): any of the owner's passkeys (they are discoverable)."""
        options = webauthn.generate_authentication_options(
            rp_id=self.rp_id, user_verification=structs.UserVerificationRequirement.REQUIRED)
        self._remember(options.challenge, "login", None)
        return webauthn.options_to_json(options)

    def passkey_login(self, credential, device: str) -> str:
        """A new session's token for a valid assertion of a known passkey; AuthError else."""
        challenge = self._take(credential, "login", None)
        row = self.db.execute("select * from web_passkeys where credential_id = ?",
                              (credential.get("id") if isinstance(credential.get("id"), str) else "",)).fetchone()
        if row is None:
            raise self._error(400, "This passkey is unknown here: log in with a code from the bot.",
                              "Этот ключ здесь неизвестен: войдите по коду от бота.")
        try:
            verified = webauthn.verify_authentication_response(
                credential=credential, expected_challenge=challenge, expected_rp_id=self.rp_id,
                expected_origin=self.origin, credential_public_key=row["public_key"],
                credential_current_sign_count=row["sign_count"], require_user_verification=True)
        except Exception as e:
            log.info("a passkey login failed: %s", e)
            raise self._error(400, "The passkey was not accepted.", "Ключ не принят.") from None
        with self.db:
            self.db.execute("update web_passkeys set sign_count = ?, used_at = ? where id = ?",
                            (verified.new_sign_count, int(self.now()), row["id"]))
        log.info("the web app: a login by passkey on %s", device)
        return self.start(device)
