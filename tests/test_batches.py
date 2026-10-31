import asyncio
import datetime as dt
from pathlib import Path

import pytest

from infomaster import clock
from infomaster.collectors.ruz import parse
from infomaster.llm.budget import BudgetExceeded
from infomaster.llm.client import BadAnswer, LLMError
from infomaster.outputs.timetable import Timetable
from infomaster.processing import batches as batches_module
from infomaster.processing.batches import Batches, Changes, check
from infomaster.processing.events import event_uid
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings

CHAT = -1001
FEED = (Path(__file__).parent / "fixtures" / "ruz.ics").read_bytes()
PDF = [{"kind": "pdf", "name": "hw5.pdf", "mime": "application/pdf", "size": 10}]


class FakeLLM:
    """Answers in order; an exception in the list is raised instead."""

    def __init__(self):
        self.answers, self.prompts, self.documents = [], [], []

    async def json(self, task, system, prompt, schema, *, max_tokens, chat_id, documents=()):
        assert task == "batch" and chat_id is not None
        self.prompts.append(prompt)
        self.documents.append(list(documents))
        answer = self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return answer


def item(msg_ids, full="Calc HW 5 due Thu Oct 22", short="Calc HW 5 → Oct 22", importance=5, replaces="",
         deadlines=(), events=()):
    return {"msg_ids": msg_ids, "news": {"full": full, "short": short, "importance": importance, "replaces": replaces},
            "deadlines": list(deadlines), "events": list(events)}


def deadline(label="Calc HW 5", due="2026-10-22", moves="", course="Математический анализ"):
    return {"course": course, "label": label, "due": due, "moves": moves}


@pytest.fixture
def batches(store, now, schedule):
    inbox, chat_settings = schedule.inbox, ChatSettings(store)
    inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    chat_settings.update(CHAT, {"enabled": True, "study": True})
    batches = Batches(store, Settings(store), chat_settings, inbox, News(store), schedule.deadlines, schedule,
                      FakeLLM(), now=now)
    batches.notices = []
    batches.notify = lambda line: batches.notices.append(line.ru)  # a Phrase: the forum picks its language
    return batches


def add(batches, msg_id, text="", ago=dt.timedelta(minutes=30), chat_id=CHAT, **extra):
    batches.inbox.add(chat_id, msg_id, {"sender_id": 7, "sender_name": "Староста", "at": batches.now() - ago,
                                        "text": text, **extra})


async def parse_chat(batches, chat_id=CHAT) -> Changes:
    changes = Changes()
    await batches.parse_chat(chat_id, changes)
    return changes


async def test_news_and_deadlines(batches):
    add(batches, 1, "ДЗ 5 по матану до четверга")
    add(batches, 2, "спасибо")
    batches.llm.answers = [{"items": [
        item([1, 99], deadlines=[deadline(), deadline(label="Calc HW 6", due="2026-13-01")]),
        item([98]),  # not from the batch
    ]}]
    changes = await parse_chat(batches)

    prompt = batches.llm.prompts[0]
    assert 'Chat: "Матан" (group, study)' in prompt and "Current news:\n(none)" in prompt
    assert "- 1 · Староста · Tue 2026-10-20 11:30\n  ДЗ 5 по матану до четверга" in prompt
    [news] = batches.news.live(batches.now(), 24)
    assert news["msg_ids"] == [1] and news["study"] and news["importance"] == 5
    assert changes.news == [news["id"]] and len(changes.deadlines) == 1
    [saved] = batches.deadlines.upcoming(clock.local_day(batches.now()))
    assert (saved["label"], saved["due"], saved["course"]) == ("Calc HW 5", "2026-10-22", "Математический анализ")
    assert batches.inbox.pending(CHAT) == []  # message 2 got no item and is parsed all the same

    add(batches, 3, "напоминаю: ДЗ 5 до 22.10")
    batches.llm.answers = [{"items": [item([3], deadlines=[deadline(label="calc  hw 5")])]}]
    changes = await parse_chat(batches)
    assert changes.deadlines == [] and len(batches.deadlines.upcoming(clock.local_day(batches.now()))) == 1
    assert f'- n{news["id"]}: "Calc HW 5 due Thu Oct 22"' in batches.llm.prompts[1]
    assert f'- d{saved["id"]}: "Calc HW 5" 2026-10-22' in batches.llm.prompts[1]


