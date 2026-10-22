from pathlib import Path

import pytest

from infomaster.collectors.ruz import Change, Diff, parse
from infomaster.outputs.timetable import BUTTON, Timetable
from infomaster.processing.batches import Changes
from infomaster.processing.events import event_uid

FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()
CHAT = -1001234567890
OWNER = 42
LINK = '<a href="https://t.me/c/1234567890/{}">Матан</a>'


def add(timetable, msg_id=1, **values) -> int:
    event = {"uid": event_uid("tg", CHAT, 1), "source": "tg", "chat_id": CHAT, "msg_ids": [msg_id],
             "course": "Алгебра", "title": "Alg consult", "kind": "consultation", "start": "2026-10-23T18:10:00",
             "end": "2026-10-23T19:30:00", "room": "R205", "required": True, **values}
    with timetable.db:
        return timetable.events.add(event, timetable.now())


def move(timetable, old_id, msg_id, **values) -> int:
    new_id = add(timetable, msg_id, **values)
    with timetable.db:
        timetable.events.supersede(old_id, new_id)
    return new_id


def fix(timetable, uid, msg_id, **values) -> int:
    item = timetable.ruz.by_uid(uid)
    return add(timetable, msg_id, **{"uid": uid, "class_uid": uid, "course": item["subject"], "title": "Discrete Math",
                                     "kind": item["kind"], "start": item["start"], "end": item["end"],
                                     "room": item["room"], **values})


def labels(buttons) -> list[list[tuple[str, bytes]]]:
    return [[(button.text, button.type.data) for button in row] for row in buttons or []]


@pytest.fixture
async def timetable(schedule, forum, now):
    schedule.inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    await schedule.after_poll(schedule.ruz._sync(parse(FEED)))
    return Timetable(forum, schedule.ruz.store, schedule.inbox, schedule, OWNER, now=now)


async def test_new_event(timetable, forum):
    timetable.schedule.subjects.set_manual("Алгебра", "Algebra", "Alg")
    event_id = add(timetable)
    await timetable.publish(Changes(events=[event_id]))
    [(msg_id, (topic, text))] = forum.messages.items()
    assert topic == "schedule"
    assert text == f"🆕 <b>Alg consult</b> — консультация\nпт 23.10 18:10–19:30 · R205\nAlgebra\n{LINK.format(1)}"
    assert labels(forum.buttons[msg_id]) == [
        [("✓ Основной", f"ev:main:{event_id}".encode()), ("Необязательный", f"ev:optional:{event_id}".encode())],
        [("Скрыть", f"ev:hide:{event_id}".encode())]]
    row = timetable.db.execute("select * from bot_posts").fetchone()
    assert (row["topic"], row["kind"], row["ref"], row["msg_id"]) == ("schedule", "event", event_id, msg_id)


async def test_moves_cancels_and_withdrawals(timetable, forum):
    first = add(timetable)
    moved = move(timetable, first, 2, start="2026-10-24T12:00:00", end="2026-10-24T13:20:00", room="")
    room = move(timetable, moved, 3, start="2026-10-24T12:00:00", end="2026-10-24T13:20:00",
                room="https://zoom.us/j/1")
    cancelled = move(timetable, room, 4, start="2026-10-24T12:00:00", end="2026-10-24T13:20:00", cancelled=True)
    back = move(timetable, cancelled, 5, start="2026-10-24T12:00:00", end="2026-10-24T13:20:00")
    await timetable.publish(Changes(events_moved=[(first, moved), (moved, room), (room, cancelled),
                                                  (cancelled, back)]))
    assert forum.texts("schedule") == [
        f"↪️ <b>Alg consult</b> перенесено: пт 23.10 18:10 → сб 24.10 12:00–13:20\n{LINK.format(2)}",
        f'🚪 <b>Alg consult</b> (сб 24.10 12:00): другое место · <a href="https://zoom.us/j/1">ссылка</a>\n'
        f"{LINK.format(3)}",
        f"✖️ <b>Alg consult</b> (сб 24.10 12:00) отменено\n{LINK.format(4)}",
        f"↩️ <b>Alg consult</b> снова в силе: сб 24.10 12:00–13:20 · R205\n{LINK.format(5)}",
    ]
    cancel_post = list(forum.messages)[2]
    assert labels(forum.buttons[cancel_post])[1] == [("Скрыть", f"ev:hide:{back}".encode())]  # the newest version

    forum.messages.clear()
    with timetable.db:
        timetable.events.drop([back], timetable.now())  # message 5 deleted: the cancellation is back
        own = add(timetable, 9, uid="other@x", title="Talk", start="2026-10-25T12:00:00")
        timetable.events.drop([own], timetable.now())
    await timetable.publish(Changes(events_gone=[back, own]))
    assert forum.texts("schedule") == [
        "✖️ Изменение <b>Alg consult</b> снято: сообщение удалено или исправлено; снова: отменено\n"
        + LINK.format(5),
        f"✖️ <b>Talk</b> (вс 25.10 12:00) снято: сообщение удалено или исправлено\n{LINK.format(9)}",
    ]


