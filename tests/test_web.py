"""The web app (§8.6, D61): its files, the login by a code from the bot, sessions per device, passkeys."""

import base64
import hashlib
import json
import os
import re
from pathlib import Path

import cbor2
import pytest
from aiohttp import web
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import ec
from test_http import add_row, env

from infomaster.collectors.ruz import parse
from infomaster.web import WebApp
from infomaster.web.auth import (
    ATTEMPTS,
    CODE_TTL,
    CODES_PER_DAY,
    CODES_PER_HOUR,
    RESEND_AFTER,
    SESSION_IDLE,
    USER_ID,
    device_name,
)
from infomaster.web.views import Data

ORIGIN = "https://infomaster.example.ts.net"
RP_ID = "infomaster.example.ts.net"
IPHONE = ("Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) "
          "Version/18.1 Mobile/15E148 Safari/604.1")
MAC = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 "
       "Safari/537.36")


class Seconds:
    def __init__(self):
        self.value = 1_800_000_000.0

    def __call__(self):
        return self.value


@pytest.fixture
def seconds():
    return Seconds()


@pytest.fixture
def sent():
    return []


@pytest.fixture
def webapp(store, seconds, sent):
    async def send_code(text):
        sent.append(text)

    return WebApp(env(), store, send_code=send_code, lang=lambda: "ru", now=seconds)


@pytest.fixture
async def client(aiohttp_client, webapp):
    return await aiohttp_client(webapp.attach(web.Application()))


