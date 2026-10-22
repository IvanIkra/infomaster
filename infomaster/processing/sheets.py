"""Листочки (§6.3, р. 14): PDFs with tasks from the chats, kept as rows here and as files in the forum.

1. The batch reads a PDF by its name and its first page (a scan: the first page as an image) and says whether it is a
   sheet, with its course, number and due date (processing/batches.py). The sheet becomes a row here, `new`, in the
   transaction of the batch, so nothing is lost when the forum is down. The same file again (forwarded, posted twice)
   is no new sheet.
2. Before the forum gets it, the sheet is prepared once: its pages are read locally for the next version's diff, and
   it is matched against earlier sheets (`previous`). A newer version: the same message with a new file, or the same
   chat and course with the same number or almost the same file name (no number to tell them apart), the older sheet
   at most 30 days old. With no certainty it is a new sheet; the owner can join it to an older one in the Mini App.
3. With `pdf_content` on in the chat, the model reads the sheet (task "pdf", an optional one: off above 80 % of the
   limit): at most `pdf_pages` pages and `pdf_tokens` tokens, what comes first. It writes a short summary for the post
   and gives the deadlines of its pages; they join the deadlines from the chat (source tg, the sheet's message), and an
   edit of the message's text does not withdraw them, its deletion does. For a newer version the model sees only the
   diff of the texts and says what changed. The summary and the changes are written in the forum's language (р. 41),
   as it is when the sheet is prepared.

4. The owner changes a sheet's course, number, due date and summary in the Mini App, and adds sheets of their own:
   a PDF uploaded there, of the chat OWN_CHAT (р. 44). An own sheet is posted as a chat's, gets newer versions by
   the same rules among the own ones, and is the only kind the owner may delete.

The forum side is outputs/sheets.py. Writing methods commit themselves, except `add`: the batch calls it inside its
own transaction.
"""

import asyncio
import datetime as dt
import logging
import re
from collections.abc import Callable

from .. import clock
from ..collectors.files import OWN_CHAT, Files
from ..collectors.inbox import Inbox
from ..i18n import Phrase, tr
from ..llm.budget import CHARS_PER_TOKEN, PAGE_TOKENS, BudgetExceeded
from ..llm.client import Document, Haiku, LLMError
from ..settings import ChatSettings, Settings
from ..store import Store
from . import pdf
from .deadlines import Deadlines
from .subjects import Subjects, clean

log = logging.getLogger(__name__)

VERSION_WINDOW = dt.timedelta(days=30)  # р. 14: an older sheet gets no newer version by the rules
TEXT_PAGES = 50           # read locally for the diff of the next version
TEXT_CHARS = 200_000
DIFF_CHARS = 8_000        # of the diff sent to the model
SUMMARY_CHARS = 600
CHANGES_CHARS = 400
LABEL_CHARS = 15          # a deadline label, as in the batches (Tablo's column, §2.1)
MAX_TOKENS = 2_000

READ_SYSTEM = """\
You read a sheet (листок) of a university course: a PDF with tasks for students (HSE University, Moscow, first year, \
Software Engineering). The student sees your summary in Telegram under the file.

Output:
- summary: in {language}, at most 3 short sentences: what the tasks are about, how many there are, how and when to \
hand in, anything unusual (a defence, a variant to choose, bonus tasks). No greeting, no "{no_intro}".
- deadlines: every date the student must hand something in by, as written in the sheet: label "<abbr> <work>" \
in English within 14 characters with the course abbreviation given ("Calc HW 3", "DM ИДЗ-1" → "DM IDZ-1"), due \
YYYY-MM-DD; a date without a year is in the academic year of the posting date. [] when the sheet gives no date.
"""

READ_SCHEMA = {
    "type": "object",
    "properties": {
        "summary": {"type": "string"},
        "deadlines": {"type": "array", "items": {
            "type": "object",
            "properties": {"label": {"type": "string"}, "due": {"type": "string"}},
            "required": ["label", "due"],
            "additionalProperties": False,
        }},
    },
    "required": ["summary", "deadlines"],
    "additionalProperties": False,
}