async def test_corrections_of_classes(timetable, forum):
    uid = "2000004@ruz.hse.ru"  # Discrete Mathematics, Thu Oct 22 16:40-18:00, online
    moved = fix(timetable, uid, 1, start="2026-10-23T13:00:00", end="2026-10-23T14:20:00", room="R405")
    cancelled = fix(timetable, "2000005@ruz.hse.ru", 2, title="Intro to SE", cancelled=True)
    back = fix(timetable, uid, 3)
    with timetable.db:
        timetable.events.supersede(moved, back)
    await timetable.publish(Changes(events=[moved, cancelled, back]))
    assert forum.texts("schedule") == [
        f"↪️ Пара <b>Discrete Math</b> (семинар) перенесена: чт 22.10 16:40 → пт 23.10 13:00–14:20 · R405\n"
        f"{LINK.format(1)}",
        f"✖️ Пара <b>Intro to SE</b> (семинар, пт 23.10 09:30) отменена\n{LINK.format(2)}",
        f"↩️ Пара <b>Discrete Math</b> (семинар) снова как в РУЗ: чт 22.10 16:40–18:00 · онлайн\n{LINK.format(3)}",
    ]
    assert labels(forum.buttons[list(forum.messages)[1]]) == [[("Не применять", f"ev:hide:{cancelled}".encode())]]

    forum.messages.clear()
    with timetable.db:
        timetable.events.drop([cancelled], timetable.now())
    await timetable.publish(Changes(events_gone=[cancelled]))
    assert forum.texts("schedule") == [
        "✖️ Изменение пары <b>Intro to SE</b> снято: сообщение удалено или исправлено; пара — пт 23.10 09:30–10:50\n"
        + LINK.format(2)]


async def test_hidden_and_past_events_are_quiet(timetable, forum):
    hidden = add(timetable, hidden=True)
    past = add(timetable, 2, uid="past@x", start="2026-10-19T18:00:00", end=None)
    old_class = fix(timetable, "2000001@ruz.hse.ru", 3, cancelled=True)  # Mon Oct 19
    await timetable.publish(Changes(events=[hidden, past, old_class], events_gone=[past]))
    assert forum.messages == {}


async def test_ruz_changes(timetable, forum, now):
    first = Diff(first=True)
    await timetable.publish_ruz(first)
    assert forum.texts("system") == ["РУЗ подключён: в расписании 9 пар."] and forum.texts("schedule") == []

    ruz = timetable.ruz
    algebra, dm, se = (ruz.by_uid(f"200000{i}@ruz.hse.ru") for i in (2, 4, 5))
    calc_past = ruz.by_uid("2000001@ruz.hse.ru")
    diff = Diff(changes=[
        Change("moved", algebra, {**algebra, "start": "2026-10-21T13:00:00", "end": "2026-10-21T14:20:00"}),
        Change("room", dm, {**dm, "room": "R510"}),
        Change("cancelled", se, {**se, "cancelled": True}),
        Change("removed", se, se),
        Change("new", None, {**algebra, "start": "2026-10-28T13:00:00", "end": "2026-10-28T14:20:00"}),
        Change("restored", None, se),
        Change("moved", calc_past, {**calc_past, "room": "D209"}),  # yesterday's: no news
    ])
    await timetable.publish_ruz(diff)
    [text] = forum.texts("schedule")
    assert text.split("\n") == [
        "<b>РУЗ: изменения в расписании</b>",
        "↪️ Algebra (лекция): вт 20.10 13:00 → ср 21.10 13:00–14:20 · R401",
        "🚪 Discrete Math (семинар) чт 22.10 16:40: online → R510",
        "✖️ Intro to SE (семинар) пт 23.10 09:30 отменена",
        "✖️ Intro to SE (семинар) пт 23.10 09:30 убрана из расписания",
        "🆕 Algebra (лекция) — ср 28.10 13:00–14:20 · R401",
        "↩️ Intro to SE (семинар) пт 23.10 09:30–10:50 снова в расписании",
    ]
    assert timetable.db.execute("select kind from bot_posts").fetchone()["kind"] == "ruz"

    forum.messages.clear()
    await timetable.publish_ruz(Diff())
    assert forum.messages == {}


