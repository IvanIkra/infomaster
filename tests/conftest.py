import asyncio
import datetime as dt

import pytest

from uni_master import clock
from uni_master.collectors.inbox import Inbox
from uni_master.collectors.ruz import Ruz
from uni_master.llm.budget import Budget
from uni_master.llm.journal import Journal
from uni_master.processing.deadlines import Deadlines
from uni_master.processing.events import Events
from uni_master.processing.hseapp import HseAppDeadlines
from uni_master.processing.merge import DeadlineList
from uni_master.processing.schedule import Schedule
from uni_master.processing.subjects import Subjects
from uni_master.settings import Settings
from uni_master.store import Store

NAMES = {
    "Математический анализ": ("Calculus", "Calc"),
    "Алгебра": ("Algebra", "Alg"),
    "Algebra": ("Algebra", "Alg"),
    "Discrete Mathematics": ("Discrete Math", "DM"),
    "Introduction into Software Engineering": ("Intro to SE", "SE"),
    "Основы российской государственности": ("Rus Statehood", "RSF"),
    "Foundations of Russian Statehood": ("Rus Statehood", "RSF"),
    "Введение в программирование": ("Intro to Prog", "Prog"),
    "Calculus": ("Calculus", "Calc"),
}


class FakeNamer:
    """Haiku for the dictionary: names the courses it knows from NAMES, or raises `error`."""

    def __init__(self, names=None):
        self.names = dict(NAMES if names is None else names)
        self.prompts, self.error = [], None

    async def json(self, task, system, prompt, schema, *, max_tokens):
        assert task == "subjects"
        self.prompts.append(prompt)
        if self.error is not None:
            raise self.error
        items = []
        for line in prompt.splitlines():
            if line.startswith("- id "):
                index, name = line[5:].split(": ", 1)
                if name.strip('"') in self.names:
                    title, abbr = self.names[name.strip('"')]
                    items.append({"id": index, "title": title, "abbr": abbr})
        return {"items": items}


class Clock:
    """A settable now() for the budget; starts at noon in Moscow."""

    def __init__(self):
        self.moment = dt.datetime(2026, 10, 20, 12, 0, tzinfo=clock.TZ).astimezone(dt.UTC)

    def __call__(self) -> dt.datetime:
        return self.moment


class FakeForum:
    """The bot's forum: posts are kept by id; every post is HTML."""

    def __init__(self):
        self.messages: dict[int, tuple[str, str]] = {}  # msg_id -> (topic, text)
        self.buttons: dict[int, list] = {}
        self.files: dict[int, tuple[bytes, str]] = {}  # msg_id -> (the file's bytes, its name)
        self.replies: dict[int, int] = {}              # msg_id -> the message it answers
        self.pinned, self.edited, self.deleted = [], [], []
        self.next_id = 100
        self.tasks: list[asyncio.Task] = []

    async def post(self, topic, text, buttons=None, *, html=False, reply_to=None):
        assert html
        self.next_id += 1
        self.messages[self.next_id] = (topic, text)
        self.buttons[self.next_id] = buttons
        if reply_to is not None:
            assert self.messages[reply_to][0] == topic
            self.replies[self.next_id] = reply_to
        return self.next_id

    async def post_file(self, topic, path, name, caption):
        msg_id = await self.post(topic, caption, html=True)
        self.files[msg_id] = (path.read_bytes(), name)
        return msg_id

    async def edit_file(self, msg_id, path, name, caption):
        if not await self.edit(msg_id, caption, html=True):
            return False
        self.files[msg_id] = (path.read_bytes(), name)
        return True

    async def edit(self, msg_id, text, *, html=False):
        if msg_id not in self.messages:
            return False
        self.messages[msg_id] = (self.messages[msg_id][0], text)
        self.edited.append(msg_id)
        return True

    async def delete(self, msg_ids):
        for msg_id in msg_ids:
            self.messages.pop(msg_id, None)
            self.files.pop(msg_id, None)
        self.deleted += msg_ids

    async def pin(self, msg_id):
        self.pinned.append(msg_id)

    def texts(self, topic):
        return [text for t, text in self.messages.values() if t == topic]

    def spawn(self, coro):
        self.tasks.append(asyncio.ensure_future(coro))

    async def settle(self):
        """Wait for what was spawned, also what that spawned."""
        while self.tasks:
            tasks, self.tasks = self.tasks, []
            await asyncio.gather(*tasks)


