"""Files of chat messages (§5.1, §6.3, §9): PDFs and images, fetched by the twin when a batch needs them, never ahead.

A batch asks for the PDF of a message it is about to parse, or for its image in a chat with «Картинки» on. The first
time the twin downloads it into `<data>/files/<sha256>.<ext>`, a PDF's first page is read locally, an image's type is
checked, and a row of `files` keeps the result, also a failure of the file itself (it is gone, broken, no image), so a
message edited later does not download the same file again; a message whose file was replaced gets the new one. A
download that fails on the way (the twin is offline, the network, a flood wait) is not kept: the batch leaves the
message in the queue for its next run (NotNow). What Haiku read on an image (processing/images.py)
lives in the same row. Files and rows go with their messages after 14 days (D29); a sheet made of a file keeps its
text (processing/sheets.py), and the file lives on in the forum.

A letter's PDF attachment is kept when the letter arrives (collectors/mail.py), in a row of source `mail`: the mailbox
is not asked again. A message of a study chat or a letter without a PDF of its own may link to one (D59): the batch
asks for it (`fetch_link`), and the row keeps the link it came from. A download that fails for another reason than a
gone or broken file is tried again at the next runs, MAX_TRIES times in all. And the PDFs the owner uploads in the
Mini App as sheets of their own (D44): kept the same way, in rows of source `own` of the chat OWN_CHAT.
"""

import asyncio
import datetime as dt
import hashlib
import json
import logging
import secrets
from collections.abc import Awaitable, Callable
from pathlib import Path

from .. import clock
from ..i18n import Phrase
from ..processing import pdf
from ..store import Store
from .inbox import MAILBOX, RETENTION
from .links import LinkError, document_links

log = logging.getLogger(__name__)

MAX_SIZE = 50 * 1024 * 1024  # bytes: a bigger PDF is a book, not a sheet, and is not downloaded
UPLOAD_MAX_SIZE = 20 * 1024 * 1024  # bytes of a PDF the owner uploads
OWN_CHAT = -1                # the chat of the owner's own sheets: no chat has this id (the mailbox is 0)
IMAGE_MAX_SIZE = 3_750_000   # bytes: in base64 it stays within the API's 5 MB for an image
FIRST_PAGE_CHARS = 4000      # kept of the first page
MAX_TRIES = 3                # failed downloads of one file, at runs of the batches, before it is given up
PDF_TYPES = ("application/pdf", "application/x-pdf", "application/acrobat")

# What the model reads, by the first bytes of the file: the extension it is kept under and its media type.
IMAGE_TYPES = {"image/jpeg": "jpg", "image/png": "png", "image/gif": "gif", "image/webp": "webp"}


class NotNow(RuntimeError):
    """The file cannot be fetched now, but may be later: nothing is kept, the message waits."""


def is_pdf(entry: dict) -> bool:
    """A `files` entry of a PDF: by its kind, or a document whose type or name says PDF (a teacher's client may send
    one as application/octet-stream)."""
    return entry["kind"] == "pdf" or entry["kind"] == "document" and (
        (entry.get("mime") or "").lower() in PDF_TYPES or (entry.get("name") or "").lower().endswith(".pdf"))


def pdf_file(message: dict) -> dict | None:
    """The PDF of a message (its `files` entry), if it has one."""
    return next((f for f in message["files"] if is_pdf(f)), None)


def image_file(message: dict) -> dict | None:
    """The image of a message: a photo, or a picture sent as a file. Stickers and GIF animations are not images."""
    return next((f for f in message["files"]
                 if f["kind"] == "photo" or (f["kind"] == "document" and f.get("mime") in IMAGE_TYPES)), None)


def media_type(data: bytes) -> str | None:
    """The type of an image the model reads, by its signature; None for anything else (HEIC, BMP, not an image)."""
    if data.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if data.startswith((b"GIF87a", b"GIF89a")):
        return "image/gif"
    if data[:4] == b"RIFF" and data[8:12] == b"WEBP":
        return "image/webp"
    return None


