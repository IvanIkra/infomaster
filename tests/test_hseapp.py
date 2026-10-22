"""HSE App X against a fake Keycloak and a fake API: never the real ones, never real tokens."""

import base64
import datetime as dt
import hashlib
import json
import logging
from urllib.parse import parse_qs, urlsplit

import pytest
from aiohttp import web
from conftest import FakeNamer
from test_http import env, init_data, owner

from infomaster.collectors import hseapp
from infomaster.collectors.hseapp import HseApp, Raw, parse
from infomaster.llm.budget import BudgetExceeded
from infomaster.llm.client import BadAnswer, LLMError
from infomaster.outputs import miniapp
from infomaster.processing import hseapp as labels_module
from infomaster.processing.hseapp import HseAppDeadlines
from infomaster.processing.subjects import Subjects
from infomaster.settings import ChatSettings, Settings

CALLBACK = "https://infomaster.example.ts.net/hseapp/callback"


def item(title, expires_at, discipline=None, status="ACTIVE", id=None):
    return {"id": id, "title": title, "description": "", "discipline": discipline, "expires_at": expires_at,
            "completion_status": status, "is_pinned": False, "groups": [], "emails": []}


ITEMS = [
    item("Дедлайн ДЗ 3 по  Дискретной Математике", "2026-10-22T20:59:00.000Z",
         "Дискретная математика (2026/2027 модули: 1,2,3,4) (Иванов И.И.)", id="8f1c2a0e"),
    item("Доп лист Матан", "2026-10-19T20:59:01.000Z", id="91aa07d3"),  # overdue: kept, the screen picks
    item("Done one", "2026-10-25T20:59:00.000Z", status="COMPLETED", id="c0ffee01"),
]
DONE = [
    item("ДЗ 2 по БЖД", "2026-10-13T20:59:00.000Z",
         "Безопасность жизнедеятельности (2026/2027 модуль: 1) (Петров П.П.)", status="COMPLETED_LATE", id="b2d70c44"),
    item("Active one slipped in", "2026-10-25T20:59:00.000Z", id="ac71ve00"),
]
LABELS = {"Дедлайн ДЗ 3 по Дискретной Математике": "DM HW 3", "Доп лист Матан": "Calc extra",
          "ДЗ 2 по БЖД": "Safety HW 2"}
NAMES = {"Дискретная математика": ("Discrete Math", "DM"), "Безопасность жизнедеятельности": ("Life Safety", "Safety")}


class FakeLabeler:
    """Haiku for the labels: labels the titles it knows; `errors` are raised first, `broken` titles break a call."""

    def __init__(self, labels=None):
        self.labels = dict(LABELS if labels is None else labels)
        self.prompts, self.max_tokens, self.errors, self.broken = [], [], [], set()

    async def json(self, task, system, prompt, schema, *, max_tokens):
        assert task == "hseapp_labels" and system == labels_module.SYSTEM and schema == labels_module.SCHEMA
        self.prompts.append(prompt)
        self.max_tokens.append(max_tokens)
        if self.errors:
            raise self.errors.pop(0)
        items = []
        for line in prompt.splitlines():
            if line.startswith("- id "):
                raw_id, rest = line[5:].split(": ", 1)
                title = rest.split('"')[1]
                if title in self.broken:
                    raise BadAnswer("request declined (cyber)")
                if title in self.labels:
                    items.append({"id": raw_id, "label": self.labels[title]})
        return {"items": items}


