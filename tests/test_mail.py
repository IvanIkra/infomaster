import datetime as dt
from email.message import EmailMessage
from email.utils import format_datetime

import pytest

from infomaster.collectors.inbox import MAILBOX, Inbox
from infomaster.collectors.mail import NOT_SET, STATE_KEY, Mail, parse
from infomaster.config import Env
from infomaster.processing.batches import Batches, Changes
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings

PASSWORD = "app-password-123"


def env(user: str = "student@edu.hse.ru", password: str = PASSWORD) -> Env:
    return Env(api_id=1, api_hash="h", bot_token="1:t", owner_id=42, forum_id=-100, api_token="tok",
               public_url="https://infomaster.example.ts.net", mail_user=user, mail_password=password)


def letter(subject="ДЗ 3 по алгебре", body="Сдать до пятницы 23:59.", at=None, html=None, files=(),
           sender='"Иванов Пётр" <ivanov@hse.ru>') -> bytes:
    msg = EmailMessage()
    msg["From"] = sender
    msg["Subject"] = subject
    msg["Date"] = format_datetime(at or dt.datetime(2026, 10, 20, 9, 0, tzinfo=dt.UTC))
    msg.set_content(body)
    if html is not None:
        msg.add_alternative(html, subtype="html")
    for name in files:
        msg.add_attachment(b"%PDF-1.4", maintype="application", subtype="pdf", filename=name)
    return msg.as_bytes()


def test_parse_cuts_quotes_and_signature():
    body = ("Добрый день!\nКР переносится на 5 ноября.\n> старая цитата\n-- \nИванов П.\n"
            "18.10.2026, 12:00, \"Студент\" <s@edu.hse.ru>:\nвопрос")
    parsed = parse(letter(body=body, files=["task.pdf"]), uid=7)
    assert parsed.uid == 7 and parsed.sender == "Иванов Пётр" and parsed.subject == "ДЗ 3 по алгебре"
    assert parsed.text == "Добрый день!\nКР переносится на 5 ноября."
    assert parsed.files == ("task.pdf",)
    assert parsed.fields()["files"] == [{"kind": "pdf", "name": "task.pdf", "mime": None, "size": None}]


def test_parse_takes_html_when_the_plain_part_is_empty():
    parsed = parse(letter(body=" ", html="<p>Консультация</p><p>в среду в 18:10, R405</p><style>p{}</style>"))
    assert parsed.text == "Консультация\n\nв среду в 18:10, R405"


@pytest.fixture
def mail(store, now):
    return Mail(env(), store, Settings(store), Inbox(store), now=now)


def test_first_poll_takes_only_the_last_day(mail, now):
    fresh, old = now() - dt.timedelta(hours=2), now() - dt.timedelta(days=3)
    saved = mail.take({}, 555, 12, [(10, letter(at=fresh)), (11, letter(at=old)), (12, b"\x00 broken")], first=True)
    assert saved == 2  # the broken one still makes a message: parse() never gives up on a letter
    assert mail.store.get_json(STATE_KEY) == {"uidvalidity": 555, "last": 12, "base": 0}
    assert mail.inbox.message(MAILBOX, 10, "mail")["subject"] == "ДЗ 3 по алгебре"
    assert mail.inbox.message(MAILBOX, 11, "mail") is None


def test_new_uidvalidity_moves_past_every_msg_id(mail, now):
    mail.take({}, 555, 12, [(12, letter(at=now()))], first=True)
    state = mail.store.get_json(STATE_KEY)
    mail.take(state, 777, 3, [(3, letter(subject="Новый ящик", at=now()))], first=True)
    assert mail.store.get_json(STATE_KEY) == {"uidvalidity": 777, "last": 3, "base": 12}
    assert mail.inbox.message(MAILBOX, 15, "mail")["subject"] == "Новый ящик"
    assert mail.inbox.message(MAILBOX, 12, "mail")["subject"] == "ДЗ 3 по алгебре"


async def test_poll_hides_the_password_and_recovers(mail, now):
    def failing(*args):
        raise OSError(f"login failed for {PASSWORD}")

    told = []
    mail.notify = lambda line: told.append(line.ru)  # a Phrase: the forum picks its language
    mail.fetcher = failing
    assert await mail.poll() == 0
    assert PASSWORD not in mail.status()["error"] and "***" in mail.status()["error"]
    now.moment += dt.timedelta(hours=4)
    await mail.poll()
    assert len(told) == 1 and PASSWORD not in told[0]

    mail.fetcher = lambda *args: (555, 1, [(1, letter(at=now()))])
    assert await mail.poll() == 1
    status = mail.status()
    assert status["error"] is None and status["ok_at"] and status["last"] == 1 and status["letters"] == 1
    assert told[-1] == "Почта снова доступна."


async def test_not_configured(store, now):
    mail = Mail(env(user="", password=""), store, Settings(store), Inbox(store), now=now)
    assert not mail.configured and mail.status()["error"] == NOT_SET
    await mail.run()  # returns at once


class FakeLLM:
    def __init__(self, answers):
        self.answers, self.prompts = answers, []

    async def json(self, task, system, prompt, schema, *, max_tokens, chat_id, documents=()):
        self.prompts.append(prompt)
        return self.answers.pop(0)


async def test_batches_parse_letters_as_study_mail(store, now, schedule, mail):
    mail.take({}, 555, 4, [(4, letter(at=now() - dt.timedelta(days=2)))], first=False)
    llm = FakeLLM([{"items": [{"msg_ids": [4], "news": {"full": "Alg HW 3 due Fri 23:59", "short": "Alg HW 3 → Fri",
                                                       "importance": 5, "replaces": ""},
                               "deadlines": [{"course": "Алгебра", "label": "Alg HW 3", "due": "2026-10-23",
                                              "moves": ""}], "events": []}]}])
    chat_settings = ChatSettings(store)
    batches = Batches(store, Settings(store), chat_settings, schedule.inbox, News(store), schedule.deadlines, schedule,
                      llm, now=now)
    batches.mail = True
    schedule.inbox.put_chat(-1002, "Мемы", "supergroup", None, 0)
    schedule.inbox.put_chat(-1003, "Алгебра", "supergroup", None, 0)
    chat_settings.update(-1002, {"enabled": True})
    chat_settings.update(-1003, {"enabled": True, "study": True})
    assert batches._ordered(batches._boxes()) == [-1003, MAILBOX, -1002]  # D30

    changes = Changes()
    await batches.parse_chat(MAILBOX, changes)
    assert "university mailbox" in llm.prompts[0]
    [news_id] = changes.news  # two days old, still parsed: every letter is study
    news = batches.news.get(news_id)
    assert news["source"] == "mail" and news["study"] and news["chat_id"] == MAILBOX
    [deadline] = [d for d in batches.deadlines.upcoming(now().date()) if d["source"] == "mail"]
    assert deadline["label"] == "Alg HW 3"
    assert schedule.inbox.pending(MAILBOX, "mail") == []
