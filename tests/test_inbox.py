import datetime as dt

import pytest

from uni_master import clock
from uni_master.collectors.inbox import Inbox, message_link

NOW = dt.datetime(2026, 10, 20, 9, 0, tzinfo=dt.UTC)


@pytest.fixture
def inbox(store):
    inbox = Inbox(store)
    inbox.put_chat(-1001, "Матан", "supergroup", None, 100)
    inbox.put_chat(-5, "Группа", "group", None, 100)
    inbox.put_chat(7, "Староста", "user", "starosta", 100)
    return inbox


def fields(text="Сдать ДЗ 5 до 22.10", at=NOW, **extra) -> dict:
    return {"sender_id": 7, "sender_name": "Староста", "at": at, "text": text, **extra}


def test_chat_put_keeps_last_id(inbox):
    assert inbox.put_chat(-1001, "Матанализ", "forum", "calc", 1) is False
    chat = inbox.chat(-1001)
    assert (chat["title"], chat["kind"], chat["username"], chat["last_id"]) == ("Матанализ", "forum", "calc", 100)
    inbox.advance(-1001, 120)
    inbox.advance(-1001, 110)  # a late live update never moves it back
    assert inbox.chat(-1001)["last_id"] == 120


def test_add_once(inbox):
    files = [{"kind": "pdf", "name": "hw5.pdf", "mime": "application/pdf", "size": 10}]
    assert inbox.add(-1001, 101, fields(files=files, forward={"from": "Кафедра", "chat_id": -1009, "msg_id": 3}))
    assert not inbox.add(-1001, 101, fields(text="другое"))  # the catch-up meets a live update
    message = inbox.message(-1001, 101)
    assert message["text"] == "Сдать ДЗ 5 до 22.10" and message["files"] == files
    assert message["at"] == "2026-10-20T12:00:00"  # Moscow local, no zone
    assert message["forward"]["from"] == "Кафедра" and message["parsed_at"] is None
    assert inbox.chats()[0]["messages"] == 0 and inbox.chats()[1]["messages"] == 1  # by title: Группа, Матан


def test_edit(inbox):
    inbox.add(-1001, 101, fields())
    assert not inbox.edit(-1001, 101, "Сдать ДЗ 5 до 22.10", [], NOW)  # a reaction: the content is the same
    assert inbox.message(-1001, 101)["edited_at"] is None
    assert inbox.edit(-1001, 101, "Сдать ДЗ 5 до 24.10", [], NOW + dt.timedelta(hours=1))
    message = inbox.message(-1001, 101)
    assert message["text"].endswith("24.10") and message["edited_at"] == "2026-10-20T13:00:00"
    assert not inbox.edit(-1001, 999, "нет такого", [], NOW)


def test_edit_of_a_legacy_photo(inbox):
    """Stored before stage 9: a photo without its size, maybe a link preview's picture. Its first edit is no news."""
    inbox.add(-1001, 101, fields(text="Расписание", files=[{"kind": "photo"}]))
    inbox.add(-1001, 102, fields(text="https://hse.ru", files=[{"kind": "photo"}]))
    photo = [{"kind": "photo", "mime": "image/jpeg", "size": 900}]
    assert not inbox.edit(-1001, 101, "Расписание", photo, NOW)
    assert not inbox.edit(-1001, 102, "https://hse.ru", [], NOW)  # the preview is no file now
    assert inbox.message(-1001, 101)["files"] == [{"kind": "photo"}]
    assert inbox.edit(-1001, 101, "Расписание", [{"kind": "pdf", "name": "a.pdf", "size": 5}], NOW)
    assert inbox.edit(-1001, 102, "https://hse.ru/news", [], NOW)  # a new text is


def test_delete(inbox):
    for chat_id in (-1001, -5, 7):
        inbox.add(chat_id, 101, fields())
    assert inbox.delete([101], None, NOW) == 2  # account-wide ids: the group and the private chat, not the channel
    assert inbox.message(-1001, 101)["deleted_at"] is None
    assert inbox.delete([101], -1001, NOW) == 1
    assert inbox.delete([101], -1001, NOW) == 0


def test_pin_and_senders(inbox):
    inbox.add(-1001, 101, fields())
    inbox.add(-1001, 102, fields(sender_id=8, sender_name="Преподаватель"))
    inbox.add(-1001, 103, fields(sender_id=8, sender_name="Преподаватель"))
    inbox.pin(-1001, [101], True)
    assert inbox.message(-1001, 101)["pinned"] is True
    inbox.pin(-1001, [101], False)
    assert inbox.message(-1001, 101)["pinned"] is False
    assert [s["id"] for s in inbox.senders(-1001)] == [8, 7]


def test_prune(inbox):
    inbox.add(-1001, 101, fields(at=NOW - dt.timedelta(days=15)))
    inbox.add(-1001, 102, fields(at=NOW - dt.timedelta(days=13)))
    assert inbox.prune(NOW) == 1
    assert inbox.message(-1001, 101) is None and inbox.message(-1001, 102) is not None
    assert clock.local_iso(NOW) == "2026-10-20T12:00:00"


def test_queue(inbox):
    inbox.add(-1001, 101, fields())
    inbox.add(-1001, 102, fields(text="удалённое"))
    inbox.delete([102], -1001, NOW)
    assert [m["msg_id"] for m in inbox.pending(-1001)] == [101] and inbox.pending_counts() == {-1001: 1}
    with inbox.db:
        inbox.mark_parsed(-1001, [101], NOW)
    assert inbox.pending(-1001) == []
    inbox.edit(-1001, 101, "Сдать ДЗ 5 до 24.10", [], NOW + dt.timedelta(minutes=1))
    assert [m["msg_id"] for m in inbox.pending(-1001)] == [101]  # edited after its batch: parsed again


def test_wake(inbox):
    woken = []
    inbox.wake = lambda chat_id, msg_ids: woken.append((chat_id, msg_ids))
    inbox.add(-1001, 101, fields())
    inbox.add(-1001, 101, fields())  # stored already: no news
    inbox.pin(-1001, [101], False)
    inbox.pin(-1001, [101], True)
    assert woken == [(-1001, [101]), (-1001, [101])]


def test_message_link():
    assert message_link({"id": -1001234567890, "kind": "channel", "username": "hse_news"}, 5) == \
        "https://t.me/hse_news/5"
    assert message_link({"id": -1001234567890, "kind": "supergroup", "username": None}, 5) == \
        "https://t.me/c/1234567890/5"
    assert message_link({"id": -5, "kind": "group", "username": None}, 5) is None
    assert message_link({"id": 7, "kind": "user", "username": None}, 5) is None
