"""Deadline reminders (D47, D69): on the eve and in the morning, once per slot, to the web app's notifier."""

import datetime as dt

import pytest

from infomaster import clock
from infomaster.outputs.reminders import DONE_KEY, DeadlineReminders
from infomaster.settings import Settings

CHAT = -1001234567890


@pytest.fixture
def reminders(merged, store, now):
    reminders = DeadlineReminders(store, merged, Settings(store), now=now)
    reminders.told = []
    reminders.notify = lambda name, due: reminders.told.append((name, [d.title for d in due]))
    reminders.start()  # Tue Oct 20 12:00: the morning's slot has come, the eve's is yesterday's
    return reminders


def add(reminders, label, due, msg_id=3) -> int:
    with reminders.store.db:
        return reminders.merged.deadlines.add({"source": "tg", "chat_id": CHAT, "msg_ids": [msg_id],
                                               "course": "Матанализ", "label": label, "due": due}, reminders.now())


def at(now, day: int, hour: int, minute: int = 0) -> None:
    """Moscow time, October 2026."""
    now.moment = dt.datetime(2026, 10, day, hour, minute, 5, tzinfo=clock.TZ).astimezone(dt.UTC)


async def test_the_eve_and_the_morning(reminders, now):
    add(reminders, "Calc HW 5", "2026-10-21")
    add(reminders, "Calc HW 6", "2026-10-22", msg_id=4)
    reminders.merged.add("Dentist", dt.date(2026, 10, 21), "personal")
    await reminders.tick()
    assert reminders.told == []  # the first start: what has come is no news

    at(now, 20, 20)
    await reminders.tick()
    assert reminders.told == [("eve", ["Calc HW 5", "Dentist"])]
    await reminders.tick()
    assert len(reminders.told) == 1  # once per slot

    at(now, 21, 9)
    await reminders.tick()
    assert reminders.told[1] == ("day", ["Calc HW 5", "Dentist"])
    assert reminders.store.get_json(DONE_KEY) == {"day": "2026-10-21", "eve": "2026-10-20"}


async def test_nothing_due_and_off(reminders, now):
    at(now, 20, 20)
    await reminders.tick()  # nothing due tomorrow: nothing told
    add(reminders, "Calc HW 5", "2026-10-22")
    reminders.settings.set("deadline_reminders", False)
    at(now, 21, 21)
    await reminders.tick()
    assert reminders.told == [] and reminders.store.get_json(DONE_KEY)["eve"] == "2026-10-21"  # off: not sent later


async def test_a_missed_slot_comes_late_the_same_day(reminders, now):
    add(reminders, "Calc HW 5", "2026-10-21")
    at(now, 20, 23, 30)  # the service was down at 20:00
    await reminders.tick()
    assert reminders.told == [("eve", ["Calc HW 5"])]
    reminders.settings.set("reminder_eve", "23:45")  # a later time today: the eve is sent already
    at(now, 20, 23, 50)
    await reminders.tick()
    assert len(reminders.told) == 1


async def test_hidden_deadlines_are_not_reminded(reminders, now):
    add(reminders, "Calc HW 5", "2026-10-21")
    add(reminders, "Calc KR", "2026-10-21", msg_id=5)
    [hw] = [d for d in reminders.merged.visible() if d.title == "Calc HW 5"]
    reminders.merged.remove(hw)
    at(now, 20, 20)
    await reminders.tick()
    assert reminders.told == [("eve", ["Calc KR"])]
