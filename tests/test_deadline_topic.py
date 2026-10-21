"""The «Дедлайны» topic (§6.4, §8.3): the pinned merged list, a message per new or moved deadline, the buttons."""

import asyncio
import datetime as dt

import pytest
from conftest import Press, labels

from uni_master.collectors.inbox import Inbox
from uni_master.outputs import deadlines as topic_module
from uni_master.outputs.deadlines import BUTTON, DeadlineTopic, due_mark

CHAT = -1001234567890
OWNER = 42
LINK = '<a href="https://t.me/c/1234567890/3">Матан &lt;2026&gt;</a>'
HEADER = "<b>Дедлайны</b>\n"


@pytest.fixture
def topic(merged, forum, store, now):
    inbox = Inbox(store)
    inbox.put_chat(CHAT, "Матан <2026>", "supergroup", None, 0)
    topic = DeadlineTopic(forum, store, inbox, merged, OWNER, now=now)
    merged.on_change = topic.changed
    return topic


def add(topic, label="Calc HW 5", due="2026-10-22", source="tg", course="Матанализ", msg_id=3) -> int:
    with topic.db:
        return topic.merged.deadlines.add({"source": source, "chat_id": None if source == "ical" else CHAT,
                                           "msg_ids": [] if source == "ical" else [msg_id], "course": course,
                                           "label": label, "due": due}, topic.now())


def move(topic, old_id, due) -> int:
    old = topic.merged.deadlines.get(old_id)
    new_id = add(topic, old["label"], due, old["source"], old["course"])
    with topic.db:
        topic.merged.deadlines.move(old_id, new_id)
    return new_id


def hseapp(topic, items=(), done=()):
    def rows(raws):
        return [{"id": raw_id, "label": label, "due": due, "course": course, "title": title}
                for raw_id, label, due, course, title in raws]
    topic.store.put_json("hseapp.deadlines", {"fetched_at": "2026-10-20T12:00:00", "items": rows(items),
                                              "done": rows(done)})


def board(forum) -> str:
    return forum.messages[forum.pinned[-1]][1]


def posts(forum) -> list[str]:
    """The deadline messages, without the pinned list."""
    return [text for msg_id, (t, text) in forum.messages.items() if t == "deadlines" and msg_id not in forum.pinned]


async def test_the_first_refresh_takes_the_list_as_it_is(topic, forum):
    add(topic)
    add(topic, "RSF Midterm", "2026-10-26", "ical", "Основы российской государственности")
    add(topic, "Old HW", "2026-10-19", course=None, msg_id=4)  # a chat's, a day overdue
    hseapp(topic, items=[("h1", "DM HW 3", "2026-10-20", "Дискретная математика", "ДЗ 3 по ДМ"),
                         ("h2", "Записаться на майнор", "2026-10-21", None, "Записаться на майнор")])
    await topic.refresh()
    assert posts(forum) == [] and forum.pinned == [101]  # no flood: only the list
    assert board(forum) == HEADER + "\n".join([
        "",
        "🎓 <b>DM HW 3</b> — сегодня · Дискретная математика",
        "🎓 <b>Записаться на майнор</b> — завтра",
        "💬 <b>Calc HW 5</b> — чт 22.10 · Матанализ",
        "📅 <b>RSF Midterm</b> — пн 26.10 · Основы российской государственности",
        "💬 <b>Old HW</b> — просрочен (пн 19.10)"])
    assert [row["ref"] for row in topic.store.get_json("deadlines.shown")] == ["hseapp:h1", "hseapp:h2", "dl:1",
                                                                              "dl:2", "dl:3"]
    await topic.refresh()
    assert forum.edited == [] and len(forum.messages) == 1  # nothing changed: nothing edited


