"""Web Push of the web app (§8.6, stage 16.4): subscriptions per device and the sending, by RFC 8030, 8291 and 8292.

- A device that turns notifications on gives its browser's subscription (an endpoint of its push service and two
  keys); it is kept per web session (`web_push`), so ending the session elsewhere, or 90 days without it, ends its
  notifications too. The endpoint must be https on a known push service: Apple's, Google's, Mozilla's, Microsoft's.
- The server's VAPID key pair (P-256) is made at the first start and kept in `kv`: the browser binds a subscription to
  its public key, so a new pair would make every device subscribe again.
- A message is JSON {title, body, url, tag}, encrypted for each device (aes128gcm, RFC 8291) and posted with a VAPID
  token (ES256, 12 hours). A subscription the service answers 404 or 410 to is gone and deleted; one that fails
  `MAX_FAILURES` times in a row is deleted too.

No third-party push library: pywebpush would bring the synchronous requests; this is a page of cryptography.
"""

import asyncio
import base64
import json
import logging
import os
import time
from collections.abc import Awaitable, Callable
from urllib.parse import urlsplit

import aiohttp
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

from ..store import Store

log = logging.getLogger(__name__)

SCHEMA = """
create table if not exists web_push (     -- devices that get the web app's notifications (§8.6)
    id integer primary key,
    session_id integer not null,          -- web_sessions.id: the device; its subscription goes with it
    endpoint text not null unique,        -- the push service's URL for this browser
    p256dh text not null,                 -- the browser's public key, base64url
    auth text not null,                   -- its auth secret, base64url
    created_at integer not null,          -- unix seconds
    sent_at integer,                      -- the last message the service took
    failures integer not null default 0   -- failed sends in a row
);
"""
VAPID_KEY = "web.vapid"
PUSH_HOSTS = ("push.apple.com", "fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com")
MAX_FAILURES = 10
TOKEN_LIFE = 12 * 3600
RECORD_SIZE = 4096
TTL = 24 * 3600          # a message waits a day for a device that is off
PAYLOAD_LIMIT = 3000     # bytes of JSON; the services take about 4 KB encrypted


class PushError(Exception):
    """A subscription refused: the text says why."""


def b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def unb64(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


def _hkdf(salt: bytes, info: bytes, length: int, ikm: bytes) -> bytes:
    return HKDF(algorithm=hashes.SHA256(), length=length, salt=salt, info=info).derive(ikm)


def _raw(public: ec.EllipticCurvePublicKey) -> bytes:
    return public.public_bytes(serialization.Encoding.X962, serialization.PublicFormat.UncompressedPoint)


def encrypt(payload: bytes, p256dh: str, auth: str, *, salt: bytes | None = None,
            server: ec.EllipticCurvePrivateKey | None = None) -> bytes:
    """The body of a push message for one browser (RFC 8291, aes128gcm, one record). `salt` and `server` (a new key
    pair each time) are given only by tests."""
    browser_raw = unb64(p256dh)
    browser = ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), browser_raw)
    server = server or ec.generate_private_key(ec.SECP256R1())
    server_raw = _raw(server.public_key())
    salt = salt or os.urandom(16)
    secret = server.exchange(ec.ECDH(), browser)
    ikm = _hkdf(unb64(auth), b"WebPush: info\x00" + browser_raw + server_raw, 32, secret)
    key = _hkdf(salt, b"Content-Encoding: aes128gcm\x00", 16, ikm)
    nonce = _hkdf(salt, b"Content-Encoding: nonce\x00", 12, ikm)
    sealed = AESGCM(key).encrypt(nonce, payload + b"\x02", None)  # \x02: the last record, no padding
    return salt + RECORD_SIZE.to_bytes(4, "big") + bytes([len(server_raw)]) + server_raw + sealed


class Vapid:
    """The server's key pair for VAPID (RFC 8292), made once and kept."""

    def __init__(self, store: Store, subject: str):
        self.subject = subject  # https://… of the service: the push services may write to its owner
        saved = store.get_json(VAPID_KEY)
        if saved is None:
            key = ec.generate_private_key(ec.SECP256R1())
            pem = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                                    serialization.NoEncryption()).decode()
            store.put_json(VAPID_KEY, {"private": pem})
            saved = {"private": pem}
        self.key = serialization.load_pem_private_key(saved["private"].encode(), None)
        self.public = b64(_raw(self.key.public_key()))  # the applicationServerKey of the browsers

    def token(self, endpoint: str, now: float) -> str:
        parts = urlsplit(endpoint)
        header = b64(json.dumps({"typ": "JWT", "alg": "ES256"}, separators=(",", ":")).encode())
        claims = b64(json.dumps({"aud": f"{parts.scheme}://{parts.netloc}", "exp": int(now) + TOKEN_LIFE,
                                 "sub": self.subject}, separators=(",", ":")).encode())
        r, s = decode_dss_signature(self.key.sign(f"{header}.{claims}".encode(), ec.ECDSA(hashes.SHA256())))
        return f"{header}.{claims}.{b64(r.to_bytes(32, 'big') + s.to_bytes(32, 'big'))}"

    def authorization(self, endpoint: str, now: float) -> str:
        return f"vapid t={self.token(endpoint, now)}, k={self.public}"


