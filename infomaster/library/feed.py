"""What feeds the library (D58) from the chats and the bot; the sheets add themselves (processing/sheets.py).

Study chats. The batch model marks the messages that carry course material (processing/batches.py, `materials`):
a photo of the board, notes, slides, a recording, a link to one. They are queued in `library_queue` in the batch's
own transaction, so a run that ends after its messages are marked parsed loses none of them, and taken into the
library after it (`drain`), outside any transaction: a recording may be hundreds of megabytes, and the twin streams
it to disk. A file the twin cannot download now (NotNow: it is offline, a flood wait) keeps its row and is tried
again after the next batch run, at most every hour; so is one that fails otherwise, up to MAX_TRIES times. A row
goes when its item is made, when its message is deleted, or when the message leaves the inbox after its 14 days.

The bot. A private message of the owner to the bot (a forward, a file, a link, a note) goes to the library's inbox
as it is: Claude sorts it later.
"""

import asyncio
import datetime as dt
import logging
import mimetypes
import re
from collections.abc import Awaitable, Callable
from pathlib import Path

from .. import clock
from ..collectors.files import NotNow
from ..collectors.inbox import Inbox
from ..store import Store
from . import MAX_FILE, Library, LibraryError

log = logging.getLogger(__name__)

SCHEMA = """
create table if not exists library_queue (  -- materials the batches found, waiting to go into the library (D58)
    chat_id integer not null,
    msg_id integer not null,
    course text,                -- as the model named it; null: not sure, the item goes to the inbox
    kind text not null,         -- what the model took it for: photo, notes, slides, audio, video, link, text, file
    title text not null default '',
    tries integer not null default 0,   -- failures other than NotNow
    created_at text not null,
    primary key (chat_id, msg_id)
);
"""

# Kinds the batch model may give a material (the library's KINDS without sheets, which have their own field, and
# transcripts, which Claude makes).
MATERIAL_KINDS = ("photo", "notes", "slides", "audio", "video", "link", "text", "file")
FILE_KINDS = {"notes", "slides"}  # what the model may tell of a file beyond its type: else the type decides
# Files of a message that can be material: stickers and GIF animations never are.
MEDIA = {"photo", "document", "pdf", "audio", "voice", "video", "video_note"}
MAX_TRIES = 3
URL = re.compile(r"https?://[^\s<>()\"'«»]+")


def material_file(message: dict) -> dict | None:
    """The message's own file that can be material (its `files` entry), if any."""
    return next((f for f in message.get("files") or () if f.get("kind") in MEDIA), None)


def first_url(text: str | None) -> str | None:
    found = URL.search(text or "")
    return found.group(0).rstrip(".,;:!?") if found else None


def file_name(file: dict, msg_id: int) -> str:
    """The file's own name; a photo or a voice message has none: «photo-<msg_id>.jpg»."""
    if file.get("name"):
        return file["name"]
    extension = mimetypes.guess_extension(file.get("mime") or "") or ""
    return f"{file.get('kind') or 'file'}-{msg_id}{extension}"


def moment(at: str) -> dt.datetime:
    """A local ISO time of the database as an aware moment."""
    return dt.datetime.fromisoformat(at).replace(tzinfo=clock.TZ)