async def test_new_moved_and_withdrawn(topic, forum):
    await topic.refresh()
    assert board(forum) == HEADER + "\nДедлайнов нет."
    first = add(topic)
    gone = add(topic, "Calc KR", "2026-10-23", msg_id=3)
    past = add(topic, "Old HW", "2026-10-19", msg_id=3)
    await topic.refresh()
    assert posts(forum) == [f"🆕 <b>Calc HW 5</b> — чт 22.10\nМатанализ\n{LINK}",
                            f"🆕 <b>Calc KR</b> — пт 23.10\nМатанализ\n{LINK}",
                            f"🆕 <b>Old HW</b> — пн 19.10\nМатанализ\n{LINK}"]
    assert labels(forum.buttons[102]) == [[("Скрыть", b"dl:hide")]]
    rows = topic.db.execute("select kind, ref, key from bot_posts where kind = 'deadline' order by id").fetchall()
    assert [tuple(row) for row in rows] == [("deadline", first, f"dl:{first}"), ("deadline", gone, f"dl:{gone}"),
                                            ("deadline", past, f"dl:{past}")]

    forum.messages = {msg_id: v for msg_id, v in forum.messages.items() if msg_id in forum.pinned}
    moved = move(topic, first, "2026-10-24")
    with topic.db:
        topic.merged.deadlines.drop([gone, past], topic.now())
    await topic.refresh()
    assert posts(forum) == [f"↪️ <b>Calc HW 5</b> перенесён: чт 22.10 → сб 24.10\n{LINK}",
                            f"✖️ <b>Calc KR</b> (пт 23.10) снят: сообщение удалено или исправлено\n{LINK}"]  # past: no
    assert topic.db.execute("select ref, key from bot_posts where kind = 'deadline' order by id desc").fetchone()[
        "key"] is None  # «снят» has no button
    assert board(forum) == HEADER + "\n💬 <b>Calc HW 5</b> — сб 24.10 · Матанализ" and moved


async def test_renames_only_edit_the_list(topic, forum):
    midterm = add(topic, "Основы российской государственности Midterm", "2026-10-26", "ical",
                  "Основы российской государственности")
    hseapp(topic, items=[("h1", "ДЗ 3 по ДМ", "2026-10-22", "Дискретная математика", "ДЗ 3 по ДМ")])
    await topic.refresh()
    with topic.db:
        topic.merged.deadlines.rename(midterm, "RSF Midterm", "Основы российской государственности")
    hseapp(topic, items=[("h1", "DM HW 3", "2026-10-22", "Дискретная математика", "ДЗ 3 по ДМ")])  # Haiku answered
    await topic.refresh()
    assert posts(forum) == [] and "<b>RSF Midterm</b>" in board(forum) and "<b>DM HW 3</b>" in board(forum)

    first = add(topic, "Prog HW 1", "2026-10-23")
    await topic.refresh()
    forum.messages = {msg_id: v for msg_id, v in forum.messages.items() if msg_id in forum.pinned}
    newer = add(topic, "Prog HW 1 (part 2)", "2026-10-23")  # an edit of the message: a new row, the same date
    with topic.db:
        topic.merged.deadlines.move(first, newer)
    await topic.refresh()
    assert posts(forum) == [] and "<b>Prog HW 1 (part 2)</b>" in board(forum)


async def test_hseapp_changes(topic, forum):
    await topic.refresh()
    hseapp(topic, items=[("h1", "DM HW 3", "2026-10-22", "Дискретная математика", "Дедлайн ДЗ 3 по ДМ"),
                         ("h2", "Calc HW 1", "2026-10-23", None, "Calc HW 1")])
    topic.quiet(["hseapp:h1", "hseapp:h2"])  # the first poll: app.py says so before the refresh
    await topic.refresh()
    assert posts(forum) == [] and topic._quiet == set()

    hseapp(topic, items=[("h1", "DM HW 3", "2026-10-24", "Дискретная математика", "Дедлайн ДЗ 3 по ДМ"),
                         ("h3", "Eng essay", "2026-10-25", "Английский язык", "Эссе по английскому"),
                         ("h4", "Hist HW", "2026-10-26", None, "Hist HW")],
           done=[("h2", "Calc HW 1", "2026-10-23", None, "Calc HW 1")])  # h2 done: it leaves quietly
    await topic.refresh()
    assert posts(forum) == ["↪️ <b>DM HW 3</b> перенесён: чт 22.10 → сб 24.10\nHSE App X",
                            "🆕 <b>Eng essay</b> — вс 25.10\nАнглийский язык\nHSE App X: «Эссе по английскому»",
                            "🆕 <b>Hist HW</b> — пн 26.10\nHSE App X"]

    forum.messages = {msg_id: v for msg_id, v in forum.messages.items() if msg_id in forum.pinned}
    hseapp(topic, items=[("h1", "DM HW 3", "2026-10-24", "Дискретная математика", "Дедлайн ДЗ 3 по ДМ")])
    await topic.refresh()
    assert posts(forum) == ["✖️ <b>Eng essay</b> (вс 25.10) снят: его больше нет в HSE App X\nHSE App X",
                            "✖️ <b>Hist HW</b> (пн 26.10) снят: его больше нет в HSE App X\nHSE App X"]


