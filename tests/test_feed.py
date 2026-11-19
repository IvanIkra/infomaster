import asyncio
import datetime as dt

import pytest

from infomaster import clock
from infomaster.collectors.inbox import Inbox
from infomaster.outputs import feed as feed_module
from infomaster.outputs.feed import Feed, remember, split
from infomaster.processing.batches import Changes
from infomaster.processing.news import News
from infomaster.settings import Settings
from infomaster.store import Store

CHANNEL = -1001234567890
OWNER = 42


@pytest.fixture
def feed(store, now, forum):
    inbox = Inbox(store)
    inbox.put_chat(CHANNEL, "Матан <2026>", "supergroup", None, 0)
    return Feed(forum, store, inbox, News(store), Settings(store), OWNER, now=now)


def add_news(feed, full, importance=3, msg_id=1, ago=dt.timedelta(hours=1)) -> int:
    with feed.db:
        return feed.news.add({"source": "tg", "chat_id": CHANNEL, "msg_ids": [msg_id], "study": True,
                              "posted_at": clock.local_iso(feed.now() - ago), "full": full, "short": full[:28],
                              "importance": importance}, feed.now())


async def test_board_and_no_alerts(feed):
    forum = feed.forum
    await feed.publish(Changes())
    assert forum.texts("news") == ["<b>Новости</b>\n\nЖивых новостей нет."] and forum.pinned == [101]

    first = add_news(feed, "Alg seminar moved to Thu", importance=5, msg_id=7)
    await feed.publish(Changes(news=[first]))
    board = forum.messages[101][1]
    assert board == ('<b>Новости</b>\n\n💬 <b>Alg seminar moved to Thu</b> · '
                     '<a href="https://t.me/c/1234567890/7">Матан &lt;2026&gt;</a>')
    assert list(forum.messages) == [101] and forum.edited == [101]  # importance 5: no message of its own (D55)
    assert forum.buttons[101] is None and forum.notified == set()  # the list has no buttons and notifies no one

    await feed.publish(Changes())  # nothing changed: no edits
    assert forum.edited == [101]

    second = add_news(feed, "Moscow up to +19 today", importance=2, ago=dt.timedelta(minutes=5))
    await feed.publish(Changes(news=[second]))
    assert forum.texts("news")[0].split("\n")[2:] == [board.split("\n")[2], "💬 Moscow up to +19 today · "
                                                     '<a href="https://t.me/c/1234567890/1">Матан &lt;2026&gt;</a>']
    assert len(forum.texts("news")) == 1

    with feed.db:
        feed.news.hide(second)  # hidden: off the list
    await feed.publish(Changes())
    assert "+19" not in forum.texts("news")[0]

    feed.now.moment += dt.timedelta(hours=24)  # past the 24 hours
    await feed.publish(Changes())
    assert forum.texts("news") == ["<b>Новости</b>\n\nЖивых новостей нет."] and forum.notified == set()


async def test_alerts_of_an_older_version_are_deleted(feed):
    """The short messages an older version sent for important news go at the next rebuild."""
    forum = feed.forum
    item = add_news(feed, "Alg seminar moved to Thu", importance=5)
    old = await forum.post("news", "🔔 Alg seminar moved to Thu", html=True)
    remember(feed.db, "news", old, "news_alert", item, feed.now())
    await feed.publish(Changes())
    assert old in forum.deleted and old not in forum.messages
    assert feed.db.execute("select count(*) from bot_posts where kind = 'news_alert'").fetchone()[0] == 0
    assert len(forum.texts("news")) == 1


async def test_long_board_takes_several_messages(feed, monkeypatch):
    monkeypatch.setattr(feed_module, "MESSAGE_LIMIT", 300)
    for i in range(6):
        add_news(feed, f"News number {i} with some words", msg_id=i + 1, ago=dt.timedelta(minutes=i))
    await feed.publish(Changes())
    board = list(feed.forum.messages)
    assert len(board) > 1 and feed.forum.pinned == [101]
    assert all(len(text) <= 300 for text in feed.forum.texts("news"))
    assert sum(text.count("News number") for text in feed.forum.texts("news")) == 6

    with feed.db:
        feed.news.drop([1, 2, 3, 4], feed.now())
    await feed.publish(Changes())
    assert list(feed.forum.messages) == [101] and feed.forum.deleted == board[1:]


async def test_board_deleted_by_hand_starts_anew(feed):
    add_news(feed, "Alg seminar moved to Thu")
    await feed.publish(Changes())
    del feed.forum.messages[101]
    add_news(feed, "Calc KR on Friday", msg_id=2)
    await feed.publish(Changes())
    assert list(feed.forum.messages) == [102] and feed.forum.pinned == [101, 102]
    assert "Calc KR on Friday" in feed.forum.messages[102][1]


async def test_concurrent_publishes_build_one_board(feed):
    """Two runs publish at once: one board, one pin."""
    forum, post = feed.forum, feed.forum.post

    async def slow_post(*args, **kwargs):
        await asyncio.sleep(0.01)  # Telegram answers later: the other task runs meanwhile
        return await post(*args, **kwargs)

    forum.post = slow_post
    first = add_news(feed, "Alg seminar moved to Thu", importance=5)
    add_news(feed, "Moscow up to +19 today", importance=2, msg_id=2)
    await asyncio.gather(feed.publish(Changes(news=[first])), feed.publish(Changes()))
    assert len(forum.texts("news")) == 1 and forum.pinned == [101]


def test_old_news_table_gets_hidden(tmp_path):
    path = tmp_path / "infomaster.db"
    store = Store(path)
    with store.db:
        store.db.execute("drop table news")
        store.db.execute("drop table bot_posts")
        store.db.execute("create table news (id integer primary key, source text not null, chat_id integer not null, "
                         "msg_ids text not null, study integer not null, posted_at text not null, full text not null, "
                         "short text not null, importance integer not null, replaced_by integer, gone_at text, "
                         "created_at text not null)")  # as stage 4 left it
        store.db.execute("insert into news(source, chat_id, msg_ids, study, posted_at, full, short, importance, "
                         "created_at) values('tg', 1, '[1]', 1, '2026-10-20T11:00:00', 'x', 'x', 3, "
                         "'2026-10-20T11:00:00')")
        store.db.execute("create table bot_posts (id integer primary key, topic text not null, "
                         "msg_id integer not null, kind text not null, ref integer, digest text, at text not null)")
    store.close()
    store = Store(path)
    assert News(store).get(1)["hidden"] is False
    assert "key" in {row["name"] for row in store.db.execute("pragma table_info(bot_posts)")}
    Store(path).close()  # the next start upgrades nothing


def test_split():
    assert split("H\n", ["a", "b"], 100) == ["H\n\na\nb"]
    assert split("H", ["aaaa", "bbbb", "cc"], limit=8) == ["H\naaaa", "bbbb\ncc"]


async def test_english_forum(feed):
    """The forum's language (D41): the frame in English, the news as the model wrote them."""
    forum = feed.forum
    forum.lang = "en"
    await feed.publish(Changes())
    assert forum.texts("news") == ["<b>News</b>\n\nNo live news."]
    first = add_news(feed, "Alg seminar moved to Thu", importance=5, msg_id=7)
    await feed.publish(Changes(news=[first]))
    assert forum.messages[101][1].startswith("<b>News</b>\n\n💬 <b>Alg seminar moved to Thu</b>")
    assert list(forum.messages) == [101]
