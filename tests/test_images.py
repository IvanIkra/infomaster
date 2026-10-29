"""Images (D12): the image of a message fetched for its batch, read by Haiku within the daily number and the budget,
what was read in the batch's prompt and in the summaries; the daily number on the Mini App's «Чаты»."""

import datetime as dt
from types import SimpleNamespace

import pytest
from conftest import spend
from test_http import OWNER, env, init_data

from infomaster.collectors import files as files_module
from infomaster.collectors.files import Files, NotNow, image_file, media_type
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.llm.budget import IMAGE_TOKENS, BudgetExceeded
from infomaster.llm.client import BadAnswer, Haiku, Image, LLMError, Rejected
from infomaster.outputs import miniapp
from infomaster.processing.batches import Batches, Changes
from infomaster.processing.images import Images
from infomaster.processing.news import News
from infomaster.processing.summaries import Summaries
from infomaster.settings import ChatSettings, Settings

CHAT = -1001234567890
JPEG = b"\xff\xd8\xff\xe0\x00\x10JFIF" + b"timetable" * 20
PNG = b"\x89PNG\r\n\x1a\n" + b"notice" * 20
HEIC = b"\x00\x00\x00\x18ftypheic" + b"x" * 40
PHOTO = {"kind": "photo", "mime": "image/jpeg"}
TIMETABLE = {"shows": "Screenshot of the Calculus timetable", "text": "Пн 26.10 | 9:30 | R405\nКР-1 30.10 18:10"}


class FakeLLM:
    """Haiku behind the real budget and journal: an image call is logged like a real one (the daily number counts
    them); answers come in order, the batch's and the images' from their own lists."""

    def __init__(self, budget):
        self.budget = budget
        self.answers, self.readings, self.calls = [], [], []

    async def json(self, task, system, prompt, schema, *, max_tokens, chat_id=None, documents=(), images=()):
        ticket = self.budget.reserve(task, 0.0001)
        try:
            self.calls.append(SimpleNamespace(task=task, prompt=prompt, images=list(images)))
            spend(self.budget.journal, 0.0, self.budget.now(), task=task, chat_id=chat_id)
            answer = (self.answers if task == "batch" else self.readings).pop(0)
            if isinstance(answer, Exception):
                raise answer
            return answer
        finally:
            self.budget.release(ticket)

    def tasks(self):
        return [call.task for call in self.calls]


@pytest.fixture
def files(store, tmp_path):
    files = Files(store, tmp_path)
    files.remote, files.downloads, files.offline = {}, [], False

    async def download(chat_id, msg_id, path):
        files.downloads.append(msg_id)
        if files.offline:
            raise NotNow("the twin is not connected to Telegram")
        if (chat_id, msg_id) not in files.remote:
            return False
        path.write_bytes(files.remote[chat_id, msg_id])
        return True

    files.download = download
    return files


@pytest.fixture
def llm(budget):
    return FakeLLM(budget)


@pytest.fixture
def images(store, budget, files, llm, now):
    return Images(Settings(store), budget, files, llm, now=now)


@pytest.fixture
def batches(store, now, schedule, files, images, llm):
    chat_settings = ChatSettings(store)
    schedule.inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    chat_settings.update(CHAT, {"enabled": True, "study": True, "images": True})
    batches = Batches(store, Settings(store), chat_settings, schedule.inbox, News(store), schedule.deadlines,
                      schedule, llm, now=now)
    batches.files, batches.images = files, images
    return batches


def send(batches, msg_id, data: bytes | None, text="", meta=PHOTO, ago=dt.timedelta(minutes=30)):
    """A message of the chat with an image the twin can download."""
    files = []
    if data is not None:
        batches.files.remote[CHAT, msg_id] = data
        files = [{**meta, "size": len(data)}]
    batches.inbox.add(CHAT, msg_id, {"sender_id": 7, "sender_name": "Староста", "at": batches.now() - ago,
                                     "text": text, "files": files})


def news_item(msg_id, full="Calc KR-1 Oct 30 18:10"):
    return {"msg_ids": [msg_id], "news": {"full": full, "short": "Calc KR-1 Oct 30", "importance": 5,
                                          "replaces": ""}, "deadlines": [], "events": [], "sheets": []}


async def parse(batches) -> Changes:
    changes = Changes()
    await batches.parse_chat(CHAT, changes)
    return changes