def reading(row: dict | None) -> dict | None:
    """What Haiku read on an image: {shows, text}; None while unread or when it could not."""
    return json.loads(row["reading"]) if row and row.get("reading") else None


class Files:
    def __init__(self, store: Store, data_dir: Path):
        self.db, self.data_dir = store.db, data_dir
        self.directory = data_dir / "files"
        # The twin's download of a message's file into a path; False when the message or its file is gone, NotNow when
        # it cannot be done now. None without a twin: nothing is fetched then, and the batch reads the file's name only.
        self.download: Callable[[int, int, Path], Awaitable[bool]] | None = None
        # The PDF behind a link into a path: its name, None when there is none, LinkError when it cannot be read now
        # (collectors/links.fetch). None: links are not followed.
        self.fetch_url: Callable[[str, Path, int], Awaitable[str | None]] | None = None

    def path(self, row: dict) -> Path | None:
        """Where the file is, while it is kept."""
        if not row or not row["path"]:
            return None
        path = self.data_dir / row["path"]
        return path if path.exists() else None

    def by_hash(self, sha256: str) -> Path | None:
        """A PDF by its hash, while it is kept: a sheet finds its file this way even after its message's file was
        replaced."""
        path = self.directory / f"{sha256}.pdf"
        return path if path.exists() else None

    async def fetch(self, message: dict) -> dict | None:
        """The row of the message's PDF: stored, or downloaded and read now. None when the message has no PDF, it is
        too big, or there is no twin; a row with `error` when the file is gone or unreadable. Raises NotNow when the
        download failed on the way."""
        meta = pdf_file(message)
        if meta is None or (meta.get("size") or 0) > MAX_SIZE:
            return None
        return await self._fetch(message, meta, "pdf")

    async def fetch_image(self, message: dict) -> dict | None:
        """The row of the message's image, as `fetch` for a PDF. One too big for the model is not downloaded."""
        meta = image_file(message)
        if meta is None or (meta.get("size") or 0) > IMAGE_MAX_SIZE:
            return None
        return await self._fetch(message, meta, "image")

    def read_as(self, row: dict) -> str | None:
        """The reading of the same image in another message (a forward, a repost): it is not read twice."""
        found = self.db.execute("select reading from files where sha256 = ? and kind = 'image' and reading != '' "
                                "and id != ? limit 1", (row["sha256"], row["id"])).fetchone()
        return found["reading"] if found else None

    def set_reading(self, row: dict, value: str, error: str | None = None) -> dict:
        """Keep what Haiku read on the image of a row ("" when it could not, with the error)."""
        with self.db:
            self.db.execute("update files set reading = ?, error = ? where id = ?", (value, error, row["id"]))
        return {**row, "reading": value, "error": error}

    async def keep(self, data: bytes, name: str, at: dt.datetime) -> dict:
        """A PDF the owner uploads: stored as a downloaded one, its first page read; the row of source `own` goes
        after 14 days like the others. ValueError when it is too big or no PDF that can be read."""
        if len(data) > UPLOAD_MAX_SIZE:
            mb = UPLOAD_MAX_SIZE // 2**20
            raise ValueError(Phrase(f"the file is over {mb} MB", f"файл больше {mb} МБ"))
        self.directory.mkdir(mode=0o700, exist_ok=True)
        part = self.directory / f"own_{secrets.token_hex(6)}.part"
        await asyncio.to_thread(part.write_bytes, data)
        try:
            values = await asyncio.to_thread(self._store, part, "pdf")
        except pdf.PdfError as e:
            raise ValueError(Phrase(f"not a PDF that can be read: {e}",
                                    f"это не PDF или его не прочитать: {e}")) from None
        finally:
            part.unlink(missing_ok=True)
        with self.db:
            msg_id = self.db.execute("select coalesce(max(msg_id), 0) + 1 n from files where source = 'own'"
                                     ).fetchone()["n"]
            self.db.execute("insert into files(source, chat_id, msg_id, at, name, size, sha256, path, pages, "
                            "first_page, error, kind) values('own', ?, ?, ?, ?, ?, ?, ?, ?, ?, null, 'pdf')",
                            (OWN_CHAT, msg_id, clock.local_iso(at), name, len(data), values["sha256"], values["path"],
                             values["pages"], values["first_page"]))
        return dict(self.db.execute("select * from files where source = 'own' and msg_id = ?", (msg_id,)).fetchone())

    def row(self, source: str, chat_id: int, msg_id: int) -> dict | None:
        found = self.db.execute("select * from files where source = ? and chat_id = ? and msg_id = ?",
                                (source, chat_id, msg_id)).fetchone()
        return dict(found) if found else None

    async def _fetch(self, message: dict, meta: dict, kind: str) -> dict | None:
        if message["source"] == "mail":  # kept when the letter came, or never
            row = self.row("mail", message["chat_id"], message["msg_id"])
            return row if row is not None and row["kind"] == kind and not row["url"] else None
        if message["source"] != "tg":
            return None
        chat_id, msg_id = message["chat_id"], message["msg_id"]
        row = self.row("tg", chat_id, msg_id)
        same = row is not None and (row["kind"], row["name"], row["size"], row["url"]) == \
            (kind, meta.get("name"), meta.get("size"), None)
        if same and (row["sha256"] or row["tries"] >= MAX_TRIES):
            return row
        if self.download is None:
            return None
        tries = row["tries"] if same else 0
        values = {"sha256": None, "path": None, "pages": None, "first_page": None, "error": None}
        self.directory.mkdir(mode=0o700, exist_ok=True)
        part = self.directory / f"{chat_id}_{msg_id}.part"
        try:
            if not await self.download(chat_id, msg_id, part):
                raise FileNotFoundError("the message or its file is gone")
            values.update(await asyncio.to_thread(self._store, part, kind))
        except NotNow:
            part.unlink(missing_ok=True)
            raise
        except Exception as e:
            log.warning("%s of message %s in %s not read: %s", kind, msg_id, chat_id, e)
            values["error"] = str(e) or type(e).__name__
            # A gone or broken file stays so; anything else (Telegram refused it once) is tried at the next runs.
            tries = MAX_TRIES if isinstance(e, FileNotFoundError | ValueError | pdf.PdfError) else tries + 1
        finally:
            part.unlink(missing_ok=True)
        return self._put("tg", chat_id, msg_id, message["at"], meta.get("name"), meta.get("size"), values, kind,
                         None, tries)

    def _put(self, source: str, chat_id: int, msg_id: int, at: str, name: str | None, size: int | None,
             values: dict, kind: str, url: str | None, tries: int) -> dict:
        with self.db:
            self.db.execute(
                "insert into files(source, chat_id, msg_id, at, name, size, sha256, path, pages, first_page, error, "
                "kind, reading, url, tries) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, null, ?, ?) "
                "on conflict(source, chat_id, msg_id) do update set "
                "at = excluded.at, name = excluded.name, size = excluded.size, sha256 = excluded.sha256, "
                "path = excluded.path, pages = excluded.pages, first_page = excluded.first_page, "
                "error = excluded.error, kind = excluded.kind, reading = null, url = excluded.url, "
                "tries = excluded.tries",
                (source, chat_id, msg_id, at, name, size, values["sha256"], values["path"], values["pages"],
                 values["first_page"], values["error"], kind, url, tries))
        return self.row(source, chat_id, msg_id)

    async def keep_attachment(self, msg_id: int, at: dt.datetime, name: str, data: bytes) -> dict | None:
        """A letter's PDF attachment, kept when the letter arrives (Mail.poll), before the letter itself: the batch
        that reads the letter finds it. None for one too big; a row with `error` for a PDF that cannot be read."""
        if len(data) > MAX_SIZE:
            return None
        self.directory.mkdir(mode=0o700, exist_ok=True)
        part = self.directory / f"mail_{msg_id}_{secrets.token_hex(4)}.part"
        values = {"sha256": None, "path": None, "pages": None, "first_page": None, "error": None}
        try:
            await asyncio.to_thread(part.write_bytes, data)
            values.update(await asyncio.to_thread(self._store, part, "pdf"))
        except Exception as e:
            log.warning("the attachment %s of letter %s not read: %s", name, msg_id, e)
            values["error"] = str(e) or type(e).__name__
        finally:
            part.unlink(missing_ok=True)
        return self._put("mail", MAILBOX, msg_id, clock.local_iso(at), name, len(data), values, "pdf", None,
                         MAX_TRIES)

    async def fetch_link(self, message: dict) -> dict | None:
        """The PDF behind the first link of a message that leads to one (collectors/links.py), for a message without a
        PDF of its own: the row, stored or downloaded now; None when the message links to none or links are not
        followed. A link that cannot be read now is tried at the next runs, MAX_TRIES times in all."""
        links = document_links(message["text"])
        if not links:
            return None
        source, chat_id, msg_id = message["source"], message["chat_id"], message["msg_id"]
        row = self.row(source, chat_id, msg_id)
        same = row is not None and row["url"] in links
        if same and (row["sha256"] or row["tries"] >= MAX_TRIES):
            return row
        if self.fetch_url is None:
            return None
        tries = row["tries"] if same else 0
        self.directory.mkdir(mode=0o700, exist_ok=True)
        part = self.directory / f"link_{chat_id}_{msg_id}.part"
        values = {"sha256": None, "path": None, "pages": None, "first_page": None, "error": None}
        url, name, failed = links[0], None, False
        try:
            for link in links:
                try:
                    name = await self.fetch_url(link, part, MAX_SIZE)
                except LinkError as e:
                    failed, values["error"] = True, f"{link}: {e}"
                    continue
                if name is not None:
                    url = link
                    values.update(await asyncio.to_thread(self._store, part, "pdf"))
                    values["error"] = None
                    break
            else:
                if not failed:
                    values["error"] = "no PDF behind the links"
        except Exception as e:  # a PDF that cannot be read
            log.warning("the PDF of %s (message %s in %s) not read: %s", url, msg_id, chat_id, e)
            values["error"] = str(e) or type(e).__name__
        finally:
            part.unlink(missing_ok=True)
        tries = tries + 1 if failed and not values["sha256"] else MAX_TRIES
        if values["sha256"]:
            log.info("the PDF of %s (message %s in %s) is read", url, msg_id, chat_id)
        return self._put(source, chat_id, msg_id, message["at"], name, None, values, "pdf", url, tries)

    def _store(self, part: Path, kind: str) -> dict:
        """The downloaded file under its hash, a PDF with its first page read. In a thread."""
        data = part.read_bytes()
        sha = hashlib.sha256(data).hexdigest()
        values = {"sha256": sha}
        if kind == "pdf":
            extract = pdf.extract(part, 1)
            values.update(pages=extract.pages, first_page=extract.first[:FIRST_PAGE_CHARS])
            extension = "pdf"
        else:
            found = media_type(data)
            if found is None:
                raise ValueError("not a JPEG, PNG, GIF or WebP image")
            if len(data) > IMAGE_MAX_SIZE:
                raise ValueError("the image is too big for the model")
            extension = IMAGE_TYPES[found]
        target = self.directory / f"{sha}.{extension}"
        part.replace(target)
        target.chmod(0o600)
        return {**values, "path": str(target.relative_to(self.data_dir))}

    def prune(self, now) -> int:
        """Rows of messages past their 14 days, and every file no row has: theirs, and those a replaced file left."""
        with self.db:
            removed = self.db.execute("delete from files where at < ?",
                                      (clock.local_iso(now - RETENTION),)).rowcount
        kept = {row["path"] for row in self.db.execute("select path from files where path is not null")}
        if self.directory.exists():
            for path in self.directory.iterdir():
                if path.suffix != ".part" and str(path.relative_to(self.data_dir)) not in kept:
                    path.unlink(missing_ok=True)
        return removed
