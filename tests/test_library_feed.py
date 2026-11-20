"""What feeds the library (D58): materials the batches find in study chats, and the owner's messages to the bot. The
sheets' part is in test_sheets.py."""

import datetime as dt
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from telethon.tl import types

from infomaster import clock
from infomaster.collectors.files import NotNow
from infomaster.library import MAX_FILE, Library
from infomaster.library.feed import ChatMaterials, file_name, first_url, from_bot
from infomaster.outputs.bot import Bot
from infomaster.processing.batches import SCHEMA, STUDY_SCHEMA, Batches, Changes, check
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings

CHAT, OTHER = -1001, -1002
OWNER = 42


class FakeLLM:
    def __init__(self):
        self.answers, self.prompts, self.schemas = [], [], []

    async def json(self, task, system, prompt, schema, *, max_tokens, chat_id, documents=()):
        self.prompts.append(prompt)
        self.schemas.append(schema)
        return self.answers.pop(0)


class Twin:
    """The twin's downloads: bytes by message; offline raises NotNow, a broken file another error."""

    def __init__(self):
        self.remote, self.offline, self.broken, self.calls = {}, False, set(), []

    async def download(self, chat_id, msg_id, path):
        self.calls.append(msg_id)
        if self.offline:
            raise NotNow("offline")
        if msg_id in self.broken:
            raise RuntimeError("broken file")
        if (chat_id, msg_id) not in self.remote:
            return False
        path.write_bytes(self.remote[chat_id, msg_id])
        return True


@pytest.fixture
def library(store, tmp_path, now):
    with store.db:
        store.db.execute("insert into classes(uid, subject, start, end, kind, room, seen_at) "
                         "values('calc-s', 'Calculus', '2026-10-20T10:00:00', '2026-10-20T11:20:00', 'Семинар', '', "
                         "'2026-10-20T10:00:00')")
    library = Library(store, tmp_path, now=now)
    library.same_course = lambda a, b: bool(a and b and (a.casefold() == b.casefold()
                                                         or {a, b} == {"Математический анализ", "Calculus"}))
    return library


@pytest.fixture
def twin():
    return Twin()


@pytest.fixture
def batches(store, now, schedule, library, twin):
    inbox, chat_settings = schedule.inbox, ChatSettings(store)
    inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    inbox.put_chat(OTHER, "Флуд", "supergroup", None, 0)
    chat_settings.update(CHAT, {"enabled": True, "study": True})
    chat_settings.update(OTHER, {"enabled": True, "study": False})
    batches = Batches(store, Settings(store), chat_settings, inbox, News(store), schedule.deadlines, schedule,
                      FakeLLM(), now=now)
    batches.materials = ChatMaterials(store, library, inbox, now=now)
    batches.materials.download = twin.download
    return batches


def add(batches, msg_id, text="", chat_id=CHAT, **extra):
    batches.inbox.add(chat_id, msg_id, {"sender_id": 7, "sender_name": "Студентка А", "text": text,
                                        "at": batches.now() - dt.timedelta(minutes=30), **extra})


def photo(size=5000):
    return [{"kind": "photo", "mime": "image/jpeg", "size": size}]


def material(msg_id, course="Математический анализ", kind="photo", title="Calc seminar board"):
    return {"msg_id": msg_id, "course": course, "kind": kind, "title": title}


def quiet(msg_ids, materials=()):
    """An item that only carries material: no news."""
    return {"msg_ids": msg_ids, "news": {"full": "", "short": "", "importance": 1, "replaces": ""}, "deadlines": [],
            "events": [], "sheets": [], "materials": list(materials)}


def test_helpers():
    assert first_url("Запись: https://disk.example.org/rec1. Смотрите") == "https://disk.example.org/rec1"
    assert first_url("слайды (https://slides.example.org/a)") == "https://slides.example.org/a"
    assert first_url("без ссылок") is None
    assert file_name({"kind": "photo", "mime": "image/jpeg"}, 5) == "photo-5.jpg"
    assert file_name({"kind": "document", "name": "notes.pdf"}, 5) == "notes.pdf"
    answer = {"items": [quiet([1], [material(1), material(1, kind="odd"), material(9)]),
                        {**quiet([2], [material(2)]), "sheets": [{"msg_id": 2, "course": "", "number": "1",
                                                                   "due": ""}]}]}
    first, sheet = check(answer, {1, 2}, {2}, materials=True)
    assert first["news"] is None and first["materials"] == [material(1, kind="file")]  # one per message
    assert sheet["materials"] == []  # message 2 is a sheet: no material
    [sheet] = check(answer, {1, 2}, {2})  # not asked for: none
    assert sheet["msg_ids"] == [2] and sheet["materials"] == []