async def test_replace_and_move(batches):
    add(batches, 1, "ДЗ 5 до 22.10")
    batches.llm.answers = [{"items": [item([1], deadlines=[deadline()])]}]
    await parse_chat(batches)
    [old_news] = batches.news.live(batches.now(), 24)
    [old_deadline] = batches.deadlines.upcoming(clock.local_day(batches.now()))

    add(batches, 2, "ДЗ 5 переносится на 24.10")
    add(batches, 3, "и ещё раз: на 24.10, в 23:59")
    batches.llm.answers = [{"items": [
        item([2], full="Calc HW 5 moved to Sat Oct 24", replaces=f"n{old_news['id']}",
             deadlines=[deadline(due="2026-10-24", moves=f"d{old_deadline['id']}")]),
        item([3], full="Calc HW 5 moved to Oct 24 23:59", replaces="m2"),
    ]}]
    changes = await parse_chat(batches)
    [live] = batches.news.live(batches.now(), 24)
    assert live["full"] == "Calc HW 5 moved to Oct 24 23:59"
    assert batches.news.get(old_news["id"])["replaced_by"] == changes.news[0]
    assert batches.news.get(changes.news[0])["replaced_by"] == live["id"]
    [(moved_from, moved_to)] = changes.moved
    assert moved_from == old_deadline["id"] and changes.deadlines == []
    assert [d["due"] for d in batches.deadlines.upcoming(clock.local_day(batches.now()))] == ["2026-10-24"]


async def test_selection(batches):
    batches.chat_settings.update(CHAT, {"study": False, "filter": "important", "people": [8]})
    add(batches, 1, "Пара в среду отменяется", ago=dt.timedelta(hours=30))  # not study and past the 24 hours
    add(batches, 2, "", files=[{"kind": "photo"}])                         # nothing to read: «Картинки» is off
    add(batches, 3, "кто идёт в столовку?")                                # not important
    add(batches, 4, "Встреча в 18:30")                                     # a time
    add(batches, 5, "а где?", reply_to=4)                                  # a reply to an important one
    add(batches, 6, "", files=PDF)                                         # a file, read by its name
    add(batches, 7, "ок", sender_id=8)                                     # a listed person
    batches.llm.answers = [{"items": []}]
    await parse_chat(batches)
    prompt = batches.llm.prompts[0]
    assert "not a study chat" in prompt
    assert [line.split(" · ")[0] for line in prompt.splitlines() if line.startswith("- ")][-4:] == \
        ["- 4", "- 5", "- 6", "- 7"]
    assert "reply to 4" in prompt and "files: hw5.pdf" in prompt and "listed person" in prompt
    assert batches.inbox.pending(CHAT) == []


async def test_retry_for_length(batches):
    add(batches, 1, "ДЗ 5 до 22.10")
    long = "Calc homework number five has to be handed in by Thursday October 22"
    batches.llm.answers = [
        {"items": [item([1], full=long, deadlines=[deadline(label="Calculus homework 5")])]},
        {"items": [item([1], full="Calc HW 5 due Thu Oct 22", deadlines=[deadline()])]},
    ]
    await parse_chat(batches)
    assert "Your previous answer did not fit: full has 68 characters, keep it within 48; deadline label " \
           '"Calculus homework 5" has 19 characters, keep it within 14. Write it shorter.' in batches.llm.prompts[1]
    assert batches.news.live(batches.now(), 24)[0]["full"] == "Calc HW 5 due Thu Oct 22"


async def test_retry_failure_keeps_answer(batches):
    add(batches, 1, "ДЗ 5 до 22.10")
    batches.llm.answers = [{"items": [item([1], full="x" * 70, short="Calc HW 5 → Oct 22")]}, LLMError("down")]
    await parse_chat(batches)
    assert batches.news.live(batches.now(), 24)[0]["full"] == "Calc HW 5 → Oct 22"  # the short one stands in


async def test_bad_answer_is_narrowed_down(batches):
    add(batches, 1, "ДЗ 5 до 22.10")
    add(batches, 2, "что-то, на что модель не отвечает")
    batches.llm.answers = [BadAnswer("answer was cut at max_tokens"), {"items": [item([1])]},
                           BadAnswer("request declined")]
    await parse_chat(batches)
    assert len(batches.news.live(batches.now(), 24)) == 1 and batches.inbox.pending(CHAT) == []
    assert "пропущено разбором: request declined" in batches.notices[0]


