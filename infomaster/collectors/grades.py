"""Teachers' grade sheets (§5.2, §6.7, Tablo's §3.1, §3.3): the collector reads, the processing lays out.

The sheets are open to anyone with the link (the setting grades_sheets): each is downloaded as xlsx every 30 minutes.
A Google Sheet is exported as xlsx and its title is read once from the HTML view; a Dropbox link to an .xlsx file is
downloaded as it is (dl=1) and its title is the file's name without the extension.

In every visible tab the student's own row is found by the name (grades_student); of a tab the top rows above it (at
most TOP_ROWS) and that row are kept (TabView). Where the header ends among the top
rows is the model's to say, so the top rows go to it whole, other students' rows among them (§10); the own name does
not. The layout by Haiku, the 0-10 scale and what counts as a new grade are processing/grades.py's: `on_sheet` gets a
sheet read and says what came of it (Read). The own row's formulas and the teachers' comments on its cells are read
too (17.6, D75), still read-only: a full load of a large gradebook takes more memory than the server's container has.

Failures count per sheet: the download, a file that is no workbook (a sheet closed to the link gives a login page,
a deleted or private Dropbox link an HTML page),
no own row in any tab (a wrong name, or the row is not there yet: the grades kept are not dropped for it), the layout
(Haiku, the limit). A failed sheet keeps its grades as they were. A sheet failing for 12 hours is told to the owner
once (pushed), and once more when it reads again; the first read of a sheet is told too. New grades are not pushed:
the web app and the API show them (D55).
"""

import asyncio
import datetime as dt
import html
import io
import logging
import posixpath
import re
import zipfile
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import PurePosixPath
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit
from xml.etree import ElementTree

import aiohttp

from .. import clock
from ..i18n import Alert, Phrase, say
from ..llm.budget import BudgetExceeded
from ..llm.client import LLMError
from ..net import public_session
from ..settings import DROPBOX_PREFIX, Settings, dropbox_file, grade_sheet_id, not_xlsx

log = logging.getLogger(__name__)

EXPORT_URL = "https://docs.google.com/spreadsheets/d/{id}/export?format=xlsx"
TITLE_URL = "https://docs.google.com/spreadsheets/d/{id}/htmlview"
POLL_EVERY = 30 * 60        # seconds (Tablo's poll_minutes)
TIMEOUT = 120               # seconds
XLSX_MAX = 20 * 2**20       # bytes of a downloaded sheet (D79: the container has 256 MB)
UNZIPPED_MAX = 120 * 2**20  # bytes of its parts unpacked, all together: a small file may unpack to gigabytes
ROWS_MAX, COLUMNS_MAX = 5000, 400  # of a tab read: a gradebook is far smaller; one cell far away pads every row
STALE_AFTER = dt.timedelta(hours=12)
TOP_ROWS = 15               # rows of a tab above the own row the model sees: the header is somewhere among them
COURSE_FROM_TITLE = 8       # characters of the sheet's title that name the course when its label is not set
NO_ROW = Phrase("the own row is on no tab: check the name in the web app, Machine → Gradebooks",
                "своей строки нет ни на одной вкладке: проверьте ФИО в приложении, Машина → Таблицы оценок")


def sheet_id(url: str) -> str:
    """A Google Sheet's id, or "dropbox-" and the id of a Dropbox link; SettingsError (a ValueError) for other links."""
    return grade_sheet_id(url)


DROPBOX_CONTENT = "dl.dropboxusercontent.com"


def is_dropbox(sid: str) -> bool:
    return sid.startswith(DROPBOX_PREFIX)


def download_url(url: str) -> str:
    """Where a sheet's xlsx comes from: Google's export, or the Dropbox link's file on Dropbox's content host with
    dl=1 (rlkey and the other parameters kept). Some resolvers block www.dropbox.com, and dropbox.com redirects there:
    dl.dropboxusercontent.com serves the same shared file directly."""
    sid = sheet_id(url)
    if not is_dropbox(sid):
        return EXPORT_URL.format(id=sid)
    parts = urlsplit(url.strip())
    query = parse_qsl(parts.query, keep_blank_values=True)
    query = [(key, "1" if key == "dl" else value) for key, value in query] + \
        ([] if any(key == "dl" for key, _ in query) else [("dl", "1")])
    return urlunsplit(("https", DROPBOX_CONTENT, parts.path, urlencode(query), ""))


def file_title(name: str) -> str | None:
    """A Dropbox sheet's title: its file name without the extension."""
    return PurePosixPath(name).stem.strip() or None


# --- reading a workbook ------------------------------------------------------------------