def b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def unb64(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


async def call(client, method, path, data=None, token=None, origin=ORIGIN, agent=IPHONE):
    """A request of the app: its origin, the session's cookie by hand (the test client keeps no Secure cookie over
    plain http)."""
    headers = {"User-Agent": agent}
    if origin:
        headers["Origin"] = origin
    if token:
        headers["Cookie"] = f"im_session={token}"
    return await client.request(method, "/web/api" + path, json=data, headers=headers)


def code_of(sent) -> str:
    return re.match(r"(\d{6}) ", sent[-1]).group(1)


def token_of(response) -> str:
    return response.cookies["im_session"].value


async def log_in(client, sent, seconds, agent=IPHONE) -> str:
    seconds.value += RESEND_AFTER
    assert (await call(client, "POST", "/login/code", {})).status == 200
    response = await call(client, "POST", "/login/code/check", {"code": code_of(sent)}, agent=agent)
    assert response.status == 200
    return token_of(response)


class Authenticator:
    """A platform authenticator in software: an EC P-256 key, «none» attestation, user presence and verification."""

    def __init__(self, rp_id=RP_ID, origin=ORIGIN):
        self.key = ec.generate_private_key(ec.SECP256R1())
        self.credential_id = os.urandom(16)
        self.count = 0
        self.rp_id, self.origin = rp_id, origin

    def _cose(self) -> bytes:
        numbers = self.key.public_key().public_numbers()
        return cbor2.dumps({1: 2, 3: -7, -1: 1, -2: numbers.x.to_bytes(32, "big"), -3: numbers.y.to_bytes(32, "big")})

    def _client_data(self, kind, challenge) -> bytes:
        return json.dumps({"type": kind, "challenge": challenge, "origin": self.origin, "crossOrigin": False}).encode()

    def _auth_data(self, flags, attested=False) -> bytes:
        data = hashlib.sha256(self.rp_id.encode()).digest() + bytes([flags]) + self.count.to_bytes(4, "big")
        if attested:
            data += bytes(16) + len(self.credential_id).to_bytes(2, "big") + self.credential_id + self._cose()
        return data

    def create(self, options) -> dict:
        assert options["authenticatorSelection"]["residentKey"] == "required"
        client_data = self._client_data("webauthn.create", options["challenge"])
        attestation = cbor2.dumps({"fmt": "none", "attStmt": {}, "authData": self._auth_data(0x45, attested=True)})
        return {"id": b64(self.credential_id), "rawId": b64(self.credential_id), "type": "public-key",
                "response": {"clientDataJSON": b64(client_data), "attestationObject": b64(attestation),
                             "transports": ["internal"]},
                "clientExtensionResults": {}}

    def get(self, options) -> dict:
        self.count += 1
        client_data = self._client_data("webauthn.get", options["challenge"])
        auth_data = self._auth_data(0x05)
        signature = self.key.sign(auth_data + hashlib.sha256(client_data).digest(), ec.ECDSA(hashes.SHA256()))
        return {"id": b64(self.credential_id), "rawId": b64(self.credential_id), "type": "public-key",
                "response": {"clientDataJSON": b64(client_data), "authenticatorData": b64(auth_data),
                             "signature": b64(signature), "userHandle": b64(USER_ID)},
                "clientExtensionResults": {}}


async def add_passkey(client, token, authenticator) -> dict:
    options = await (await call(client, "POST", "/passkeys/options", {}, token)).json()
    response = await call(client, "POST", "/passkeys", {"credential": authenticator.create(options)}, token)
    assert response.status == 200, await response.text()
    return await response.json()


async def passkey_login(client, authenticator, agent=MAC):
    options = await (await call(client, "POST", "/login/passkey/options", {})).json()
    return await call(client, "POST", "/login/passkey", {"credential": authenticator.get(options)}, agent=agent)


# --- the files -------------------------------------------------------------------------------------------------

async def test_files(client):
    page = await client.get("/web/")
    assert page.status == 200 and page.headers["Cache-Control"] == "no-cache"
    assert '<script type="importmap">' in await page.text()
    assert (await client.get("/web", allow_redirects=False)).headers["Location"] == "/web/"
    worker = await client.get("/web/sw.js")
    assert worker.headers["Content-Type"].startswith("text/javascript")
    manifest = await client.get("/web/manifest.webmanifest")
    assert manifest.headers["Content-Type"].startswith("application/manifest+json")
    assert json.loads(await manifest.text())["scope"] == "/web/"
    for path in ("app.js", "app.css", "fonts.css", "vendor/preact.module.js", "icons/icon-512.png",
                 "fonts/Lora-normal-cyrillic.woff2"):
        response = await client.get("/web/static/" + path)
        assert response.status == 200 and response.headers["Cache-Control"] == "no-cache", path


async def test_the_pages_run_only_their_own_scripts(client):
    """D79: the CSP lets the page run its files and its import map, no other script; nothing frames or sniffs it."""
    import base64
    import hashlib
    import re

    page = await client.get("/web/")
    csp = page.headers["Content-Security-Policy"]
    [importmap] = re.findall(r'<script type="importmap">(.*?)</script>', await page.text(), re.S)
    digest = base64.b64encode(hashlib.sha256(importmap.encode()).digest()).decode()
    script_src = csp.split("script-src")[1].split(";")[0]
    assert script_src == f" 'self' 'sha256-{digest}'"
    assert "frame-ancestors 'none'" in csp and page.headers["X-Frame-Options"] == "DENY"
    assert page.headers["X-Content-Type-Options"] == "nosniff"
    api = await client.get("/web/api/me")
    assert api.headers["Cache-Control"] == "no-store" and api.headers["X-Content-Type-Options"] == "nosniff"


async def test_everything_static_is_referenced_files_exist(client):
    """The files that index.html, fonts.css and the manifest name are all there."""
    from infomaster.web import STATIC
    index = (STATIC / "index.html").read_text()
    names = re.findall(r'"/web/static/([^"]+)"', index)
    names += ["fonts/" + n for n in re.findall(r"url\(fonts/([^)]+)\)", (STATIC / "fonts.css").read_text())]
    manifest = json.loads((STATIC / "manifest.webmanifest").read_text())
    names += [i["src"].removeprefix("/web/static/") for i in manifest["icons"]]
    for name in names:
        assert (STATIC / name).is_file(), name


# --- the code from the bot -------------------------------------------------------------------------------------

async def test_code_login(client, sent, seconds):
    assert (await call(client, "GET", "/me")).status == 401
    assert (await call(client, "POST", "/login/code", {})).status == 200
    assert "код для входа в приложение infomaster" in sent[0]
    wrong = await call(client, "POST", "/login/code/check", {"code": "000000" if code_of(sent) != "000000" else "1"})
    assert wrong.status == 400 and (await wrong.json())["error"] == "Неверный код."
    response = await call(client, "POST", "/login/code/check", {"code": " ".join(code_of(sent))})
    assert response.status == 200
    cookie = response.cookies["im_session"]
    assert cookie["httponly"] and cookie["secure"] and cookie["samesite"] == "Strict" and cookie["path"] == "/web"
    me = await call(client, "GET", "/me", token=cookie.value)
    assert await me.json() == {"lang": "ru", "session": 1, "device": "iPhone · Safari", "passkeys": 0,
                                 "push": False}
    again = await call(client, "POST", "/login/code/check", {"code": code_of(sent)})
    assert again.status == 400 and (await again.json())["error"] == "Сначала запросите код."


async def test_code_limits(client, sent, seconds):
    assert (await call(client, "POST", "/login/code", {})).status == 200
    assert (await call(client, "POST", "/login/code", {})).status == 429  # a moment ago
    for _ in range(CODES_PER_HOUR - 1):
        seconds.value += RESEND_AFTER
        assert (await call(client, "POST", "/login/code", {})).status == 200
    seconds.value += RESEND_AFTER
    too_many = await call(client, "POST", "/login/code", {})
    assert too_many.status == 429 and "Слишком много кодов" in (await too_many.json())["error"]
    seconds.value += 3600
    assert (await call(client, "POST", "/login/code", {})).status == 200
    for _ in range(CODES_PER_DAY - CODES_PER_HOUR - 1):  # the day's limit holds across the hours
        seconds.value += 600
        assert (await call(client, "POST", "/login/code", {})).status == 200
    seconds.value += 3600
    assert (await call(client, "POST", "/login/code", {})).status == 429
    seconds.value += 86400
    assert (await call(client, "POST", "/login/code", {})).status == 200


async def test_codes_asked_at_once_pass_the_limits_once(client, sent):
    import asyncio

    answers = await asyncio.gather(*(call(client, "POST", "/login/code", {}) for _ in range(20)))
    assert sorted(a.status for a in answers) == [200] + [429] * 19 and len(sent) == 1


async def test_code_attempts_and_age(client, sent, seconds):
    await call(client, "POST", "/login/code", {})
    right = code_of(sent)
    for _ in range(ATTEMPTS):
        await call(client, "POST", "/login/code/check", {"code": "x"})
    assert (await call(client, "POST", "/login/code/check", {"code": right})).status == 400  # dropped
    seconds.value += RESEND_AFTER
    await call(client, "POST", "/login/code", {})
    seconds.value += CODE_TTL + 1
    assert (await call(client, "POST", "/login/code/check", {"code": code_of(sent)})).status == 400


async def test_bot_down(aiohttp_client, store):
    async def failing(text):
        raise RuntimeError("blocked")

    for send, status in ((failing, 502), (None, 503)):
        app = WebApp(env(), store, send_code=send).attach(web.Application())
        client = await aiohttp_client(app)
        assert (await call(client, "POST", "/login/code", {})).status == status


async def test_origin(client, sent, seconds):
    assert (await call(client, "POST", "/login/code", {}, origin=None)).status == 403
    assert (await call(client, "POST", "/login/code", {}, origin="https://evil.example")).status == 403
    assert not sent
    token = await log_in(client, sent, seconds)
    assert (await call(client, "POST", "/logout", {}, token, origin="https://evil.example")).status == 403
    assert (await call(client, "GET", "/me", token=token, origin=None)).status == 200  # reading needs no origin


# --- sessions --------------------------------------------------------------------------------------------------

async def test_sessions(client, sent, seconds):
    phone = await log_in(client, sent, seconds)
    mac = await log_in(client, sent, seconds, agent=MAC)
    listed = await (await call(client, "GET", "/devices", token=phone)).json()
    assert listed["current"] == 1 and [s["device"] for s in listed["sessions"]] == ["Mac · Chrome", "iPhone · Safari"]
    after = await (await call(client, "DELETE", "/sessions/2", token=phone)).json()
    assert [s["id"] for s in after["sessions"]] == [1]
    assert (await call(client, "GET", "/me", token=mac)).status == 401
    assert (await call(client, "DELETE", "/sessions/2", token=phone)).status == 404
    out = await call(client, "POST", "/logout", {}, phone)
    assert out.status == 200 and out.cookies["im_session"].value == ""
    assert (await call(client, "GET", "/me", token=phone)).status == 401


async def test_idle_session(client, sent, seconds):
    token = await log_in(client, sent, seconds)
    seconds.value += SESSION_IDLE - 10
    assert (await call(client, "GET", "/me", token=token)).status == 200  # its use is noted: idle again from now
    seconds.value += SESSION_IDLE - 10
    assert (await call(client, "GET", "/me", token=token)).status == 200
    seconds.value += SESSION_IDLE + 1
    assert (await call(client, "GET", "/me", token=token)).status == 401


# --- passkeys --------------------------------------------------------------------------------------------------

async def test_passkey(client, sent, seconds):
    token = await log_in(client, sent, seconds)
    phone = Authenticator()
    devices = await add_passkey(client, token, phone)
    assert [(p["id"], p["device"], p["used_at"]) for p in devices["passkeys"]] == [(1, "iPhone · Safari", None)]
    assert "ключ доступа на устройстве iPhone · Safari" in sent[-1]  # D79: the owner hears of a new key

    options = await (await call(client, "POST", "/passkeys/options", {}, token)).json()
    assert options["excludeCredentials"][0]["id"] == b64(phone.credential_id)
    assert options["rp"]["id"] == RP_ID and options["authenticatorSelection"]["userVerification"] == "required"

    seconds.value += 100
    response = await passkey_login(client, phone)
    assert response.status == 200, await response.text()
    new = token_of(response)
    me = await (await call(client, "GET", "/me", token=new)).json()
    assert me["device"] == "Mac · Chrome" and me["passkeys"] == 1
    listed = await (await call(client, "GET", "/devices", token=new)).json()
    assert listed["passkeys"][0]["used_at"] == int(seconds.value)
    assert (await passkey_login(client, phone)).status == 200  # the count grew again

    removed = await (await call(client, "DELETE", "/passkeys/1", token=new)).json()
    assert removed["passkeys"] == []
    gone = await passkey_login(client, phone)
    assert gone.status == 400 and "неизвестен" in (await gone.json())["error"]


async def test_passkey_refused(client, sent, seconds):
    token = await log_in(client, sent, seconds)
    phone = Authenticator()
    await add_passkey(client, token, phone)

    # an answer to a challenge the service did not give, or gave already
    options = await (await call(client, "POST", "/login/passkey/options", {})).json()
    credential = phone.get(options)
    assert (await call(client, "POST", "/login/passkey", {"credential": credential})).status == 200
    assert (await call(client, "POST", "/login/passkey", {"credential": credential})).status == 400
    forged = phone.get({"challenge": b64(os.urandom(32))})
    assert (await call(client, "POST", "/login/passkey", {"credential": forged})).status == 400

    # a challenge too old
    options = await (await call(client, "POST", "/login/passkey/options", {})).json()
    seconds.value += 301
    assert (await call(client, "POST", "/login/passkey", {"credential": phone.get(options)})).status == 400

    # another site's page
    phishing = Authenticator(origin="https://infomaster.example.com")
    phishing.key, phishing.credential_id, phishing.count = phone.key, phone.credential_id, phone.count
    response = await passkey_login(client, phishing)
    assert response.status == 400 and (await response.json())["error"] == "Ключ не принят."

    # a registration challenge used to log in, and a passkey added without a session
    options = await (await call(client, "POST", "/passkeys/options", {}, token)).json()
    as_login = Authenticator().create(options)
    assert (await call(client, "POST", "/login/passkey", {"credential": as_login})).status == 400
    assert (await call(client, "POST", "/passkeys", {"credential": as_login})).status == 401
    assert (await call(client, "POST", "/login/passkey", {"credential": "nonsense"})).status == 400


def test_device_name():
    assert device_name(IPHONE) == "iPhone · Safari"
    assert device_name(MAC) == "Mac · Chrome"
    assert device_name("Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) "
                       "Chrome/131.0 Mobile Safari/537.36") == "Android · Chrome"
    assert device_name("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0") == \
        "Windows · Firefox"
    assert device_name("") == "?"


# --- the screens (stage 16.2) ----------------------------------------------------------------------------------

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()


@pytest.fixture
def screens(store, seconds, sent, schedule, merged, now):
    """The timetable of the fixture (Tuesday 20.10.2026, noon), the dictionary for two courses, and the web app
    reading them."""
    schedule.ruz._sync(parse(FEED))
    schedule.subjects.set_manual("Discrete Mathematics", "Discrete Mathematics", "DM")
    schedule.subjects.set_manual("Algebra", "Algebra", "Alg")
    schedule.inbox.put_chat(-1001, "DM seminar chat", "supergroup", None, 0)

    async def send_code(text):
        sent.append(text)

    return WebApp(env(), store, send_code=send_code, lang=lambda: "en", now=seconds,
                  data=Data(schedule, merged, lang=lambda: "en"))


@pytest.fixture
async def reader(aiohttp_client, screens, sent, seconds):
    client = await aiohttp_client(screens.attach(web.Application()))
    client.token = await log_in(client, sent, seconds)
    return client


async def read(client, path):
    response = await call(client, "GET", path, token=client.token)
    assert response.status == 200, await response.text()
    return await response.json()


async def test_a_session_has_an_end_however_used(client, sent, seconds):
    """D79: used every day, a session still ends SESSION_MAX after its login."""
    from infomaster.web.auth import SESSION_MAX

    token = await log_in(client, sent, seconds)
    for _ in range(SESSION_MAX // SESSION_IDLE):
        seconds.value += SESSION_IDLE - 10
        assert (await call(client, "GET", "/me", token=token)).status == 200
    seconds.value += SESSION_MAX % SESSION_IDLE + 100
    assert (await call(client, "GET", "/me", token=token)).status == 401


async def test_screens_need_a_session(aiohttp_client, screens):
    client = await aiohttp_client(screens.attach(web.Application()))
    for path in ("/feed", "/week", "/deadline?uid=x", "/class?uid=x", "/course?key=DM"):
        assert (await call(client, "GET", path)).status == 401


async def test_feed(reader, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")  # due at Thursday's seminar
    add_row(merged, "Essay", "2026-10-25", course=None)
    add_row(merged, "Old quiz", "2026-10-19", course="Algebra")
    feed = await read(reader, "/feed")
    assert feed["today"] == "2026-10-20" and feed["now"] == "2026-10-20T12:00"
    assert feed["current"]["title"] == "Algebra" and feed["current"]["start"] == "2026-10-20T13:00:00"
    assert [d["title"] for d in feed["overdue"]] == ["Old quiz"]
    assert feed["courses"][0] == {"key": "Alg", "title": "Algebra"}  # the soonest class first
    days = {day["date"]: [(i["kind"], i["title"]) for i in day["items"]] for day in feed["days"]}
    assert days["2026-10-22"] == [("class", "Discrete Mathematics"), ("deadline", "Sheet 5")]
    assert days["2026-10-25"] == [("deadline", "Essay")]
    thursday = next(day for day in feed["days"] if day["date"] == "2026-10-22")["items"]
    assert thursday[1]["at_class"]["uid"] == thursday[0]["uid"] and thursday[1]["course"] == "DM"
    assert thursday[0]["due"] == [{"uid": thursday[1]["uid"], "title": "Sheet 5"}]
    assert thursday[0]["online"] and thursday[1]["from"] == "DM seminar chat"

    lens = await read(reader, "/feed?course=DM")
    titles = [i["title"] for day in lens["days"] for i in day["items"]]
    assert titles == ["Discrete Mathematics", "Sheet 5"] and lens["overdue"] == [] and lens["current"] is None


async def test_week(reader, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    week = await read(reader, "/week?start=2026-10-21")
    assert week["start"] == "2026-10-19" and len(week["days"]) == 7
    monday = week["days"][0]["items"]
    assert monday[0]["title"] == "Математический анализ" and monday[0]["past"]  # no dictionary entry yet
    assert [i["kind"] for i in week["days"][3]["items"]] == ["class", "deadline"]
    assert (await call(reader, "GET", "/week?start=soon", token=reader.token)).status == 400


async def test_deadline_card(reader, merged, schedule, now):
    schedule.inbox.add(-1001, 1, {"sender_id": 5, "sender_name": "Starosta", "at": now(),
                                  "text": "Sheet 5 is due on Sunday"})
    first = add_row(merged, "Sheet 5", "2026-10-25", course="Discrete Mathematics")
    moved = add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    with merged.db:
        merged.deadlines.move(first, moved)
    feed = await read(reader, "/feed")
    item = next(i for day in feed["days"] for i in day["items"] if i["kind"] == "deadline")
    card = await read(reader, "/deadline?uid=" + item["uid"])
    assert card["title"] == "Sheet 5" and card["at_class"]["title"] == "Discrete Mathematics"
    assert card["quote"]["text"] == "Sheet 5 is due on Sunday" and card["quote"]["sender"] == "Starosta"
    assert [(h["what"], h.get("was"), h["due"]) for h in card["history"]] == \
        [("found", None, "2026-10-25"), ("moved", "2026-10-25", "2026-10-22")]
    assert (await call(reader, "GET", "/deadline?uid=nothing", token=reader.token)).status == 404


async def test_class_card(reader, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    feed = await read(reader, "/feed")
    seminar = next(i for day in feed["days"] for i in day["items"] if i["title"] == "Discrete Mathematics")
    card = await read(reader, "/class?uid=" + seminar["uid"])
    assert card["type"] == "Seminar" and card["course"] == "DM" and card["course_title"] == "Discrete Mathematics"
    assert [d["title"] for d in card["due"]] == ["Sheet 5"] and card["materials"] == 0 and card["quote"] is None
    assert (await call(reader, "GET", "/class?uid=nothing", token=reader.token)).status == 404


async def test_course(reader, merged):
    add_row(merged, "Sheet 5", "2026-10-22", course="Discrete Mathematics")
    lens = await read(reader, "/course?key=DM")
    assert lens["title"] == "Discrete Mathematics" and lens["next"]["start"] == "2026-10-22T16:40:00"
    assert lens["next"]["due"][0]["title"] == "Sheet 5"
    assert lens["grades"] == {"count": 0, "pending": 0, "new": 0, "average": None, "works": [], "books": []}
    assert lens["documents"] == [] and lens["sheets"] == {"count": 0}
    assert (await call(reader, "GET", "/course?key=Nope", token=reader.token)).status == 404