async def test_a_copy_taking_the_place_is_no_news(topic, forum, merged):
    merged.subjects.set_manual("Дискретная математика", "Discrete Math", "DM")
    tg = add(topic, "DM HW 3", "2026-10-22", course="Дискретная математика")
    await topic.refresh()
    hseapp(topic, items=[("h1", "DM HW 3", "2026-10-22", "Дискретная математика", "ДЗ 3")])
    with topic.db:
        topic.merged.deadlines.drop([tg], topic.now())  # even withdrawn: the work is still due
    await topic.refresh()
    assert posts(forum) == [] and "🎓 <b>DM HW 3</b>" in board(forum)


async def test_quiet_midterms_and_what_leaves_quietly(topic, forum):
    await topic.refresh()
    midterm = add(topic, "RSF Midterm", "2026-10-26", "ical", "Основы российской государственности")
    topic.quiet([f"dl:{midterm}"])  # a first RUZ poll
    await topic.refresh()
    assert posts(forum) == []
    with topic.db:
        topic.merged.deadlines.drop([midterm], topic.now())
    await topic.refresh()
    assert posts(forum) == ["✖️ <b>RSF Midterm</b> (пн 26.10) снят: пары больше нет в расписании"]

    forum.messages = {msg_id: v for msg_id, v in forum.messages.items() if msg_id in forum.pinned}
    item = add(topic, "Alg HW 2", "2026-10-21")
    await topic.refresh()
    forum.messages = {msg_id: v for msg_id, v in forum.messages.items() if msg_id in forum.pinned}
    [hidden] = topic.merged.all()
    topic.merged.remove(hidden)  # by the owner: the list changes, no message
    own = topic.merged.add("Dentist", dt.date(2026, 10, 23), "personal")
    await forum.settle()
    assert posts(forum) == [] and board(forum) == HEADER + "\n🏠 <b>Dentist</b> — пт 23.10"
    topic.merged.edit(own, None, dt.date(2026, 10, 24))  # moved by the owner: quiet too
    await forum.settle()
    assert posts(forum) == [] and "сб 24.10" in board(forum)
    topic.now.moment += dt.timedelta(days=10)  # past: it leaves quietly
    await topic.refresh()
    assert posts(forum) == [] and board(forum) == HEADER + "\nДедлайнов нет." and item


async def test_buttons(topic, forum):
    await topic.refresh()
    first = add(topic)
    await topic.refresh()
    [post_id] = [msg_id for msg_id in forum.messages if msg_id not in forum.pinned]

    stranger = Press(BUTTON, b"dl:hide", sender_id=7, message_id=post_id)
    await topic.on_button(stranger)
    assert stranger.answers == [("Нет доступа", True)] and topic.merged.todo.hidden_rows() == []

    moved = move(topic, first, "2026-10-24")  # the button under the old post acts on the newest version
    await topic.refresh()
    press = Press(BUTTON, b"dl:hide", message_id=post_id)
    await topic.on_button(press)
    await forum.settle()
    assert press.answers == [("Скрыт: в списке, в Tablo и в HA его больше нет", False)]
    assert labels(press.edits[0]) == [[("Вернуть", b"dl:show")]]
    assert [row["due"] for row in topic.merged.todo.hidden_rows()] == ["2026-10-24"]
    assert board(forum) == HEADER + "\nДедлайнов нет."
    again = Press(BUTTON, b"dl:hide", message_id=post_id)
    await topic.on_button(again)
    assert again.answers == [("Уже скрыт", False)] and len(topic.merged.todo.hidden_rows()) == 1

    back = Press(BUTTON, b"dl:show", message_id=post_id)
    await topic.on_button(back)
    await forum.settle()
    assert back.answers == [("Снова в списке", False)] and labels(back.edits[0]) == [[("Скрыть", b"dl:hide")]]
    assert topic.merged.todo.hidden_rows() == [] and "сб 24.10" in board(forum)
    assert len(posts(forum)) == 2  # the 🆕 and the move: coming back is no news

    with topic.db:
        topic.merged.deadlines.drop([moved], topic.now())
    gone = Press(BUTTON, b"dl:hide", message_id=post_id)
    await topic.on_button(gone)
    assert gone.answers == [("Этого дедлайна больше нет в списке", True)] and gone.edits == [None]
    unknown = Press(BUTTON, b"dl:hide", message_id=999)
    await topic.on_button(unknown)
    assert unknown.answers == [("Этого дедлайна больше нет в списке", True)]


