"""Листочки (§6.3, D14): PDFs with tasks from the chats, kept as rows here; the web app's shelves show them.

1. The batch reads a PDF by its name and its first page (a scan: the first page as an image) and says whether it is a
   sheet, with its course, number and due date (processing/batches.py). The sheet becomes a row here, `new`, in the
   transaction of the batch, so nothing is lost when a run fails. The same file again (forwarded, posted twice)
   is no new sheet.
2. Before it is published, the sheet is prepared once: its pages are read locally for the next version's diff, and
   it is matched against earlier sheets (`previous`). A newer version: the same message with a new file, or the same
   chat and course with the same number or almost the same file name (no number to tell them apart), the older sheet
   at most 30 days old. With no certainty it is a new sheet; the owner can join it to an older one in the web app.
3. With `pdf_content` on in the chat, the model reads the sheet (task "pdf", an optional one: off above 80 % of the
   limit): at most `pdf_pages` pages and `pdf_tokens` tokens, what comes first. It writes a short summary of it
   and gives the deadlines of its pages; they join the deadlines from the chat (source tg, the sheet's message), and an
   edit of the message's text does not withdraw them, its deletion does. For a newer version the model sees only the
   diff of the texts and says what changed. The summary and the changes are written in the app's language (D41),
   as it is when the sheet is prepared.

4. The owner changes a sheet's course, number, due date and summary in the web app, and adds sheets of their own:
   a PDF uploaded there, of the chat OWN_CHAT (D44). An own sheet is published as a chat's, gets newer versions by
   the same rules among the own ones, and is the only kind the owner may delete.
5. A sheet without a due date is due before the next seminar of its course (`due_assumed`): the first seminar or
   practical class in the timetable as the feeds show it (corrections applied, cancelled ones skipped) that starts
   after its message, within 21 days (processing/schedule.py, `next_class`); a course without seminars takes its next
   class of any kind; none found, no date. The date is a deadline of its own (source tg, the sheet's message,
   `assumed`), "<abbr> sheet <number>", and both are shown as assumed. It follows the timetable: synced with the
   Midterms after every RUZ poll, batch run and correction by the owner (`follow`), it moves with its class and is
   withdrawn when no class is left; once the class has started it stays. A real date wins and the mark goes: the
   sheet's pages, a later message (the batch moves the assumed deadline or gives the same work with a date), or the
   owner (the sheet's date, or the date of its deadline). A newer version takes the older one's
   assumed deadline over and finds its own date the same way. Sheets of before the rule are not assumed.
6. A prepared sheet, a newer version too, is copied into the library (D58, §6.8) as kind `sheet`, at the class of its
   course that was on when it was sent (the seminar it was handed out at), else at its course alone: there it outlives
   the 14 days of its file here.

Publishing is outputs/sheets.py; the status `posted` means published (D69). Writing methods commit themselves,
except `add`: the batch calls it inside its own transaction.
"""

import asyncio
import datetime as dt
import logging
import re
import shutil
from collections.abc import Callable

from .. import clock
from ..collectors.files import OWN_CHAT, Files
from ..collectors.inbox import MAILBOX, Inbox
from ..i18n import Phrase, tr
from ..library import Library
from ..llm.budget import CHARS_PER_TOKEN, PAGE_TOKENS, BudgetExceeded
from ..llm.client import Document, Haiku, LLMError
from ..settings import ChatSettings, Settings
from ..store import Store
from . import pdf
from .deadlines import Deadlines, same_label
from .schedule import SHEET_DAYS, MidtermChanges, Schedule
from .subjects import Subjects, clean
from .todo import Deadline

log = logging.getLogger(__name__)

VERSION_WINDOW = dt.timedelta(days=30)  # D14: an older sheet gets no newer version by the rules
TEXT_PAGES = 50           # read locally for the diff of the next version
TEXT_CHARS = 200_000
DIFF_CHARS = 8_000        # of the diff sent to the model
SUMMARY_CHARS = 600
CHANGES_CHARS = 400
LABEL_CHARS = 15          # a deadline label, as in the batches (Tablo's column, §2.1)
MAX_TOKENS = 2_000
FOLLOW_WINDOW = dt.timedelta(days=SHEET_DAYS + 1)  # past that, no class can be a sheet's next seminar any more

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