@pytest.fixture
async def fake(aiohttp_server, monkeypatch):
    """Keycloak and the deadlines API. The refresh token rotates like the real one."""
    state = {"access": "access-secret-0", "refresh": "refresh-secret-1", "issued": 0, "grants": [], "redirects": [],
             "codes": {}, "reject_access": set(), "pages": [ITEMS], "completed": DONE, "status": 200, "bodies": []}

    async def token(request):
        form = await request.post()
        assert form["client_id"] == hseapp.CLIENT_ID
        state["grants"].append(form["grant_type"])
        if form["grant_type"] == "refresh_token":
            if form["refresh_token"] != state["refresh"]:
                return web.json_response({"error": "invalid_grant", "error_description": "Token is not active"},
                                         status=400)
        else:
            assert form["grant_type"] == "authorization_code"
            state["redirects"].append(form["redirect_uri"])
            challenge = state["codes"].pop(form["code"], None)
            digest = hashlib.sha256(form["code_verifier"].encode()).digest()
            if challenge != base64.urlsafe_b64encode(digest).rstrip(b"=").decode():
                return web.json_response({"error": "invalid_grant"}, status=400)
        state["issued"] += 1
        state["access"] = f"access-secret-{state['issued']}"
        state["refresh"] = f"refresh-secret-{state['issued'] + 1}"
        return web.json_response({"access_token": state["access"], "expires_in": 10800,
                                  "refresh_token": state["refresh"], "refresh_expires_in": 1814400})

    async def deadlines(request):
        if state["status"] != 200:
            return web.Response(status=state["status"])
        auth = request.headers.get("Authorization", "")
        if auth != f"Bearer {state['access']}" or state["access"] in state["reject_access"]:
            return web.Response(status=401)
        assert request.headers["Accept-Language"] == "ru"
        body = await request.json()
        state["bodies"].append((body, request.query.get("cursor")))
        if body == {"is_completed": True}:
            return web.json_response({"next_cursor": None, "items": state["completed"]})
        assert body == {"is_completed": False}
        page = int(request.query.get("cursor", 0))
        following = page + 1 if page + 1 < len(state["pages"]) else None
        return web.json_response({"next_cursor": str(following) if following else None,
                                  "items": state["pages"][page]})

    app = web.Application()
    app.router.add_post("/token", token)
    app.router.add_post("/deadlines", deadlines)
    server = await aiohttp_server(app)
    monkeypatch.setattr(hseapp, "TOKEN_URL", str(server.make_url("/token")))
    monkeypatch.setattr(hseapp, "DEADLINES_URL", str(server.make_url("/deadlines")))
    return state


def source(store, now) -> HseApp:
    src = HseApp(store, Settings(store), CALLBACK, now=now)
    src.notices, src.polls = [], []
    src.notify = lambda text, login: src.notices.append((text.ru, login))  # a Phrase: the forum picks its language

    async def on_poll(active, done):
        src.polls.append((active, done))

    src.on_poll = on_poll
    return src


def seed(src, refresh="refresh-secret-1", access="access-secret-0", access_left=0):
    """Tokens as a login left them; the access token runs out in `access_left` seconds."""
    at = src.now().timestamp()
    src.store.put_json(hseapp.AUTH_KEY, {"access_token": access, "expires_at": at + access_left,
                                         "refresh_token": refresh, "refresh_expires_at": at + 21 * 86400,
                                         "refreshed_at": at})


def query(url: str) -> dict[str, str]:
    return {key: values[0] for key, values in parse_qs(urlsplit(url).query).items()}


# --- the collector ---------------------------------------------------------------------


def test_parse():
    active = parse(ITEMS)
    assert [r.title for r in active] == ["Дедлайн ДЗ 3 по Дискретной Математике", "Доп лист Матан"]
    assert active[0] == Raw("8f1c2a0e", "Дедлайн ДЗ 3 по Дискретной Математике", "Дискретная математика",
                            dt.date(2026, 10, 22))  # 20:59Z is 23:59 in Moscow: the same day
    assert active[1].discipline is None and active[1].due == dt.date(2026, 10, 19)
    done = parse(DONE, completed=True)
    assert [(r.title, r.discipline) for r in done] == [("ДЗ 2 по БЖД", "Безопасность жизнедеятельности")]

    odd = [item("Без срока", None, id="1"), item("Кривой срок", "завтра", id="2"), item("  ", "2026-10-22T20:59Z"),
           item("Без id", "2026-10-22T21:00:00Z"), item("Без статуса", "2026-10-22T20:59:00Z", status=None),
           "not a deadline", {**item("Наивный срок", "2026-10-22T21:30:00"), "id": 7}]
    assert [(r.id, r.title, r.due) for r in parse(odd)] == [
        ("Без id", "Без id", dt.date(2026, 10, 23)),  # 21:00Z is past midnight in Moscow; no id: the title
        ("7", "Наивный срок", dt.date(2026, 10, 23))]  # no zone: UTC, as the API gives it
    assert hseapp.trim_discipline("Алгебра (рус) (Иванов)") == "Алгебра" and hseapp.trim_discipline("") is None