async def test_failure_keeps_the_queue(batches):
    add(batches, 1, "ДЗ 5 до 22.10")
    batches.llm.answers = [LLMError("cannot reach the Anthropic API")]
    await batches.tick()
    assert len(batches.inbox.pending(CHAT)) == 1 and "Anthropic" in batches.status()["error"]
    assert batches.store.get_json("batches.done") is None  # the slot is made up at the next tick
    assert batches.status()["queue"] == 1

    batches.llm.answers = [BudgetExceeded("spent")]
    await batches.tick()
    assert len(batches.inbox.pending(CHAT)) == 1

    batches.llm.answers = [{"items": []}]
    await batches.tick()
    assert batches.inbox.pending(CHAT) == [] and batches.status() == {
        "ok_at": "2026-10-20T12:00:00", "error": None, "queue": 0}


async def test_edit_replaces_what_the_message_gave(batches, now):
    add(batches, 1, "ДЗ 5 до 22.10, КР в пятницу")
    batches.llm.answers = [{"items": [item([1], deadlines=[deadline(), deadline(label="Calc KR", due="2026-10-23")])]}]
    await parse_chat(batches)
    [old_news] = batches.news.live(batches.now(), 24)
    old_hw, old_kr = batches.deadlines.upcoming(clock.local_day(batches.now()))

    now.moment += dt.timedelta(minutes=5)
    batches.inbox.edit(CHAT, 1, "ДЗ 5 до 24.10", [], now())
    batches.llm.answers = [{"items": [item([1], full="Calc HW 5 due Sat Oct 24",
                                           deadlines=[deadline(due="2026-10-24")])]}]
    changes = await parse_chat(batches)
    assert f"(edited after it was read; its earlier news: n{old_news['id']})" in batches.llm.prompts[1]
    assert batches.news.get(old_news["id"])["replaced_by"] == changes.news[0]
    assert changes.moved == [(old_hw["id"], changes.moved[0][1])] and changes.gone == [old_kr["id"]]
    assert batches.deadlines.get(old_kr["id"])["gone_at"] is not None

    now.moment += dt.timedelta(minutes=5)
    batches.inbox.edit(CHAT, 1, "ДЗ 5 до 24.10!", [], now())  # the same deadline again: nothing new
    batches.llm.answers = [{"items": [item([1], full="Calc HW 5 due Sat Oct 24",
                                           deadlines=[deadline(due="2026-10-24")])]}]
    changes = await parse_chat(batches)
    assert changes.deadlines == changes.moved == changes.gone == []
    assert [d["due"] for d in batches.deadlines.upcoming(clock.local_day(batches.now()))] == ["2026-10-24"]


async def test_deleted_message_withdraws(batches):
    add(batches, 1, "ДЗ 5 до 22.10")
    add(batches, 2, "дополнение", grouped_id=55)
    add(batches, 3, "фото", grouped_id=55)
    batches.llm.answers = [{"items": [item([1], deadlines=[deadline()]), item([2, 3], full="Album")]}]
    await parse_chat(batches)
    assert "album with 2" in batches.llm.prompts[0]
    batches.inbox.delete([1, 2], CHAT, batches.now())
    changes = await parse_chat(batches)
    assert len(changes.gone) == 1 and batches.deadlines.upcoming(clock.local_day(batches.now())) == []
    assert [n["full"] for n in batches.news.live(batches.now(), 24)] == ["Album"]  # message 3 is still there
    assert batches.llm.prompts[1:] == []


async def test_slots(batches, now):
    assert batches._slot(CHAT) == "2026-10-20T12:00:00"
    await batches.tick()
    assert batches.store.get_json("batches.done") == {str(CHAT): "2026-10-20T12:00:00"}

    add(batches, 1, "ДЗ 5 до 22.10")
    now.moment += dt.timedelta(hours=2, minutes=30)  # 14:30: the 12:00 slot is done, 15:00 is not here yet
    await batches.tick()
    assert batches.llm.prompts == []
    batches.chat_settings.update(CHAT, {"batch_hours": 1})
    assert batches._slot(CHAT) == "2026-10-20T14:00:00"
    batches.llm.answers = [{"items": []}]
    await batches.tick()
    assert len(batches.llm.prompts) == 1
    assert batches._seconds_to_tick() == 30 * 60 + batches_module.TICK_DELAY


