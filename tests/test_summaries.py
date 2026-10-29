import asyncio
import datetime as dt
from types import SimpleNamespace

import pytest
from conftest import labels, spend
from telethon.tl import types

from infomaster import clock
from infomaster.collectors.inbox import Inbox
from infomaster.llm.budget import BudgetExceeded, DoesNotFit, Level
from infomaster.llm.client import BadAnswer, LLMError
from infomaster.outputs import summaries as topic_module
from infomaster.outputs.summaries import BUTTON, COMMAND, DONE, LIMIT_SPENT, SummaryTopic, forwarded_chat, split_args
from infomaster.processing import summaries as summaries_module
from infomaster.processing.summaries import PeriodError, Summaries, parse_period, span
from infomaster.settings import ChatSettings, Settings, SettingsError

CHAT = -1001234567890     # a supergroup: its messages have t.me/c links
OTHER = -1009876543210
PRIVATE = 77              # a private chat: no links
OWNER, FORUM = 42, -1005555555555


class FakeLLM:
    """Answers in order (an exception is raised instead); an empty list answers no points."""

    def __init__(self):
        self.answers, self.calls = [], []

    async def json(self, task, system, prompt, schema, *, max_tokens, chat_id, asked):
        assert task == "summary"
        self.calls.append({"system": system, "prompt": prompt, "chat_id": chat_id, "asked": asked})
        answer = self.answers.pop(0) if self.answers else {"points": []}
        if isinstance(answer, Exception):
            raise answer
        return answer


def points(*items) -> dict:
    return {"points": [{"text": text, "msg_id": msg_id} for text, msg_id in items]}


@pytest.fixture
def inbox(store):
    inbox = Inbox(store)
    inbox.put_chat(CHAT, "Матан 2026", "supergroup", None, 0)
    inbox.put_chat(OTHER, "Алгебра", "supergroup", None, 0)
    inbox.put_chat(PRIVATE, "Петя", "user", None, 0)
    return inbox


@pytest.fixture
def chat_settings(store):
    chat_settings = ChatSettings(store)
    chat_settings.update(CHAT, {"enabled": True, "study": True, "summary": True})
    chat_settings.update(OTHER, {"enabled": True, "summary": True})
    chat_settings.update(PRIVATE, {"enabled": True})
    return chat_settings


@pytest.fixture
def summaries(chat_settings, inbox, now):
    return Summaries(chat_settings, inbox, FakeLLM(), now=now)


@pytest.fixture
def topic(forum, store, inbox, chat_settings, summaries, now):
    return SummaryTopic(forum, store, inbox, chat_settings, summaries, Settings(store), OWNER, FORUM, now=now)


def add(inbox, now, msg_id, text="", ago=dt.timedelta(hours=1), chat_id=CHAT, **extra):
    inbox.add(chat_id, msg_id, {"sender_id": 7, "sender_name": "Староста", "at": now() - ago, "text": text, **extra})


# --- the period ---------------------------------------------------------------------------

def test_periods(now):
    moment = now()  # Tue 2026-10-20 12:00 in Moscow
    three = parse_period("3h", moment)
    assert (three.start, three.end, three.label) == (moment - dt.timedelta(hours=3), moment, "3h")
    assert parse_period("2д", moment).label == "2d" and parse_period("5ч", moment).label == "5h"
    days = parse_period("18.10-19.10", moment)
    assert days.start == clock.day_start_utc(dt.date(2026, 10, 18))
    assert days.end == clock.day_start_utc(dt.date(2026, 10, 20)) and days.label == "18.10-19.10"
    assert parse_period("20.10", moment).label == "20.10"
    assert parse_period("25.12", moment).start == clock.day_start_utc(dt.date(2025, 12, 25))  # later: last year
    assert parse_period("Матан", moment) is None
    for wrong in ("8d", "200h", "0h", "10.10-19.10", "19.10-18.10", "31.02"):
        with pytest.raises(PeriodError):
            parse_period(wrong, moment)
    assert split_args("Матан 2026 18.10 - 19.10", moment)[0] == "Матан 2026"
    assert split_args("Матан 2026 3h", moment)[1].label == "3h"
    assert split_args("Матан 2026", moment) == ("Матан 2026", None, None)
    assert split_args("", moment) == ("", None, None)
    # the language at the end, before or after the period (D41)
    query, period, lang = split_args("Матан 2026 18.10 - 19.10 EN", moment)
    assert (query, period.label, lang) == ("Матан 2026", "18.10-19.10", "en")
    query, period, lang = split_args("Матан 2026 ru 3h", moment)
    assert (query, period.label, lang) == ("Матан 2026", "3h", "ru")
    assert split_args("ru", moment) == ("", None, "ru")
    assert split_args("English club", moment) == ("English club", None, None)