async def test_poll_refreshes_rotates_and_hands_both_lists(fake, store, now):
    src = source(store, now)
    seed(src)  # the access token has run out
    assert await src.poll() is True
    assert fake["grants"] == ["refresh_token"]
    assert store.get_json(hseapp.AUTH_KEY)["refresh_token"] == "refresh-secret-2"  # rotated: saved at once
    [(active, done)] = src.polls
    assert [r.id for r in active] == ["8f1c2a0e", "91aa07d3"] and [r.id for r in done] == ["b2d70c44"]
    assert [body for body, _ in fake["bodies"]] == [{"is_completed": False}, {"is_completed": True}]
    assert src.status() == {"enabled": True, "logged_in": True, "ok_at": "2026-10-20T12:00:00", "error": None,
                            "login_needed": False}

    assert await src.poll() is True
    assert fake["grants"] == ["refresh_token"]  # the new access token lives 3 hours

    now.moment += dt.timedelta(hours=2, minutes=51)  # under 10 minutes left
    await src.poll()
    assert fake["grants"] == ["refresh_token"] * 2 and src.auth.data["refresh_token"] == "refresh-secret-3"


async def test_a_401_refreshes_once_and_retries(fake, store, now):
    src = source(store, now)
    seed(src, access_left=3 * 3600)  # valid by its time, but the server revoked it
    fake["reject_access"].add("access-secret-0")
    assert await src.poll() is True and fake["grants"] == ["refresh_token"]

    fake["reject_access"].update({"access-secret-1", "access-secret-2"})  # refused after the refresh too: no loop
    assert await src.poll() is False
    assert src.error == "HTTP 401 Unauthorized" and fake["grants"] == ["refresh_token"] * 2


async def test_pages_are_joined_up_to_ten(fake, store, now):
    src = source(store, now)
    seed(src)
    fake["pages"] = [ITEMS[:1], ITEMS[1:]]
    await src.poll()
    assert len(src.polls[0][0]) == 2 and [cursor for _, cursor in fake["bodies"]] == [None, "1", None]

    fake["pages"] = [[item(f"ДЗ {n}", "2026-10-30T20:59:00Z", id=str(n))] for n in range(12)]
    await src.poll()
    assert [r.title for r in src.polls[1][0]] == [f"ДЗ {n}" for n in range(10)]


async def test_dead_session_means_login_needed_told_once(fake, store, now, caplog):
    caplog.set_level(logging.DEBUG)
    src = source(store, now)
    seed(src, refresh="refresh-secret-old")
    assert await src.poll() is False
    assert store.get_json(hseapp.AUTH_KEY) == {}  # erased: a dead token is never sent again
    status = src.status()
    assert status["login_needed"] and not status["logged_in"] and status["error"] == "login needed: invalid_grant"
    [(text, login)] = src.notices
    assert login and text.startswith("Вход в HSE App X больше не действует (invalid_grant)")
    for _ in range(3):  # the loop's next rounds: no poll without a login, no second notice
        src._tell_login()
    assert len(src.notices) == 1 and len(src.polls) == 0
    assert "refresh-secret" not in caplog.text and "access-secret" not in caplog.text

    url = src.login_link()
    fake["codes"]["code-1"] = query(url)["code_challenge"]
    await src.login(f"{CALLBACK}?state={query(url)['state']}&session_state=s&code=code-1")
    assert src.notices[-1] == ("Вход в HSE App X выполнен: дедлайны обновляются.", False)
    assert src.status()["login_needed"] is False and src.status()["error"] is None
    assert src._wake.is_set()  # the loop polls at once
    assert await src.poll() is True and len(src.notices) == 2


async def test_first_start_asks_for_a_login_once(store, now):
    src = source(store, now)
    assert src.status()["login_needed"] is True
    src._tell_login()
    [(text, login)] = src.notices
    assert login and text.startswith("Нужен вход в HSE App X")
    restarted = source(store, now)
    restarted._tell_login()  # told already
    assert restarted.notices == [] and len(src.notices) == 1

    src.set_enabled(False)  # off: no login needed, and nothing to say
    assert src.status()["login_needed"] is False and len(src.notices) == 1 and src._wake.is_set()
    src.set_enabled(True)
    assert len(src.notices) == 2 and src.notices[1][1]


