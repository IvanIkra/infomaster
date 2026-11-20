"""News in the web app (§8.6, stage 16.7): the live news in rank order, an item's card with its message, «Hide» with
its undo, and the mark of the important ones on the feed."""

import datetime as dt

import actions_app
import pytest
from test_http import TWIN, env
from test_web import FEED, Seconds, call, log_in

from infomaster import clock
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.inbox import MAILBOX
from infomaster.collectors.ruz import parse
from infomaster.connector import Connector
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings
from infomaster.web import WebApp
from infomaster.web.views import Data


def add(news, store, now, chat_id, msg_id, full, importance, study=True, hours=1, source="tg"):
    with store.db:
        return news.add({"source": source, "chat_id": chat_id, "msg_ids": [msg_id], "study": study,
                         "posted_at": clock.local_iso(now() - dt.timedelta(hours=hours)), "full": full,
                         "short": full[:20], "importance": importance}, now())


@pytest.fixture
async def reader(aiohttp_client, store, budget, journal, schedule, merged, now):
    """The public port with the news; Tuesday 20.10.2026, noon in Moscow, a logged-in device."""
    schedule.ruz._sync(parse(FEED))
    sent, seconds = [], Seconds()
    seconds.value = now().timestamp()

    async def send_code(text):
        sent.append(text)

    news = News(store)
    inbox = schedule.inbox
    inbox.put_chat(-1001, "Course chat", "supergroup", "course_chat", 0)
    inbox.add(-1001, 11, {"sender_id": 5, "sender_name": "Assistant", "at": now() - dt.timedelta(hours=1),
                          "text": "The test moves to Thursday, 13:00"})
    hse = HseApp(store, Settings(store), env().public_url + CALLBACK_PATH)
    app = actions_app.app(env(), budget, journal, inbox, ChatSettings(store), TWIN, schedule.subjects, schedule,
                             hse, merged, budget.settings)
    connector = Connector(env(), store, news, now=now, seconds=seconds)
    connector.attach(app)
    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "en", now=seconds,
                    data=Data(schedule, merged, lang=lambda: "en", news=news,
                              ttl_hours=lambda: budget.settings.get("news_ttl_hours")))
    webapp.connector = connector
    client = await aiohttp_client(webapp.attach(app))
    client.token = await log_in(client, sent, seconds)
    client.news, client.connector = news, connector
    return client


async def get(client, path, status=200):
    response = await call(client, "GET", path, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def post(client, path, status=200):
    response = await call(client, "POST", path, {}, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def test_news_list_card_and_hide(reader, store, now):
    news = reader.news
    test = add(news, store, now, -1001, 11, "The test moves to Thursday, 13:00", 5)
    party = add(news, store, now, -1001, 12, "Board games on Friday", 2, study=False)
    letter = add(news, store, now, MAILBOX, 3, "Minor applications until 25 November", 4, hours=3, source="mail")
    add(news, store, now, -1001, 13, "Yesterday's news", 3, hours=30)  # older than news_ttl_hours: not live

    listed = await get(reader, "/news")
    assert listed["ttl_hours"] == 24
    assert [i["id"] for i in listed["items"]] == [test, letter, party]  # importance + study, as Tablo has them
    assert listed["items"][0]["from"] == {"kind": "supergroup", "name": "Course chat"}
    assert listed["items"][1]["from"] == {"kind": "mail", "name": None}  # the letter is not kept
    assert (await get(reader, "/feed"))["news"] == 2  # the important ones: the mark on the News view

    card = await get(reader, f"/news/{test}")
    assert card["quote"]["text"] == "The test moves to Thursday, 13:00" and card["quote"]["sender"] == "Assistant"
    assert card["link"] == "https://t.me/course_chat/11" and card["live"]
    assert (await get(reader, "/news/999", status=404))["error"] == "no such news item"

    hidden = await post(reader, f"/news/{party}/hide")
    assert hidden["change"]["undo"] and "Board games" in hidden["change"]["summary"]
    assert [i["id"] for i in (await get(reader, "/news"))["items"]] == [test, letter]
    assert (await post(reader, f"/news/{party}/hide", status=400))["error"] == "it is hidden already"
    await get(reader, f"/news/{party}", status=404)
    assert reader.connector.changes.recent()[0]["via"] == "web"

    await post(reader, f"/changes/{hidden['change']['id']}/undo")
    assert [i["id"] for i in (await get(reader, "/news"))["items"]] == [test, letter, party]


def test_unhide_follows_replacements(store, now):
    """A hidden item replaced since (its message was edited) comes back with its replacement."""
    news = News(store)
    first = add(news, store, now, -1001, 1, "Old text", 3)
    with store.db:
        news.hide(first)
    second = add(news, store, now, -1001, 1, "New text", 3)
    with store.db:
        news.replace(first, second)
    assert news.get(second)["hidden"]  # hiding passes to the replacement
    with store.db:
        assert news.unhide(first)
    assert not news.get(first)["hidden"] and not news.get(second)["hidden"]
    assert [i["id"] for i in news.live(now(), 24)] == [second]