async def test_study_chats_go_first(batches):
    batches.inbox.put_chat(-1002, "Афиша", "channel", "afisha", 0)
    batches.inbox.put_chat(-1003, "Алгебра", "supergroup", None, 0)
    batches.chat_settings.update(-1002, {"enabled": True})
    batches.chat_settings.update(-1003, {"enabled": True, "study": True})
    assert batches._ordered([-1002, CHAT, -1003]) == [-1003, CHAT, -1002]


def test_urgent_trigger(batches):
    batches.inbox.wake = batches.wake
    add(batches, 1, "ДЗ", files=PDF)
    assert batches._urgent == {}  # the chat is not urgent
    batches.chat_settings.update(CHAT, {"urgent": True, "people": [8]})
    add(batches, 2, "просто сообщение")
    assert batches._urgent == {}
    for trigger in (lambda: add(batches, 3, "ДЗ", files=PDF), lambda: add(batches, 4, "ок", sender_id=8),
                    lambda: batches.inbox.pin(CHAT, [2], True)):
        trigger()
        assert list(batches._urgent) == [CHAT]
        batches._urgent.clear()


def test_urgent_burst(batches, monkeypatch):
    clock_s = [1000.0]
    batches.monotonic = lambda: clock_s[0]
    batches.inbox.wake = batches.wake
    batches.chat_settings.update(CHAT, {"urgent": True})
    batches.inbox.put_chat(-1002, "Алгебра", "supergroup", None, 0)
    batches.chat_settings.update(-1002, {"enabled": True, "urgent": True})

    def at(seconds, msg_id, chat_id=CHAT, **extra):
        clock_s[0] = 1000.0 + seconds
        add(batches, msg_id, "текст", chat_id=chat_id, **extra)

    at(0, 1, pinned=True)          # a lone pin: a minute of silence and it goes
    assert batches._ready_bursts() == []
    clock_s[0] = 1000.0 + batches_module.URGENT_QUIET
    assert batches._ready_bursts() == [CHAT] and batches._urgent == {}

    at(100, 2, files=PDF)          # an announcement in several messages: the burst waits for the last one
    at(140, 3)                     # not urgent itself, but part of the burst
    at(150, 4, chat_id=-1002, pinned=True)
    clock_s[0] = 1000.0 + 199
    assert batches._ready_bursts() == []
    clock_s[0] = 1000.0 + 200
    assert batches._ready_bursts() == [CHAT] and list(batches._urgent) == [-1002]

    batches._urgent.clear()
    for n, seconds in enumerate(range(400, 800, 30)):  # a chat that never goes quiet: five minutes at most
        at(seconds, 10 + n, files=PDF if n == 0 else [])
        if seconds < 400 + batches_module.URGENT_MAX_WAIT:
            assert batches._ready_bursts() == []
    assert batches._ready_bursts() == [CHAT]


async def test_run_parses_a_burst_once(batches, monkeypatch):
    monkeypatch.setattr(batches_module, "URGENT_QUIET", 0.05)
    batches.inbox.wake = batches.wake
    batches.chat_settings.update(CHAT, {"urgent": True})
    batches.llm.answers = [{"items": []}]
    task = asyncio.create_task(batches.run())
    try:
        await asyncio.sleep(0.01)  # the first tick: nothing to parse yet
        add(batches, 1, "ДЗ 5", files=PDF)
        await asyncio.sleep(0.03)
        add(batches, 2, "до пятницы")
        await asyncio.sleep(0.03)
        assert batches.llm.prompts == []  # still inside the burst
        await asyncio.sleep(0.05)
        [prompt] = batches.llm.prompts
        assert "- 1 · " in prompt and "- 2 · " in prompt and batches.inbox.pending(CHAT) == []
    finally:
        task.cancel()


def test_check():
    answer = {"items": [
        {"msg_ids": [1], "news": {"full": "", "short": "Only short", "importance": 9, "replaces": ""},
         "deadlines": [{"course": "", "label": "", "due": "2026-10-22", "moves": ""}]},
        {"msg_ids": [2], "news": {"full": " ", "short": "", "importance": 3, "replaces": ""}, "deadlines": []},
    ]}
    [only] = check(answer, {1, 2})
    assert only["news"] == {"full": "Only short", "short": "Only short", "importance": 5, "replaces": ""}
    assert only["deadlines"] == []