class Press:
    """Telethon's CallbackQuery event, as much of it as the handler uses."""

    def __init__(self, data: bytes, sender_id: int = OWNER):
        self.sender_id, self.pattern_match = sender_id, BUTTON.match(data)
        self.answers, self.edits = [], []

    async def answer(self, text="", alert=False):
        self.answers.append((text, alert))

    async def edit(self, buttons=None):
        self.edits.append(buttons)


async def test_buttons(timetable):
    first = add(timetable)
    moved = move(timetable, first, 2, start="2026-10-24T12:00:00")
    stranger = Press(f"ev:hide:{first}".encode(), sender_id=7)
    await timetable.on_button(stranger)
    assert stranger.answers == [("Нет доступа", True)] and not timetable.events.get(moved)["hidden"]

    press = Press(f"ev:optional:{first}".encode())  # under the old post: acts on the newest version
    await timetable.on_button(press)
    assert press.answers == [("Теперь в необязательном календаре", False)]
    assert not timetable.events.get(moved)["required"] and timetable.events.get(moved)["chosen"]
    assert labels(press.edits[0])[0] == [("Основной", f"ev:main:{moved}".encode()),
                                         ("✓ Необязательный", f"ev:optional:{moved}".encode())]
    press = Press(f"ev:hide:{moved}".encode())
    await timetable.on_button(press)
    assert timetable.events.get(moved)["hidden"] and labels(press.edits[0])[1] == [
        ("Вернуть в календари", f"ev:show:{moved}".encode())]
    assert all(e.uid != timetable.events.get(moved)["uid"] for e in timetable.schedule.entries())

    correction = fix(timetable, "2000005@ruz.hse.ru", 3, cancelled=True)
    press = Press(f"ev:hide:{correction}".encode())
    await timetable.on_button(press)
    assert press.answers == [("Правка не применяется: пара как в РУЗ", False)]
    assert labels(press.edits[0]) == [[("Применить снова", f"ev:show:{correction}".encode())]]

    with timetable.db:
        timetable.events.drop([moved], timetable.now())
        timetable.events.drop([first], timetable.now())
    press = Press(f"ev:show:{moved}".encode())
    await timetable.on_button(press)
    assert press.answers == [("Этого события больше нет", True)] and press.edits == [None]
    press = Press(b"ev:main:999")
    await timetable.on_button(press)
    assert press.answers == [("Этого события больше нет", True)]


def test_button_pattern():
    assert BUTTON.match(b"ev:main:12") and BUTTON.match(b"ev:show:3")
    assert not BUTTON.match(b"ev:delete:12") and not BUTTON.match(b"llm:raise:2026-10-20")


async def test_kinds_are_escaped(timetable, forum):
    """A kind is the model's reading of a chat or the feed's first DESCRIPTION line: never markup in the post."""
    event_id = add(timetable, kind='<a href="https://x.test/">q&a</a>')
    await timetable.publish(Changes(events=[event_id]))
    algebra = timetable.ruz.by_uid("2000002@ruz.hse.ru")
    await timetable.publish_ruz(Diff(changes=[Change("room", algebra, {**algebra, "kind": "Q&A <b>", "room": "R5"})]))
    new, ruz = forum.texts("schedule")
    assert new.startswith("🆕 <b>Alg consult</b> — &lt;a href=&quot;https://x.test/&quot;&gt;q&amp;a&lt;/a&gt;\n")
    assert "Algebra (q&amp;a &lt;b&gt;) вт 20.10 13:00: R401 → R5" in ruz and "x.test/\">" not in new + ruz


async def test_english_forum(timetable, forum):
    """The forum's language (р. 41): the type, the day and the buttons in English, the titles as they are."""
    forum.lang = "en"
    timetable.schedule.subjects.set_manual("Алгебра", "Algebra", "Alg")
    event_id = add(timetable, room="online")
    await timetable.publish(Changes(events=[event_id]))
    [(msg_id, (_, text))] = forum.messages.items()
    assert text == f"🆕 <b>Alg consult</b> — consultation\nFri 23.10 18:10–19:30 · online\nAlgebra\n{LINK.format(1)}"
    assert labels(forum.buttons[msg_id]) == [
        [("✓ Main", f"ev:main:{event_id}".encode()), ("Optional", f"ev:optional:{event_id}".encode())],
        [("Hide", f"ev:hide:{event_id}".encode())]]
    moved = move(timetable, event_id, 2, start="2026-10-24T18:10:00", end="2026-10-24T19:30:00")
    await timetable.publish(Changes(events_moved=[(event_id, moved)]))
    assert forum.texts("schedule")[-1].startswith(
        "↪️ <b>Alg consult</b> moved: Fri 23.10 18:10 → Sat 24.10 18:10–19:30 · R205")