def pending(batches) -> list[int]:
    return [m["msg_id"] for m in batches.inbox.pending(CHAT)]


# --- the files --------------------------------------------------------------------------------

def test_what_is_an_image():
    assert media_type(JPEG) == "image/jpeg" and media_type(PNG) == "image/png"
    assert media_type(b"GIF89a....") == "image/gif" and media_type(b"RIFF\x00\x00\x00\x00WEBPVP8 ") == "image/webp"
    assert media_type(HEIC) is None and media_type(b"%PDF-1.4") is None
    assert image_file({"files": [PHOTO]}) == PHOTO
    screenshot = {"kind": "document", "name": "s.png", "mime": "image/png", "size": 10}
    assert image_file({"files": [screenshot]}) == screenshot
    assert image_file({"files": [{"kind": "document", "name": "a.heic", "mime": "image/heic"}]}) is None
    assert image_file({"files": [{"kind": "sticker", "mime": "image/webp"}, {"kind": "gif", "mime": "video/mp4"}]}) \
        is None


async def test_images_are_fetched_once(batches, files, now):
    send(batches, 1, JPEG)
    row = await files.fetch_image(batches.inbox.message(CHAT, 1))
    assert files.downloads == [1] and row["kind"] == "image" and row["reading"] is None
    assert row["path"] == f"files/{row['sha256']}.jpg" and files.path(row).read_bytes() == JPEG
    assert (await files.fetch_image(batches.inbox.message(CHAT, 1)))["id"] == row["id"] and files.downloads == [1]
    assert await files.fetch(batches.inbox.message(CHAT, 1)) is None  # no PDF in it

    send(batches, 2, HEIC, meta={"kind": "document", "name": "a.png", "mime": "image/png"})  # a lie in its name
    failed = await files.fetch_image(batches.inbox.message(CHAT, 2))
    assert failed["sha256"] is None and "not a JPEG" in failed["error"]
    batches.inbox.add(CHAT, 3, {"at": now(), "text": "", "files": [{**PHOTO, "size": files_module.IMAGE_MAX_SIZE + 1}]})
    assert await files.fetch_image(batches.inbox.message(CHAT, 3)) is None  # too big for the model: not downloaded

    send(batches, 4, PNG, meta={"kind": "document", "name": "notice.png", "mime": "image/png"})
    png = await files.fetch_image(batches.inbox.message(CHAT, 4))
    assert png["path"].endswith(".png")
    assert files.prune(now() + dt.timedelta(days=15)) == 3
    assert list(files.directory.iterdir()) == []


# --- reading ----------------------------------------------------------------------------------

async def read(batches, images, msg_id):
    message = batches.inbox.message(CHAT, msg_id)
    row = await batches.files.fetch_image(message)
    return await images.read(batches.inbox.chat(CHAT), {"study": True}, message, row)


async def test_an_image_is_read_once(batches, images, llm):
    send(batches, 1, JPEG, text="Расписание на неделю")
    llm.readings = [TIMETABLE]
    assert await read(batches, images, 1) == TIMETABLE
    [call] = llm.calls
    assert call.task == "image" and call.images == [Image(JPEG, "image/jpeg")]
    assert 'Chat: "Матан" (study)' in call.prompt and "The message with the image: Расписание на неделю" in call.prompt
    assert await read(batches, images, 1) == TIMETABLE and len(llm.calls) == 1  # kept: an edit does not read it again

    send(batches, 2, JPEG)  # the same picture forwarded: its reading is taken
    assert await read(batches, images, 2) == TIMETABLE and len(llm.calls) == 1
    assert images.status() == {"ok_at": "2026-10-20T12:00:00", "error": None, "daily": 30, "today": 1}


async def test_the_daily_number_of_images(batches, images, llm, now):
    images.settings.set("images_daily", 2)
    spend(images.budget.journal, 0.0, now() - dt.timedelta(days=1), task="image")  # yesterday's does not count
    spend(images.budget.journal, 0.0, now(), task="image")
    send(batches, 1, JPEG)
    send(batches, 2, PNG)
    llm.readings = [TIMETABLE]
    assert await read(batches, images, 1) == TIMETABLE
    assert await read(batches, images, 2) is None and len(llm.calls) == 1  # 2 of 2 today
    # The row stays unread: an edit tomorrow may read it.
    assert images.files.db.execute("select reading from files where msg_id = 2").fetchone()["reading"] is None