class Press:
    """Telethon's CallbackQuery event, as much of it as the handlers use: the button's data, who, under which post."""

    def __init__(self, pattern, data: bytes, sender_id: int = 42, message_id: int | None = None):
        self.sender_id, self.message_id, self.pattern_match = sender_id, message_id, pattern.match(data)
        self.answers, self.edits = [], []

    async def answer(self, text="", alert=False):
        self.answers.append((text, alert))

    async def edit(self, buttons=None):
        self.edits.append(buttons)


def labels(buttons) -> list[list[tuple[str, bytes]]]:
    return [[(button.text, button.type.data) for button in row] for row in buttons or []]


@pytest.fixture
def forum():
    return FakeForum()


@pytest.fixture
def now():
    return Clock()


@pytest.fixture
def store():
    return Store()


@pytest.fixture
def journal(store):
    return Journal(store)


@pytest.fixture
def budget(store, journal, now):
    budget = Budget(Settings(store), journal, store, now=now)
    budget.notices = []
    budget.notify = budget.notices.append
    return budget


def spend(journal: Journal, usd: float, at: dt.datetime, task: str = "batch", chat_id: int | None = None) -> None:
    journal.add({"at": clock.utc_iso(at), "task": task, "chat_id": chat_id, "model": "claude-haiku-5-5", "ok": True,
                 "error": None, "input_tokens": 1, "output_tokens": 1, "cache_write": 0, "cache_read": 0,
                 "cost_usd": usd, "duration_s": 0.1})


@pytest.fixture
def namer():
    return FakeNamer()


@pytest.fixture
def schedule(store, now, namer):
    """The timetable without a RUZ link: classes come from Ruz._sync in the tests."""
    return Schedule(store, Ruz("", store, now=now), Events(store), Deadlines(store), Subjects(store, namer),
                    Inbox(store), now=now)


@pytest.fixture
def merged(store, now, schedule):
    """The merged deadlines over the schedule's tables and HSE App X's lists (no labels asked: no Haiku)."""
    merged = DeadlineList(store, schedule.deadlines, HseAppDeadlines(store, schedule.subjects, None, now=now),
                          schedule.subjects, now=now)
    merged.changes = []
    merged.on_change = merged.changes.append
    return merged


def make_pdf(pages: list[str]) -> bytes:
    """A PDF with one page per text (ASCII, one line each, Helvetica); "" makes a page without text, like a scan."""
    objects = ["<< /Type /Catalog /Pages 2 0 R >>", None, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"]
    kids = []
    for text in pages:
        lines = [line.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)") for line in text.splitlines()]
        stream = "".join(f"BT /F1 12 Tf 50 {750 - 16 * i} Td ({line}) Tj ET\n" for i, line in enumerate(lines))
        objects.append(f"<< /Length {len(stream)} >>\nstream\n{stream}endstream")
        objects.append(f"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> "
                       f"/Contents {len(objects)} 0 R >>")
        kids.append(f"{len(objects)} 0 R")
    objects[1] = f"<< /Type /Pages /Kids [{' '.join(kids)}] /Count {len(kids)} >>"
    out, offsets = b"%PDF-1.4\n", []
    for number, body in enumerate(objects, 1):
        offsets.append(len(out))
        out += f"{number} 0 obj\n{body}\nendobj\n".encode()
    xref = len(out)
    out += f"xref\n0 {len(objects) + 1}\n0000000000 65535 f \n".encode()
    out += "".join(f"{offset:010d} 00000 n \n" for offset in offsets).encode()
    out += f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode()
    return out
