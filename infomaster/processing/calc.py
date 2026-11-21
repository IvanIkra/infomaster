"""A gradebook as a small spreadsheet of the student's own row (stage 17.6): how a grade is counted, and what if.

Built from the view a read keeps (processing/grades.py: each tab's own cells under their headers, its formulas and the
works laid out in it) and the few cells outside the own row its formulas read, the header's (`constants`: a maximum
in row 1, the stars marking bonus problems in row 2). A formula reading another student's row is not computed: that
row is never kept.

- `name` and `explain`: a formula written with the names of its cells, «10·SUM(1…6) / 6», for the grade card.
- `targets`, `inputs`, `compute`, `solve`: the calculator. A target is a formula cell (the course's grade, a work's
  points), its inputs the works and the hand-filled cells it is counted from, a formula that is no work being
  followed through. Any input may be given another value, and the target is counted again as the sheet would;
  `solve` finds the least value of one input that brings the target to a goal (by bisection: a grade grows with
  its works, rounding and thresholds included).
"""

import math
import re
from dataclasses import dataclass, field

from ..collectors.grades import NAME, TabView
from . import formulas as fx

FINAL = re.compile(r"итог|оценк|grade|final|\bИК|\bНК", re.IGNORECASE)
MAX_INPUTS = 40
MAX_DEPTH = 60
PENDING_SHARE = 0.5      # as grades.GRADED_SHARE: a zero in a work most of the group has no points in yet


def cell_id(tab: str, col: str) -> str:
    return f"{tab}!{col}"


def cell_value(raw: str | None) -> fx.Value:
    """A kept cell's text as the value a formula reads: a number when it is one."""
    if raw is None or raw == "":
        return None
    try:
        return float(raw.replace(",", ".")) if re.fullmatch(r"\s*-?[\d.,]+(e[-+]?\d+)?\s*", raw, re.I) else raw
    except ValueError:
        return raw


def short(header: str) -> str:
    """A column's name without the header rows that hold numbers only (a maximum): «7 / 1а» -> «1а»."""
    parts = [p.strip() for p in (header or "").split(" / ") if p.strip()]
    named = [p for p in parts if cell_value(p) is None or isinstance(cell_value(p), str)]
    return (named or parts or [""])[-1]


def constants(views: list[TabView]) -> dict[str, str]:
    """The cells outside the own rows that the own rows' formulas read, by "tab!A1": a header's, never a student's
    (a row whose name column holds a person's name is left out, and so the formula reading it is not computed)."""
    by_tab = {v.tab: v for v in views}
    found = {}
    for v in views:
        for formula in v.formulas.values():
            try:
                tree = fx.parse(formula)
            except fx.Unsupported:
                continue
            for node in fx.refs(tree):
                for tab, col, row in fx.cells(node):
                    other = by_tab.get(tab or v.tab)
                    if other is None or row == other.own_row:
                        continue
                    cells = dict(other.rows).get(row)
                    if cells is None:
                        continue
                    name = cells.get(other.name_col, "")
                    if name and NAME.fullmatch(" ".join(name.split())) and not any(ch.isdigit() for ch in name):
                        continue
                    if col in cells:
                        found[f"{other.tab}!{col}{row}"] = cells[col]
    return found


@dataclass
class Tab:
    name: str
    own_row: int
    values: dict[str, fx.Value]
    headers: dict[str, str]
    formulas: dict[str, str]
    notes: dict[str, str]
    works: dict[str, dict]                       # by column: the works laid out, and their bonus columns
    trees: dict[str, fx.Node | None] = field(default_factory=dict)

    def tree(self, col: str) -> fx.Node | None:
        if col not in self.trees:
            try:
                self.trees[col] = fx.parse(self.formulas[col]) if col in self.formulas else None
            except fx.Unsupported:
                self.trees[col] = None
        return self.trees[col]


