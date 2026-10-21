import asyncio
import datetime as dt

import pytest
from conftest import Press, labels

from uni_master import clock
from uni_master.collectors.inbox import Inbox
from uni_master.outputs import feed as feed_module
from uni_master.outputs.feed import BUTTON, Feed, split
from uni_master.processing.batches import Changes
from uni_master.processing.news import News
from uni_master.settings import Settings
from uni_master.store import Store

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


async def test_board_and_alerts(feed):
    forum = feed.forum
    await feed.publish(Changes())
    assert forum.texts("news") == ["<b>Новости</b>\n\nЖивых новостей нет."] and forum.pinned == [101]

    first = add_news(feed, "Alg seminar moved to Thu", importance=5, msg_id=7)
    await feed.publish(Changes(news=[first]))
    board, alert = forum.messages[101][1], forum.messages[102][1]
    assert board == ('<b>Новости</b>\n\n💬 <b>Alg seminar moved to Thu</b> · '
                     '<a href="https://t.me/c/1234567890/7">Матан &lt;2026&gt;</a>')
    assert alert.startswith("🔔 Alg seminar moved to Thu\n")
    assert labels(forum.buttons[102]) == [[("Скрыть", f"nw:hide:{first}".encode())]]
    assert forum.buttons[101] is None  # the list has no buttons

    await feed.publish(Changes())  # nothing changed: no edits, the alert stays
    assert forum.edited == [101] and 102 in forum.messages

    second = add_news(feed, "Moscow up to +19 today", importance=2, ago=dt.timedelta(minutes=5))
    await feed.publish(Changes(news=[second]))
    assert 102 not in forum.messages  # the rebuild deleted the alert
    assert forum.texts("news")[0].split("\n")[2:] == [board.split("\n")[2], "💬 Moscow up to +19 today · "
                                                     '<a href="https://t.me/c/1234567890/1">Матан &lt;2026&gt;</a>']
    assert len(forum.texts("news")) == 1  # importance 2: no alert

    feed.now.moment += dt.timedelta(hours=24)  # both are past the 24 hours
    await feed.publish(Changes())
    assert forum.texts("news") == ["<b>Новости</b>\n\nЖивых новостей нет."]


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
    """The batches publish while the owner hides an item: one board, one pin, the new alert kept."""
    forum, post = feed.forum, feed.forum.post

    async def slow_post(*args, **kwargs):
        await asyncio.sleep(0.01)  # Telegram answers later: the other task runs meanwhile
        return await post(*args, **kwargs)

    forum.post = slow_post
    alert = add_news(feed, "Alg seminar moved to Thu", importance=5)
    other = add_news(feed, "Moscow up to +19 today", importance=2, msg_id=2)
    await asyncio.gather(feed.publish(Changes(news=[alert])), feed.publish(Changes()),
                         feed.on_button(Press(BUTTON, f"nw:hide:{other}".encode())))
    board = [text for text in forum.texts("news") if text.startswith("<b>Новости</b>")]
    assert len(board) == 1 and forum.pinned == [101] and "+19" not in board[0]
    assert any(text.startswith("🔔 Alg seminar moved to Thu") for text in forum.texts("news"))


async def test_hiding_an_alert(feed):
    forum = feed.forum
    shown = add_news(feed, "Alg seminar moved to Thu", importance=5, msg_id=7)
    kept = add_news(feed, "Calc KR on Friday", importance=4, msg_id=8, ago=dt.timedelta(minutes=5))
    await feed.publish(Changes(news=[shown, kept]))
    assert len(forum.texts("news")) == 3  # the list and two alerts

    stranger = Press(BUTTON, f"nw:hide:{shown}".encode(), sender_id=7)
    await feed.on_button(stranger)
    assert stranger.answers == [("Нет доступа", True)] and not feed.news.get(shown)["hidden"]

    press = Press(BUTTON, f"nw:hide:{shown}".encode())
    await feed.on_button(press)
    assert press.answers == [("Скрыто: в списке новостей её больше нет", False)] and feed.news.get(shown)["hidden"]
    link = '<a href="https://t.me/c/1234567890/8">Матан &lt;2026&gt;</a>'
    assert forum.texts("news") == [f"<b>Новости</b>\n\n💬 Calc KR on Friday · {link}",
                                   f"🔔 Calc KR on Friday\n{link}"]  # the other alert stays
    assert feed.news.live(feed.now(), 24) == [feed.news.get(kept)]  # later /api/news reads the same

    with feed.db:  # its message edited: the new item replaces the hidden one and stays hidden
        newer = feed.news.add({"source": "tg", "chat_id": CHANNEL, "msg_ids": [7], "study": True,
                               "posted_at": clock.local_iso(feed.now()), "full": "Alg seminar on Fri",
                               "short": "Alg seminar Fri", "importance": 5}, feed.now())
        feed.news.replace(shown, newer)
    await feed.publish(Changes(news=[newer]))
    assert all("Alg seminar" not in text for text in forum.texts("news"))

    gone = Press(BUTTON, f"nw:hide:{shown}".encode())  # an alert of a replaced item
    await feed.on_button(gone)
    assert gone.answers[0][1] is True and gone.edits == [None]


async def test_hiding_an_alert_whose_item_is_past(feed):
    """The item left the list already: the list stays, the alert goes."""
    item = add_news(feed, "Alg seminar moved to Thu", importance=5)
    await feed.publish(Changes(news=[item]))
    feed.now.moment += dt.timedelta(hours=25)
    await feed.on_button(Press(BUTTON, f"nw:hide:{item}".encode()))
    assert len(feed.forum.texts("news")) == 1 and feed.forum.texts("news")[0].endswith("Живых новостей нет.")


def test_old_news_table_gets_hidden(tmp_path):
    path = tmp_path / "uni-master.db"
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
