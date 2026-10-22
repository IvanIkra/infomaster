"""Grades (§6.7; Tablo's §3.4–§3.7, §10.2): the layout of a tab by Haiku, grades on 0-10, the new ones.

Haiku sees a tab's name, its top rows and the student's own row (collectors/grades.py) and says which of the top
rows are the header, which columns are graded works, their maximum, a bonus column and a short English label (task
"grades": Tablo's prompt, plus the header rows: Tablo guessed where the header ends, and a sheet with students in
lower case or a statistics row above them broke the guess). The answer is cached in kv per sheet, tab and label
budget together with the text of the header rows it named: while that text stays, the layout stands, whatever the
other rows do (another student's grade, a new student, an average); a changed header asks again. Without header
rows named, any change in the top rows asks again. An answer the model cannot give (refused, cut) is cached as a
skipped tab until the top rows change: the same input would fail again, at every poll.

A grade's key is its place: sheet, tab, column and the column's header, not its label, so a new label is no new grade.
The first read of a sheet is the baseline: it fills grade_items and records nothing. After it, a work new to the
sheet, or a changed grade or bonus, is a new grade (grade_events), unless it is a placeholder zero (Tablo's
compromise: a real zero for a work never shows). What Tablo did not have: a grade that leaves the sheet (a tab
hidden, the row gone from one tab, a cell cleared) is kept as gone, so its return with the same grade is no new grade;
a column moved by one inserted before it keeps its grade (matched by tab, header and grade among the works that left
in the same read); a work listed twice by the model counts once; a tab with many columns gets room for its answer
(TOKENS_PER_WORK), and a tab the model could not answer for is told in the status rather than skipped silently; a
sheet removed in the settings, or a new name, drops its grades, so the next read is a baseline again.

The «Оценки» topic posts the events (outputs/grades.py); the API gives the newest event of each work within
grades_window_days, newest first (Tablo's §3.7).
"""

import datetime as dt
import hashlib
import json
import logging
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from .. import clock
from ..collectors.grades import Read, Sheet, TabView, sheet_id, short_course, text
from ..llm.client import BadAnswer, Haiku
from ..settings import SPECS, Settings, SettingsError
from ..store import Store

log = logging.getLogger(__name__)

LABEL_BUDGET = 23       # characters of "<course> <work>": the grades block of Tablo (§3.7)
MIN_BUDGET = 8
LAYOUT_VERSION = 3      # bump when the prompt changes, to drop the cached layouts
MAX_TOKENS = 4000
TOKENS_PER_WORK = 60    # {"column", "label", "max", "bonus_column"}: a tab of dates must not cut the answer
SETTINGS = ("grades_student", "grades_sheets", "grades_window_days")

LAYOUT_SYSTEM = """You map one tab of a teacher's grade sheet to the graded works shown on a student's e-ink dashboard.

You see the top rows of the tab, each with its number, and the student's own row, as `column=value` pairs. Column
letters are spreadsheet columns. The top rows hold the header (titles, dates, problem numbers, maximum points) and may
also hold other students' rows and statistics: use them only to understand the layout.

A graded work is the overall result of one homework, quiz, test, seminar, project, exam or extra sheet.
Not graded works: points for single problems (headers like 1, 2, 3a, №1, 4*), course or semester totals,
sums, averages, final or cumulative grades (ИТОГ, Итог, Средний балл, Сумма, НК, ИК), attendance, activity
points, group, comments, penalties.
When a work has per-problem columns plus a total column for that work (for example "Балл"), the work is
that total column. When columns are just numbers, the tab name says what the work is (tab "ДЗ-1А" is
homework 1A, "КР1" is test 1, "Э1" is exam 1).

Return skip=true when the tab is a summary or statistics tab (Итоги, Статистика), an archive (old),
or has no graded works.

header_rows: the numbers of the top rows that describe the columns - titles, dates, problem numbers, maximum points -
in order. Never a row of a student, never statistics that change with the grades (averages, sums or counts over
students).

For each graded work:
- column: its column letter.
- max: the maximum points. It may be written in a header row anywhere, e.g. "Max за обыкновенные" followed
  by numbers - the first number is the maximum of the main score, the next one of the bonus. If the column
  is already a 0-10 grade, max is 10. If you cannot tell, null.
- bonus_column: the column with extra points for the same work (for example "Балл*"), or null.
- label: English, telegraphic, at most {budget} characters: "HW 1A", "Quiz 2", "Sem 3", "Test 1", "Exam",
  "Project", "Extra 2", "LMS". Dates as "HW Sep 17". No course name - it is added separately.
  Russian letter suffixes go by alphabet position, not by shape: а→A, б→B, в→C, г→D, д→E
  (tab "ДЗ-1В" is "HW 1C", not "HW 1B"). Labels must differ between works.

List works in sheet order."""