async def test_pkce_login(fake, store, now):
    src = source(store, now)
    url = src.login_link()
    q = query(url)
    assert url.startswith(hseapp.AUTH_URL + "?")
    assert (q["client_id"], q["redirect_uri"], q["response_type"], q["scope"], q["code_challenge_method"]) == \
        (hseapp.CLIENT_ID, CALLBACK, "code", "profile email", "S256")
    fake["codes"]["code-1"] = q["code_challenge"]

    with pytest.raises(ValueError, match="out of date"):
        await src.login(f"{CALLBACK}?state=forged&code=code-1")
    with pytest.raises(ValueError, match="has no code"):
        await src.login("https://example.org/somewhere")
    with pytest.raises(ValueError, match="HSE did not let you in: Login was cancelled"):
        await src.login(f"{CALLBACK}?error=access_denied&error_description=Login+was+cancelled&state={q['state']}")
    await src.login(f"  {CALLBACK}?state={q['state']}&session_state=s&code=code-1 ")
    assert src.auth.logged_in and fake["redirects"] == [CALLBACK]
    with pytest.raises(ValueError, match="out of date"):  # a state works once
        await src.login(f"{CALLBACK}?state={q['state']}&code=code-1")

    # The fallback: the app's own address, pasted by hand; the code exchange names the same address.
    app_url = src.login_link(app=True)
    assert query(app_url)["redirect_uri"] == hseapp.APP_REDIRECT_URI
    fake["codes"]["code-2"] = query(app_url)["code_challenge"]
    await src.login(f"{hseapp.APP_REDIRECT_URI}?state={query(app_url)['state']}&code=code-2")
    assert fake["redirects"] == [CALLBACK, hseapp.APP_REDIRECT_URI]

    with pytest.raises(ValueError, match="HSE did not take the code"):  # a code Keycloak does not know
        await src.login(f"{CALLBACK}?state={query(src.login_link())['state']}&code=unknown")


async def test_only_the_last_five_links_work(fake, store, now):
    src = source(store, now)
    links = [query(src.login_link()) for _ in range(6)]
    with pytest.raises(ValueError, match="out of date"):
        await src.login(f"{CALLBACK}?state={links[0]['state']}&code=c")
    fake["codes"]["c"] = links[1]["code_challenge"]
    await src.login(f"{CALLBACK}?state={links[1]['state']}&code=c")
    assert src.auth.logged_in


async def test_login_server_down(store, now, monkeypatch):
    monkeypatch.setattr(hseapp, "TOKEN_URL", "http://127.0.0.1:9/token")  # nothing listens there
    src = source(store, now)
    state = query(src.login_link())["state"]
    with pytest.raises(ValueError, match="the HSE login server is unavailable"):
        await src.login(f"{CALLBACK}?state={state}&code=c")
    assert not src.auth.logged_in


async def test_api_failure_is_told_after_three_hours(fake, store, now):
    src = source(store, now)
    seed(src)
    await src.poll()
    fake["status"] = 503
    assert await src.poll() is False
    assert src.error == "HTTP 503 Service Unavailable" and src.notices == [] and src.ok_at == "2026-10-20T12:00:00"
    now.moment += dt.timedelta(hours=3)
    await src.poll()
    await src.poll()
    [(text, login)] = src.notices
    assert text.startswith("HSE App X недоступен с 20.10 12:00: HTTP 503 Service Unavailable.") and not login
    fake["status"] = 200
    assert await src.poll() is True
    assert src.notices[-1] == ("HSE App X снова доступен.", False) and src.error is None
    assert len(src.polls) == 2  # failed polls hand nothing over: the lists stay


async def test_run_polls_only_when_on_and_logged_in(fake, store, now):
    src = source(store, now)
    naps = []

    async def nap(seconds):
        naps.append(seconds)
        if len(naps) == 1:
            src.set_enabled(False)
        elif len(naps) == 2:
            seed(src)
        elif len(naps) == 3:
            src.set_enabled(True)
        else:
            raise RuntimeError("stop")

    src._nap = nap
    with pytest.raises(RuntimeError, match="stop"):
        await src.run()
    # on without a login: the notice, no poll; off; off and logged in: still no poll; on and logged in: a poll
    assert naps == [hseapp.POLL_EVERY] * 4 and len(src.polls) == 1 and len(src.notices) == 1


# --- labels and lists -------------------------------------------------------------------


@pytest.fixture
def lists(store, now):
    return HseAppDeadlines(store, Subjects(store, FakeNamer(NAMES)), FakeLabeler(), now=now)