# --- events (stage 4) ------------------------------------------------------------------

def happening(title="Alg consult", start="2026-10-23T18:10", end="", room="R205", kind="consultation",
              required=True, moves="", cancels="", course="Алгебра"):
    return {"course": course, "title": title, "start": start, "end": end, "room": room, "kind": kind,
            "required": required, "moves": moves, "cancels": cancels}


@pytest.fixture
async def timetable(batches):
    """RUZ classes of the fixture feed, their subjects named."""
    await batches.schedule.after_poll(batches.schedule.ruz._sync(parse(FEED)))
    return batches


async def test_events_and_the_timetable_in_the_prompt(timetable):
    batches = timetable
    add(batches, 1, "Консультация по алгебре 23.10 в 18:10, R205")
    batches.llm.answers = [{"items": [item([1], events=[happening(), happening(title="", start="2026-10-24T10:00")])]}]
    changes = await batches.run_chats([CHAT])
    prompt = batches.llm.prompts[0]
    assert "event title at most 13 characters" in prompt and "Upcoming events:\n(none)" in prompt
    assert '- c2: Tue 2026-10-20 13:00-14:20 "Algebra" Lecture, R401' in prompt
    assert "- c1:" not in prompt  # yesterday's class
    assert '- c7: Tue 2026-10-27 09:30-10:50 "Calculus" Seminar, D208, cancelled' in prompt
    assert '- c8: Thu 2026-10-29 10:00-13:00 "Intro to Prog" (Введение в программирование) Interim assessment, R401' \
        in prompt
    [event_id] = changes.events
    event = batches.events.get(event_id)
    assert (event["uid"], event["title"], event["start"], event["end"], event["required"]) == (
        event_uid("tg", CHAT, 1), "Alg consult", "2026-10-23T18:10:00", None, True)
    assert batches.subjects.abbr("Алгебра") == "Alg"  # the course went to the dictionary after the run

    add(batches, 2, "Напоминаю про консультацию")
    batches.llm.answers = [{"items": [item([2], events=[happening()])]}]
    changes = await parse_chat(batches)
    assert changes.events == [] and f'- e{event_id}: Fri 2026-10-23 18:10 "Alg consult" consultation, R205, ' \
                                      "required" in batches.llm.prompts[1]
    assert '"Calculus" (Математический анализ)' not in batches.llm.prompts[1]  # the same name: no repeat
    assert '"Rus Statehood" (Основы российской государственности) Lecture, online' in batches.llm.prompts[1]


async def test_no_timetable_for_a_chat_that_is_not_study(timetable):
    timetable.chat_settings.update(CHAT, {"study": False})
    add(timetable, 1, "Ярмарка вакансий 24.10")
    timetable.llm.answers = [{"items": []}]
    await parse_chat(timetable)
    assert "Timetable classes" not in timetable.llm.prompts[0] and "Upcoming events:" in timetable.llm.prompts[0]


async def test_move_and_cancel_an_event(timetable):
    batches = timetable
    add(batches, 1, "Консультация 23.10 в 18:10")
    batches.llm.answers = [{"items": [item([1], events=[happening(end="2026-10-23T19:30")])]}]
    [first] = (await parse_chat(batches)).events
    batches.events.set_required(first, False)  # the owner's choice

    add(batches, 2, "Консультацию переносим на субботу 12:00")
    batches.llm.answers = [{"items": [item([2], events=[happening(start="2026-10-24T12:00", room="",
                                                                  moves=f"e{first}")])]}]
    changes = await parse_chat(batches)
    [(old, moved)] = changes.events_moved
    version = batches.events.get(moved)
    assert old == first and (version["start"], version["end"], version["room"]) == (
        "2026-10-24T12:00:00", "2026-10-24T13:20:00", "R205")  # the same length and room
    assert version["uid"] == batches.events.get(first)["uid"] and version["required"] is False

    add(batches, 3, "Консультации не будет")
    batches.llm.answers = [{"items": [item([3], events=[happening(start="", cancels=f"e{moved}")])]}]
    [(_, cancelled)] = (await parse_chat(batches)).events_moved
    assert batches.events.get(cancelled)["cancelled"] and batches.events.get(cancelled)["start"] == \
        "2026-10-24T12:00:00"

    batches.inbox.delete([3], CHAT, batches.now())  # the cancelling message is gone: the event is back
    changes = await parse_chat(batches)
    assert changes.events_gone == [cancelled] and batches.events.by_uid(version["uid"])["id"] == moved