def norm(text: str) -> str:
    """Lower case, ё as е, punctuation but "-" as spaces: "ИВАНОВ  Иван," -> "иванов иван"."""
    text = text.lower().replace("ё", "е")
    return " ".join(re.sub(r"[^\w\s-]", " ", text).split())


def matches(cell, student: list[str]) -> bool:
    """The cell starts with the words of the name: "Иванов Иван" finds "ИВАНОВ Иван Петрович"."""
    if not isinstance(cell, str):
        return False
    words = norm(cell).split()
    return len(words) >= len(student) and words[:len(student)] == student


def text(value) -> str:
    if value is None:
        return ""
    if isinstance(value, dt.datetime):
        return value.date().isoformat() if value.time() == dt.time() else value.isoformat(" ", "minutes")
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value).strip()


def column(index: int) -> str:
    """0 -> A, 26 -> AA."""
    letters = ""
    index += 1
    while index:
        index, rest = divmod(index - 1, 26)
        letters = chr(65 + rest) + letters
    return letters


@dataclass(frozen=True)
class TabView:
    """What is kept of one tab: its top rows and the student's own row."""

    tab: str
    rows: list[tuple[int, dict[str, str]]]  # the top rows that are not empty: (row number, column letter -> text)
    mine: dict[str, object]       # column letter -> raw value in the student's row, without the name
    name_col: str
    own_row: int                  # its number in the tab
    # column letter -> the share of the other students with a number above zero there (17.3, D72): whether a work
    # is graded for the group, so the student's zero in it is a real one. Only the share is kept, no one's grade.
    graded: dict[str, float] = field(default_factory=dict)
    # 17.6: the own row's formulas (column letter -> "=..."), and the teachers' comments on its cells (column letter
    # -> text, without the author). Never another row's comments.
    formulas: dict[str, str] = field(default_factory=dict)
    notes: dict[str, str] = field(default_factory=dict)

    def header(self, numbers: list[int]) -> list[dict[str, str]]:
        """The rows the model named the header, in tab order."""
        return [cells for number, cells in self.rows if number in numbers]

    def column_header(self, col: str, numbers: list[int]) -> str:
        return " / ".join(row[col] for row in self.header(numbers) if row.get(col))


def positive(value) -> bool:
    if isinstance(value, bool) or value is None:
        return False
    try:
        return float(str(value).strip().replace(",", ".")) > 0
    except ValueError:
        return False


NAME = re.compile(r"[^\W\d_][\w.\-]*(?: [^\W\d_][\w.\-]*){1,3}")  # two to four words of letters: a person


GROUP_HEADERS = {"группа", "group"}  # a column of the students' groups: a stream's sheet is graded group by group


def groups(rows: list[list], own_i: int, name_j: int) -> list[str | None] | None:
    """Each row's group when a column above the own row is headed «Группа»: the last group named at or above the row
    (a group's number stands once at its first row, or on every row). None without such a column."""
    j = next((j for row in rows[:own_i] for j, c in enumerate(row)
              if j != name_j and isinstance(c, str) and " ".join(c.split()).casefold() in GROUP_HEADERS), None)
    if j is None:
        return None
    out, current = [], None
    for row in rows:
        current = text(row[j]) if j < len(row) and text(row[j]) else current
        out.append(current)
    return out


def graded_shares(rows: list[list], own_i: int, name_j: int) -> dict[str, float]:
    """For each column of the own row: the share of the other students (a row whose name column holds a person's
    name, not a header's words) who have a number above zero in it. In a stream's sheet with a group column they are
    the student's own group (D80): another group not graded yet does not make the own group's zero a placeholder."""
    others = [i for i, row in enumerate(rows) if i != own_i and name_j < len(row) and isinstance(row[name_j], str)
              and NAME.fullmatch(" ".join(row[name_j].split())) and not any(ch.isdigit() for ch in row[name_j])]
    group = groups(rows, own_i, name_j)
    if group and group[own_i] and (same := [i for i in others if group[i] == group[own_i]]):
        others = same
    others = [rows[i] for i in others]
    if not others:
        return {}
    return {column(j): round(sum(positive(row[j]) for row in others if j < len(row)) / len(others), 3)
            for j, value in enumerate(rows[own_i]) if j != name_j and text(value)}