def test_summary_time_setting(store):
    settings = Settings(store)
    assert settings.get("summary_time") == "21:00"
    assert settings.set("summary_time", "9:30") == "09:30"
    for wrong in ("24:00", "21", "21:60", 2100):
        with pytest.raises(SettingsError):
            settings.set("summary_time", wrong)


def test_asked_calls(budget, journal, now):
    spend(journal, 0.45, now())  # 90 % of the limit: optional tasks are off
    assert budget.level() is Level.SOFT
    with pytest.raises(BudgetExceeded):
        budget.reserve("summary", 0.01)
    budget.release(budget.reserve("summary", 0.01, asked=True))  # /summary runs above 80 %
    # One too big for what is left is refused alone: the batches go on (a required call would stop them all).
    with pytest.raises(DoesNotFit):
        budget.reserve("summary", 0.10, asked=True)
    assert budget.level() is Level.SOFT
    budget.release(budget.reserve("batch", 0.01))


# --- which messages -----------------------------------------------------------------------

def test_collect_since_the_last_summary(summaries, inbox, chat_settings, now):
    add(inbox, now, 1, "КР перенесли на четверг", ago=dt.timedelta(days=8))  # older than 7 days
    add(inbox, now, 2, "КР перенесли на четверг", ago=dt.timedelta(days=2))
    add(inbox, now, 3, "", files=[{"kind": "sticker", "name": None}])        # nothing to read
    add(inbox, now, 4, "удалю")
    inbox.delete([4], CHAT, now())
    add(inbox, now, 5, "спасибо", ago=dt.timedelta(minutes=5), sender_id=8)
    collected = summaries.collect(CHAT, None)
    assert [m["msg_id"] for m in collected.seen] == [2, 3, 5]
    assert [m["msg_id"] for m in collected.chosen] == [2, 5]
    assert collected.fresh and collected.start == dt.datetime(2026, 10, 18, 12, 0)
    assert span(collected) == "18.10 12:00 – 20.10 12:00"

    summaries.mark(collected)
    assert summaries.collect(CHAT, None).seen == []
    now.moment += dt.timedelta(minutes=30)
    add(inbox, now, 6, "новое")
    assert [m["msg_id"] for m in summaries.collect(CHAT, None).seen] == [6]
    # A period asked for looks back at the marked ones too, and marks nothing.
    asked = summaries.collect(CHAT, parse_period("3d", now()))
    assert [m["msg_id"] for m in asked.chosen] == [2, 6, 5] and not asked.fresh
    summaries.mark(asked)
    assert [m["msg_id"] for m in summaries.collect(CHAT, None).seen] == [6]

    # The `important` filter: listed people, dates, and the replies to them.
    chat_settings.update(CHAT, {"filter": "important", "people": [9]})
    add(inbox, now, 7, "Всем привет", sender_id=9)
    add(inbox, now, 8, "а можно вопрос", sender_id=8)
    add(inbox, now, 9, "ок", sender_id=8, reply_to=7)
    assert [m["msg_id"] for m in summaries.collect(CHAT, None).chosen] == [7, 9]