async def test_move_and_cancel_a_class(timetable):
    batches = timetable
    add(batches, 1, "Семинар по дискретке в четверг переносится на пятницу 13:00, R405")
    batches.llm.answers = [{"items": [item([1], events=[happening(title="Discrete Math", start="2026-10-23T13:00",
                                                                  room="R405", kind="class", moves="c4")])]}]
    [fix] = (await parse_chat(batches)).events
    correction = batches.events.get(fix)
    assert (correction["uid"], correction["class_uid"], correction["end"], correction["room"]) == (
        "2000004@ruz.hse.ru", "2000004@ruz.hse.ru", "2026-10-23T14:20:00", "R405")
    assert (correction["course"], correction["title"]) == ("Discrete Mathematics", "Discrete Math")
    assert correction["required"]

    add(batches, 2, "Пары по алгебре сегодня не будет")
    batches.llm.answers = [{"items": [item([2], events=[happening(start="", cancels="c2")]),
                                      item([2], events=[happening(start="", cancels="c7")])]}]  # cancelled in RUZ
    [cancel] = (await parse_chat(batches)).events
    assert batches.events.get(cancel)["cancelled"] and batches.events.get(cancel)["class_uid"] == "2000002@ruz.hse.ru"
    prompt = batches.llm.prompts[1]
    assert '- c4: Thu 2026-10-22 16:40-18:00 "Discrete Math" (Discrete Mathematics) Seminar, online, moved by a ' \
           "chat message to Fri 2026-10-23 13:00-14:20, R405" in prompt

    add(batches, 3, "Всё же семинар по дискретке остаётся в четверг")
    batches.llm.answers = [{"items": [item([3], events=[happening(start="2026-10-22T16:40", end="2026-10-22T18:00",
                                                                  room="", moves="c4")])]}]
    [back] = (await parse_chat(batches)).events
    assert batches.events.get(fix)["replaced_by"] == back
    assert "moved by a chat" not in _class_line_of(batches, 4) and "cancelled by a chat" in _class_line_of(batches, 2)


def _class_line_of(batches, class_id):
    from infomaster.processing.batches import _class_line
    return next(_class_line(c) for c in batches.schedule.upcoming_classes(dt.date(2026, 10, 20))
                if c["id"] == class_id)


async def test_edits_and_deletions_of_event_messages(batches, now):
    add(batches, 1, "Консультация 23.10 в 18:10 и лекция гостя 24.10 в 12:00")
    batches.llm.answers = [{"items": [item([1], events=[happening(), happening(title="Guest talk", kind="talk",
                                                                               start="2026-10-24T12:00",
                                                                               required=False)])]}]
    consult, talk = (await parse_chat(batches)).events
    assert batches.events.get(talk)["uid"] == event_uid("tg", CHAT, 1, 1)  # two events of one message

    now.moment += dt.timedelta(minutes=5)
    batches.inbox.edit(CHAT, 1, "Консультация 23.10 в 19:00", [], now())
    batches.llm.answers = [{"items": [item([1], events=[happening(start="2026-10-23T19:00")])]}]
    changes = await parse_chat(batches)
    [(old, new)] = changes.events_moved
    assert old == consult and batches.events.get(new)["uid"] == batches.events.get(consult)["uid"]
    assert changes.events_gone == [talk] and changes.events == []

    now.moment += dt.timedelta(minutes=5)
    batches.inbox.edit(CHAT, 1, "Консультация 23.10 в 19:00!", [], now())  # the same event again: nothing
    batches.llm.answers = [{"items": [item([1], events=[happening(start="2026-10-23T19:00")])]}]
    changes = await parse_chat(batches)
    assert changes.events == changes.events_moved == changes.events_gone == []

    batches.inbox.delete([1], CHAT, now())
    changes = await parse_chat(batches)
    assert changes.events_gone == [new] and batches.events.upcoming(dt.date(2026, 10, 20)) == []