async def test_images_stop_with_the_budget(batches, images, llm, now):
    send(batches, 1, JPEG)
    spend(images.budget.journal, 0.42, now())  # 84 % of $0.50: optional tasks are off
    assert await read(batches, images, 1) is None and llm.calls == []
    spend(images.budget.journal, 0.10, now())  # all of it: the batch waits for midnight with its images
    with pytest.raises(BudgetExceeded):
        await read(batches, images, 1)


async def test_a_failed_reading(batches, images, llm):
    send(batches, 1, JPEG)
    send(batches, 2, PNG)
    send(batches, 3, b"\xff\xd8\xff" + b"other" * 9)
    llm.readings = [BadAnswer("request declined (None)"), Rejected("Anthropic API error 400: bad image"),
                    LLMError("cannot reach the Anthropic API")]
    assert await read(batches, images, 1) is None
    assert await read(batches, images, 2) is None
    assert await read(batches, images, 1) is None and await read(batches, images, 2) is None
    assert len(llm.calls) == 2  # the same image would fail again: not tried twice
    with pytest.raises(LLMError):  # on the way: the message stays in the queue
        await read(batches, images, 3)
    assert images.status()["error"] == "cannot reach the Anthropic API"
    llm.readings = [{"shows": "", "text": ""}]  # nothing on it
    assert await read(batches, images, 3) is None and images.status()["error"] is None


# --- the batch --------------------------------------------------------------------------------

async def test_batch_sees_what_was_read(batches, llm):
    send(batches, 1, JPEG)                                   # a photo alone
    send(batches, 2, PNG, text="кто-нибудь понял?")
    send(batches, 3, b"\xff\xd8\xff" + b"cat" * 9)           # a photo alone with nothing to read on it
    batches.inbox.add(CHAT, 4, {"sender_id": 7, "at": batches.now(), "text": "", "files": [{"kind": "sticker"}]})
    llm.readings = [TIMETABLE, {"shows": "Meme about exams", "text": ""}, {"shows": "", "text": ""}]
    llm.answers = [{"items": [news_item(1)]}]
    changes = await parse(batches)

    assert llm.tasks() == ["image", "image", "image", "batch"]
    prompt = llm.calls[-1].prompt
    assert "- 1 · Староста" in prompt and "- 2 · Староста" in prompt
    assert "- 3 ·" not in prompt and "- 4 ·" not in prompt  # nothing to read: parsed without a call
    assert ("  Image, as another model read it: Screenshot of the Calculus timetable\n  | Пн 26.10 | 9:30 | R405\n"
            "  | КР-1 30.10 18:10") in prompt
    assert "  кто-нибудь понял?\n  Image, as another model read it: Meme about exams" in prompt
    assert len(changes.news) == 1 and pending(batches) == []

    # An edit parses the message again, and its image is not read again.
    batches.inbox.edit(CHAT, 1, "КР перенесли", [{**PHOTO, "size": len(JPEG)}],
                       batches.now() + dt.timedelta(minutes=1))
    llm.answers = [{"items": [news_item(1, "Calc KR-1 moved")]}]
    await parse(batches)
    assert llm.tasks()[4:] == ["batch"] and "Screenshot of the Calculus timetable" in llm.calls[-1].prompt


async def test_without_images_a_photo_alone_is_skipped(batches, llm, images):
    batches.chat_settings.update(CHAT, {"images": False})
    send(batches, 1, JPEG)
    send(batches, 2, JPEG, text="Смотрите расписание")
    llm.answers = [{"items": []}]
    await parse(batches)
    assert llm.tasks() == ["batch"] and batches.files.downloads == []
    assert "- 1 ·" not in llm.calls[0].prompt and "files: photo" in llm.calls[0].prompt
    assert pending(batches) == []

    batches.chat_settings.update(CHAT, {"images": True})
    images.settings.set("images_daily", 0)  # none a day: a photo alone is parsed without a call
    send(batches, 3, PNG)
    await parse(batches)
    assert llm.tasks() == ["batch"] and pending(batches) == []


