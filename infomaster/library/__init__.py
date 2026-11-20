"""The library (stage 14, D58): the raw material of the courses and the knowledge base made of it.

Two stores, kept for good under data/library/:
- *materials*: what arrives raw. Photos of the board, audio, notes of the owner and classmates, slides, sheets, links
  (remembered, never fetched) and texts. Each has a course (a name of the timetable, `course`) and a class of RUZ
  (`class_uid`), or neither: then it waits in the inbox. Each is `new` until Claude has made it into the knowledge
  base (`processed`). An item may hang on another one (`parent`): a transcript of a recording.
- *knowledge*: what Claude made of it, per course. A `book` (the whole course), a `lecture` or a `seminar` (one
  class), and a `cheatsheet` (notation, formatting, the topics covered: for Claude itself). Every version is kept: a
  version is a source (`.tex`, or Markdown for a cheat sheet) and a PDF. The server never runs TeX: the PDF comes
  built.

A file arrives in parts (an upload, a Telegram download) at a `.part` path and is moved into place once complete, so
a half file is never listed. Files that arrive by an upload link (links.py) wait as a draft row until their bytes
are in.
"""

import asyncio
import datetime as dt
import hashlib
import json
import logging
import re
import secrets
import shutil
from collections.abc import AsyncIterator, Awaitable, Callable
from pathlib import Path

from .. import clock
from ..processing.schedule import type_of
from ..store import Store

log = logging.getLogger(__name__)

SCHEMA = """
create table if not exists materials (  -- raw material of the courses (D58): kept for good
    id integer primary key,
    course text,                -- a name of the timetable (classes.subject) or as a source names it; null: the inbox
    class_uid text,             -- its RUZ class (classes.uid); null: the course as a whole, or not known
    day text,                   -- YYYY-MM-DD: of its class, else of its arrival
    kind text not null,         -- photo, audio, video, notes, slides, sheet, link, text, transcript, file
    title text not null default '',
    author text not null default '',  -- who wrote or sent it: "owner", a classmate's name, a chat's
    text text,                  -- a text's or a link's words; a transcript
    url text,                   -- a link: remembered, never fetched
    path text,                  -- its file under the data directory; null for a link or a text
    name text,                  -- the file's name, as it came
    media_type text,
    size integer,
    sha256 text,
    parent integer,             -- the item it belongs to: a transcript's recording
    source text not null,       -- chat, bot, miniapp, connector, sheet, web
    chat_id integer,            -- where it came from
    msg_id integer,
    status text not null,       -- uploading (a draft: no bytes yet), new, processed
    note text not null default '',  -- why it is in the inbox; what Claude made of it
    created_at text not null,
    processed_at text,
    removed_at text             -- removed by the owner or Claude: listed nowhere, the file goes after REMOVED_KEEP
);
create index if not exists materials_course on materials(course, day);
create index if not exists materials_origin on materials(source, chat_id, msg_id);
create index if not exists materials_sha256 on materials(sha256);
create table if not exists knowledge (  -- the knowledge base (D58): one document per course and class
    id integer primary key,
    course text not null,
    kind text not null,         -- book, lecture, seminar, cheatsheet
    class_uid text,             -- a lecture's or a seminar's RUZ class
    day text,                   -- YYYY-MM-DD of that class
    title text not null,
    created_at text not null
);
create index if not exists knowledge_course on knowledge(course, kind);
create table if not exists knowledge_versions (
    id integer primary key,
    doc_id integer not null,
    version integer,            -- 1, 2…; null for a draft whose files are still coming
    source_path text,           -- the .tex (Markdown of a cheat sheet)
    pdf_path text,
    note text not null default '',      -- what changed
    materials text not null default '[]',  -- JSON [material id] it was made from
    created_at text not null,
    published_at text
);
create index if not exists knowledge_doc on knowledge_versions(doc_id, version);
"""