LAYOUT_SCHEMA = {
    "type": "object",
    "properties": {
        "skip": {"type": "boolean"},
        "reason": {"type": "string"},
        "header_rows": {"type": "array", "items": {"type": "integer"}},
        "items": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "column": {"type": "string"},
                    "label": {"type": "string"},
                    "max": {"anyOf": [{"type": "number"}, {"type": "null"}]},
                    "bonus_column": {"anyOf": [{"type": "string"}, {"type": "null"}]},
                },
                "required": ["column", "label", "max", "bonus_column"],
                "additionalProperties": False,
            },
        },
    },
    "required": ["skip", "reason", "header_rows", "items"],
    "additionalProperties": False,
}


def budget(course: str) -> int:
    """Characters left for the work's label after the course and a space."""
    return max(MIN_BUDGET, LABEL_BUDGET - len(course) - 1)


def layout_key(sid: str, view: TabView, label_budget: int) -> str:
    payload = json.dumps([sid, view.tab, LAYOUT_VERSION], ensure_ascii=False)
    return f"grades.layout:{hashlib.sha256(payload.encode()).hexdigest()[:24]}:{label_budget}"


def layout_prompt(view: TabView) -> str:
    def line(cells: dict) -> str:
        return " | ".join(f"{c}={v}" for c, v in cells.items()) or "(empty)"

    rows = "\n".join(f"row {row}: {line(cells)}" for row, cells in view.rows) or "(no rows above the student)"
    mine = {c: text(v) for c, v in view.mine.items()}
    return (f'Tab: "{view.tab}"\nStudent name is in column {view.name_col}.\n{rows}\n'
            f"student row (row {view.own_row}): {line(mine)}")


def number(value) -> float | None:
    if isinstance(value, bool) or value is None:
        return None
    if isinstance(value, int | float):
        return float(value)
    try:
        return float(str(value).strip().replace(",", "."))
    except ValueError:
        return None


@dataclass(frozen=True)
class Item:
    key: str
    tab: str
    header: str
    label: str
    value: float            # 0-10
    raw: float
    max: float
    bonus: float | None
    position: int


def extract(sid: str, course: str, view: TabView, spec: dict, start: int = 0) -> list[Item]:
    """The works of a laid-out tab that have a number in the student's row; positions go on from `start`."""
    if spec.get("skip"):
        return []
    items, keys, header_rows = [], set(), spec.get("header_rows") or []
    for it in spec.get("items") or []:
        col = it.get("column")
        raw = number(view.mine.get(col))
        header = view.column_header(col, header_rows) if col else ""
        key = f"{sid}|{view.tab}|{col}|{header}"
        if raw is None or key in keys:
            continue
        maximum = number(it.get("max")) or 10.0
        if maximum <= 0:
            continue
        value = raw if maximum == 10 else raw / maximum * 10
        bonus = number(view.mine.get(it["bonus_column"])) if it.get("bonus_column") else None
        label = f"{course} {' '.join(str(it.get('label') or '').split())}".strip()
        keys.add(key)
        items.append(Item(key, view.tab, header, label, round(value, 2), raw, maximum, bonus or None,
                          start + len(items)))
    return items