async def test_labels_and_lists(lists, store):
    changes = []

    async def on_change(first):
        changes.append(first)

    lists.on_change = on_change
    assert lists.current() == [] and lists.status() == {"items": 0, "done": 0, "labels_error": None}
    assert await lists.after_poll(parse(ITEMS), parse(DONE, completed=True)) is True
    [prompt] = lists.llm.prompts  # active and done in one call
    assert prompt.splitlines()[0] == "Budget: at most 15 characters per label."
    assert '- id 8f1c2a0e: "Дедлайн ДЗ 3 по Дискретной Математике" (course: Дискретная математика, ' \
           'abbreviation: DM)' in prompt
    assert '- id 91aa07d3: "Доп лист Матан"' in prompt.splitlines() and "Иванов" not in prompt
    assert lists.current() == [
        {"id": "91aa07d3", "label": "Calc extra", "due": "2026-10-19", "course": None, "title": "Доп лист Матан"},
        {"id": "8f1c2a0e", "label": "DM HW 3", "due": "2026-10-22", "course": "Дискретная математика",
         "title": "Дедлайн ДЗ 3 по Дискретной Математике"}]
    assert lists.done() == [{"id": "b2d70c44", "label": "Safety HW 2", "due": "2026-10-13",
                             "course": "Безопасность жизнедеятельности", "title": "ДЗ 2 по БЖД"}]
    assert lists.lists()["fetched_at"] == "2026-10-20T12:00:00" and changes == [True]
    assert lists.status() == {"items": 2, "done": 1, "labels_error": None}

    assert await lists.after_poll(parse(ITEMS), parse(DONE, completed=True)) is False
    assert len(lists.llm.prompts) == 1 and changes == [True]  # cached for good: no call, no change

    moved = [{**ITEMS[0], "expires_at": "2026-10-23T20:59:00Z"}]
    assert await lists.after_poll(parse(moved), []) is True
    assert len(lists.llm.prompts) == 1 and changes == [True, False]
    assert lists.current()[0]["due"] == "2026-10-23" and lists.done() == []


TITLE_DM = "Дедлайн ДЗ 3 по Дискретной Математике"


class Queued:
    """Haiku for the labels answering in order; an exception in the list is raised instead."""

    def __init__(self, *answers):
        self.answers, self.prompts = list(answers), []

    async def json(self, task, system, prompt, schema, *, max_tokens):
        self.prompts.append(prompt)
        answer = self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return answer


async def test_a_new_abbr_asks_again(lists):
    await lists.after_poll(parse(ITEMS[:1]), [])
    lists.subjects.set_manual("Дискретная математика", "Discrete Math", "DMath")
    lists.llm.labels[TITLE_DM] = "DMath HW 3"
    await lists.after_poll(parse(ITEMS[:1]), [])
    assert "abbreviation: DMath" in lists.llm.prompts[1] and lists.current()[0]["label"] == "DMath HW 3"


async def test_label_too_wide_gets_one_retry(lists):
    lists.llm = Queued({"items": [{"id": "8f1c2a0e", "label": "DM homework three"}]},
                       {"items": [{"id": "8f1c2a0e", "label": "DM HW 3"}, {"id": "91aa07d3", "label": "Other"}]})
    await lists.after_poll(parse(ITEMS), [])
    assert lists.llm.prompts[1].splitlines()[2:] == [
        '- id 8f1c2a0e: "Дедлайн ДЗ 3 по Дискретной Математике" (course: Дискретная математика, abbreviation: DM)',
        '  Your previous label "DM homework three" was too wide for the screen; make it shorter.']
    # only the wide one is retried; the other one got no label and waits for the next poll
    assert [row["label"] for row in lists.current()] == ["Доп лист Матан", "DM HW 3"]


async def test_a_failed_retry_keeps_the_wide_label(lists):
    lists.llm = Queued({"items": [{"id": "8f1c2a0e", "label": "DM homework three"}]},
                       LLMError("Anthropic API error 529: Overloaded"))
    await lists.after_poll(parse(ITEMS[:1]), [])
    assert lists.current()[0]["label"] == "DM homework three" and lists.labels_error is None
    await lists.after_poll(parse(ITEMS[:1]), [])  # cached: Queued has no answers left
    assert len(lists.llm.prompts) == 2