CHANGES_SYSTEM = """\
You compare two versions of a sheet (листок) of a university course: a PDF with tasks for students. You get only the \
lines that changed: "-" the old version, "+" the new one, as text extracted from the PDF (layout noise is possible).

Output changes: in {language}, one or two short sentences on what changed for the student: a task fixed or added, a \
new due date, a typo. Ignore changes of layout or spacing. "{layout_only}" when nothing else did.
"""

# The words of the prompts in each language the summary may be written in.
PROMPT_WORDS = {"en": {"language": "English", "no_intro": "this sheet", "layout_only": "Only the layout changed."},
                "ru": {"language": "Russian", "no_intro": "в этом листке",
                       "layout_only": "Изменилось только оформление."}}

CHANGES_SCHEMA = {
    "type": "object",
    "properties": {"changes": {"type": "string"}},
    "required": ["changes"],
    "additionalProperties": False,
}


def number_key(number: str | None) -> str:
    """'№ 3' -> '3', 'ДЗ-2.1' -> 'дз-2.1': what two sheets are compared by."""
    return re.sub(r"[\s№#]+", "", (number or "").casefold())


def due_date(value) -> str | None:
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}", value.strip()):
        return None
    try:
        return dt.date.fromisoformat(value.strip()).isoformat()
    except ValueError:
        return None