class Grades:
    def __init__(self, store: Store, settings: Settings, llm: Haiku, now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.db, self.settings, self.llm, self.now = store, store.db, settings, llm, now
        self.wake: Callable[[], None] | None = None  # the collector polls at once after a change of the settings

    # --- a sheet read ------------------------------------------------------------------------

    async def apply(self, sheet: Sheet) -> Read:
        """GradeSheets.on_sheet: lay out every tab, then record the grades. LLMError or BudgetExceeded when a tab
        could not be laid out: nothing is recorded then. A sheet the settings dropped while it was read, or read
        with a name the settings no longer have, records nothing either."""
        items: list[Item] = []
        unread = []
        for view in sheet.views:
            spec = await self.layout(sheet.id, view, budget(sheet.course))
            if spec.get("failed"):
                unread.append(view.tab)
            items += extract(sheet.id, sheet.course, view, spec, len(items))
        if not self._current(sheet):
            return Read(False, 0)
        first = self.record(sheet.id, sheet.title, items)
        return Read(first, self.db.execute("select count(*) n from grade_items where sheet_id = ? and not gone",
                                           (sheet.id,)).fetchone()["n"], tuple(unread))

    def _current(self, sheet: Sheet) -> bool:
        return sheet.student == self.settings.get("grades_student") and \
            sheet.id in {sheet_id(s["url"]) for s in self.settings.get("grades_sheets")}

    async def layout(self, sid: str, view: TabView, label_budget: int) -> dict:
        """The cached layout while the rows it watches have the same text, else Haiku's; the spec's header_rows are
        rows of the view."""
        key = layout_key(sid, view, label_budget)
        cached = self.store.get_json(key)
        if cached is not None and view.header(cached["watch"]) == cached["header"]:
            return cached["spec"]
        system = LAYOUT_SYSTEM.replace("{budget}", str(label_budget))
        numbers = [row for row, _ in view.rows]
        columns = set(view.mine).union(*(cells for _, cells in view.rows))
        try:
            spec = await self.llm.json("grades", system, layout_prompt(view), LAYOUT_SCHEMA,
                                       max_tokens=max(MAX_TOKENS, TOKENS_PER_WORK * len(columns)))
            spec["header_rows"] = sorted({row for row in spec.get("header_rows") or [] if row in numbers})
        except BadAnswer as e:  # the same input would fail again: skipped until the top rows change
            log.warning("layout of tab %r: %s; the tab is skipped", view.tab, e)
            spec = {"skip": True, "reason": f"no answer: {e}", "header_rows": [], "items": [], "failed": True}
        # The header the model named is what the layout stands on; with none named, the whole top.
        watch = spec["header_rows"] or numbers
        self.store.put_json(key, {"watch": watch, "header": view.header(watch), "spec": spec})
        log.info("layout of tab %r: %s", view.tab,
                 "skip" if spec.get("skip") else [it.get("label") for it in spec.get("items") or []])
        return spec

    def record(self, sid: str, title: str | None, items: list[Item]) -> bool:
        """Store the grades of a sheet and the new ones among them; True for the baseline (the first read)."""
        db = self.db
        first = db.execute("select 1 from grade_sheets where sheet_id = ?", (sid,)).fetchone() is None
        prev = {row["key"]: dict(row) for row in db.execute("select * from grade_items where sheet_id = ?", (sid,))}
        keys = {item.key for item in items}
        # A column inserted before others shifts their letters and so their keys: a work that leaves one key in this
        # read and is found under another with the same tab, header and grade is the same work, not a new grade.
        shifted: dict[tuple, list[dict]] = {}
        for row in prev.values():
            if row["key"] not in keys and not row["gone"]:
                shifted.setdefault((row["tab"], row["header"], row["value"], row["bonus"]), []).append(row)
        now, seen, moved = clock.local_iso(self.now()), set(), set()
        with db:
            for item in items:
                old = prev.get(item.key)
                if old is None and (same := shifted.get((item.tab, item.header, item.value, item.bonus))):
                    old = same.pop()
                    moved.add(old["key"])
                # A zero before any real grade is a placeholder for "not graded yet" (Tablo's §3.6).
                if item.value == 0 and not item.bonus and (old is None or old["value"] == 0):
                    continue
                seen.add(item.key)
                db.execute(
                    "insert into grade_items(key, sheet_id, tab, header, label, value, raw, max, bonus, position) "
                    "values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?) on conflict(key) do update set label = excluded.label, "
                    "value = excluded.value, raw = excluded.raw, max = excluded.max, bonus = excluded.bonus, "
                    "position = excluded.position, gone = 0",
                    (item.key, sid, item.tab, item.header, item.label, item.value, item.raw, item.max, item.bonus,
                     item.position))
                changed = old is None or old["value"] != item.value or (old["bonus"] or 0) != (item.bonus or 0)
                if changed and not first:
                    db.execute("insert into grade_events(key, sheet_id, label, value, raw, max, bonus, previous, "
                               "seen_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?)",
                               (item.key, sid, item.label, item.value, item.raw, item.max, item.bonus,
                                old["value"] if old else None, now))
            # A moved work lives under its new key now; the others that left may come back.
            db.executemany("delete from grade_items where key = ?", [(key,) for key in moved])
            db.executemany("update grade_items set gone = 1 where key = ?",
                           [(key,) for key in set(prev) - seen - moved])
            db.execute("insert into grade_sheets(sheet_id, title, baseline_at) values(?, ?, ?) "
                       "on conflict(sheet_id) do update set title = coalesce(excluded.title, title)",
                       (sid, title, now))
        return first

    # --- the forum ---------------------------------------------------------------------------

    def waiting(self) -> list[dict]:
        """New grades not posted yet, within the window, oldest first; with their sheet's link, title and course."""
        sheets = {sheet_id(s["url"]): s for s in self.settings.get("grades_sheets")}
        titles = {row["sheet_id"]: row["title"] for row in self.db.execute("select * from grade_sheets")}
        rows = self.db.execute("select * from grade_events where post_id is null and seen_at >= ? order by id",
                               (self._since(),))
        return [{**dict(row), "url": sheets[row["sheet_id"]]["url"], "title": titles.get(row["sheet_id"]),
                 "course": short_course(sheets[row["sheet_id"]]["label"], titles.get(row["sheet_id"]), row["sheet_id"])}
                for row in rows if row["sheet_id"] in sheets]

    def posted(self, event_ids: list[int], msg_id: int) -> None:
        with self.db:
            self.db.executemany("update grade_events set post_id = ? where id = ?",
                                [(msg_id, event_id) for event_id in event_ids])

    # --- the API, the status and the Mini App -----------------------------------------------------

    def new(self) -> list[dict]:
        """The newest event of each work within the window, newest first (Tablo's §3.7)."""
        ids = {sheet_id(s["url"]) for s in self.settings.get("grades_sheets")}
        rows = self.db.execute(
            "select * from grade_events where id in (select max(id) from grade_events where seen_at >= ? group by key) "
            "order by seen_at desc, id desc", (self._since(),))
        return [{"key": row["key"], "label": row["label"], "value": row["value"], "bonus": row["bonus"],
                 "seen_at": row["seen_at"]} for row in rows if row["sheet_id"] in ids]

    def items(self, sid: str) -> list[dict]:
        """The grades of a sheet in sheet order, the new ones marked."""
        fresh = {row["key"] for row in self.db.execute(
            "select distinct key from grade_events where sheet_id = ? and seen_at >= ?", (sid, self._since()))}
        return [{"label": row["label"], "value": row["value"], "raw": row["raw"], "max": row["max"],
                 "bonus": row["bonus"], "new": row["key"] in fresh}
                for row in self.db.execute(
                    "select * from grade_items where sheet_id = ? and not gone order by position", (sid,))]

    def status(self) -> dict:
        return {"llm": self.llm.available if self.llm is not None else False, "new": len(self.new())}

    def _since(self) -> str:
        return clock.local_iso(self.now() - dt.timedelta(days=self.settings.get("grades_window_days")))

    # --- the owner's settings --------------------------------------------------------------------

    def configure(self, values: dict[str, Any]) -> None:
        """{grades_student?, grades_sheets?, grades_window_days?}, all checked before any is saved. A sheet
        removed, or every sheet when the name changes, loses its grades: the next read is a baseline. SettingsError
        when a value is wrong."""
        if not values or set(values) - set(SETTINGS):
            raise SettingsError(f"expected some of {', '.join(SETTINGS)}")
        checked = {name: SPECS[name][1](value) for name, value in values.items()}
        before = {sheet_id(s["url"]) for s in self.settings.get("grades_sheets")}
        renamed = "grades_student" in checked and checked["grades_student"] != self.settings.get("grades_student")
        for name, value in checked.items():
            self.settings.set(name, value)
        after = {sheet_id(s["url"]) for s in self.settings.get("grades_sheets")}
        self.forget(before | after if renamed else before - after)
        if self.wake is not None:
            self.wake()

    def forget(self, sids: set[str]) -> None:
        with self.db:
            for table in ("grade_items", "grade_events", "grade_sheets"):
                self.db.executemany(f"delete from {table} where sheet_id = ?", [(sid,) for sid in sids])
        if sids:
            log.info("grades of %d sheet(s) forgotten", len(sids))