def origin(sheet: dict) -> str:
    """The source of a sheet's message: a letter's PDF (D59) is of the mailbox, any other of a chat."""
    return "mail" if sheet["chat_id"] == MAILBOX else "tg"


def number_key(number: str | None) -> str:
    """'№ 3' -> '3', 'ДЗ-2.1' -> 'дз-2.1': what two sheets are compared by."""
    return re.sub(r"[\s№#]+", "", (number or "").casefold())


def sheet_label(course: str, number: str) -> str:
    """The label of a sheet's assumed deadline, as the batches write a sheet's: "Calc sheet 3"."""
    return " ".join(f"{course} sheet {number}".split())


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
        self.schedule: Schedule | None = None  # where the next seminar is found; without it no date is assumed
        # The owner's changes of the assumed deadlines (the refs it touched): no news to notify of.
        self.on_change: Callable[[set[str]], None] | None = None
        self.library: Library | None = None  # where every sheet's PDF is kept for good (D58)

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
            "insert into sheets(chat_id, msg_id, name, sha256, course, number, due, due_assumed, sent_at, status, "
            "created_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?)",
            (chat_id, message["msg_id"], file["name"] or "sheet.pdf", file["sha256"],
             sheet["course"] or None, sheet["number"], sheet["due"], int(not sheet["due"]), message["at"],
             clock.local_iso(at)))
        return cursor.lastrowid

    def add_own(self, file: dict, course: str | None, number: str, due: str | None, at: dt.datetime) -> int:
        """An own sheet of an uploaded PDF (`file`: its files row); it waits to be published as a chat's."""
        with self.db:
            return self.db.execute(
                "insert into sheets(chat_id, msg_id, name, sha256, course, number, due, due_assumed, sent_at, "
                "status, created_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?)",
                (OWN_CHAT, file["msg_id"], file["name"] or "sheet.pdf", file["sha256"], course or None, number, due,
                 int(not due), clock.local_iso(at), clock.local_iso(at))).lastrowid

    def edit(self, sheet_id: int, fields: dict) -> dict | None:
        """The owner's change: course, number and due date of the sheet as it is now (its newest version), the
        summary of its first version. A due date given (or cleared) is real: the assumed
        one and its mark go; a new course or number makes the assumed date follow it. The sheet, or None for no such
        sheet."""
        sheet = self.get(sheet_id)
        if sheet is None:
            return None
        touched: set[int] = set()
        with self.db:
            for name in ("course", "number"):
                if name in fields:
                    self.db.execute(f"update sheets set {name} = ? where id = ?", (fields[name], sheet_id))
            if "summary" in fields:
                self.db.execute("update sheets set summary = ? where id = ?", (fields["summary"],
                                                                               self.first(sheet)["id"]))
            if "due" in fields:
                touched = self._settle(self.get(sheet_id), fields["due"])
            elif "course" in fields or "number" in fields:
                touched = self._follow(self.get(sheet_id), MidtermChanges())
        self._changed(touched)
        return self.get(sheet_id)

    def _changed(self, deadline_ids: set[int]) -> None:
        """The owner changed these deadlines: they are no news."""
        if deadline_ids and self.on_change is not None:
            self.on_change({f"dl:{deadline_id}" for deadline_id in deadline_ids})

    def remove_own(self, sheet_id: int) -> list[dict] | None:
        """An own sheet deleted with every version of it: the rows deleted, or None when it is no own sheet."""
        sheet = self.get(sheet_id)
        if sheet is None or sheet["chat_id"] != OWN_CHAT:
            return None
        chain = [self.first(sheet)]
        while (newer := self.db.execute("select * from sheets where previous = ?", (chain[-1]["id"],)).fetchone()):
            chain.append(dict(newer))
        if any(s["chat_id"] != OWN_CHAT for s in chain):
            return None  # a chat's sheet joined to it: that one is not the owner's to delete
        assumed = [row["id"] for s in chain for row in self._assumed(s["id"])]
        with self.db:
            self.db.execute(f"delete from sheets where id in ({', '.join('?' * len(chain))})",
                            [s["id"] for s in chain])
            if assumed:
                self.deadlines.drop(assumed, self.now())
        self._changed(set(assumed))
        return chain

    def waiting(self) -> list[dict]:
        """Sheets not published yet, oldest first."""
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

    def posted(self, sheet_id: int) -> None:
        """Published (D69)."""
        with self.db:
            self.db.execute("update sheets set status = 'posted' where id = ?", (sheet_id,))

    def file_gone(self) -> str:
        """The reason a sheet failed when its file is gone, for the web app: kept in the row, so in its language
        of now."""
        return tr(self.settings.get("language"), "the file is deleted already", "файл уже удалён")

    def failed(self, sheet_id: int, error: str) -> None:
        with self.db:
            self.db.execute("update sheets set status = 'failed', error = ? where id = ?", (error, sheet_id))

    # --- versions ----------------------------------------------------------------------------

    def _course_key(self, course: str | None) -> str:
        return (self.subjects.abbr(course) or clean(course)).casefold()

    def same(self, new: dict, old: dict) -> bool:
        """A newer version of `old` by the rules of D14 (the age is the caller's)."""
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

    # --- preparing for publishing ---------------------------------------------------------------

    async def prepare(self, sheet_id: int) -> dict | None:
        """Read the sheet, find what it updates, and with `pdf_content` let the model read it; once. None when its
        file is gone (the sheet fails). A prepared sheet also goes to the library."""
        sheet = await self._prepare(sheet_id)
        if sheet is not None and self.library is not None:
            try:
                await self._shelve(sheet)
            except Exception:  # the sheet is published all the same
                log.exception("sheet %d did not go to the library", sheet_id)
        return sheet

    async def _shelve(self, sheet: dict) -> int | None:
        """The sheet's PDF into the library (D58), kind `sheet`, at its seminar (the nearest one); once per file, so
        a new version in the same message is taken too. Its item's id, or None when it is there already."""
        if self.library.from_origin("sheet", sheet["chat_id"], sheet["msg_id"], sheet["sha256"]) is not None:
            return None
        path = self.files.by_hash(sheet["sha256"])
        if path is None:
            return None
        sent = dt.datetime.fromisoformat(sheet["sent_at"]).replace(tzinfo=clock.TZ) if sheet["sent_at"] else None
        found = self.library.find_class(sheet["course"], sent, seminar=True) if sheet["course"] and sent else None
        author = "owner" if sheet["chat_id"] == OWN_CHAT else \
            (self.inbox.message(sheet["chat_id"], sheet["msg_id"], origin(sheet)) or {}).get("sender_name") or ""

        async def copy(part) -> bool:
            await asyncio.to_thread(shutil.copyfile, path, part)
            return True

        return await self.library.add_download(
            copy, name=sheet["name"], media_type="application/pdf", kind="sheet", source="sheet",
            chat_id=sheet["chat_id"], msg_id=sheet["msg_id"], course=sheet["course"],
            class_uid=found["uid"] if found else None, title=" ".join(f"Sheet {sheet['number'] or ''}".split()),
            author=author, at=sent, note="" if sheet["course"] else "the sheet's course is not known")

    async def _prepare(self, sheet_id: int) -> dict | None:
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
            if previous is not None:
                self._inherit(previous, sheet)
            touched = self._follow(self.get(sheet_id), MidtermChanges())
        if sheet["chat_id"] == OWN_CHAT:
            self._changed(touched)  # an own sheet is the owner's change; a chat's is told after its batch run
        return self.get(sheet_id)

    def _save_deadlines(self, sheet: dict, deadlines: list[dict]) -> None:
        """Deadlines of the sheet's pages that the chat has not given yet; one the service only assumed is told
        for real now. Inside `with self.db`."""
        for deadline in deadlines:
            known = self.deadlines.find(deadline["label"], deadline["due"])
            if known is None:
                self.deadlines.add({"source": origin(sheet), "chat_id": sheet["chat_id"], "msg_ids": [sheet["msg_id"]],
                                    "course": sheet["course"], "label": deadline["label"], "due": deadline["due"],
                                    "sheet_id": sheet["id"]}, self.now())
            elif known["assumed"]:
                self.deadlines.confirm(known["id"])

    # --- the assumed due date (§6.3) ---------------------------------------------------------------

    def _assumed(self, sheet_id: int) -> list[dict]:
        """The sheet's assumed deadlines in force, oldest first (one, unless a version took another's over)."""
        return [row for row in self.deadlines.of_sheet(sheet_id)
                if row["assumed"] and row["moved_by"] is None and row["gone_at"] is None]

    def _label(self, sheet: dict) -> str:
        """The label of the sheet's assumed deadline: the course by its abbr once the dictionary knows it."""
        return sheet_label(self.subjects.abbr(sheet["course"]) or clean(sheet["course"]), sheet["number"])

    def _told(self, sheet: dict) -> dict | None:
        """The earliest real date of the sheet in force: a deadline read in its pages, an assumed one a message moved
        or told again, or the same work from the chats or mail due on the sheet's day or later."""
        from .merge import same_work  # here: merge -> hseapp -> batches -> sheets would be a cycle

        found = []
        for row in self.deadlines.of_sheet(sheet["id"]):
            latest = self.deadlines.latest(row["id"])
            if latest is not None and latest["moved_by"] is None and latest["gone_at"] is None and \
                    not latest["assumed"]:
                found.append(latest)
        if sheet["course"] and sheet["sent_at"]:
            label, day = self._label(sheet), dt.date.fromisoformat(sheet["sent_at"][:10])
            mine = Deadline(label, day, "study", "tg", self.subjects.abbr(sheet["course"]))
            for row in self.deadlines.upcoming(day, dt.timedelta()):
                if row["assumed"]:
                    continue
                theirs = Deadline(row["label"], dt.date.fromisoformat(row["due"]), "study", row["source"],
                                  self.subjects.abbr(row["course"]))
                # Without a number, "Calc sheet" would be the same work as every sheet of the course.
                if same_label(row["label"], label) or (sheet["number"] and same_work(mine, theirs)):
                    found.append(row)
        return min(found, key=lambda row: (row["due"], row["id"]), default=None)

    def _hidden(self, deadline_id: int) -> bool:
        """The owner hid the deadline (or changed it into an own copy): it is theirs now, it does not move."""
        return self.db.execute("select 1 from hidden_deadlines where ref = ?",
                               (f"dl:{deadline_id}",)).fetchone() is not None

    def _set_due(self, sheet: dict, due: str | None, assumed: bool, due_at: str | None) -> None:
        """The sheet's date as it is now."""
        if (sheet["due"], bool(sheet["due_assumed"]), sheet["due_at"]) == (due, assumed, due_at):
            return
        self.db.execute("update sheets set due = ?, due_assumed = ?, due_at = ? where id = ?",
                        (due, int(assumed), due_at, sheet["id"]))

    def _settle(self, sheet: dict, due: str | None, told: dict | None = None) -> set[int]:
        """The sheet's real date (None: no date at all): the mark goes, and its assumed deadlines move to the one that
        tells it (`told`; without one, a new one on that date), are told for real when the date is the same (or the
        owner hid them), or are withdrawn without a date. The deadlines touched. Inside `with self.db`."""
        self._set_due(sheet, due, False, None)
        touched: set[int] = set()
        for row in self._assumed(sheet["id"]):
            touched.add(row["id"])
            if self._hidden(row["id"]) or (told is None and row["due"] == due):
                self.deadlines.confirm(row["id"])
            elif due is None:
                self.deadlines.drop([row["id"]], self.now())
            else:
                if told is None:
                    told = self.deadlines.get(self.deadlines.add(
                        {"source": origin(sheet), "chat_id": sheet["chat_id"], "msg_ids": row["msg_ids"],
                         "course": row["course"], "label": row["label"], "due": due, "sheet_id": sheet["id"]},
                        self.now()))
                    touched.add(told["id"])
                self.deadlines.move(row["id"], told["id"])
        return touched

    def _inherit(self, previous: dict, sheet: dict) -> None:
        """A newer version takes the older one's assumed deadline over: moved into its own one if it has one, else
        made its own (with its message: deleting the older message does not withdraw it). Inside `with self.db`."""
        mine = self._assumed(sheet["id"])
        for row in self._assumed(previous["id"]):
            if mine:
                self.deadlines.move(row["id"], mine[0]["id"])
            else:
                self.db.execute("update deadlines set sheet_id = ?, chat_id = ?, msg_ids = ? where id = ?",
                                (sheet["id"], sheet["chat_id"], f"[{sheet['msg_id']}]", row["id"]))

    def follow(self) -> MidtermChanges:
        """Schedule.followers: the assumed due dates of the recent sheets follow the timetable and give way to real
        ones; what changed, as no news."""
        changes = MidtermChanges()
        since = clock.local_iso(self.now() - FOLLOW_WINDOW)
        rows = self.db.execute(
            "select * from sheets where status != 'failed' and sent_at >= ? and (due_assumed = 1 or id in "
            "(select sheet_id from deadlines where assumed = 1 and moved_by is null and gone_at is null)) "
            "order by id", (since,)).fetchall()
        with self.db:
            for row in rows:
                if self._latest(row["id"]):
                    self._follow(dict(row), changes)
        return changes

    def _follow(self, sheet: dict, changes: MidtermChanges) -> set[int]:
        """One sheet: a real date told since settles it; else its assumed date is the next seminar's as the timetable
        is now (unless that class has started or the owner hid the deadline). The deadlines touched. Inside
        `with self.db`."""
        current = self._assumed(sheet["id"])
        if not (sheet["due_assumed"] or current):
            return set()
        if (told := self._told(sheet)) is not None:
            changes.moved += [(row["id"], told["id"]) for row in current]
            return self._settle(sheet, told["due"], told)
        if not sheet["due_assumed"]:  # a version with a date of its own took an older one's assumed deadline over
            return self._settle(sheet, sheet["due"])
        assumed = [row for row in self.deadlines.of_sheet(sheet["id"]) if row["assumed"]]
        if not current and assumed and assumed[-1]["gone_at"] and assumed[-1]["moved_by"] is None \
                and sheet["due"] is not None:  # withdrawn with its message: the sheet has no date any more
            self._set_due(sheet, None, False, None)
            return set()
        if self.schedule is None or (sheet["due_at"] and sheet["due_at"] <= clock.local_iso(self.now())) or \
                any(self._hidden(row["id"]) for row in current):
            return set()
        touched: set[int] = set()
        keep = current[0] if current else None
        for extra in current[1:]:  # an older version's, taken over
            self.deadlines.move(extra["id"], keep["id"])
            touched.add(extra["id"])
        entry = self.schedule.next_class(sheet["course"], dt.datetime.fromisoformat(sheet["sent_at"])) \
            if sheet["sent_at"] else None
        due = entry.start.date().isoformat() if entry else None
        due_at = entry.start.isoformat(timespec="seconds") if entry else None
        self._set_due(sheet, due, True, due_at)
        label = self._label(sheet)
        new = {"source": origin(sheet), "chat_id": sheet["chat_id"], "msg_ids": [sheet["msg_id"]],
               "course": sheet["course"], "label": label, "due": due, "sheet_id": sheet["id"], "assumed": True}
        if due is None:
            if keep is not None:
                self.deadlines.drop([keep["id"]], self.now())
                changes.gone.append(keep["id"])
                touched.add(keep["id"])
        elif keep is None:
            new_id = self.deadlines.add(new, self.now())
            changes.deadlines.append(new_id)
            touched.add(new_id)
        elif keep["due"] != due:
            new_id = self.deadlines.add({**new, "msg_ids": keep["msg_ids"]}, self.now())
            self.deadlines.move(keep["id"], new_id)
            changes.moved.append((keep["id"], new_id))
            touched |= {keep["id"], new_id}
        elif (keep["label"], keep["course"]) != (label, sheet["course"]):
            self.deadlines.rename(keep["id"], label, sheet["course"])  # the dictionary named the course, or the owner
            touched.add(keep["id"])
        return touched

    def _context(self, sheet: dict) -> list[str]:
        chat = self.inbox.chat(sheet["chat_id"])
        abbr = self.subjects.abbr(sheet["course"])
        where = "the university mailbox" if origin(sheet) == "mail" else \
            f"\"{chat['title'] if chat else sheet['chat_id']}\""
        return [f"Posted: {dt.datetime.fromisoformat(sheet['created_at']):%a %Y-%m-%d}",
                f"Chat: {where}",
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
        """What changed against the older version, from the diff alone (D14)."""
        changed = pdf.diff(old, new, DIFF_CHARS)
        if not changed:  # a scan has no text to compare
            return tr(self.settings.get("language"), "The text is the same.", "Текст не изменился.") \
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
        """A system prompt asking for the app's language."""
        return system.format(**PROMPT_WORDS[self.settings.get("language")])

    # --- the owner's joining (the web app) -------------------------------------------------------

    async def join(self, sheet_id: int, previous_id: int) -> dict:
        """Make a sheet published as new a newer version of another one: the sheet now. ValueError (with a Phrase)
        when it cannot be: a sheet with versions of its own, the same sheet."""
        sheet, older = self.get(sheet_id), self.get(previous_id)
        if sheet is None or older is None or sheet["status"] != "posted" or older["status"] != "posted":
            raise ValueError(Phrase("no such sheet", "такого листка нет"))
        if sheet["previous"] is not None or not self._latest(sheet_id):
            raise ValueError(Phrase("this sheet has versions already: only a sheet without them can be joined",
                                    "у этого листка уже есть версии: присоединить можно только листок без них"))
        older = self.versions(self.first(older))[-1]  # the newest version of the other sheet
        if self.first(older)["id"] == sheet_id:
            raise ValueError(Phrase("it is the same sheet", "это тот же листок"))
        changes = None
        if self.chat_settings.get(sheet["chat_id"], "pdf_content"):
            changes = await self._describe(sheet, older["text"] or "", sheet["text"] or "")
        with self.db:
            self.db.execute("update sheets set previous = ?, version = ?, changes = ? where id = ?",
                            (older["id"], older["version"] + 1, changes, sheet_id))
            self._inherit(older, sheet)
            touched = self._follow(self.get(sheet_id), MidtermChanges())
        self._changed(touched)
        return self.get(sheet_id)

    def listing(self) -> list[dict]:
        """The Листочки screen: every sheet with its versions, the newest first."""
        rows = self.db.execute("select * from sheets where previous is null order by id desc limit 200")
        listed = []
        for first in map(dict, rows):
            chain = self.versions(first) if first["status"] == "posted" else [first]
            latest = chain[-1]
            chat = self.inbox.chat(first["chat_id"])
            if origin(first) == "mail":
                chat = {"title": "✉ " + (self.inbox.subject([first["msg_id"]]) or "mail")}
            listed.append({
                "id": latest["id"], "first": first["id"], "versions": len(chain), "status": first["status"],
                "error": first["error"], "course": latest["course"],
                "title": self.subjects.title(latest["course"]) if latest["course"] else None,
                "number": latest["number"], "name": latest["name"], "due": latest["due"],
                "due_assumed": bool(latest["due_assumed"]), "due_at": latest["due_at"],
                "chat": chat["title"] if chat else None, "own": first["chat_id"] == OWN_CHAT,
                "created_at": first["created_at"],
                "updated_at": latest["created_at"], "summary": first["summary"],
                "file": self.files.by_hash(latest["sha256"]) is not None,
            })
        return listed