class ChatMaterials:
    def __init__(self, store: Store, library: Library, inbox: Inbox,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.db, self.library, self.inbox, self.now = store.db, library, inbox, now
        self.db.executescript(SCHEMA)
        # The twin's download of a message's file into a path (Twin.download): False when it is gone, NotNow when it
        # cannot be done now. None without a twin: files wait.
        self.download: Callable[[int, int, Path], Awaitable[bool]] | None = None
        self._lock = asyncio.Lock()
        self._task: asyncio.Task | None = None

    def queue(self, chat_id: int, materials: list[dict], at: dt.datetime) -> None:
        """Materials of a batch's answer ({msg_id, course, kind, title}); a message told again (an edit) updates its
        row. Call inside the batch's `with store.db`."""
        for material in materials:
            self.db.execute(
                "insert into library_queue(chat_id, msg_id, course, kind, title, created_at) values(?, ?, ?, ?, ?, ?) "
                "on conflict(chat_id, msg_id) do update set course = excluded.course, kind = excluded.kind, "
                "title = excluded.title",
                (chat_id, material["msg_id"], material["course"] or None, material["kind"], material["title"],
                 clock.local_iso(at)))

    def waiting(self) -> int:
        return self.db.execute("select count(*) n from library_queue").fetchone()["n"]

    def kick(self) -> None:
        """Drain the queue in the background (after a batch run), unless a drain is running already."""
        if self._task is None or self._task.done():
            self._task = asyncio.create_task(self._guarded())

    async def settle(self) -> None:
        """Wait for the background drain (tests)."""
        if self._task is not None:
            await self._task

    async def _guarded(self) -> None:
        try:
            await self.drain()
        except Exception:
            log.exception("taking the chats' materials into the library failed")

    async def drain(self) -> int:
        """Every queued material into the library, the oldest first; how many items were made. After the first
        NotNow (the twin cannot download now) the other files wait too, without asking; texts and links go on."""
        made, offline = 0, False
        async with self._lock:
            rows = [dict(row) for row in self.db.execute("select * from library_queue order by created_at, msg_id")]
            for row in rows:
                try:
                    done = await self._take(row, offline)
                except NotNow as e:
                    if not offline:
                        log.info("library materials wait for the twin: %s", e)
                    offline = True
                    continue
                except Exception as e:
                    tries = row["tries"] + 1
                    log.warning("material %s of %s not taken (try %d): %s", row["msg_id"], row["chat_id"], tries, e)
                    if tries < MAX_TRIES:
                        with self.db:
                            self.db.execute("update library_queue set tries = ? where chat_id = ? and msg_id = ?",
                                            (tries, row["chat_id"], row["msg_id"]))
                        continue
                    done = None
                made += done is not None
                with self.db:
                    self.db.execute("delete from library_queue where chat_id = ? and msg_id = ?",
                                    (row["chat_id"], row["msg_id"]))
        return made

    async def _take(self, row: dict, offline: bool = False) -> int | None:
        """One queued material: its item's id, or None when there is nothing to take (the message is gone or deleted,
        it is in the library already, its file is gone or too big). NotNow passes through."""
        chat_id, msg_id = row["chat_id"], row["msg_id"]
        message = self.inbox.message(chat_id, msg_id)
        if message is None or message["deleted_at"] is not None:
            return None
        if self.library.from_origin("chat", chat_id, msg_id) is not None:
            return None
        at = moment(message["at"])
        course = self.library.course(row["course"])
        found = self.library.find_class(course, at) if course else None
        chat = self.inbox.chat(chat_id)
        fields = {"source": "chat", "chat_id": chat_id, "msg_id": msg_id, "course": course,
                  "class_uid": found["uid"] if found else None, "title": row["title"],
                  "author": message["sender_name"] or (chat["title"] if chat else ""), "at": at,
                  "note": "" if course else "the batch could not tell its course"}
        text = message["text"].strip() or None
        file = material_file(message)
        if file is not None:
            if (file.get("size") or 0) > MAX_FILE:
                log.info("material %s of %s skipped: its file is over %d MB", msg_id, chat_id, MAX_FILE // 2**20)
                return None
            if self.download is None or offline:
                raise NotNow("no twin to download the file")
            name = file_name(file, msg_id)
            kind = row["kind"] if row["kind"] in FILE_KINDS else None  # else by the file's type
            try:
                return await self.library.add_download(lambda path: self.download(chat_id, msg_id, path), name=name,
                                                       media_type=file.get("mime"), kind=kind, text=text, **fields)
            except LibraryError as e:
                log.info("material %s of %s skipped: %s", msg_id, chat_id, e)
                return None
        if url := first_url(text):
            return self.library.add("link", url=url, text=text, **fields)
        if text:
            return self.library.add("text", text=text, **fields)
        return None


async def from_bot(library: Library, message: dict, download: Callable[[Path], Awaitable[bool]]) -> int | None:
    """A private message of the owner to the bot, into the library's inbox; the item's id, or None when it carries
    nothing to keep (a sticker) or was taken already. `message`: chat_id, msg_id, at, text, files and forward as
    collectors/telegram.py gives them; `download(path)` streams its file. LibraryError for a file the library refuses
    (too big, empty)."""
    chat_id, msg_id = message["chat_id"], message["msg_id"]
    if library.from_origin("bot", chat_id, msg_id) is not None:
        return None
    text = (message.get("text") or "").strip() or None
    forward = message.get("forward") or {}
    fields = {"source": "bot", "chat_id": chat_id, "msg_id": msg_id, "at": message.get("at"),
              "author": forward.get("from") or ("" if forward else "owner"), "note": "sent to the bot"}
    file = material_file(message)
    if file is not None:
        if (file.get("size") or 0) > MAX_FILE:
            raise LibraryError(f"the file is over {MAX_FILE // 2**20} MB")
        title = " ".join((text or "").split())[:200]
        return await library.add_download(download, name=file_name(file, msg_id), media_type=file.get("mime"),
                                          title=title, text=text, **fields)
    if url := first_url(text):
        return library.add("link", url=url, text=text, **fields)
    if text:
        return library.add("text", text=text, **fields)
    return None