async def test_event_title_retry(batches):
    add(batches, 1, "Консультация по алгебре 23.10")
    batches.llm.answers = [
        {"items": [item([1], events=[happening(title="Algebra exam consultation")])]},
        {"items": [item([1], events=[happening()])]},
    ]
    await parse_chat(batches)
    assert 'event title "Algebra exam consultation" has 25 characters, keep it within 13' in batches.llm.prompts[1]
    assert batches.events.upcoming(dt.date(2026, 10, 20))[0]["title"] == "Alg consult"


async def test_course_names_wait_for_haiku(batches, namer):
    namer.error = LLMError("down")
    add(batches, 1, "Консультация по алгебре 23.10")
    batches.llm.answers = [{"items": [item([1], events=[happening()])]}]
    await batches.run_chats([CHAT])
    assert batches.subjects.get("Алгебра") is None and batches.subjects.error == "down"
    assert batches.events.upcoming(dt.date(2026, 10, 20))  # the event is saved all the same
    namer.error = None
    await batches.tick()  # the next run asks again
    assert batches.subjects.abbr("Алгебра") == "Alg"


def test_check_events():
    answer = {"items": [{"msg_ids": [1], "news": {"full": "", "short": "", "importance": 3, "replaces": ""},
                         "deadlines": [], "events": [
                             happening(start="23.10 18:10"),                                       # bad time
                             happening(title="", start="", cancels="e5", moves="c3"),               # a cancel
                             happening(end="2026-10-23T17:00", moves="x5", required="yes", kind=""),
                             happening(title=" ", start="2026-10-23T18:10"),                        # no title
                             happening(title="", start="2026-10-24T12:00", moves="c3"),             # a move: kept
                             happening(title="", start="", moves="e5")]}]}                          # no time
    [only] = check(answer, {1})
    cancel, odd, move = only["events"]
    assert (cancel["cancels"], cancel["moves"], cancel["start"]) == ("e5", "", None)
    assert (odd["end"], odd["moves"], odd["required"], odd["kind"]) == (None, "", True, "event")
    assert (move["title"], move["moves"], move["start"]) == ("", "c3", "2026-10-24T12:00:00")
    assert only["news"] is None


async def test_course_dictionary_in_the_prompt(batches):
    add(batches, 1, "ДЗ 5 по матану до четверга")
    batches.llm.answers = [{"items": []}, {"items": []}]
    await parse_chat(batches)
    header = 'Course dictionary (abbreviation "title": the names it goes by):\n'
    assert header + "(empty)\n" in batches.llm.prompts[0]

    batches.subjects.set_manual("Алгебра", "Algebra", "Alg")
    batches.subjects.set_manual("Линал", "Algebra", "Alg")
    add(batches, 2, "КР по алгебре 7.11")
    await parse_chat(batches)
    assert header + '- Alg "Algebra": "Алгебра", "Линал"\n' in batches.llm.prompts[1]


async def test_a_chat_moves_and_cancels_a_midterm(timetable, now):
    batches = timetable
    prog = batches.deadlines.get(batches.schedule.ruz.by_uid("2000007@ruz.hse.ru")["deadline_id"])
    assert (prog["label"], prog["due"]) == ("Prog Midterm", "2026-10-29")
    add(batches, 1, "РК по программированию переносится на четверг 5 ноября, 10:00")
    batches.llm.answers = [{"items": [item([1], events=[happening(title="", start="2026-11-05T10:00", room="",
                                                                  kind="test", moves="c8")])]}]
    changes = await batches.run_chats([CHAT])
    [fix] = changes.events  # a move without a title: the class keeps its own
    assert batches.events.get(fix)["title"] == "Intro to Prog"
    [(old, new)] = changes.moved  # the Midterm follows the class, for the Дедлайны topic
    assert old == prog["id"] and (batches.deadlines.get(new)["label"], batches.deadlines.get(new)["due"]) == (
        "Prog Midterm", "2026-11-05")

    add(batches, 2, "РК по программированию отменяется")
    batches.llm.answers = [{"items": [item([2], events=[happening(title="", start="", cancels="c8")])]}]
    changes = await batches.run_chats([CHAT])
    assert changes.gone == [new] and changes.moved == []

    batches.inbox.delete([2], CHAT, now())  # the cancellation is withdrawn: the move is in force again
    changes = await batches.run_chats([CHAT])
    [back] = changes.deadlines
    assert batches.deadlines.get(back)["due"] == "2026-11-05"


