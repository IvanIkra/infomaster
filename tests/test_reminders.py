"""Deadline reminders in «Дедлайны» (D47): on the eve and in the morning, once per slot, with a button each."""

import datetime as dt

import pytest
from conftest import Press, labels

from infomaster import clock
from infomaster.collectors.inbox import Inbox
from infomaster.outputs.deadlines import DeadlineTopic
from infomaster.outputs.reminders import BUTTON, DONE_KEY, DeadlineReminders
from infomaster.settings import Settings

CHAT = -1001234567890
OWNER = 42
LINK = '<a href="https://t.me/c/1234567890/3">Матан</a>'


@pytest.fixture
def reminders(merged, forum, store, now):
    inbox = Inbox(store)
    inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    topic = DeadlineTopic(forum, store, inbox, merged, OWNER, now=now)
    reminders = DeadlineReminders(forum, store, topic, Settings(store), OWNER, now=now)
    reminders.start()  # Tue Oct 20 12:00: the morning's slot has come, the eve's is yesterday's
    return reminders


def add(reminders, label, due, msg_id=3) -> int:
    with reminders.db:
        return reminders.topic.merged.deadlines.add({"source": "tg", "chat_id": CHAT, "msg_ids": [msg_id],
                                                     "course": "Матанализ", "label": label, "due": due},
                                                    reminders.now())


def at(now, day: int, hour: int, minute: int = 0) -> None:
    """Moscow time, October 2026."""
    now.moment = dt.datetime(2026, 10, day, hour, minute, 5, tzinfo=clock.TZ).astimezone(dt.UTC)


async def test_the_eve_and_the_morning(reminders, forum, now):
    add(reminders, "Calc HW 5", "2026-10-21")
    add(reminders, "Calc HW 6", "2026-10-22", msg_id=4)
    own = reminders.topic.merged.add("Dentist", dt.date(2026, 10, 21), "personal")
    await reminders.tick()
    assert forum.messages == {}  # the first start: what has come is no news

    at(now, 20, 20)
    await reminders.tick()
    [(msg_id, (topic, text))] = forum.messages.items()
    assert topic == "deadlines" and text == "\n".join([
        "⏰ <b>Завтра срок</b>",
        f"💬 <b>Calc HW 5</b> · Матанализ · {LINK}",
        "🏠 <b>Dentist</b> · свой"])
    assert labels(forum.buttons[msg_id]) == [[("Скрыть: Calc HW 5", b"dr:hide:0")],
                                             [("Удалить: Dentist", b"dr:hide:1")]]
    await reminders.tick()
    assert len(forum.messages) == 1  # once per slot

    at(now, 21, 9)
    await reminders.tick()
    assert forum.texts("deadlines")[1].startswith("⏰ <b>Сегодня срок</b>\n💬 <b>Calc HW 5</b>")
    assert reminders.store.get_json(DONE_KEY) == {"day": "2026-10-21", "eve": "2026-10-20"}
    assert own


async def test_nothing_due_and_off(reminders, forum, now):
    at(now, 20, 20)
    await reminders.tick()  # nothing due tomorrow: no message
    add(reminders, "Calc HW 5", "2026-10-22")
    reminders.settings.set("deadline_reminders", False)
    at(now, 21, 21)
    await reminders.tick()
    assert forum.messages == {} and reminders.store.get_json(DONE_KEY)["eve"] == "2026-10-21"  # off: not sent later


async def test_a_missed_slot_comes_late_the_same_day(reminders, forum, now):
    add(reminders, "Calc HW 5", "2026-10-21")
    at(now, 20, 23, 30)  # the service was down at 20:00
    await reminders.tick()
    assert forum.texts("deadlines")[0].startswith("⏰ <b>Завтра срок</b>")
    reminders.settings.set("reminder_eve", "23:45")  # a later time today: the eve is sent already
    at(now, 20, 23, 50)
    await reminders.tick()
    assert len(forum.messages) == 1


async def test_buttons(reminders, forum, now):
    first = add(reminders, "Calc HW 5", "2026-10-21")
    add(reminders, "Calc KR", "2026-10-21", msg_id=5)
    at(now, 20, 20)
    await reminders.tick()
    [msg_id] = forum.messages

    stranger = Press(BUTTON, b"dr:hide:0", sender_id=7, message_id=msg_id)
    await reminders.on_button(stranger)
    assert stranger.answers == [("Нет доступа", True)] and reminders.topic.merged.todo.hidden_rows() == []

    press = Press(BUTTON, b"dr:hide:0", message_id=msg_id)
    await reminders.on_button(press)
    assert press.answers == [("Скрыт: в списке, в Tablo и в HA его больше нет", False)]
    assert labels(press.edits[0]) == [[("Вернуть: Calc HW 5", b"dr:show:0")], [("Скрыть: Calc KR", b"dr:hide:1")]]
    back = Press(BUTTON, b"dr:show:0", message_id=msg_id)
    await reminders.on_button(back)
    assert back.answers == [("Снова в списке", False)] and reminders.topic.merged.todo.hidden_rows() == []

    with reminders.db:
        reminders.topic.merged.deadlines.drop([first], reminders.now())
    gone = Press(BUTTON, b"dr:hide:0", message_id=msg_id)
    await reminders.on_button(gone)
    assert gone.answers == [("Этого дедлайна больше нет в списке", True)]
    assert labels(gone.edits[0]) == [[("Скрыть: Calc KR", b"dr:hide:1")]]
    unknown = Press(BUTTON, b"dr:hide:3", message_id=999)
    await reminders.on_button(unknown)
    assert unknown.answers == [("Этого дедлайна больше нет в списке", True)]