async def test_summary_in_one_call(summaries, inbox, now):
    add(inbox, now, 1, "Завтрашний семинар по матану переносится на четверг", sender_id=9, pinned=True)
    add(inbox, now, 2, "а во сколько?", sender_id=8, reply_to=1, ago=dt.timedelta(minutes=30))
    summaries.chat_settings.update(CHAT, {"people": [9]})
    summaries.llm.answers = [points(("Семинар по матану перенесли на четверг, 22.10.", 1), ("  ", 2),
                                    ("Спросили, во сколько.", 99))]
    collected = summaries.collect(CHAT, None)
    result = await summaries.summarize(collected, asked=False, lang="ru")
    assert result == [{"text": "Семинар по матану перенесли на четверг, 22.10.", "msg_id": 1},
                      {"text": "Спросили, во сколько.", "msg_id": None}]  # 99 is none of the messages
    [call] = summaries.llm.calls
    assert not call["asked"] and call["chat_id"] == CHAT
    assert "1-2 sentences in Russian" in call["system"] and '"в пятницу, 23.10"' in call["system"]
    prompt = call["prompt"]
    assert 'Chat: "Матан 2026" (group, study)' in prompt and "Task: the summary of the period." in prompt
    assert "- 1 · Староста · listed person · Tue 2026-10-20 11:00 · pinned\n  Завтрашний семинар" in prompt
    assert "- 2 · Староста · Tue 2026-10-20 11:30 · reply to 1\n  а во сколько?" in prompt


async def test_a_long_period_goes_by_days(summaries, inbox, now, monkeypatch):
    monkeypatch.setattr(summaries_module, "CALL_CHARS", 400)  # two messages of 100 characters
    add(inbox, now, 1, "а" * 100, ago=dt.timedelta(days=2))
    add(inbox, now, 2, "б" * 100, ago=dt.timedelta(days=2) - dt.timedelta(minutes=1))
    add(inbox, now, 3, "в" * 100, ago=dt.timedelta(days=2) - dt.timedelta(minutes=2))  # a day in two calls
    add(inbox, now, 4, "г" * 100, ago=dt.timedelta(hours=2))
    summaries.llm.answers = [points(("Первое.", 1)), points(("Третье.", 3)), points(),
                             points(("Итог.", 3), ("Чужой id.", 4))]
    collected = summaries.collect(CHAT, parse_period("3d", now()))
    assert await summaries.summarize(collected, asked=True, lang="en") == [
        {"text": "Итог.", "msg_id": 3}, {"text": "Чужой id.", "msg_id": None}]  # 4 gave no point of a digest
    calls = summaries.llm.calls
    assert len(calls) == 4 and all(call["asked"] and "1-2 sentences in English" in call["system"] for call in calls)
    assert "Task: the digest of one day" in calls[0]["prompt"] and "- 1 · " in calls[0]["prompt"]
    assert "- 3 · " in calls[1]["prompt"] and "- 4 · " in calls[2]["prompt"]
    merge = calls[3]["prompt"]
    assert "Sun 2026-10-18:\n- 1: Первое.\n- 3: Третье.\nTue 2026-10-20:\n(nothing of substance)" in merge


# --- the daily summary ----------------------------------------------------------------------

async def test_daily(topic, summaries, inbox, forum, now):
    add(inbox, now, 1, "Семинар переносится", ago=dt.timedelta(hours=3))
    add(inbox, now, 2, "мемчик", chat_id=OTHER)
    add(inbox, now, 3, "привет", chat_id=PRIVATE)  # no summary for this chat
    summaries.llm.answers = [points(("Семинар перенесли на четверг & в R405.", 1)), points()]
    await topic.daily()
    assert [call["chat_id"] for call in summaries.llm.calls] == [CHAT, OTHER]  # study chats first
    assert forum.texts("summaries") == [
        "<b>Матан 2026</b> · 20.10, 09:00–12:00\n• Семинар перенесли на четверг &amp; в R405. "
        '<a href="https://t.me/c/1234567890/1">→</a>',
        "Ничего важного: «Алгебра»."]
    assert summaries.collect(CHAT, None).seen == [] and summaries.collect(OTHER, None).seen == []
    assert topic.status()["error"] is None

    await topic.daily()  # nothing new: no calls, no posts
    assert len(summaries.llm.calls) == 2 and len(forum.messages) == 2


async def test_daily_failures(topic, summaries, inbox, forum, now):
    add(inbox, now, 1, "один")
    add(inbox, now, 2, "два", chat_id=OTHER)
    summaries.llm.answers = [BadAnswer("request declined"), LLMError("Anthropic rate limit")]
    await topic.daily()
    assert forum.texts("summaries") == ["Сводка не собрана:\n«Матан 2026»: request declined, её сообщения пропущены\n"
                                        "«Алгебра»: Anthropic rate limit, сообщения войдут в следующую"]
    assert summaries.collect(CHAT, None).seen == []           # the same input would be declined again
    assert len(summaries.collect(OTHER, None).seen) == 1      # waits for the next summary

    summaries.llm.answers = [BudgetExceeded("optional LLM tasks are off")]
    await topic.daily()
    assert forum.texts("summaries")[-1] == LIMIT_SPENT.ru and len(summaries.llm.calls) == 3
    assert len(summaries.collect(OTHER, None).seen) == 1 and topic.status()["error"].ru == "лимит LLM"