async def test_odd_answers_are_skipped(lists):
    lists.llm = Queued({"items": [{"id": "zzz", "label": "Stray"}, {"id": "8f1c2a0e", "label": "  "}, "junk",
                                  {"id": "91aa07d3", "label": " Calc   extra "}]},
                       {"items": [{"id": "8f1c2a0e", "label": "DM HW 3"}]})
    await lists.after_poll(parse(ITEMS), [])
    assert [row["label"] for row in lists.current()] == ["Calc extra", TITLE_DM]
    await lists.after_poll(parse(ITEMS), [])  # asks again for the one left without a label
    assert '"Доп лист Матан"' not in lists.llm.prompts[1]
    assert [row["label"] for row in lists.current()] == ["Calc extra", "DM HW 3"]


async def test_without_haiku_cached_labels_stay_and_the_rest_waits(lists):
    await lists.after_poll(parse(ITEMS[:1]), [])
    lists.llm.errors = [LLMError("ANTHROPIC_API_KEY is not set")]
    await lists.after_poll(parse(ITEMS[:2]), [])
    assert [row["label"] for row in lists.current()] == ["Доп лист Матан", "DM HW 3"]
    assert lists.status()["labels_error"] == "ANTHROPIC_API_KEY is not set"

    await lists.after_poll(parse(ITEMS[:2]), [])  # the next poll asks again, for the new one only
    assert [row["label"] for row in lists.current()] == ["Calc extra", "DM HW 3"]
    assert lists.labels_error is None and '"Доп лист Матан"' in lists.llm.prompts[-1]
    assert "Дискретной" not in lists.llm.prompts[-1]


async def test_budget_exceeded_waits_quietly(lists):
    lists.llm.errors = [BudgetExceeded("the daily LLM limit is spent, calls wait for midnight")]
    await lists.after_poll(parse(ITEMS), [])
    assert [row["label"] for row in lists.current()] == ["Доп лист Матан", "Дедлайн ДЗ 3 по Дискретной Математике"]
    assert lists.labels_error is None


async def test_a_bad_answer_is_split_to_find_the_deadline(lists):
    lists.llm.broken = {"Доп лист Матан"}
    await lists.after_poll(parse(ITEMS), parse(DONE, completed=True))
    labels = {row["id"]: row["label"] for row in lists.current() + lists.done()}
    assert labels == {"8f1c2a0e": "DM HW 3", "91aa07d3": "Доп лист Матан", "b2d70c44": "Safety HW 2"}
    calls = len(lists.llm.prompts)
    await lists.after_poll(parse(ITEMS), parse(DONE, completed=True))
    assert len(lists.llm.prompts) == calls  # the broken one keeps its title for good: no call every poll


async def test_a_long_list_gets_room_to_answer(lists):
    many = [item(f"ДЗ {n}", "2026-10-30T20:59:00Z", id=f"id-{n}") for n in range(100)]
    await lists.after_poll([], parse(many, completed=False))
    assert lists.llm.max_tokens == [100 * labels_module.TOKENS_PER_LABEL]
    await lists.after_poll(parse(ITEMS[1:2]), [])
    assert lists.llm.max_tokens[-1] == labels_module.MAX_TOKENS


async def test_dictionary_down_labels_without_abbr(store, now):
    namer = FakeNamer(NAMES)
    namer.error = LLMError("cannot reach the Anthropic API")
    subjects = Subjects(store, namer)
    lists = HseAppDeadlines(store, subjects, FakeLabeler(), now=now)
    await lists.after_poll(parse(ITEMS[:1]), [])
    assert "(course: Дискретная математика)" in lists.llm.prompts[0] and subjects.error


# --- the Mini App and the callback --------------------------------------------------------


@pytest.fixture
async def client(aiohttp_client, fake, store, now, budget, journal, schedule, merged):
    src = source(store, now)
    lists = HseAppDeadlines(store, schedule.subjects, FakeLabeler(), now=now)
    sources = {"hseapp": lambda: {**src.status(), **lists.status()}}
    settings = Settings(store)
    app = miniapp.create_app(env(), budget, journal, schedule.inbox, ChatSettings(store), sources, schedule.subjects,
                             schedule, src, merged, settings)
    test_client = await aiohttp_client(app)
    test_client.src, test_client.settings = src, settings
    return test_client


