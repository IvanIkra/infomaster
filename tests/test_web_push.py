"""Web Push of the web app (§8.6, stage 16.4): the encryption and VAPID, subscriptions per device, the notifier."""

import asyncio
import datetime as dt
import json

import pytest
from aiohttp import web
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import encode_dss_signature
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from test_http import env
from test_web import ORIGIN, call, log_in

from infomaster.i18n import Alert, Phrase
from infomaster.llm.budget import Level, Notice
from infomaster.processing.todo import Deadline
from infomaster.web import WebApp
from infomaster.web.notify import Notifier
from infomaster.web.push import MAX_FAILURES, Push, PushError, b64, encrypt, unb64

ENDPOINT = "https://fcm.googleapis.com/fcm/send/device-1"


class Seconds:
    def __init__(self):
        self.value = 1_800_000_000.0

    def __call__(self):
        return self.value


class Browser:
    """A browser's side of a subscription: its key pair and auth secret; it reads what the server sent."""

    def __init__(self, endpoint=ENDPOINT):
        self.key = ec.generate_private_key(ec.SECP256R1())
        self.auth = b"0123456789abcdef"
        raw = self.key.public_key().public_bytes(serialization.Encoding.X962,
                                                 serialization.PublicFormat.UncompressedPoint)
        self.subscription = {"endpoint": endpoint, "keys": {"p256dh": b64(raw), "auth": b64(self.auth)}}

    def read(self, body: bytes) -> dict:
        """RFC 8291 from the receiving end."""
        salt, size, length = body[:16], int.from_bytes(body[16:20], "big"), body[20]
        server_raw, sealed = body[21:21 + length], body[21 + length:]
        assert size == 4096 and length == 65
        server = ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), server_raw)
        own = unb64(self.subscription["keys"]["p256dh"])

        def hkdf(salt, info, n, ikm):
            return HKDF(algorithm=hashes.SHA256(), length=n, salt=salt, info=info).derive(ikm)

        ikm = hkdf(self.auth, b"WebPush: info\x00" + own + server_raw, 32, self.key.exchange(ec.ECDH(), server))
        plain = AESGCM(hkdf(salt, b"Content-Encoding: aes128gcm\x00", 16, ikm)).decrypt(
            hkdf(salt, b"Content-Encoding: nonce\x00", 12, ikm), sealed, None)
        assert plain.endswith(b"\x02")
        return json.loads(plain[:-1])


class Service:
    """A push service: what it was sent, and the status it answers."""

    def __init__(self):
        self.sent, self.status = [], 201

    async def __call__(self, url, body, headers):
        self.sent.append((url, body, headers))
        return self.status


def test_rfc8291_example():
    """The example of RFC 8291, Appendix A."""
    server = ec.derive_private_key(int.from_bytes(unb64("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"), "big"),
                                   ec.SECP256R1())
    body = encrypt(b"When I grow up, I want to be a watermelon",
                   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
                   "BTBZMqHH6r4Tts7J_aSIgg", salt=unb64("DGv6ra1nlYgDCS1FRnbzlw"), server=server)
    assert b64(body) == ("DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWA"
                         "mS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Q"
                         "ulcy4a-fN")


def test_vapid(store):
    push = Push(store, "https://infomaster.example.ts.net", Seconds())
    assert Push(store, "https://infomaster.example.ts.net").vapid.public == push.vapid.public  # kept in kv
    scheme, _, rest = push.vapid.authorization(ENDPOINT, 1_800_000_000).partition(" ")
    token, key = (part.split("=", 1)[1] for part in rest.split(", "))
    assert scheme == "vapid" and key == push.vapid.public
    header, claims, signature = token.split(".")
    assert json.loads(unb64(header)) == {"typ": "JWT", "alg": "ES256"}
    assert json.loads(unb64(claims)) == {"aud": "https://fcm.googleapis.com", "exp": 1_800_000_000 + 12 * 3600,
                                         "sub": "https://infomaster.example.ts.net"}
    raw = unb64(signature)
    public = ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), unb64(key))
    public.verify(encode_dss_signature(int.from_bytes(raw[:32], "big"), int.from_bytes(raw[32:], "big")),
                  f"{header}.{claims}".encode(), ec.ECDSA(hashes.SHA256()))


@pytest.fixture
async def device(aiohttp_client, store):
    """A logged-in device of the web app; its push service answers as `client.service` says."""
    sent, seconds = [], Seconds()

    async def send_code(text):
        sent.append(text)

    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "ru", now=seconds)
    client = await aiohttp_client(webapp.attach(web.Application()))
    client.token = await log_in(client, sent, seconds)
    client.service = webapp.push.post = Service()
    client.webapp = webapp
    return client