def read_views(data: bytes, student: str) -> list[TabView]:
    """The visible tabs that have the student's row. ValueError when the data is no workbook.

    Read-only, as the server's memory asks (a full load of a large gradebook took more than the container has): the
    values in one pass, the own row's formulas in a second, the comments from the file itself (read-only openpyxl
    skips them)."""
    from openpyxl import load_workbook

    target = norm(student).split()
    check_zip(data)
    try:
        wb = load_workbook(io.BytesIO(data), read_only=True, data_only=True)
    except Exception as e:  # a login page or a broken file: zipfile, openpyxl and XML errors alike
        raise ValueError(f"not a workbook, is the sheet open to anyone with the link? ({type(e).__name__})") from None
    views = []
    try:
        for ws in wb.worksheets:
            if ws.sheet_state != "visible":
                continue
            ws.reset_dimensions()  # the file's own <dimension> would pad every row to it
            rows = []
            for cells in ws.iter_rows(values_only=True):
                if len(rows) >= ROWS_MAX:
                    break
                rows.append(list(cells[:COLUMNS_MAX]))
            own = next(((i, j) for i, r in enumerate(rows) for j, c in enumerate(r) if matches(c, target)), None)
            if own is None:
                continue
            own_i, name_j = own
            top = [(i + 1, cells) for i, row in enumerate(rows[:min(own_i, TOP_ROWS)])
                   if (cells := {column(j): text(c) for j, c in enumerate(row) if text(c)})]
            mine = {column(j): c for j, c in enumerate(rows[own_i]) if j != name_j and text(c)}
            views.append(TabView(tab=ws.title, rows=top, mine=mine, name_col=column(name_j), own_row=own_i + 1,
                                 graded=graded_shares(rows, own_i, name_j)))
    finally:
        wb.close()
    if not views:
        return views
    try:
        formulas = own_formulas(data, {v.tab: v.own_row for v in views})
    except Exception as e:  # the values are read: the formulas are an extra
        log.warning("the formulas of a sheet cannot be read: %s", e)
        formulas = {}
    try:
        notes = own_notes(data, {v.tab: v.own_row for v in views})
    except Exception as e:
        log.warning("the comments of a sheet cannot be read: %s", e)
        notes = {}
    return [TabView(tab=v.tab, rows=v.rows, mine=v.mine, name_col=v.name_col, own_row=v.own_row, graded=v.graded,
                    formulas={c: f for c, f in formulas.get(v.tab, {}).items() if c != v.name_col},
                    notes={c: n for c, n in notes.get(v.tab, {}).items() if c != v.name_col}) for v in views]


NOTE_CHARS = 1_000


def check_zip(data: bytes) -> None:
    """ValueError for a workbook whose parts unpack to more than UNZIPPED_MAX (a zip bomb), or that is no zip."""
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as z:
            unpacked = sum(info.file_size for info in z.infolist())
    except zipfile.BadZipFile:
        raise ValueError("not a workbook, is the sheet open to anyone with the link? (BadZipFile)") from None
    if unpacked > UNZIPPED_MAX:
        raise ValueError(f"the workbook unpacks to {unpacked >> 20} MB: too big to read")


def own_formulas(data: bytes, own_rows: dict[str, int]) -> dict[str, dict[str, str]]:
    """The formulas of the own row of each tab: tab -> column letter -> "=...". A formula's text only, no values."""
    from openpyxl import load_workbook

    found: dict[str, dict[str, str]] = {}
    wb = load_workbook(io.BytesIO(data), read_only=True, data_only=False)
    try:
        for ws in wb.worksheets:
            row = own_rows.get(ws.title)
            if row is None:
                continue
            ws.reset_dimensions()
            for cells in ws.iter_rows(min_row=row, max_row=row, values_only=True):
                found[ws.title] = {column(j): c for j, c in enumerate(cells[:COLUMNS_MAX])
                                   if isinstance(c, str) and c.startswith("=")}
    finally:
        wb.close()
    return found


MAIN = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
RELS = "{http://schemas.openxmlformats.org/package/2006/relationships}"
DOC_RID = "{http://schemas.openxmlformats.org/officeDocument/2006/relationships}id"
CELL_REF = re.compile(r"([A-Z]+)(\d+)")