async def test_batch_materials(batches, library, twin, store):
    add(batches, 1, "Доска с семинара", files=photo(), grouped_id=77)
    add(batches, 2, files=photo(), grouped_id=77)                     # the album's other photo, without text
    add(batches, 3, files=[{"kind": "voice", "mime": "audio/ogg", "size": 300}])
    add(batches, 4, "Запись лекции: https://disk.example.org/rec1.")
    add(batches, 5, "Конспект: признак Коши, примеры 1-3")
    add(batches, 6, files=[{"kind": "sticker", "mime": "image/webp", "size": 10}])  # nothing for the model
    add(batches, 7, files=photo(MAX_FILE + 1))
    twin.remote.update({(CHAT, 1): b"board-1", (CHAT, 2): b"board-2", (CHAT, 3): b"voice"})
    twin.offline = True
    batches.llm.answers = [{"items": [
        quiet([1, 2], [material(1), material(2)]),
        quiet([3], [material(3, course="", kind="audio", title="Lecture recording")]),
        quiet([4], [material(4, kind="link", title="Calc lecture recording")]),
        quiet([5], [material(5, kind="notes", title="Calc seminar notes")]),
        quiet([7], [material(7)]),
    ]}]
    changes = await batches.run_chats([CHAT])
    await batches.materials.settle()

    prompt = batches.llm.prompts[0]
    assert batches.llm.schemas == [STUDY_SCHEMA]
    assert "- 2 · Студентка А · Tue 2026-10-20 11:30 · album with 1 · files: photo\n  (no text)" in prompt
    assert "files: voice" in prompt and "- 6 ·" not in prompt
    assert changes.news == [] and batches.inbox.pending(CHAT) == []

    # The twin is offline: the link and the note are in, the files wait in the queue.
    link, note = sorted(library.items(), key=lambda i: i["msg_id"])
    assert (link["kind"], link["url"], link["course"], link["class_uid"], link["title"]) == (
        "link", "https://disk.example.org/rec1", "Calculus", "calc-s", "Calc lecture recording")
    assert (link["source"], link["chat_id"], link["author"], link["day"]) == ("chat", CHAT, "Студентка А", "2026-10-20")
    assert (note["kind"], note["text"]) == ("text", "Конспект: признак Коши, примеры 1-3")
    assert batches.materials.waiting() == 3 and twin.calls == [1]  # one try, the others not asked; 7 is too big

    twin.offline = False
    assert await batches.materials.drain() == 3
    by_msg = {i["msg_id"]: i for i in library.items()}
    assert set(by_msg) == {1, 2, 3, 4, 5}  # 7 is too big: skipped, not downloaded
    board = by_msg[1]
    assert (board["kind"], board["course"], board["class_uid"], board["text"], board["name"]) == (
        "photo", "Calculus", "calc-s", "Доска с семинара", "photo-1.jpg")
    assert library.path(by_msg[2]).read_bytes() == b"board-2"
    voice = by_msg[3]
    assert (voice["kind"], voice["course"], voice["media_type"]) == ("audio", None, "audio/ogg")
    assert voice["note"] and library.inbox_count() == 1
    assert batches.materials.waiting() == 0 and 7 not in twin.calls
    assert await batches.materials.drain() == 0

    # An edited message told again is not taken twice; a deleted one is not taken at all.
    batches.inbox.edit(CHAT, 5, "Конспект: признак Коши, примеры 1-4", [], batches.now())
    add(batches, 8, files=photo())
    batches.inbox.delete([8], CHAT, batches.now())
    batches.materials.queue(CHAT, [material(5, kind="notes"), material(8)], batches.now())
    assert await batches.materials.drain() == 0 and batches.materials.waiting() == 0
    assert len(library.items()) == 5


async def test_failures_are_tried_three_times(batches, library, twin):
    add(batches, 1, files=photo())
    twin.broken.add(1)
    with batches.db:
        batches.materials.queue(CHAT, [material(1)], batches.now())
    for _ in range(2):
        assert await batches.materials.drain() == 0 and batches.materials.waiting() == 1
    assert await batches.materials.drain() == 0 and batches.materials.waiting() == 0
    assert twin.calls == [1, 1, 1] and library.items() == []


async def test_other_chats_are_not_asked(batches, library):
    add(batches, 1, "мем", chat_id=OTHER)
    add(batches, 2, files=photo(), chat_id=OTHER)  # a photo alone outside a study chat: skipped as before
    batches.llm.answers = [{"items": [quiet([1], [material(1)])]}]
    await batches.parse_chat(OTHER, Changes())
    assert batches.llm.schemas == [SCHEMA] and "- 2 ·" not in batches.llm.prompts[0]
    assert batches.materials.waiting() == 0

    batches.materials = None  # without the library a study chat is not asked either
    add(batches, 3, files=photo())
    add(batches, 4, "доска")
    batches.llm.answers = [{"items": []}]
    await batches.parse_chat(CHAT, Changes())
    new = batches.llm.prompts[-1].split("New messages:\n")[1]  # 3, skipped, is an earlier message now
    assert batches.llm.schemas[-1] == SCHEMA and "- 3 ·" not in new


# --- the bot -----------------------------------------------------------------------------------------