KINDS = ("photo", "audio", "video", "notes", "slides", "sheet", "link", "text", "transcript", "file")
SOURCES = ("chat", "bot", "miniapp", "connector", "sheet", "web")
DOC_KINDS = ("book", "lecture", "seminar", "cheatsheet")
CLASS_DOCS = ("lecture", "seminar")          # a document of one class
PARTS = {"source": ".tex", "pdf": ".pdf"}    # a version's files; a cheat sheet's source is .md
MAX_FILE = 2 * 2**30            # bytes of one raw file (a lecture's video)
MAX_DOC_FILE = 64 * 2**20       # bytes of a version's file
MAX_TEXT = 1_000_000            # characters of a text item
CLASS_BEFORE = dt.timedelta(minutes=20)      # an item sent this long before its class starts still belongs to it
CLASS_AFTER = dt.timedelta(days=7)           # and the last class of its course this long ago
DRAFT_KEEP = dt.timedelta(days=1)            # a draft whose files never came
REMOVED_KEEP = dt.timedelta(days=30)         # a removed item: undo brings it back until then
CHUNK = 1 << 16

MEDIA_KINDS = (("image/", "photo"), ("audio/", "audio"), ("video/", "video"))
SLIDE_TYPES = ("application/vnd.ms-powerpoint", "application/vnd.openxmlformats-officedocument.presentationml")


class LibraryError(ValueError):
    """A request the library refuses: the text says why."""


def guess_kind(media_type: str | None, name: str | None = None) -> str:
    """The kind of a file by its type and name: photo, audio, video, slides, or file."""
    media_type, name = (media_type or "").lower(), (name or "").lower()
    for prefix, kind in MEDIA_KINDS:
        if media_type.startswith(prefix):
            return kind
    if media_type.startswith(SLIDE_TYPES) or name.endswith((".ppt", ".pptx", ".key", ".odp")):
        return "slides"
    return "file"


def safe_name(name: str | None, fallback: str) -> str:
    """A file name to keep and to serve: no directories, no control characters, at most 120 characters."""
    name = re.sub(r"[\x00-\x1f/\\]", "_", (name or "").strip()).strip(". ") or fallback
    if len(name) > 120:
        stem, dot, ext = name.rpartition(".")
        name = (stem[:110] + dot + ext[:9]) if dot and len(ext) <= 9 else name[:120]
    return name


def class_kind(kind: str) -> str:
    """A class's kind of the timetable as a document's: lecture, seminar, or "" (an exam, a midterm)."""
    found = type_of(kind, True)
    if found == "Lecture":
        return "lecture"
    if found in ("Seminar", "Lab", "Class"):
        return "seminar"
    return ""


async def write_parts(chunks: AsyncIterator[bytes], target: Path, limit: int) -> tuple[int, str]:
    """Write an incoming stream to `target`; (size, sha256). LibraryError past `limit` bytes, the part removed."""
    digest, size = hashlib.sha256(), 0
    target.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    try:
        with target.open("wb") as out:
            async for chunk in chunks:
                size += len(chunk)
                if size > limit:
                    raise LibraryError(f"the file is over {limit // 2**20} MB")
                digest.update(chunk)
                await asyncio.to_thread(out.write, chunk)
    except BaseException:
        target.unlink(missing_ok=True)
        raise
    target.chmod(0o600)
    return size, digest.hexdigest()