def own_notes(data: bytes, own_rows: dict[str, int]) -> dict[str, dict[str, str]]:
    """The comments on the own row's cells of each tab, read from the xlsx's comment parts: tab -> column -> text.
    Only the own row's are taken, and never the author (a teacher's name)."""
    found: dict[str, dict[str, str]] = {}
    with zipfile.ZipFile(io.BytesIO(data)) as z:
        names = set(z.namelist())

        def rels(part: str) -> dict[str, tuple[str, str]]:
            path = posixpath.join(posixpath.dirname(part), "_rels", posixpath.basename(part) + ".rels")
            if path not in names:
                return {}
            return {e.get("Id"): (e.get("Type", ""), target(part, e.get("Target", "")))
                    for e in ElementTree.fromstring(z.read(path)) if e.tag == RELS + "Relationship"}

        def target(part: str, to: str) -> str:
            if to.startswith("/"):
                return to.lstrip("/")
            return posixpath.normpath(posixpath.join(posixpath.dirname(part), to))

        book = ElementTree.fromstring(z.read("xl/workbook.xml"))
        sheets = rels("xl/workbook.xml")
        for sheet in book.iter(MAIN + "sheet"):
            row = own_rows.get(sheet.get("name"))
            part = sheets.get(sheet.get(DOC_RID), ("", ""))[1]
            if row is None or part not in names:
                continue
            for kind, comments in rels(part).values():
                if not kind.endswith("/comments") or comments not in names:
                    continue
                for c in ElementTree.fromstring(z.read(comments)).iter(MAIN + "comment"):
                    m = CELL_REF.fullmatch(c.get("ref", "").replace("$", ""))
                    if m is None or int(m.group(2)) != row:
                        continue
                    note = "\n".join(line.strip() for line in "".join(t.text or "" for t in c.iter(MAIN + "t"))
                                      .strip().splitlines()).strip()
                    if note:
                        found.setdefault(sheet.get("name"), {})[m.group(1)] = note[:NOTE_CHARS]
    return found


# --- the source ----------------------------------------------------------------------------


@dataclass(frozen=True)
class Sheet:
    """One sheet read: what on_sheet gets."""
    id: str
    url: str
    title: str | None       # as Google names it, or a Dropbox file's name; None when the title could not be fetched
    course: str             # the short name in front of the labels: the setting's label, else the title's start
    student: str
    views: list[TabView]


@dataclass(frozen=True)
class Read:
    """What on_sheet made of a sheet."""
    first: bool             # the baseline: the first read of the sheet records no new grades
    items: int              # grades in the sheet now
    unread: tuple[str, ...] = ()  # tabs the model could not lay out: skipped until their header changes


@dataclass
class SheetStatus:
    label: str = ""         # the setting's: names the sheet until its title is known
    title: str | None = None
    ok_at: str | None = None
    error: str | Phrase | None = None
    tabs: int = 0
    items: int = 0
    unread: tuple[str, ...] = ()
    failing_since: dt.datetime | None = field(default=None, repr=False)
    told: bool = False

    @property
    def name(self) -> str:
        return self.title or self.label or "?"

    def public(self) -> dict:
        return {"title": self.title, "ok_at": self.ok_at, "error": self.error, "tabs": self.tabs, "items": self.items,
                "unread": list(self.unread)}


def short_course(label: str, title: str | None, sid: str) -> str:
    return label or (title or sid)[:COURSE_FROM_TITLE].strip()