class Sheets:
    def __init__(self, store: Store, settings: Settings, chat_settings: ChatSettings, inbox: Inbox, files: Files,
                 deadlines: Deadlines, subjects: Subjects, llm: Haiku, now: Callable[[], dt.datetime] = clock.now_utc):
        self.db, self.settings, self.chat_settings, self.inbox, self.files = (
            store.db, settings, chat_settings, inbox, files)
        self.deadlines, self.subjects, self.llm, self.now = deadlines, subjects, llm, now
        self.error: str | None = None  # of the last reading that failed, for the status

    def status(self) -> dict:
        waiting = self.db.execute("select count(*) n from sheets where status = 'new'").fetchone()["n"]
        return {"waiting": waiting, "error": self.error}

    # --- rows -------------------------------------------------------------------------------

    def get(self, sheet_id: int) -> dict | None:
        row = self.db.execute("select * from sheets where id = ?", (sheet_id,)).fetchone()
        return dict(row) if row else None

    def add(self, chat_id: int, message: dict, sheet: dict, at: dt.datetime) -> int | None:
        """A sheet the batch found (`sheet`: course, number, due) in a message with its file read (`message["pdf"]`, a
        files row). None when its file is no sheet to post: not downloaded, unreadable, or posted already. Call
        inside `with store.db`."""
        file = message.get("pdf")
        if not file or not file["sha256"]:
            return None
        if self.db.execute("select 1 from sheets where sha256 = ? and status != 'failed'",
                           (file["sha256"],)).fetchone():
            return None
        cursor = self.db.execute(
            "insert into sheets(chat_id, msg_id, name, sha256, course, number, due, status, created_at) "
            "values(?, ?, ?, ?, ?, ?, ?, 'new', ?)",
            (chat_id, message["msg_id"], file["name"] or "sheet.pdf", file["sha256"],
             sheet["course"] or None, sheet["number"], sheet["due"], clock.local_iso(at)))
        return cursor.lastrowid

    def add_own(self, file: dict, course: str | None, number: str, due: str | None, at: dt.datetime) -> int:
        """An own sheet of an uploaded PDF (`file`: its files row); it waits for the forum as a chat's."""
        with self.db:
            return self.db.execute(
                "insert into sheets(chat_id, msg_id, name, sha256, course, number, due, status, created_at) "
                "values(?, ?, ?, ?, ?, ?, ?, 'new', ?)",
                (OWN_CHAT, file["msg_id"], file["name"] or "sheet.pdf", file["sha256"], course or None, number, due,
                 clock.local_iso(at))).lastrowid

    def edit(self, sheet_id: int, fields: dict) -> dict | None:
        """The owner's change: course, number and due date of the sheet as it is now (its newest version), the
        summary of its first version, which the caption carries. The sheet, or None for no such sheet."""
        sheet = self.get(sheet_id)
        if sheet is None:
            return None
        with self.db:
            for name in ("course", "number", "due"):
                if name in fields:
                    self.db.execute(f"update sheets set {name} = ? where id = ?", (fields[name], sheet_id))
            if "summary" in fields:
                self.db.execute("update sheets set summary = ? where id = ?", (fields["summary"],
                                                                               self.first(sheet)["id"]))
        return self.get(sheet_id)

    def remove_own(self, sheet_id: int) -> list[dict] | None:
        """An own sheet deleted with every version of it: the rows deleted (the forum's posts of them go too), or
        None when it is no own sheet."""
        sheet = self.get(sheet_id)
        if sheet is None or sheet["chat_id"] != OWN_CHAT:
            return None
        chain = [self.first(sheet)]
        while (newer := self.db.execute("select * from sheets where previous = ?", (chain[-1]["id"],)).fetchone()):
            chain.append(dict(newer))
        if any(s["chat_id"] != OWN_CHAT for s in chain):
            return None  # a chat's sheet joined to it: that one is not the owner's to delete
        with self.db:
            self.db.execute(f"delete from sheets where id in ({', '.join('?' * len(chain))})",
                            [s["id"] for s in chain])
        return chain

    def waiting(self) -> list[dict]:
        """Sheets the forum has not got yet, oldest first."""
        return [dict(row) for row in self.db.execute("select * from sheets where status = 'new' order by id")]

    def first(self, sheet: dict) -> dict:
        """The first version of a sheet: the one whose post every version edits."""
        seen = set()
        while sheet["previous"] is not None and sheet["id"] not in seen:
            seen.add(sheet["id"])
            older = self.get(sheet["previous"])
            if older is None:
                break
            sheet = older
        return sheet

    def versions(self, first: dict) -> list[dict]:
        """A sheet's versions in order, from its first one."""
        chain = [first]
        while (newer := self.db.execute("select * from sheets where previous = ? and status = 'posted' order by id "
                                        "limit 1", (chain[-1]["id"],)).fetchone()) is not None:
            chain.append(dict(newer))
        return chain

    def _latest(self, sheet_id: int) -> bool:
        """No posted sheet updates it."""
        return self.db.execute("select 1 from sheets where previous = ? and status = 'posted'",
                               (sheet_id,)).fetchone() is None

    def course_names(self, today: dt.date) -> set[str]:
        """Courses of the recent sheets: the dictionary names them, for the post's title and hashtag."""
        since = clock.local_iso(dt.datetime.combine(today, dt.time()) - VERSION_WINDOW)
        rows = self.db.execute("select distinct course from sheets where course is not null and created_at >= ?",
                               (since,))
        return {row["course"] for row in rows}

    def posted(self, sheet_id: int, post_id: int) -> None:
        with self.db:
            self.db.execute("update sheets set status = 'posted', post_id = ? where id = ?", (post_id, sheet_id))

    def file_gone(self) -> str:
        """The reason a sheet failed when its file is gone, for the Mini App: kept in the row, so in its language
        of now."""
        return tr(self.settings.get("miniapp_language"), "the file is deleted already", "файл уже удалён")

    def failed(self, sheet_id: int, error: str) -> None:
        with self.db:
            self.db.execute("update sheets set status = 'failed', error = ? where id = ?", (error, sheet_id))

    # --- versions ----------------------------------------------------------------------------

    def _course_key(self, course: str | None) -> str:
        return (self.subjects.abbr(course) or clean(course)).casefold()

    def same(self, new: dict, old: dict) -> bool:
        """A newer version of `old` by the rules of р. 14 (the age is the caller's)."""
        if (new["chat_id"], new["msg_id"]) == (old["chat_id"], old["msg_id"]):
            return True  # the message's file was replaced
        if new["chat_id"] != old["chat_id"] or self._course_key(new["course"]) != self._course_key(old["course"]):
            return False
        a, b = number_key(new["number"]), number_key(old["number"])
        if a and b:
            return a == b
        return pdf.similar_names(new["name"], old["name"])

    def find_previous(self, sheet: dict) -> dict | None:
        """The newest version of an earlier sheet this one updates, if any."""
        since = clock.local_iso(dt.datetime.fromisoformat(sheet["created_at"]) - VERSION_WINDOW)
        rows = self.db.execute("select * from sheets where status = 'posted' and id != ? and chat_id = ? "
                               "and created_at >= ? order by id desc", (sheet["id"], sheet["chat_id"], since))
        return next((dict(row) for row in rows if self._latest(row["id"]) and self.same(sheet, dict(row))), None)

    # --- preparing for the forum ---------------------------------------------------------------

    async def prepare(self, sheet_id: int) -> dict | None:
        """Read the sheet, find what it updates, and with `pdf_content` let the model read it; once. None when its
        file is gone (the sheet fails)."""
        sheet = self.get(sheet_id)
        if sheet["text"] is not None:
            return sheet
        path = self.files.by_hash(sheet["sha256"])
        if path is None:
            self.failed(sheet_id, self.file_gone())
            return None
        try:
            extract = await asyncio.to_thread(pdf.extract, path, TEXT_PAGES)
        except pdf.PdfError as e:
            self.failed(sheet_id, str(e))
            return None
        previous = self.find_previous(sheet)
        summary = changes = None
        deadlines: list[dict] = []
        if self.chat_settings.get(sheet["chat_id"], "pdf_content"):
            if previous is None:
                summary, deadlines = await self._read(sheet, path, extract)
            else:
                changes = await self._describe(sheet, previous["text"] or "", extract.text[:TEXT_CHARS])
        with self.db:
            self.db.execute("update sheets set text = ?, summary = ?, changes = ?, previous = ?, version = ? "
                            "where id = ?",
                            (extract.text[:TEXT_CHARS], summary, changes, previous["id"] if previous else None,
                             previous["version"] + 1 if previous else 1, sheet_id))
            self._save_deadlines(sheet, deadlines)
        return self.get(sheet_id)

    def _save_deadlines(self, sheet: dict, deadlines: list[dict]) -> None:
        """Deadlines of the sheet's pages that the chat has not given yet. Inside `with self.db`."""
        for deadline in deadlines:
            if self.deadlines.find(deadline["label"], deadline["due"]) is None:
                self.deadlines.add({"source": "tg", "chat_id": sheet["chat_id"], "msg_ids": [sheet["msg_id"]],
                                    "course": sheet["course"], "label": deadline["label"], "due": deadline["due"],
                                    "sheet_id": sheet["id"]}, self.now())

    def _context(self, sheet: dict) -> list[str]:
        chat = self.inbox.chat(sheet["chat_id"])
        abbr = self.subjects.abbr(sheet["course"])
        return [f"Posted: {dt.datetime.fromisoformat(sheet['created_at']):%a %Y-%m-%d}",
                f"Chat: \"{chat['title'] if chat else sheet['chat_id']}\"",
                f"File: {sheet['name']}",
                f"Course: {sheet['course'] or '(none)'}" + (f", abbreviation {abbr}" if abbr else ""),
                f"Number: {sheet['number'] or '(none)'}"]

    async def _read(self, sheet: dict, path, extract: pdf.Extract) -> tuple[str | None, list[dict]]:
        """The model reads the sheet within the limits: its summary and deadlines; nothing when it cannot."""
        pages = min(extract.pages, self.settings.get("pdf_pages"))
        tokens = self.settings.get("pdf_tokens")
        lines = self._context(sheet) + [""]
        documents = []
        if pdf.scanned(extract.first):  # a scan: its pages as images, as many as the tokens allow
            pages = max(1, min(pages, tokens // PAGE_TOKENS))
            documents = [Document(await asyncio.to_thread(pdf.head, path, pages), pages)]
            lines.append(f"The sheet is a scan: its first {pages} of {extract.pages} pages are attached.")
        else:
            text = "\n".join(f"--- page {i} ---\n{page}" for i, page in enumerate(extract.texts[:pages], 1))
            limit = tokens * CHARS_PER_TOKEN
            cut = len(text) > limit
            lines += [f"Text of the sheet ({pages} of {extract.pages} pages" + (", cut at the limit" if cut else "")
                      + "):", text[:limit]]
        try:
            answer = await self.llm.json("pdf", self._prompt(READ_SYSTEM), "\n".join(lines), READ_SCHEMA,
                                         max_tokens=MAX_TOKENS, chat_id=sheet["chat_id"], documents=documents)
        except BudgetExceeded as e:
            log.info("sheet %d is posted without reading: %s", sheet["id"], e)
            return None, []
        except LLMError as e:
            log.warning("reading sheet %d failed: %s", sheet["id"], e)
            self.error = str(e)
            return None, []
        self.error = None
        deadlines = [{"label": label, "due": due} for raw in answer.get("deadlines") or []
                     if (label := " ".join(str(raw.get("label") or "").split())) and len(label) <= LABEL_CHARS
                     and (due := due_date(raw.get("due")))]
        return (str(answer.get("summary") or "").strip()[:SUMMARY_CHARS] or None), deadlines

    async def _describe(self, sheet: dict, old: str, new: str) -> str | None:
        """What changed against the older version, from the diff alone (р. 14)."""
        changed = pdf.diff(old, new, DIFF_CHARS)
        if not changed:  # a scan has no text to compare
            return tr(self.settings.get("forum_language"), "The text is the same.", "Текст не изменился.") \
                if old and new else None
        prompt = "\n".join(self._context(sheet) + ["", "Changed lines:", changed])
        try:
            answer = await self.llm.json("pdf", self._prompt(CHANGES_SYSTEM), prompt, CHANGES_SCHEMA,
                                         max_tokens=MAX_TOKENS, chat_id=sheet["chat_id"])
        except BudgetExceeded as e:
            log.info("the changes of sheet %d are not described: %s", sheet["id"], e)
            return None
        except LLMError as e:
            log.warning("describing the changes of sheet %d failed: %s", sheet["id"], e)
            self.error = str(e)
            return None
        self.error = None
        return str(answer.get("changes") or "").strip()[:CHANGES_CHARS] or None

    def _prompt(self, system: str) -> str:
        """A system prompt asking for the forum's language."""
        return system.format(**PROMPT_WORDS[self.settings.get("forum_language")])

    # --- the owner's joining (the Mini App) -------------------------------------------------------

    async def join(self, sheet_id: int, previous_id: int) -> tuple[dict, int]:
        """Make a sheet posted as new a newer version of another one: (the sheet now, its own post to delete).
        ValueError (with a Phrase) when it cannot be: a sheet with versions of its own, the same sheet, its file
        gone."""
        sheet, older = self.get(sheet_id), self.get(previous_id)
        if sheet is None or older is None or sheet["status"] != "posted" or older["status"] != "posted":
            raise ValueError(Phrase("no such sheet", "такого листка нет"))
        if sheet["previous"] is not None or not self._latest(sheet_id):
            raise ValueError(Phrase("this sheet has versions already: only a sheet without them can be joined",
                                    "у этого листка уже есть версии: присоединить можно только листок без них"))
        older = self.versions(self.first(older))[-1]  # the newest version of the other sheet
        if self.first(older)["id"] == sheet_id:
            raise ValueError(Phrase("it is the same sheet", "это тот же листок"))
        if self.files.by_hash(sheet["sha256"]) is None:
            raise ValueError(Phrase("the sheet's file is deleted already (14 days passed): nothing to put into the old "
                                    "message", "файл листка уже удалён (прошло 14 дней): заменить файл в старом "
                                    "сообщении нечем"))
        changes = None
        if self.chat_settings.get(sheet["chat_id"], "pdf_content"):
            changes = await self._describe(sheet, older["text"] or "", sheet["text"] or "")
        own_post = sheet["post_id"]
        with self.db:
            self.db.execute("update sheets set previous = ?, version = ?, changes = ?, post_id = ? where id = ?",
                            (older["id"], older["version"] + 1, changes, older["post_id"], sheet_id))
        return self.get(sheet_id), own_post

    def listing(self) -> list[dict]:
        """The Листочки screen: every sheet with its versions, the newest first."""
        rows = self.db.execute("select * from sheets where previous is null order by id desc limit 200")
        listed = []
        for first in map(dict, rows):
            chain = self.versions(first) if first["status"] == "posted" else [first]
            latest = chain[-1]
            chat = self.inbox.chat(first["chat_id"])
            listed.append({
                "id": latest["id"], "first": first["id"], "versions": len(chain), "status": first["status"],
                "error": first["error"], "course": latest["course"],
                "title": self.subjects.title(latest["course"]) if latest["course"] else None,
                "number": latest["number"], "name": latest["name"], "due": latest["due"],
                "chat": chat["title"] if chat else None, "own": first["chat_id"] == OWN_CHAT,
                "created_at": first["created_at"],
                "updated_at": latest["created_at"], "summary": first["summary"],
                "file": self.files.by_hash(latest["sha256"]) is not None,
            })
        return listed