async def test_own_deadline_button_deletes(topic, forum):
    await topic.refresh()
    own = topic.merged.add("Dentist", dt.date(2026, 10, 23), "personal")
    await forum.settle()
    topic.store.put_json("deadlines.shown", [])  # as if it had come before the list was shown: a message for it
    await topic.refresh()
    [post_id] = [msg_id for msg_id in forum.messages if msg_id not in forum.pinned]
    assert forum.messages[post_id][1] == "🆕 <b>Dentist</b> — пт 23.10\nСвой дедлайн"
    assert labels(forum.buttons[post_id]) == [[("Удалить", b"dl:hide")]]
    press = Press(BUTTON, b"dl:hide", message_id=post_id)
    await topic.on_button(press)
    assert press.answers == [("Удалён", False)] and press.edits == [None] and topic.merged.todo.own() == ()
    assert own.key == "ha-1"


async def test_concurrent_refreshes_tell_once(topic, forum):
    await topic.refresh()
    post = forum.post

    async def slow_post(*args, **kwargs):
        await asyncio.sleep(0.01)
        return await post(*args, **kwargs)

    forum.post = slow_post
    add(topic)
    await asyncio.gather(topic.refresh(), topic.refresh(), topic.refresh())
    assert posts(forum) == [f"🆕 <b>Calc HW 5</b> — чт 22.10\nМатанализ\n{LINK}"] and forum.pinned == [101]


async def test_a_failing_post_does_not_stop_the_list(topic, forum):
    await topic.refresh()
    post = forum.post

    async def flaky(topic_key, text, buttons=None, *, html=False):
        if text.startswith("🆕 <b>Calc HW 5"):
            raise ConnectionError("Telegram is down")
        return await post(topic_key, text, buttons, html=html)

    forum.post = flaky
    add(topic)
    add(topic, "Calc KR", "2026-10-23")
    await topic.refresh()
    assert posts(forum) == [f"🆕 <b>Calc KR</b> — пт 23.10\nМатанализ\n{LINK}"] and "Calc HW 5" in board(forum)
    forum.post = post
    await topic.refresh()
    assert len(posts(forum)) == 1  # the lost one is not told twice


async def test_long_list_takes_several_messages(topic, forum, monkeypatch):
    monkeypatch.setattr(topic_module, "MESSAGE_LIMIT", 200)
    for i in range(8):
        add(topic, f"Calc HW {i}", f"2026-10-{22 + i}")
    await topic.refresh()
    assert len(forum.messages) > 1 and forum.pinned == [101] and all(len(t) <= 200 for t in forum.texts("deadlines"))
    assert sum(text.count("Calc HW") for text in forum.texts("deadlines")) == 8


async def test_run_refreshes_at_midnight(topic, forum, monkeypatch):
    sleeps = []

    async def sleep(seconds):
        sleeps.append(seconds)
        if len(sleeps) == 2:
            raise asyncio.CancelledError

    monkeypatch.setattr(topic_module.asyncio, "sleep", sleep)
    add(topic, due="2026-10-21")
    with pytest.raises(asyncio.CancelledError):
        await topic.run()
    assert sleeps[0] == 12 * 3600 + topic_module.MIDNIGHT_DELAY  # noon in Moscow
    assert "завтра" in board(forum)


def test_due_marks():
    today = dt.date(2026, 10, 20)
    assert [due_mark(today + dt.timedelta(days=n), today) for n in (-1, 0, 1, 2)] == [
        "просрочен (пн 19.10)", "сегодня", "завтра", "чт 22.10"]
    assert BUTTON.match(b"dl:hide") and BUTTON.match(b"dl:show") and not BUTTON.match(b"dl:hide:3")