async def answer(client, method, path, data=None, status=200):
    response = await call(client, method, path, data, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def test_subscribe_and_send(device):
    state = await answer(device, "GET", "/push")
    assert state["on"] is False and state["devices"] == 0 and len(unb64(state["key"])) == 65
    assert (await answer(device, "GET", "/me"))["push"] is False

    browser = Browser()
    state = await answer(device, "POST", "/push", {"subscription": browser.subscription})
    assert state["on"] is True and state["devices"] == 1 and (await answer(device, "GET", "/me"))["push"] is True

    assert (await answer(device, "POST", "/push/test", {})) == {"sent": 1}
    [(url, body, headers)] = device.service.sent
    assert url == ENDPOINT and headers["Content-Encoding"] == "aes128gcm" and headers["TTL"] == "86400"
    assert headers["Authorization"].startswith("vapid t=")
    assert browser.read(body) == {"title": "infomaster", "body": "Уведомления на этом устройстве работают.",
                                  "url": "/web/#/", "tag": "test"}

    # A new subscription of the same device replaces the old one.
    other = Browser("https://web.push.apple.com/QGh-device")
    await answer(device, "POST", "/push", {"subscription": other.subscription})
    assert [d["endpoint"] for d in device.webapp.push.devices()] == [other.subscription["endpoint"]]

    state = await answer(device, "DELETE", "/push")
    assert state == {**state, "on": False, "devices": 0}


async def test_refused_subscriptions(device):
    for endpoint in ("http://fcm.googleapis.com/x", "https://example.com/push", "https://fcm.googleapis.com.evil/x",
                     None):
        bad = Browser(endpoint).subscription
        assert "push service" in (await answer(device, "POST", "/push", {"subscription": bad}, 400))["error"]
    bad = Browser().subscription
    bad["keys"]["auth"] = b64(b"short")
    assert "16-byte" in (await answer(device, "POST", "/push", {"subscription": bad}, 400))["error"]
    await answer(device, "POST", "/push", {"subscription": "nothing"}, 400)
    with pytest.raises(PushError):
        device.webapp.push.subscribe(1, {"endpoint": ENDPOINT, "keys": {"p256dh": "AAAA", "auth": b64(b"x" * 16)}})
    response = await call(device, "POST", "/push", {"subscription": Browser().subscription}, token=device.token,
                          origin="https://elsewhere.example")
    assert response.status == 403


async def test_gone_failing_and_ended(device):
    push = device.webapp.push
    await answer(device, "POST", "/push", {"subscription": Browser().subscription})
    device.service.status = 410  # the browser dropped it
    assert await push.send({"title": "x", "body": "y"}) == 0 and push.devices() == []

    await answer(device, "POST", "/push", {"subscription": Browser().subscription})
    device.service.status = 503
    for _ in range(MAX_FAILURES - 1):
        await push.send({"title": "x", "body": "y"})
    assert push.devices()[0]["failures"] == MAX_FAILURES - 1
    device.service.status = 201
    await push.send({"title": "x", "body": "y"})
    assert push.devices()[0]["failures"] == 0  # a success starts the count again
    device.service.status = 500
    for _ in range(MAX_FAILURES):
        await push.send({"title": "x", "body": "y"})
    assert push.devices() == []

    await answer(device, "POST", "/push", {"subscription": Browser().subscription})
    await answer(device, "POST", "/logout", {})
    assert push.devices() == []  # the session's notifications end with it
    assert (await call(device, "GET", "/push")).status == 401
    assert (await device.get("/web/api/push", headers={"Origin": ORIGIN})).status == 401


async def test_notifier(store):
    service, seconds = Service(), Seconds()
    push = Push(store, "https://infomaster.example.ts.net", seconds, post=service)
    store.db.execute("create table if not exists web_sessions (id integer primary key)")
    store.db.execute("insert into web_sessions(id) values(1)")
    browser = Browser()
    push.subscribe(1, browser.subscription)
    lang = "ru"
    notifier = Notifier(push, lambda: lang)

    async def told():
        await asyncio.gather(*notifier._tasks)
        messages = [browser.read(body) for _, body, _ in service.sent]
        service.sent.clear()
        return messages

    d = Deadline("Calc HW 5", dt.date(2026, 10, 22), "study", "tg", "Calc", name="Матанализ")
    notifier.deadline(d)
    notifier.deadline(d, was="2026-10-24")
    new, moved = await told()
    assert new["title"] == "Новый срок: Calc HW 5" and new["body"] == "чт 22.10 · Матанализ"
    assert new["url"].startswith("/web/#/d?uid=tg-") and new["tag"] == "deadline-" + new["url"].split("=")[1]
    assert moved["title"] == "Срок перенесён: Calc HW 5" and moved["body"] == "сб 24.10 → чт 22.10 · Матанализ"
    assert moved["tag"] == new["tag"]  # a move replaces the news on the device

    guessed = Deadline("Calc sheet 3", dt.date(2026, 10, 22), "study", "tg", "Calc", assumed=True)
    notifier.deadline(guessed)
    notifier.reminder("eve", [d, guessed])
    notifier.reminder("day", [])
    first, reminder = await told()
    assert first["body"] == "чт 22.10 ≈ предположительно"
    assert reminder == {"title": "Завтра срок", "body": "Calc HW 5 · Матанализ\nCalc sheet 3", "url": "/web/#/",
                        "tag": "reminder-eve-2026-10-22"}
    assert notifier.reminder("day", []) is None and service.sent == []  # nothing due: nothing sent

    notifier.system(Phrase("infomaster is up.", "infomaster запущен."))  # the forum posts it silently: no push
    notifier.system(Alert("RUZ is unavailable.", "РУЗ недоступен."))
    notifier.system(Alert("A login is needed.", "Нужен вход."), login="hseapp")
    notifier.budget(Notice(Level.SOFT, 1.6, 2.0))
    notifier.budget(Notice(Level.HARD, 2.01, 2.0))
    lang = "en"
    notifier.reminder("day", [d])
    await asyncio.gather(*notifier._tasks)
    urgencies = [headers["Urgency"] for _, _, headers in service.sent]
    ruz, login, budget, today = await told()
    assert ruz == {"title": "infomaster", "body": "РУЗ недоступен.", "url": "/web/#/machine"}
    assert login["tag"] == "login-hseapp"
    assert budget["title"] == "Лимит LLM исчерпан" and budget["body"].startswith("$2.01 из $2.00")
    assert today["title"] == "Due today"
    assert urgencies == ["high", "high", "normal", "high"]


def test_no_loop_no_push(store):
    """Outside the service's loop (a script, an import of the Tablo data) nothing is sent and nothing fails."""
    push = Push(store, "https://infomaster.example.ts.net", post=Service())
    Notifier(push, lambda: "en").system(Alert("x", "x"))