async def test_daily_schedule(topic, store, summaries, inbox, forum, now):
    add(inbox, now, 1, "один")
    task = asyncio.create_task(topic.run())
    try:
        await asyncio.sleep(0)
        assert store.get_json(DONE) == "2026-10-19" and forum.messages == {}  # the first start only remembers
        assert topic._seconds_to_slot() == 9 * 3600 + topic_module.SLOT_DELAY

        topic.settings.set("summary_time", "11:30")  # earlier than now: today's slot has come
        topic.reschedule()
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        assert store.get_json(DONE) == "2026-10-20" and len(summaries.llm.calls) == 1

        topic.settings.set("summary_time", "13:00")  # later today: the day is done already
        topic.reschedule()
        await asyncio.sleep(0)
        assert len(summaries.llm.calls) == 1 and topic._seconds_to_slot() == 3600 + topic_module.SLOT_DELAY
    finally:
        task.cancel()


# --- /summary ---------------------------------------------------------------------------------

class Reply:
    def __init__(self, text, buttons):
        self.text, self.buttons, self.edits = text, buttons, []

    async def edit(self, text, **kwargs):
        self.edits.append(text)


class Command:
    """Telethon's NewMessage event for /summary, as much as the handler uses."""

    def __init__(self, text, replied=None):
        self.pattern_match = COMMAND.match(text)
        self.replied, self.is_reply = replied, replied is not None
        self.replies: list[Reply] = []

    async def get_reply_message(self):
        return self.replied

    async def reply(self, text, buttons=None, **kwargs):
        assert kwargs["parse_mode"] == "html"
        self.replies.append(Reply(text, buttons))
        return self.replies[-1]


class Press:
    def __init__(self, data: bytes, sender_id=OWNER):
        self.pattern_match, self.sender_id = BUTTON.match(data), sender_id
        self.answers, self.edits = [], []

    async def answer(self, text="", alert=False):
        self.answers.append(text)

    async def edit(self, text, buttons=None, **kwargs):
        self.edits.append(text)


def buttons(reply: Reply) -> list[tuple[str, bytes]]:
    return [row[0] for row in labels(reply.buttons)]


async def test_command_by_name(topic, summaries, inbox, forum, now):
    add(inbox, now, 1, "Семинар переносится")
    summaries.llm.answers = [points(("Семинар перенесли.", 1))]
    command = Command("/summary@UniBot матан 3h")
    await topic.on_command(command)
    [reply] = command.replies
    assert reply.text == "Собираю сводку «Матан 2026»…"
    [post] = forum.messages
    assert reply.edits == [f'Сводка «Матан 2026» — <a href="https://t.me/c/5555555555/{post}">в теме «Сводки»</a>.']
    assert summaries.llm.calls[0]["asked"]
    assert len(summaries.collect(CHAT, None).seen) == 1  # a period asked for marks nothing

    command = Command("/summary матан")  # since the last summary: marks
    summaries.llm.answers = [points()]
    await topic.on_command(command)
    assert command.replies[0].edits == ["В «Матан 2026» за 20.10, 11:00–12:00 ничего важного (сообщений: 1)."]
    assert summaries.collect(CHAT, None).seen == []
    command = Command("/summary матан")
    await topic.on_command(command)
    assert command.replies[0].edits == ["В «Матан 2026» с прошлой сводки читать нечего."]

    command = Command("/summary матан 8d")
    await topic.on_command(command)
    assert command.replies[0].text == "Промежуток — от 1 часа до 7 дней." and len(summaries.llm.calls) == 2