@pytest.fixture
def bot(library, tmp_path):
    env = SimpleNamespace(api_id=1, api_hash="0" * 32, owner_id=OWNER, public_url="https://example.test")
    lang = ["en"]
    bot = Bot(env, session=tmp_path / "bot", lang=lambda: lang[0])
    bot.on_material = lambda message, download: from_bot(library, message, download)
    bot.language = lang
    return bot


def private(msg_id, text="", media=None, sender=OWNER, data=b"", **extra) -> SimpleNamespace:
    """A private message to the bot, as Telethon's NewMessage event gives it."""
    message = types.Message(id=msg_id, peer_id=types.PeerUser(sender), date=clock.now_utc(), message=text,
                            media=media, **extra)

    async def download_media(file):
        Path(file).write_bytes(data)
        return file

    message.download_media = download_media
    return SimpleNamespace(sender_id=sender, chat_id=sender, raw_text=text, message=message, respond=AsyncMock())


def document(mime, name=None, size=2048, *attributes):
    attributes = [*attributes, *([types.DocumentAttributeFilename(name)] if name else [])]
    return types.MessageMediaDocument(document=types.Document(
        id=1, access_hash=1, file_reference=b"", date=None, mime_type=mime, size=size, dc_id=2, attributes=attributes))


def picture():
    return types.MessageMediaPhoto(photo=types.Photo(id=1, access_hash=1, file_reference=b"", date=None, dc_id=2,
                                                     sizes=[types.PhotoSize(type="y", w=1280, h=960, size=4000)]))


async def test_bot_takes_the_owners_messages(bot, library):
    forwarded = private(1, "Lecture 4 slides", document("application/pdf", "lec4.pdf"), data=b"%PDF-slides",
                        fwd_from=types.MessageFwdHeader(date=clock.now_utc(), from_name="Classmate B"))
    await bot._on_private(forwarded)
    [item] = library.items(inbox=True)
    assert (item["source"], item["chat_id"], item["msg_id"], item["kind"], item["author"], item["title"]) == (
        "bot", OWNER, 1, "file", "Classmate B", "Lecture 4 slides")
    assert library.path(item).read_bytes() == b"%PDF-slides" and item["name"] == "lec4.pdf"
    forwarded.respond.assert_awaited_once_with("Saved to the inbox.", silent=True)

    # An album: an item per photo, one answer.
    album = [private(i, "доска" if i == 2 else "", picture(), data=f"photo-{i}".encode(), grouped_id=55)
             for i in (2, 3)]
    for event in album:
        await bot._on_private(event)
    assert album[0].respond.await_count == 1 and album[1].respond.await_count == 0
    photos = library.items(kind="photo")
    assert len(photos) == 2 and {p["author"] for p in photos} == {"owner"}

    voice = private(4, media=document("audio/ogg", None, 300, types.DocumentAttributeAudio(duration=3, voice=True)),
                    data=b"ogg")
    await bot._on_private(voice)
    assert library.items(kind="audio")[0]["media_type"] == "audio/ogg"

    bot.language[0] = "ru"
    link = private(5, "Запись семинара https://video.example.org/s5")
    await bot._on_private(link)
    assert library.items(kind="link")[0]["url"] == "https://video.example.org/s5"
    link.respond.assert_awaited_once_with("Сохранено во входящие.", silent=True)
    await bot._on_private(private(6, "Заметка: на экзамене будет признак Даламбера"))
    assert library.items(kind="text")[0]["text"] == "Заметка: на экзамене будет признак Даламбера"
    await bot._on_private(link)  # the same message again: taken once, no answer
    assert link.respond.await_count == 1 and len(library.items()) == 6

    big = private(7, media=document("video/mp4", "rec.mp4", MAX_FILE + 1))
    await bot._on_private(big)
    assert "Не сохранено" in big.respond.await_args.args[0] and len(library.items()) == 6

    for ignored in (private(8, "/start"), private(9, "чужое", sender=7),
                    private(10, media=document("image/webp", None, 10, types.DocumentAttributeSticker(
                        alt="x", stickerset=types.InputStickerSetEmpty())))):
        await bot._on_private(ignored)
        ignored.respond.assert_not_awaited()
    assert len(library.items()) == 6


async def test_start_answers_with_the_web_app(bot):
    """D69: the bot has no forum and no Mini App: /start, an old link's too, gives the web app's address."""
    bot.client = AsyncMock()  # the owner's menu button is reset
    for sender, text in ((OWNER, "/start"), (OWNER, "/start ev-0123456789ab"), (99, "/start")):
        event = SimpleNamespace(sender_id=sender, raw_text=text, respond=AsyncMock(), get_input_sender=AsyncMock())
        await bot._on_start(event)
        if sender != OWNER:
            event.respond.assert_not_awaited()
            continue
        text, = event.respond.await_args.args
        [button] = event.respond.await_args.kwargs["buttons"]
        assert text == "infomaster is a web app: https://example.test/web/"
        assert button.type.url == "https://example.test/web/"