def file_hash(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while block := source.read(CHUNK * 16):
            digest.update(block)
    return digest.hexdigest()


class Library:
    def __init__(self, store: Store, data_dir: Path, now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.db, self.data_dir, self.now = store, store.db, data_dir, now
        self.root = data_dir / "library"
        self.db.executescript(SCHEMA)
        # Two names of one course (a chat's «Матан» and RUZ's «Математический анализ»): Schedule.same_course when the
        # service runs; the plain name otherwise.
        self.same_course: Callable[[str | None, str | None], bool] = \
            lambda a, b: bool(a and b and a.casefold() == b.casefold())

    # --- courses and classes -------------------------------------------------------------------

    def course(self, name: str | None) -> str | None:
        """The timetable's name of a course, if it has one; the name as given otherwise; None for none. A course RUZ
        renamed (its feed went from Russian to English) has several names in `classes`: the newest one wins."""
        name = (name or "").strip()
        if not name:
            return None
        found = [row["subject"] for row in self.db.execute(
            "select subject, max(start) last from classes group by subject order by last desc")
            if row["subject"] == name or self.same_course(name, row["subject"])]
        return found[0] if found else name

    def names(self, course: str | None) -> list[str]:
        """Every name of the course in the timetable: its classes under an old name are its classes too."""
        course = self.course(course)
        if course is None:
            return []
        subjects = [row["subject"] for row in self.db.execute("select distinct subject from classes")]
        return [subject for subject in subjects if self.course(subject) == course] or [course]

    def courses(self) -> list[dict]:
        """Every course of the timetable and of the library, with the counts of its material and documents."""
        names = {self.course(row["subject"]) for row in
                 self.db.execute("select distinct subject from classes where gone_at is null")}
        names |= {row["course"] for row in self.db.execute(
            "select distinct course from materials where course is not null and removed_at is null")}
        names |= {row["course"] for row in self.db.execute("select distinct course from knowledge")}
        counts = {row["course"]: dict(row) for row in self.db.execute(
            "select course, count(*) items, sum(status = 'new') new from materials "
            "where removed_at is null and status != 'uploading' group by course")}
        docs = {row["course"]: row["n"] for row in self.db.execute(
            "select k.course, count(*) n from knowledge k where exists "
            "(select 1 from knowledge_versions v where v.doc_id = k.id and v.version is not null) group by k.course")}
        return [{"course": name, "items": (counts.get(name) or {}).get("items") or 0,
                 "new": (counts.get(name) or {}).get("new") or 0, "documents": docs.get(name, 0)}
                for name in sorted(names, key=str.casefold)]

    def inbox_count(self) -> int:
        return self.db.execute("select count(*) n from materials where course is null and removed_at is null "
                               "and status != 'uploading'").fetchone()["n"]

    def classes(self, course: str, until: dt.datetime | None = None) -> list[dict]:
        """The course's classes of RUZ that have started (by `until`, now by default), the last first, each with its
        kind (lecture, seminar or "") and its counts of material."""
        until_iso = clock.local_iso(until or self.now())
        names = self.names(course)
        course = self.course(course)
        rows = self.db.execute("select uid, subject, start, end, kind, cancelled from classes where subject in "
                               f"({', '.join('?' * len(names))}) and gone_at is null and start <= ? "
                               "order by start desc", (*names, until_iso))
        counts = {row["class_uid"]: dict(row) for row in self.db.execute(
            "select class_uid, count(*) items, sum(status = 'new') new from materials where course = ? and "
            "removed_at is null and status != 'uploading' and class_uid is not null group by class_uid", (course,))}
        found = []
        for row in rows:
            count = counts.get(row["uid"]) or {}
            found.append({"uid": row["uid"], "start": row["start"], "end": row["end"], "kind": class_kind(row["kind"]),
                          "ruz_kind": row["kind"], "cancelled": bool(row["cancelled"]),
                          "items": count.get("items") or 0, "new": count.get("new") or 0})
        return found

    def class_of(self, uid: str | None) -> dict | None:
        if not uid:
            return None
        row = self.db.execute("select uid, subject, start, end, kind from classes where uid = ?", (uid,)).fetchone()
        return None if row is None else {**dict(row), "subject": self.course(row["subject"]),
                                         "kind": class_kind(row["kind"]), "ruz_kind": row["kind"]}

    def find_class(self, course: str | None, at: dt.datetime, seminar: bool = False) -> dict | None:
        """The class an item sent at `at` belongs to: the course's last class that started by a little after `at`,
        within a week. With `seminar`, the course's seminar nearest to `at` either way, within a week: a sheet comes
        the night before its seminar or a day after it. None for an unknown course or a course without classes then."""
        names = self.names(course)
        if not names:
            return None
        if seminar:
            rows = self.db.execute(
                f"select uid, start, kind from classes where subject in ({', '.join('?' * len(names))}) and "
                "gone_at is null and cancelled = 0 and start <= ? and start >= ? and length(start) > 10",
                (*names, clock.local_iso(at + CLASS_AFTER), clock.local_iso(at - CLASS_AFTER)))
            moment = at.astimezone(clock.TZ).replace(tzinfo=None)
            seminars = [row for row in rows if class_kind(row["kind"]) == "seminar"]
            if seminars:
                nearest = min(seminars, key=lambda row: abs(dt.datetime.fromisoformat(row["start"]) - moment))
                return self.class_of(nearest["uid"])
        row = self.db.execute(
            f"select uid from classes where subject in ({', '.join('?' * len(names))}) and gone_at is null and "
            "cancelled = 0 and start <= ? and start >= ? and length(start) > 10 order by start desc limit 1",
            (*names, clock.local_iso(at + CLASS_BEFORE), clock.local_iso(at - CLASS_AFTER))).fetchone()
        return self.class_of(row["uid"]) if row else None

    # --- materials -----------------------------------------------------------------------------

    def _place(self, course: str | None, class_uid: str | None, at: dt.datetime) -> tuple[str | None, str | None,
                                                                                            str]:
        """(course, class_uid, day) of an item: the class must be of the course; a class without a course gives it."""
        course = self.course(course)
        found = self.class_of(class_uid)
        if class_uid and found is None:
            raise LibraryError(f"no class {class_uid} in the timetable")
        if found is not None:
            if course is not None and course != found["subject"]:
                raise LibraryError(f"class {class_uid} is of {found['subject']}, not {course}")
            return found["subject"], found["uid"], found["start"][:10]
        return course, None, clock.local_day(at).isoformat()

    def add(self, kind: str, *, source: str, course: str | None = None, class_uid: str | None = None,
            title: str = "", author: str = "", text: str | None = None, url: str | None = None,
            chat_id: int | None = None, msg_id: int | None = None, parent: int | None = None, note: str = "",
            at: dt.datetime | None = None, status: str = "new", file: dict | None = None) -> int:
        """A new item; its id. `file` is {path, name, media_type, size, sha256} of a file already in place."""
        if kind not in KINDS:
            raise LibraryError(f"kind is one of {', '.join(KINDS)}")
        if source not in SOURCES:
            raise LibraryError(f"source is one of {', '.join(SOURCES)}")
        if text is not None and len(text) > MAX_TEXT:
            raise LibraryError(f"the text is over {MAX_TEXT} characters")
        if kind == "link" and not url:
            raise LibraryError("a link needs its url")
        if parent is not None and self.get(parent) is None:
            raise LibraryError(f"no item {parent}")
        at = at or self.now()
        course, class_uid, day = self._place(course, class_uid, at)
        file = file or {}
        with self.db:
            return self.db.execute(
                "insert into materials(course, class_uid, day, kind, title, author, text, url, path, name, "
                "media_type, size, sha256, parent, source, chat_id, msg_id, status, note, created_at) "
                "values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (course, class_uid, day, kind, title.strip(), author.strip(), text, url, file.get("path"),
                 file.get("name"), file.get("media_type"), file.get("size"), file.get("sha256"), parent, source,
                 chat_id, msg_id, status, note, clock.local_iso(at))).lastrowid

    def from_origin(self, source: str, chat_id: int, msg_id: int, sha256: str | None = None) -> dict | None:
        """The item that came from this message already, if any: a message is taken once. With `sha256`, only the
        item of that file: a message whose file was replaced (a sheet's new version) is taken again."""
        where, values = "", ()
        if sha256 is not None:
            where, values = " and sha256 = ?", (sha256,)
        row = self.db.execute("select * from materials where source = ? and chat_id = ? and msg_id = ? "
                              f"and removed_at is null{where}", (source, chat_id, msg_id, *values)).fetchone()
        return dict(row) if row else None

    def _new_part(self) -> Path:
        directory = self.root / "raw"
        directory.mkdir(parents=True, mode=0o700, exist_ok=True)
        return directory / f"{secrets.token_hex(8)}.part"

    def _keep_file(self, part: Path, name: str | None, size: int, sha256: str) -> dict:
        """A complete part moved under data/library/raw/<sha256[:2]>/<sha256>.<ext>; the same bytes are kept once."""
        suffix = Path(name or "").suffix.lower()[:10]
        target = self.root / "raw" / sha256[:2] / f"{sha256}{suffix}"
        target.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        if target.exists():
            part.unlink(missing_ok=True)
        else:
            part.replace(target)
            target.chmod(0o600)
        return {"path": str(target.relative_to(self.data_dir)), "size": size, "sha256": sha256}

    async def add_stream(self, chunks: AsyncIterator[bytes], *, name: str | None, media_type: str | None,
                         kind: str | None = None, **fields) -> int:
        """A new item from a stream of bytes (an upload); its id."""
        part = self._new_part()
        size, sha256 = await write_parts(chunks, part, MAX_FILE)
        if size == 0:
            part.unlink(missing_ok=True)
            raise LibraryError("the file is empty")
        file = {**self._keep_file(part, name, size, sha256), "name": safe_name(name, f"file-{sha256[:8]}"),
                "media_type": media_type or "application/octet-stream"}
        return self.add(kind or guess_kind(media_type, name), file=file, **fields)

    async def add_download(self, download: Callable[[Path], Awaitable[bool]], *, name: str | None,
                           media_type: str | None, kind: str | None = None, **fields) -> int | None:
        """A new item from a download into a path (Telegram): `download(path)` returns False when the file is gone,
        and the item is not made then (None). Errors of the download pass through."""
        part = self._new_part()
        try:
            if not await download(part) or not part.exists():
                return None
            size = part.stat().st_size
            if size > MAX_FILE:
                raise LibraryError(f"the file is over {MAX_FILE // 2**20} MB")
            sha256 = await asyncio.to_thread(file_hash, part)
            file = {**self._keep_file(part, name, size, sha256), "name": safe_name(name, f"file-{sha256[:8]}"),
                    "media_type": media_type or "application/octet-stream"}
        finally:
            part.unlink(missing_ok=True)
        return self.add(kind or guess_kind(media_type, name), file=file, **fields)

    async def fill(self, item_id: int, chunks: AsyncIterator[bytes]) -> dict:
        """The bytes of a draft item (made with status "uploading"): it becomes new. LibraryError for no draft."""
        row = self.get(item_id, drafts=True)
        if row is None or row["status"] != "uploading":
            raise LibraryError("this upload is used already or gone")
        part = self._new_part()
        size, sha256 = await write_parts(chunks, part, MAX_FILE)
        if size == 0:
            part.unlink(missing_ok=True)
            raise LibraryError("the file is empty")
        file = self._keep_file(part, row["name"], size, sha256)
        with self.db:
            self.db.execute("update materials set path = ?, size = ?, sha256 = ?, status = 'new' where id = ?",
                            (file["path"], size, sha256, item_id))
        return self.get(item_id)

    def get(self, item_id: int, drafts: bool = False, removed: bool = False) -> dict | None:
        row = self.db.execute("select * from materials where id = ?", (item_id,)).fetchone()
        if row is None or row["removed_at"] is not None and not removed or row["status"] == "uploading" and not drafts:
            return None
        return dict(row)

    def path(self, row: dict) -> Path | None:
        """Where an item's file is; None for a link, a text, a draft or a lost file."""
        if not row or not row.get("path"):
            return None
        path = self.data_dir / row["path"]
        return path if path.exists() else None

    def items(self, *, course: str | None = None, inbox: bool = False, class_uid: str | None = None,
              status: str | None = None, kind: str | None = None, limit: int = 200) -> list[dict]:
        """Items, the newest first: of the inbox, of a course (and a class), or all; `status` new or processed."""
        where, values = ["removed_at is null", "status != 'uploading'"], []
        if inbox:
            where.append("course is null")
        elif course is not None:
            where.append("course = ?")
            values.append(self.course(course))
        if class_uid is not None:
            where.append("class_uid = ?")
            values.append(class_uid)
        if status is not None:
            where.append("status = ?")
            values.append(status)
        if kind is not None:
            where.append("kind = ?")
            values.append(kind)
        rows = self.db.execute(f"select * from materials where {' and '.join(where)} order by day desc, id desc "
                               "limit ?", (*values, limit))
        return [dict(row) for row in rows]

    def children(self, item_id: int) -> list[dict]:
        return [dict(row) for row in self.db.execute(
            "select * from materials where parent = ? and removed_at is null and status != 'uploading' order by id",
            (item_id,))]

    def update(self, item_id: int, **fields) -> dict:
        """Change an item: course and class_uid (sorting the inbox: a course of None puts it back), title, author,
        kind, text, note, status (new or processed). The old values, for an undo."""
        row = self.get(item_id)
        if row is None:
            raise LibraryError(f"no item {item_id}")
        old = {name: row[name] for name in fields if name in row}
        values = dict(fields)
        if "course" in fields or "class_uid" in fields:
            course = fields.get("course", row["course"])
            class_uid = fields.get("class_uid", row["class_uid"] if "course" not in fields else None)
            at = dt.datetime.fromisoformat(row["created_at"]).replace(tzinfo=clock.TZ)
            values["course"], values["class_uid"], values["day"] = self._place(course, class_uid, at)
            old.setdefault("course", row["course"])
            old.setdefault("class_uid", row["class_uid"])
        if "kind" in values and values["kind"] not in KINDS:
            raise LibraryError(f"kind is one of {', '.join(KINDS)}")
        if "status" in values:
            if values["status"] not in ("new", "processed"):
                raise LibraryError("status is new or processed")
            values["processed_at"] = clock.local_iso(self.now()) if values["status"] == "processed" else None
        allowed = {"course", "class_uid", "day", "title", "author", "kind", "text", "note", "status", "processed_at"}
        if unknown := set(values) - allowed:
            raise LibraryError(f"cannot change {', '.join(sorted(unknown))}")
        with self.db:
            self.db.execute(f"update materials set {', '.join(f'{name} = ?' for name in values)} where id = ?",
                            (*values.values(), item_id))
        return old

    def remove(self, item_id: int) -> None:
        """Out of every list; the file stays REMOVED_KEEP for an undo (`restore`). Its transcripts go with it. A draft
        is removed too: its upload link then takes nothing."""
        if self.get(item_id, drafts=True) is None:
            raise LibraryError(f"no item {item_id}")
        with self.db:
            self.db.execute("update materials set removed_at = ? where (id = ? or parent = ?) and removed_at is null",
                            (clock.local_iso(self.now()), item_id, item_id))

    def restore(self, item_id: int) -> None:
        row = self.get(item_id, removed=True)
        if row is None or row["removed_at"] is None:
            raise LibraryError(f"item {item_id} is not removed")
        if row["path"] and self.path(row) is None:
            raise LibraryError("its file is gone already")
        with self.db:
            self.db.execute("update materials set removed_at = null where (id = ? or parent = ?) and removed_at = ?",
                            (item_id, item_id, row["removed_at"]))

    # --- the knowledge base --------------------------------------------------------------------

    def documents(self, course: str | None = None) -> list[dict]:
        """Documents with their newest version (null while only a draft exists), by course, kind and day."""
        where, values = "", ()
        if course is not None:
            where, values = "where k.course = ?", (self.course(course),)
        rows = self.db.execute(
            "select k.*, (select max(version) from knowledge_versions v where v.doc_id = k.id) version "
            f"from knowledge k {where} order by k.course, k.kind, k.day desc, k.id", values)
        return [dict(row) for row in rows]

    def document(self, doc_id: int) -> dict | None:
        row = self.db.execute("select * from knowledge where id = ?", (doc_id,)).fetchone()
        if row is None:
            return None
        versions = [dict(v) for v in self.db.execute(
            "select * from knowledge_versions where doc_id = ? and version is not null order by version desc",
            (doc_id,))]
        return {**dict(row), "versions": versions}

    def find_document(self, course: str, kind: str, class_uid: str | None = None) -> dict | None:
        """A course's book or cheat sheet, or a class's lecture or seminar."""
        row = self.db.execute("select id from knowledge where course = ? and kind = ? and coalesce(class_uid, '') = ?",
                              (self.course(course), kind, class_uid or "")).fetchone()
        return self.document(row["id"]) if row else None

    def version(self, version_id: int) -> dict | None:
        row = self.db.execute("select v.*, k.course, k.kind, k.title, k.class_uid from knowledge_versions v "
                              "join knowledge k on k.id = v.doc_id where v.id = ?", (version_id,)).fetchone()
        return dict(row) if row else None

    def draft(self, course: str, kind: str, *, class_uid: str | None = None, title: str = "", note: str = "",
              materials: list[int] = ()) -> dict:
        """A new version of a document, made as a draft: its files come next (`put`), then `publish`. The document is
        made with it if it is new. The draft's row."""
        if kind not in DOC_KINDS:
            raise LibraryError(f"kind is one of {', '.join(DOC_KINDS)}")
        course = self.course(course)
        if course is None:
            raise LibraryError("a document needs its course")
        day = None
        if kind in CLASS_DOCS:
            found = self.class_of(class_uid)
            if found is None:
                raise LibraryError(f"a {kind} needs class_uid, a class of the timetable")
            if found["subject"] != course:
                raise LibraryError(f"class {class_uid} is of {found['subject']}, not {course}")
            day = found["start"][:10]
        elif class_uid:
            raise LibraryError(f"a {kind} is of the whole course: no class_uid")
        for item_id in materials:
            if self.get(item_id) is None:
                raise LibraryError(f"no item {item_id}")
        now = clock.local_iso(self.now())
        found = self.find_document(course, kind, class_uid)
        with self.db:
            if found is None:
                default = {"book": f"{course}", "cheatsheet": f"{course}: notes for Claude"}.get(kind) or \
                    f"{course}: {kind} {day}"
                doc_id = self.db.execute("insert into knowledge(course, kind, class_uid, day, title, created_at) "
                                         "values(?, ?, ?, ?, ?, ?)",
                                         (course, kind, class_uid, day, title.strip() or default, now)).lastrowid
            else:
                doc_id = found["id"]
                if title.strip():
                    self.db.execute("update knowledge set title = ? where id = ?", (title.strip(), doc_id))
            version_id = self.db.execute(
                "insert into knowledge_versions(doc_id, note, materials, created_at) values(?, ?, ?, ?)",
                (doc_id, note.strip(), _json_ids(materials), now)).lastrowid
        return self.version(version_id)

    def _part_path(self, version: dict, part: str) -> Path:
        suffix = ".md" if part == "source" and version["kind"] == "cheatsheet" else PARTS[part]
        return self.root / "knowledge" / str(version["doc_id"]) / f"{version['id']}{suffix}"

    async def put(self, version_id: int, part: str, chunks: AsyncIterator[bytes]) -> dict:
        """A draft's source or PDF; again replaces it. LibraryError for a published version or an unknown part."""
        if part not in PARTS:
            raise LibraryError("the part is source or pdf")
        version = self.version(version_id)
        if version is None or version["version"] is not None:
            raise LibraryError("this draft is published already or gone")
        target = self._part_path(version, part)
        part_path = target.with_suffix(target.suffix + ".part")
        size, _ = await write_parts(chunks, part_path, MAX_DOC_FILE)
        if part == "pdf":
            with part_path.open("rb") as head:
                if head.read(5) != b"%PDF-":
                    part_path.unlink(missing_ok=True)
                    raise LibraryError("this is no PDF")
        elif size == 0:
            part_path.unlink(missing_ok=True)
            raise LibraryError("the source is empty")
        part_path.replace(target)
        with self.db:
            self.db.execute(f"update knowledge_versions set {part}_path = ? where id = ?",
                            (str(target.relative_to(self.data_dir)), version_id))
        return self.version(version_id)

    async def put_text(self, version_id: int, part: str, text: str) -> dict:
        async def chunks():
            yield text.encode()
        return await self.put(version_id, part, chunks())

    def publish(self, version_id: int) -> dict:
        """A draft becomes the document's newest version. A lecture, a seminar and a book need the source and the
        PDF; a cheat sheet its source. The materials it was made from become processed."""
        version = self.version(version_id)
        if version is None or version["version"] is not None:
            raise LibraryError("this draft is published already or gone")
        if not version["source_path"]:
            raise LibraryError("the source is not uploaded yet")
        if version["kind"] != "cheatsheet" and not version["pdf_path"]:
            raise LibraryError("the PDF is not uploaded yet")
        now = clock.local_iso(self.now())
        with self.db:
            number = self.db.execute("select coalesce(max(version), 0) + 1 n from knowledge_versions where doc_id = ?",
                                     (version["doc_id"],)).fetchone()["n"]
            self.db.execute("update knowledge_versions set version = ?, published_at = ? where id = ?",
                            (number, now, version_id))
            ids = _ids(version["materials"])
            if ids:
                self.db.execute(f"update materials set status = 'processed', processed_at = ? where status = 'new' "
                                f"and id in ({', '.join('?' * len(ids))})", (now, *ids))
        return self.version(version_id)

    def unpublish(self, version_id: int) -> None:
        """Take back the newest version of a document (an undo): its files go, the one before is the newest again."""
        version = self.version(version_id)
        if version is None or version["version"] is None:
            raise LibraryError("no such version")
        newest = self.db.execute("select max(version) n from knowledge_versions where doc_id = ?",
                                 (version["doc_id"],)).fetchone()["n"]
        if version["version"] != newest:
            raise LibraryError("a newer version came after it")
        with self.db:
            self.db.execute("delete from knowledge_versions where id = ?", (version_id,))
            if not self.db.execute("select 1 from knowledge_versions where doc_id = ?",
                                   (version["doc_id"],)).fetchone():
                self.db.execute("delete from knowledge where id = ?", (version["doc_id"],))
        for part in PARTS:
            if version[f"{part}_path"]:
                (self.data_dir / version[f"{part}_path"]).unlink(missing_ok=True)

    def part_path(self, version: dict, part: str) -> Path | None:
        relative = version.get(f"{part}_path") if part in PARTS else None
        if not relative:
            return None
        path = self.data_dir / relative
        return path if path.exists() else None

    def read_source(self, version: dict) -> str | None:
        path = self.part_path(version, "source")
        return path.read_text(errors="replace") if path else None

    # --- upkeep ---------------------------------------------------------------------------------

    def prune(self) -> int:
        """Drafts whose files never came, removed items past REMOVED_KEEP, and every file no row has. How many rows
        went."""
        now = self.now()
        stale, gone = clock.local_iso(now - DRAFT_KEEP), clock.local_iso(now - REMOVED_KEEP)
        with self.db:
            removed = self.db.execute("delete from materials where status = 'uploading' and created_at < ?",
                                      (stale,)).rowcount
            removed += self.db.execute("delete from materials where removed_at is not null and removed_at < ?",
                                       (gone,)).rowcount
            removed += self.db.execute("delete from knowledge_versions where version is null and created_at < ?",
                                       (stale,)).rowcount
            self.db.execute("delete from knowledge where not exists "
                            "(select 1 from knowledge_versions v where v.doc_id = knowledge.id)")
        kept = {row["path"] for row in self.db.execute("select path from materials where path is not null")}
        kept |= {row[0] for row in self.db.execute(
            "select source_path from knowledge_versions where source_path is not null union "
            "select pdf_path from knowledge_versions where pdf_path is not null")}
        if self.root.exists():
            day_ago = now.timestamp() - DRAFT_KEEP.total_seconds()
            for path in self.root.rglob("*"):
                if not path.is_file() or str(path.relative_to(self.data_dir)) in kept:
                    continue
                if path.suffix == ".part" and path.stat().st_mtime > day_ago:
                    continue  # still coming
                path.unlink(missing_ok=True)
        return removed

    def usage(self) -> dict:
        """Items, documents and bytes on disk: the Mini App's status."""
        size = sum(p.stat().st_size for p in self.root.rglob("*") if p.is_file()) if self.root.exists() else 0
        items = self.db.execute("select count(*) n, sum(status = 'new') new from materials where removed_at is null "
                                "and status != 'uploading'").fetchone()
        return {"items": items["n"], "new": items["new"] or 0, "inbox": self.inbox_count(),
                "documents": len([d for d in self.documents() if d["version"]]), "bytes": size,
                "free_bytes": shutil.disk_usage(self.data_dir).free if self.data_dir.exists() else None}


def _json_ids(ids) -> str:
    return json.dumps([int(i) for i in ids])


def _ids(raw: str | None) -> list[int]:
    try:
        return [int(i) for i in json.loads(raw or "[]")]
    except (ValueError, TypeError):
        return []