async def test_command_buttons(topic, summaries, inbox, chat_settings, forum, now):
    command = Command("/summary 2d")
    await topic.on_command(command)
    assert buttons(command.replies[0]) == [  # the daily ones first, study ones before others
        ("Матан 2026", f"sm:{CHAT}:2d".encode()), ("Алгебра", f"sm:{OTHER}:2d".encode()),
        ("Петя", f"sm:{PRIVATE}:2d".encode())]
    command = Command("/summary а")
    await topic.on_command(command)
    assert command.replies[0].text == "Какой из них?" and len(command.replies[0].buttons) == 2
    command = Command("/summary физра")
    await topic.on_command(command)
    assert command.replies[0].text == "Среди включённых чатов нет «физра»." and len(command.replies[0].buttons) == 3

    stranger = Press(f"sm:{OTHER}:2d".encode(), sender_id=7)
    await topic.on_button(stranger)
    assert stranger.answers == ["Нет доступа"] and stranger.edits == []
    add(inbox, now, 1, "ДЗ выложили", chat_id=OTHER, ago=dt.timedelta(days=1))
    summaries.llm.answers = [points(("Выложили ДЗ.", 1))]
    press = Press(f"sm:{OTHER}:2d".encode())
    await topic.on_button(press)
    assert press.edits[0] == "Собираю сводку «Алгебра»…" and "в теме «Сводки»" in press.edits[1]
    assert forum.texts("summaries")[0].startswith("<b>Алгебра</b> · 18.10 12:00 – 20.10 12:00\n• Выложили ДЗ.")

    chat_settings.update(CHAT, {"enabled": False})
    chat_settings.update(OTHER, {"enabled": False})
    chat_settings.update(PRIVATE, {"enabled": False})
    command = Command("/summary")
    await topic.on_command(command)
    assert command.replies[0].text == "Включённых чатов нет: их включают в Mini App."


async def test_command_on_a_forward(topic, summaries, inbox, now):
    add(inbox, now, 1, "Пост канала", chat_id=OTHER)
    add(inbox, now, 2, "Пары не будет", ago=dt.timedelta(hours=2))
    add(inbox, now, 3, "Совсем другое", ago=dt.timedelta(hours=2), chat_id=PRIVATE)
    channel = SimpleNamespace(fwd_from=SimpleNamespace(from_id=types.PeerChannel(9876543210), date=now()),
                              message="Пост канала")
    person = SimpleNamespace(fwd_from=SimpleNamespace(from_id=types.PeerUser(7), date=now() - dt.timedelta(hours=2)),
                             message="Пары не будет")
    assert forwarded_chat(inbox, channel) == OTHER
    assert forwarded_chat(inbox, person) == CHAT  # two chats wrote then: the text tells which
    assert forwarded_chat(inbox, SimpleNamespace(fwd_from=None)) is None
    lost = SimpleNamespace(fwd_from=SimpleNamespace(from_id=None, date=now() - dt.timedelta(days=20)), message="")
    assert forwarded_chat(inbox, lost) is None

    summaries.llm.answers = [points(("Пары не будет.", 2))]
    command = Command("/summary 3h", replied=person)
    await topic.on_command(command)
    assert command.replies[0].text == "Собираю сводку «Матан 2026»…" and summaries.llm.calls[0]["chat_id"] == CHAT
    command = Command("/summary", replied=lost)
    await topic.on_command(command)
    assert command.replies[0].text.startswith("Не нашёл, из какого чата это сообщение")


async def test_long_summary_takes_several_messages(topic, summaries, inbox, forum, now, monkeypatch):
    monkeypatch.setattr(topic_module, "MESSAGE_LIMIT", 120)
    add(inbox, now, 1, "текст")
    summaries.llm.answers = [points(*((f"Пункт номер {n}, довольно длинный.", 1) for n in range(6)))]
    msg_id = await topic.make(CHAT, None)
    texts = forum.texts("summaries")
    assert len(texts) > 1 and texts[0].startswith("<b>Матан 2026</b>") and all(len(t) <= 120 for t in texts)
    assert f"/{min(forum.messages)}\">" in msg_id