async def test_a_failed_image_keeps_the_messages_queued(batches, llm, now):
    send(batches, 1, JPEG)
    send(batches, 2, None, text="Завтра пары не будет")
    llm.readings = [LLMError("cannot reach the Anthropic API")]
    with pytest.raises(LLMError):
        await parse(batches)
    assert pending(batches) == [1, 2] and llm.tasks() == ["image"]

    spend(batches.images.budget.journal, 0.60, now())  # the limit is spent: the run waits with its images
    with pytest.raises(BudgetExceeded):
        await parse(batches)
    assert pending(batches) == [1, 2]


async def test_a_file_the_twin_cannot_download_now_waits(batches, llm, files):
    files.offline = True
    send(batches, 1, JPEG)
    send(batches, 2, PNG, text="Расписание")
    send(batches, 3, None, text="Завтра пары не будет")
    llm.answers = [{"items": [news_item(3)]}]
    await parse(batches)
    assert llm.tasks() == ["batch"] and "- 1 ·" not in llm.calls[0].prompt and "- 2 ·" not in llm.calls[0].prompt
    assert pending(batches) == [1, 2]  # nothing is kept of the failure: the next run downloads them
    assert files.db.execute("select count(*) n from files").fetchone()["n"] == 0

    files.offline = False
    llm.readings = [TIMETABLE, {"shows": "Notice", "text": "R405 закрыта"}]
    llm.answers = [{"items": [news_item(1)]}]
    await parse(batches)
    assert llm.tasks()[1:] == ["image", "image", "batch"] and pending(batches) == []


# --- the client and the summaries -------------------------------------------------------------------

async def test_client_sends_images_and_reserves_for_them(budget, journal, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test")
    usage = SimpleNamespace(input_tokens=1500, output_tokens=50, cache_creation_input_tokens=0,
                            cache_read_input_tokens=0)
    response = SimpleNamespace(usage=usage, stop_reason="end_turn", stop_details=None,
                               content=[SimpleNamespace(type="text", text='{"shows": "x", "text": ""}')])
    calls, reserved = [], []

    async def create(**kwargs):
        calls.append(kwargs)
        reserved.append(budget.spent())
        return response

    llm = Haiku(budget, journal)
    llm._client = SimpleNamespace(messages=SimpleNamespace(create=create))
    await llm.json("image", "sys", "prompt", {"type": "object"}, max_tokens=100, images=[Image(PNG, "image/png")])
    [image, text] = calls[0]["messages"][0]["content"]
    assert image["type"] == "image" and image["source"]["media_type"] == "image/png" and text["text"] == "prompt"
    assert reserved[0] >= IMAGE_TOKENS * 0.10 / 1e6  # the image is in the reserve
    assert journal.runs()[0]["task"] == "image"


async def test_summaries_take_what_was_read(store, schedule, files, now, batches, images, llm):
    send(batches, 1, JPEG)
    send(batches, 2, PNG)  # never read: a summary does not read images itself
    llm.readings = [TIMETABLE]
    await read(batches, images, 1)
    summaries = Summaries(batches.chat_settings, batches.inbox, None, now=now)
    assert [m["msg_id"] for m in summaries.collect(CHAT, None).chosen] == []  # without the files: photos alone
    summaries.files = files
    collected = summaries.collect(CHAT, None)
    assert [m["msg_id"] for m in collected.chosen] == [1] and collected.chosen[0]["image"] == TIMETABLE


# --- the Mini App -------------------------------------------------------------------------------

async def test_miniapp_daily_number(aiohttp_client, budget, journal, store, schedule, merged, images):
    settings = images.settings
    hse = HseApp(store, settings, env().public_url + CALLBACK_PATH)
    sources = {"telegram": lambda: {"ok_at": None, "error": None, "login_needed": False}, "images": images.status}
    client = await aiohttp_client(miniapp.create_app(env(), budget, journal, schedule.inbox, ChatSettings(store),
                                                     sources, schedule.subjects, schedule, hse, merged, settings))
    owner = {"Authorization": "tma " + init_data(OWNER)}
    state = await (await client.get("/app/api/chats", headers=owner)).json()
    assert state["images"] == {"ok_at": None, "error": None, "daily": 30, "today": 0}
    response = await client.patch("/app/api/images", json={"daily": 5}, headers=owner)
    assert (await response.json())["daily"] == 5 and settings.get("images_daily") == 5
    assert (await client.patch("/app/api/images", json={"daily": -1}, headers=owner)).status == 400
    assert (await client.patch("/app/api/images", json={"daily": 5, "x": 1}, headers=owner)).status == 400
    assert settings.get("images_daily") == 5