Post = Callable[[str, bytes, dict], Awaitable[int]]  # url, body, headers -> HTTP status


async def http_post(url: str, body: bytes, headers: dict) -> int:
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=20)) as http, \
            http.post(url, data=body, headers=headers) as response:
        if response.status >= 400:
            log.warning("push service %s answered %d: %s", urlsplit(url).netloc, response.status,
                        (await response.text())[:200])
        return response.status


def check_endpoint(endpoint) -> str:
    parts = urlsplit(endpoint) if isinstance(endpoint, str) else None
    host = (parts.hostname or "") if parts else ""
    if parts is None or parts.scheme != "https" or not any(host == h or host.endswith("." + h) for h in PUSH_HOSTS):
        raise PushError("the endpoint is not a known push service")
    return endpoint


class Push:
    def __init__(self, store: Store, subject: str, now: Callable[[], float] = time.time, post: Post = http_post):
        self.db, self.now, self.post = store.db, now, post
        self.db.executescript(SCHEMA)
        self.vapid = Vapid(store, subject)

    # --- the devices ---------------------------------------------------------------------------------------

    def subscribe(self, session_id: int, subscription) -> None:
        """A browser's PushSubscription as JSON: {endpoint, keys: {p256dh, auth}}. A device has one; a new one
        replaces it."""
        if not isinstance(subscription, dict) or not isinstance(subscription.get("keys"), dict):
            raise PushError("a subscription {endpoint, keys} expected")
        endpoint = check_endpoint(subscription.get("endpoint"))
        p256dh, auth = subscription["keys"].get("p256dh"), subscription["keys"].get("auth")
        try:
            ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), unb64(p256dh))
            if len(unb64(auth)) != 16:
                raise ValueError
        except (ValueError, TypeError, AttributeError):
            raise PushError("the subscription's keys are not P-256 and a 16-byte secret") from None
        with self.db:
            self.db.execute("delete from web_push where session_id = ? or endpoint = ?", (session_id, endpoint))
            self.db.execute("insert into web_push(session_id, endpoint, p256dh, auth, created_at) "
                            "values(?, ?, ?, ?, ?)", (session_id, endpoint, p256dh, auth, int(self.now())))

    def unsubscribe(self, session_id: int) -> bool:
        with self.db:
            return self.db.execute("delete from web_push where session_id = ?", (session_id,)).rowcount > 0

    def devices(self) -> list[dict]:
        """The subscriptions of sessions still in force; the others go (a session ended or idle)."""
        with self.db:
            self.db.execute("delete from web_push where session_id not in (select id from web_sessions)")
        return [dict(row) for row in self.db.execute("select * from web_push order by id")]

    def subscribed(self, session_id: int) -> bool:
        return any(device["session_id"] == session_id for device in self.devices())

    # --- sending ---------------------------------------------------------------------------------------------

    async def send(self, message: dict, *, session_id: int | None = None, urgency: str = "normal",
                   ttl: int = TTL) -> int:
        """`message` ({title, body, url, tag}) to every device, or to one session's; how many services took it."""
        payload = json.dumps(message, ensure_ascii=False).encode()
        if len(payload) > PAYLOAD_LIMIT:
            message = {**message, "body": message.get("body", "")[:PAYLOAD_LIMIT // 4] + "…"}
            payload = json.dumps(message, ensure_ascii=False).encode()
        devices = [d for d in self.devices() if session_id is None or d["session_id"] == session_id]
        results = await asyncio.gather(*(self._one(d, payload, urgency, ttl) for d in devices))
        return sum(results)

    async def _one(self, device: dict, payload: bytes, urgency: str, ttl: int) -> bool:
        headers = {"Authorization": self.vapid.authorization(device["endpoint"], self.now()),
                   "Content-Encoding": "aes128gcm", "Content-Type": "application/octet-stream",
                   "TTL": str(ttl), "Urgency": urgency}
        try:
            status = await self.post(device["endpoint"], encrypt(payload, device["p256dh"], device["auth"]), headers)
        except (aiohttp.ClientError, TimeoutError) as e:
            log.warning("push to %s failed: %s", urlsplit(device["endpoint"]).netloc, e)
            status = 0
        with self.db:
            if 200 <= status < 300:
                self.db.execute("update web_push set sent_at = ?, failures = 0 where id = ?",
                                (int(self.now()), device["id"]))
                return True
            if status in (404, 410):  # the browser dropped the subscription
                self.db.execute("delete from web_push where id = ?", (device["id"],))
            else:
                self.db.execute("update web_push set failures = failures + 1 where id = ?", (device["id"],))
                self.db.execute("delete from web_push where id = ? and failures >= ?", (device["id"], MAX_FAILURES))
        return False