async def test_failures_are_told(topic, summaries, inbox, forum, now, monkeypatch):
    add(inbox, now, 1, "один")
    add(inbox, now, 2, "два", chat_id=OTHER)
    summaries.llm.answers = [points(("Первое.", 1)), points(("Второе.", 2))]
    real_post = forum.post

    async def post(topic_key, text, buttons=None, *, html=False, reply_to=None):
        if "Первое" in text:
            raise ConnectionError("Telegram is away")
        return await real_post(topic_key, text, buttons, html=html, reply_to=reply_to)

    monkeypatch.setattr(forum, "post", post)
    await topic.daily()  # one chat's post fails: the next one still gets its summary
    assert forum.texts("summaries") == [
        '<b>Алгебра</b> · 20.10, 11:00–12:00\n• Второе. <a href="https://t.me/c/9876543210/2">→</a>',
        "Сводка не собрана:\n«Матан 2026»: не отправлена (Telegram is away), сообщения войдут в следующую"]
    assert len(summaries.collect(CHAT, None).seen) == 1 and summaries.collect(OTHER, None).seen == []

    summaries.llm.answers = [points(("Первое.", 1))]
    command = Command("/summary матан")
    await topic.on_command(command)  # the answer never stays at «Собираю сводку…»
    assert command.replies[0].edits == ["Сводка не собрана: Telegram is away."]

    summaries.llm.answers = [DoesNotFit("too big")]
    command = Command("/summary матан 7d")
    await topic.on_command(command)
    assert command.replies[0].edits == [
        "Сводка не собрана: не хватает остатка дневного лимита LLM. Промежуток короче может поместиться."]


async def test_command_details(topic, summaries, inbox, chat_settings, forum, now):
    forum.username = "UniBot"
    command = Command("/summary@OtherBot матан")
    await topic.on_command(command)
    assert command.replies == []  # another bot's command
    command = Command("/summary@unibot <b>*x*</b>")
    await topic.on_command(command)
    assert command.replies[0].text == "Среди включённых чатов нет «&lt;b&gt;*x*&lt;/b&gt;»."
    inbox.put_chat(CHAT, "Матан <2026>", "supergroup", None, 0)
    add(inbox, now, 1, "текст")
    command = Command("/summary матан")
    await topic.on_command(command)
    assert command.replies[0].text == "Собираю сводку «Матан &lt;2026&gt;»…"
    assert command.replies[0].edits == ["В «Матан &lt;2026&gt;» за 20.10, 11:00–12:00 ничего важного (сообщений: 1)."]


def test_point_length():
    [point] = summaries_module.check({"points": [{"text": "слово " * 200, "msg_id": 1}]}, {1}, 12)
    assert len(point["text"]) == summaries_module.POINT_CHARS and point["text"].endswith("…")


async def test_languages(topic, summaries, inbox, forum, now):
    """The bot speaks the forum's language, the summary is in the one /summary names, else in the forum's (D41)."""
    add(inbox, now, 1, "Семинар переносится")
    summaries.llm.answers = [points(("Seminar moved.", 1))]
    command = Command("/summary матан 3h en")  # the forum is Russian, the summary English
    await topic.on_command(command)
    assert command.replies[0].text == "Собираю сводку «Матан 2026»…"
    assert "1-2 sentences in English" in summaries.llm.calls[-1]["system"]
    assert "• Seminar moved." in forum.texts("summaries")[-1]

    command = Command("/summary 2d ru")
    await topic.on_command(command)
    assert buttons(command.replies[0])[0] == ("Матан 2026", f"sm:{CHAT}:2d:ru".encode())
    assert BUTTON.match(f"sm:{CHAT}:2d:ru".encode()) and BUTTON.match(f"sm:{CHAT}::en".encode())
    assert not BUTTON.match(f"sm:{CHAT}:2d:de".encode())

    forum.lang = "en"  # the forum switched: the bot and the daily summary follow
    press = Press(f"sm:{CHAT}:2d:ru".encode())
    summaries.llm.answers = [points(("Семинар перенесли.", 1))]
    await topic.on_button(press)
    assert press.edits[0] == "Making the summary of «Матан 2026»…"
    assert press.edits[1].startswith("The summary of «Матан 2026» is <a href=")
    assert "1-2 sentences in Russian" in summaries.llm.calls[-1]["system"]
    summaries.llm.answers = [points(("Seminar moved.", 1))]
    await topic.daily()
    assert "1-2 sentences in English" in summaries.llm.calls[-1]["system"]
    command = Command("/summary матан 8d")
    await topic.on_command(command)
    assert command.replies[0].text == "A period is 1 hour to 7 days."