class GradeSheets:
    def __init__(self, settings: Settings, now: Callable[[], dt.datetime] = clock.now_utc):
        self.settings, self.now = settings, now
        self.on_sheet: Callable[[Sheet], Awaitable[Read]] | None = None
        self.notify: Callable[[Phrase], None] | None = None        # a line for the owner (Web Push)
        self.sheets: dict[str, SheetStatus] = {}                    # by sheet id, the configured sheets only
        self._wake = asyncio.Event()

    def configured(self) -> bool:
        return bool(self.settings.get("grades_student") and self.settings.get("grades_sheets"))

    def status(self) -> dict:
        """The sheets by id; never the links or the name."""
        return {"configured": self.configured(), "sheets": {sid: st.public() for sid, st in self.sheets.items()}}

    def wake(self) -> None:
        """The settings changed: poll now, not in 30 minutes."""
        self._wake.set()

    async def run(self) -> None:
        while True:
            try:
                await self.poll()
            except Exception:
                log.exception("grades poll failed")
            await self._nap(POLL_EVERY)

    async def _nap(self, seconds: float) -> None:
        """Cleared after waking, not before: a change during a poll gives one more poll."""
        try:
            await asyncio.wait_for(self._wake.wait(), seconds)
        except TimeoutError:
            pass
        self._wake.clear()

    async def poll(self) -> None:
        student, sheets = self.settings.get("grades_student"), self.settings.get("grades_sheets")
        ids = [sheet_id(sheet["url"]) for sheet in sheets]
        self.sheets = {sid: self.sheets.get(sid) or SheetStatus() for sid in ids}
        if not student or not sheets:
            return
        async with public_session(timeout=aiohttp.ClientTimeout(total=TIMEOUT)) as session:
            for sid, sheet in zip(ids, sheets, strict=True):
                await self._poll_sheet(session, sid, sheet, student)

    async def _poll_sheet(self, session: aiohttp.ClientSession, sid: str, sheet: dict, student: str) -> None:
        st = self.sheets[sid]
        st.label = sheet["label"]
        try:
            if is_dropbox(sid):  # named before the download, so that a failing sheet has its name too
                st.title = st.title or file_title(dropbox_file(sheet["url"])[1])
            async with session.get(download_url(sheet["url"])) as response:  # redirects are followed
                response.raise_for_status()
                if (response.content_length or 0) > XLSX_MAX:
                    raise ValueError(f"the sheet is over {XLSX_MAX >> 20} MB")
                data = bytearray()
                async for chunk in response.content.iter_chunked(1 << 16):
                    data += chunk
                    if len(data) > XLSX_MAX:
                        raise ValueError(f"the sheet is over {XLSX_MAX >> 20} MB")
                data = bytes(data)
                disposition = response.content_disposition
            if is_dropbox(sid):
                if disposition is not None and (named := disposition.filename):  # the file may have been renamed
                    if not named.lower().endswith(".xlsx"):
                        raise ValueError(not_xlsx(named))
                    st.title = file_title(named) or st.title
            elif st.title is None:  # after the file: a sheet closed to the link has the title of a login page
                st.title = await fetch_title(session, sid)
            views = await asyncio.to_thread(read_views, data, student)
            if not views:
                raise ValueError(NO_ROW)
            read = await self.on_sheet(Sheet(sid, sheet["url"], st.title, short_course(sheet["label"], st.title, sid),
                                             student, views))
        except aiohttp.ClientResponseError as e:  # its text carries the URL, the sheet's id is in it
            self._failed(st, f"HTTP {e.status} {e.message}".strip())
            return
        except (aiohttp.ClientError, TimeoutError, ValueError, OSError) as e:
            self._failed(st, e.args[0] if e.args and isinstance(e.args[0], Phrase) else f"{type(e).__name__}: {e}")
            return
        except (LLMError, BudgetExceeded) as e:  # the layout; asked again at the next poll
            self._failed(st, str(e))
            return
        except Exception as e:  # one sheet must not stop the others
            log.exception("grades of sheet %s failed", st.name)
            self._failed(st, f"{type(e).__name__}: {e}")
            return
        st.tabs, st.items, st.unread = len(views), read.items, read.unread
        self._recovered(st)
        if read.first:
            self._notify(Phrase(f"The grade sheet «{st.name}» is connected: tabs with the own row: {len(views)}, "
                                f"grades: {read.items}. They are in the course lenses of the web app.",
                                f"Таблица оценок «{st.name}» подключена: вкладок со своей строкой — {len(views)}, "
                                f"оценок — {read.items}. Они в линзах курсов в приложении."))

    def _failed(self, st: SheetStatus, error: str | Phrase) -> None:
        st.error = error
        log.warning("grades sheet %s failed: %s", st.name, error)
        now = self.now()
        st.failing_since = st.failing_since or now
        if not st.told and now - st.failing_since >= STALE_AFTER:
            st.told = True
            since = st.failing_since.astimezone(clock.TZ)
            self._notify(Alert(f"The grade sheet «{st.name}» cannot be read since {since:%d.%m %H:%M}: "
                               f"{say(error, 'en')}.\nIts grades are not updated.",
                               f"Таблица оценок «{st.name}» не читается с {since:%d.%m %H:%M}: {say(error, 'ru')}.\n"
                               "Оценки из неё не обновляются."))

    def _recovered(self, st: SheetStatus) -> None:
        if st.told:
            self._notify(Phrase(f"The grade sheet «{st.name}» is read again.",
                                f"Таблица оценок «{st.name}» снова читается."))
        st.ok_at, st.error = clock.local_iso(self.now()), None
        st.failing_since, st.told = None, False

    def _notify(self, line: Phrase) -> None:
        if self.notify is not None:
            self.notify(line)


async def fetch_title(session: aiohttp.ClientSession, sid: str) -> str | None:
    """The sheet's title from its HTML view; None on any failure (asked again at the next poll)."""
    try:
        async with session.get(TITLE_URL.format(id=sid)) as response:
            if response.status != 200:
                return None
            page = await response.text()
    except (aiohttp.ClientError, TimeoutError, UnicodeDecodeError):
        return None
    found = re.search(r"<title>(.*?)</title>", page, re.S)
    if found is None:
        return None
    title = html.unescape(found[1]).strip().removesuffix(" - Google Диск").removesuffix(" - Google Drive").strip()
    return title or None