class Book:
    def __init__(self, view: dict):
        self.tabs: dict[str, Tab] = {}
        for t in view.get("tabs") or []:
            works = {}
            for w in t.get("works") or []:
                if w.get("column"):
                    works[w["column"]] = w
                    if w.get("bonus_column"):
                        works.setdefault(w["bonus_column"], {**w, "column": w["bonus_column"], "bonus": True,
                                                             "max": None})
            self.tabs[t["tab"]] = Tab(t["tab"], t.get("own_row") or 0,
                                      {c["column"]: cell_value(c["value"]) for c in t.get("columns") or []},
                                      {c["column"]: c.get("header") or "" for c in t.get("columns") or []},
                                      t.get("formulas") or {}, t.get("notes") or {}, works)
        self.constants = {key: cell_value(value) for key, value in (view.get("constants") or {}).items()}

    # --- names ------------------------------------------------------------------------------

    def label(self, tab: str, col: str) -> str:
        t = self.tabs.get(tab)
        if t is None:
            return f"{tab}!{col}"
        work = t.works.get(col)
        if work and work.get("label"):
            return work["label"] + ("*" if work.get("bonus") else "")
        return short(t.headers.get(col, "")) or col

    def name(self, node: fx.Node, home: str) -> str:
        """A reference or a range by the names of its cells, the tab added when it is another one."""
        tab = node[1] or home
        t = self.tabs.get(tab)
        where = "" if tab == home else f"{tab}: "
        if node[0] == "ref":
            if t is not None and node[3] == t.own_row:
                return where + self.label(tab, node[2])
            value = self.constants.get(f"{tab}!{node[2]}{node[3]}")
            return fx.number_text(value) if fx.is_number(value) else f"{tab}!{node[2]}{node[3]}" if value is None \
                else f"«{value}»"
        _, _, c1, r1, c2, r2 = node
        if t is not None and r1 == r2 == t.own_row:
            return where + f"{self.label(tab, c1)}…{self.label(tab, c2)}"
        return f"{where}{c1}{r1}:{c2}{r2}"

    def written(self, tab: str, col: str) -> str | None:
        """The formula of an own cell with names: «10·SUM(1…6) / 6»; None for no formula, or one not read."""
        t = self.tabs.get(tab)
        tree = t.tree(col) if t else None
        if tree is None or not fx.refs(tree):
            return None
        return fx.render(tree, lambda node: self.name(node, tab))

    def formula_cells(self) -> list[tuple[str, str]]:
        return [(t.name, col) for t in self.tabs.values() for col in t.formulas if t.tree(col) is not None
                and fx.refs(t.tree(col))]

    # --- values ---------------------------------------------------------------------------------

    def compute(self, tab: str, col: str, given: dict[str, float | None] | None = None) -> fx.Value:
        """The value of an own cell, counted from its formula with the given values over the sheet's. CellError when
        the sheet would show an error, or the formula reads what is not kept."""
        given = given or {}
        cache: dict[str, fx.Value] = {}

        def own(tab: str, col: str, depth: int) -> fx.Value:
            key = cell_id(tab, col)
            if key in given:
                return given[key]
            if key in cache:
                return cache[key]
            t = self.tabs[tab]
            tree = t.tree(col)
            if tree is None:
                if col in t.formulas:
                    raise fx.CellError("unsupported")
                return t.values.get(col)
            if depth > MAX_DEPTH:
                raise fx.CellError("#CYCLE!")
            cache[key] = value = fx.evaluate(tree, lambda tb, c, r: lookup(tb or tab, c, r, depth + 1))
            return value

        def lookup(tab: str, col: str, row: int, depth: int) -> fx.Value:
            t = self.tabs.get(tab)
            if t is not None and row == t.own_row:
                return own(tab, col, depth)
            key = f"{tab}!{col}{row}"
            if key in self.constants:
                return self.constants[key]
            if t is not None and row < t.own_row:  # a header cell left empty
                return None
            raise fx.CellError("#REF!")  # another student's row, or a tab not kept

        return own(tab, col, 0)

    def value(self, tab: str, col: str, given: dict | None = None) -> fx.Value:
        try:
            return self.compute(tab, col, given)
        except (fx.CellError, fx.Unsupported, KeyError, OverflowError):
            return None

    # --- the calculator -------------------------------------------------------------------------

    def depends(self, tab: str, col: str) -> list[tuple[str, str]]:
        """The inputs of a formula cell: the works and the own cells without formulas it is counted from, in order;
        a formula that is no work is followed into."""
        found: list[tuple[str, str]] = []
        seen: set[str] = set()

        def walk(tab: str, col: str, top: bool, depth: int) -> None:
            key = cell_id(tab, col)
            if key in seen or depth > MAX_DEPTH or len(found) >= MAX_INPUTS:
                return
            seen.add(key)
            t = self.tabs.get(tab)
            if t is None:
                return
            if not top and (col in t.works or col not in t.formulas):
                if col in t.values or col in t.works or row_cell(t, col):
                    found.append((tab, col))
                return
            tree = t.tree(col)
            if tree is None:
                return
            for node in fx.refs(tree):
                for tb, c, r in fx.cells(node):
                    other = self.tabs.get(tb or tab)
                    if other is not None and r == other.own_row:
                        walk(other.name, c, False, depth + 1)

        def row_cell(t: Tab, col: str) -> bool:
            return col in t.headers  # a column of the own row's span, empty now

        walk(tab, col, True, 0)
        return found

    def targets(self) -> list[dict]:
        """The formula cells a calculator may aim at, each with its value and how many inputs it has."""
        listed = []
        for tab, col in self.formula_cells():
            value = self.value(tab, col)
            if isinstance(value, bool):
                value = float(value)
            inputs = self.depends(tab, col)
            if not inputs:
                continue
            listed.append({"cell": cell_id(tab, col), "tab": tab, "name": self.label(tab, col),
                           "value": value if fx.is_number(value) or value is None else str(value),
                           "inputs": len(inputs), "text": isinstance(value, str) and value != "",
                           "header": self.tabs[tab].headers.get(col, "")})
        return listed

    def height(self, tab: str, col: str, depth: int = 0) -> int:
        """How many formulas deep a cell is counted: a course's grade sits on top of its works."""
        t = self.tabs.get(tab)
        tree = t.tree(col) if t else None
        if tree is None or depth > MAX_DEPTH:
            return 0
        below = [self.height(tb or tab, c, depth + 1) for node in fx.refs(tree) for tb, c, r in fx.cells(node)
                 if (o := self.tabs.get(tb or tab)) is not None and r == o.own_row]
        return 1 + max(below, default=0)

    def default_target(self, listed: list[dict]) -> str | None:
        """The course's grade, as well as the sheet tells it: of the cells whose value is a grade (no text, no 0/1
        flag), the one on top of the most formulas; a header like «Итог» or «Оценка» first."""
        fit = [t for t in listed if not t["text"] and not (fx.is_number(t["value"]) and t["value"] in (0, 1)
                                                            and not FINAL.search(t["header"]))]
        if not fit:
            return None
        named = [t for t in fit if FINAL.search(t["header"])]
        best = max(named or fit, key=lambda t: (self.height(*split(t["cell"])), t["inputs"], -listed.index(t)))
        return best["cell"]

    def inputs(self, target: str, given: dict[str, float | None]) -> list[dict]:
        tab, col = split(target)
        listed = []
        for tb, c in self.depends(tab, col):
            t = self.tabs[tb]
            work = t.works.get(c) or {}
            sheet = self.value(tb, c) if c in t.formulas else t.values.get(c)
            sheet = float(sheet) if isinstance(sheet, bool) else sheet
            shown = sheet if fx.is_number(sheet) or sheet is None else str(sheet)
            key = cell_id(tb, c)
            graded = work.get("graded")
            listed.append({"cell": key, "tab": tb, "name": self.label(tb, c), "sheet": shown,
                           "value": given[key] if key in given else shown, "max": work.get("max"),
                           "work": bool(work), "changed": key in given,
                           "pending": bool(work) and not work.get("bonus") and shown in (None, 0.0)
                           and (graded is None or graded < PENDING_SHARE)})
        return listed

    def solve(self, target: str, cell: str, goal: float, given: dict[str, float | None]) -> dict:
        """The least value of `cell` in [0, its maximum or 10] that brings `target` to `goal` or above, the others as
        given: {"need": x} (x rounded up to 0.01), {"need": None, "reached": False} when even the maximum does not
        bring it there, {"need": 0, "reached": True} when nothing is needed."""
        tab, col = split(target)
        t = self.tabs.get(split(cell)[0])
        work = t.works.get(split(cell)[1]) if t else None
        top = float(work["max"]) if work and work.get("max") else 10.0

        def at(x: float) -> float | None:
            v = self.value(tab, col, {**given, cell: x})
            return float(v) if fx.is_number(v) or isinstance(v, bool) else None

        high, low = at(top), at(0.0)
        if high is None or high < goal - 1e-9:
            return {"need": None, "reached": False, "max": top}
        if low is not None and low >= goal - 1e-9:
            return {"need": 0.0, "reached": True, "max": top}
        lo, hi = 0.0, top
        for _ in range(60):
            mid = (lo + hi) / 2
            v = at(mid)
            if v is not None and v >= goal - 1e-9:
                hi = mid
            else:
                lo = mid
        need = math.ceil(hi * 100 - 1e-6) / 100
        if (v := at(need)) is None or v < goal - 1e-9:
            need = hi
        return {"need": min(round(need, 2), top), "reached": True, "max": top}


def split(cell: str) -> tuple[str, str]:
    tab, _, col = cell.rpartition("!")
    return tab, col


def written_cells(book: Book) -> dict[str, str]:
    """Every own formula cell with refs, written with names: "tab!COL" -> text."""
    return {cell_id(tab, col): text for tab, col in book.formula_cells() if (text := book.written(tab, col))}