async def test_a_move_needs_no_title(timetable):
    batches = timetable
    add(batches, 1, "Консультация по алгебре 23.10 в 18:10")
    batches.llm.answers = [{"items": [item([1], events=[happening()])]}]
    [first] = (await parse_chat(batches)).events
    add(batches, 2, "Консультацию переносим на субботу 12:00")
    batches.llm.answers = [{"items": [item([2], events=[
        happening(title="", start="2026-10-24T12:00", moves=f"e{first}"),
        happening(title="", start="2026-10-25T12:00", moves="e999")])]}]
    changes = await parse_chat(batches)
    [(old, new)] = changes.events_moved
    assert old == first and batches.events.get(new)["title"] == "Alg consult"
    assert changes.events == []  # a move of nothing listed is no event of its own without a name


async def test_a_moved_class_stays_listed_after_its_day(timetable, now):
    batches = timetable
    uid = "2000004@ruz.hse.ru"  # Discrete Mathematics, Thu Oct 22 16:40
    add(batches, 1, "Семинар по дискретке в четверг переносится на вторник 27.10, 13:00")
    batches.llm.answers = [{"items": [item([1], events=[happening(title="", start="2026-10-27T13:00", room="",
                                                                  kind="class", moves="c4")])]}]
    [fix] = (await parse_chat(batches)).events

    now.moment += dt.timedelta(days=3)  # Fri Oct 23: Thursday is past, the move is ahead
    batches.inbox.edit(CHAT, 1, "Семинар по дискретке в четверг переносится на вторник 27.10, в 13:00", [], now())
    batches.llm.answers = [{"items": [item([1], events=[happening(title="", start="2026-10-27T13:00", room="",
                                                                  kind="class", moves="c4")])]}]
    changes = await parse_chat(batches)
    assert '- c4: Thu 2026-10-22 16:40-18:00 "Discrete Math"' in batches.llm.prompts[1]
    assert changes.events_gone == [] and batches.events.correction(uid)["id"] == fix
    assert '- c1:' not in batches.llm.prompts[1]  # a class past without a move is still left out

    batches.chat_settings.update(CHAT, {"study": False})  # no classes shown: the correction cannot be restated
    batches.inbox.edit(CHAT, 1, "Семинар по дискретке переносится на вторник 27.10, 13:00", [], now())
    batches.llm.answers = [{"items": []}]
    changes = await parse_chat(batches)
    assert changes.events_gone == [] and batches.events.correction(uid)["id"] == fix


async def test_deleted_superseded_version_never_comes_back(timetable, forum, now):
    batches = timetable
    add(batches, 1, "Консультация 23.10 в 18:10")
    batches.llm.answers = [{"items": [item([1], events=[happening(end="2026-10-23T19:30")])]}]
    [v1] = (await parse_chat(batches)).events
    add(batches, 2, "Консультация переносится на 24.10 12:00")
    batches.llm.answers = [{"items": [item([2], events=[happening(start="2026-10-24T12:00", moves=f"e{v1}")])]}]
    [(_, v2)] = (await parse_chat(batches)).events_moved
    add(batches, 3, "Консультация переносится на 25.10 12:00")
    batches.llm.answers = [{"items": [item([3], events=[happening(start="2026-10-25T12:00", moves=f"e{v2}")])]}]
    [(_, v3)] = (await parse_chat(batches)).events_moved
    poster = Timetable(forum, batches.store, batches.inbox, batches.schedule, 42, now=now)

    batches.inbox.delete([2], CHAT, now())  # the message of a superseded version: no news
    changes = await parse_chat(batches)
    await poster.publish(changes)
    assert changes.events_gone == [] and batches.events.get(v2)["gone_at"] is not None
    assert batches.events.by_uid(batches.events.get(v1)["uid"])["id"] == v3

    batches.inbox.delete([3], CHAT, now())  # the version in force is withdrawn: the first one is back, not v2
    changes = await parse_chat(batches)
    await poster.publish(changes)
    assert changes.events_gone == [v3] and batches.events.by_uid(batches.events.get(v1)["uid"])["id"] == v1
    [post] = forum.texts("schedule")  # one post in all: the first version is back
    assert post.startswith("✖️ Изменение <b>Alg consult</b> снято: сообщение удалено или исправлено; "
                           "снова: пт 23.10 18:10–19:30\n")
    changes = await parse_chat(batches)  # nothing more to withdraw
    assert changes.events_gone == []