async def test_miniapp_screen(client, fake, store):
    assert (await client.get("/app/api/hseapp")).status == 401
    other = {"Authorization": "tma " + init_data(7)}
    assert (await client.post("/app/api/hseapp/link", json={}, headers=other)).status == 403

    body = await (await client.get("/app/api/hseapp", headers=owner())).json()
    assert body == {"callback": CALLBACK, "status": {
        "enabled": True, "logged_in": False, "ok_at": None, "error": None, "login_needed": True, "items": 0,
        "done": 0, "labels_error": None}}

    link = await (await client.post("/app/api/hseapp/link", json={}, headers=owner())).json()
    assert query(link["url"])["redirect_uri"] == CALLBACK
    app_link = await (await client.post("/app/api/hseapp/link", json={"app": True}, headers=owner())).json()
    assert query(app_link["url"])["redirect_uri"] == hseapp.APP_REDIRECT_URI
    assert (await client.post("/app/api/hseapp/link", json={"app": "yes"}, headers=owner())).status == 400

    bad = await client.post("/app/api/hseapp/login", json={"url": f"{CALLBACK}?state=x&code=y"}, headers=owner())
    assert bad.status == 400 and await bad.text() == "this login link is out of date: open a new one in the Mini App"
    client.settings.set("miniapp_language", "ru")  # the reason in the Mini App's language (р. 41)
    bad = await client.post("/app/api/hseapp/login", json={"url": f"{CALLBACK}?state=x&code=y"}, headers=owner())
    assert bad.status == 400 and "устарела" in await bad.text()
    assert (await client.post("/app/api/hseapp/login", json={"url": " "}, headers=owner())).status == 400
    fake["codes"]["pasted"] = query(app_link["url"])["code_challenge"]
    pasted = f"{hseapp.APP_REDIRECT_URI}?state={query(app_link['url'])['state']}&code=pasted"
    body = await (await client.post("/app/api/hseapp/login", json={"url": pasted}, headers=owner())).json()
    assert body["status"]["logged_in"] and not body["status"]["login_needed"]
    assert "secret" not in json.dumps(body)  # the tokens never leave kv

    for wrong in ({}, {"enabled": 1}, {"enabled": True, "other": 1}):
        assert (await client.patch("/app/api/hseapp", json=wrong, headers=owner())).status == 400
    body = await (await client.patch("/app/api/hseapp", json={"enabled": False}, headers=owner())).json()
    assert body["status"]["enabled"] is False and Settings(store).get("hseapp_enabled") is False


async def test_callback_needs_no_init_data(client, fake):
    url = client.src.login_link()
    fake["codes"]["code-1"] = query(url)["code_challenge"]

    forged = await client.get("/hseapp/callback?state=forged&code=code-1")
    assert forged.status == 400 and forged.headers["Cache-Control"] == "no-store"
    assert '<html lang="en">' in await forged.text() and "The login failed" in await forged.text()
    assert "This login link is out of date" in await forged.text()
    client.settings.set("miniapp_language", "ru")  # the page in the Mini App's language (р. 41)
    forged = await client.get("/hseapp/callback?state=forged&code=code-1")
    assert "Вход не удался" in await forged.text() and "Эта ссылка входа устарела" in await forged.text()
    denied = await client.get(f"/hseapp/callback?error=access_denied&error_description=%3Cscript%3Ex%3C/script%3E"
                              f"&state={query(url)['state']}")
    assert denied.status == 400 and "<script>" not in await denied.text() and "&lt;script&gt;" in await denied.text()
    assert not client.src.auth.logged_in

    ok = await client.get(f"/hseapp/callback?state={query(url)['state']}&session_state=s&code=code-1")
    assert ok.status == 200 and "Вход в HSE App X выполнен" in await ok.text()
    assert client.src.auth.logged_in and fake["redirects"] == [CALLBACK]
    assert client.src.notices == [] and client.src._wake.is_set()  # no login was asked for: nothing to say


def test_status_never_holds_the_tokens(store, now):
    src = source(store, now)
    seed(src, access_left=3600)
    assert src.status()["logged_in"] and "secret" not in json.dumps(src.status())
    now.moment += dt.timedelta(days=22)  # three weeks without a refresh
    assert src.status()["login_needed"] and src.auth.ever


def test_due_is_a_moscow_date():
    assert hseapp.due_date("2026-10-31T20:59:00Z") == dt.date(2026, 10, 31)
    assert hseapp.due_date("2026-10-31T21:00:00+00:00") == dt.date(2026, 11, 1)
