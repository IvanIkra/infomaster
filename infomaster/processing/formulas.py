"""The formulas of the student's own row in a gradebook (stage 17.6): parsed, evaluated, written out by name.

A teacher's sheet computes a work from its problems (`=10*SUM(E59:J59)/6`) and a course's grade from its works
(`=ROUND(0.35*D59+0.35*E59+0.3*F59,0)`). The own row's formulas come with the read (collectors/grades.py), and this
module is what the web app's grade card and calculator stand on: a small spreadsheet language, the part of Excel's
the gradebooks use, evaluated as Excel would. No `eval` and no library: a formula is parsed into a tree of tuples,
and a function or a form this parser does not know makes the formula Unsupported, so it is shown but not computed.

Values are float, str, bool or None (an empty cell). An error of Excel's (#DIV/0!, #VALUE!) is CellError: IFERROR
catches it, and anything else gives no value.
"""

import math
import re
from collections.abc import Callable

Value = float | str | bool | None
# ("num", x) ("str", s) ("bool", b) ("ref", tab, col, row) ("range", tab, col1, row1, col2, row2)
# ("bin", op, a, b) ("neg", a) ("call", NAME, [args]); tab is None for the formula's own tab.
Node = tuple


class Unsupported(ValueError):
    """A formula this parser does not read: shown as it is, never computed."""


class CellError(ValueError):
    """An Excel error value (#DIV/0!, #VALUE!, #REF!)."""


# --- references --------------------------------------------------------------------------------

def col_index(col: str) -> int:
    """A -> 0, AA -> 26."""
    n = 0
    for ch in col:
        n = n * 26 + ord(ch) - 64
    return n - 1


def col_name(index: int) -> str:
    letters, index = "", index + 1
    while index:
        index, rest = divmod(index - 1, 26)
        letters = chr(65 + rest) + letters
    return letters


# --- tokens ------------------------------------------------------------------------------------

TOKEN = re.compile(r"""
    (?P<space>\s+)
  | (?P<str>"(?:[^"]|"")*")
  | (?P<ref>(?:(?:'(?:[^']|'')+'|[A-Za-zА-Яа-яЁё_][\wА-Яа-яЁё.]*)!)?\$?[A-Za-z]{1,3}\$?\d+
            (?::\$?[A-Za-z]{1,3}\$?\d+)?)(?![\w(])
  | (?P<num>\d+(?:\.\d*)?(?:[eE][+-]?\d+)?|\.\d+(?:[eE][+-]?\d+)?)
  | (?P<name>[A-Za-z_][A-Za-z0-9_.]*)(?=\s*\()
  | (?P<bool>TRUE|FALSE)(?![\w(])
  | (?P<op><>|<=|>=|[-+*/^&=<>(),;%])
""", re.VERBOSE | re.IGNORECASE)

CELL = re.compile(r"\$?([A-Za-z]{1,3})\$?(\d+)")


def tokens(formula: str) -> list[tuple[str, str]]:
    found, at = [], 0
    while at < len(formula):
        m = TOKEN.match(formula, at)
        if m is None:
            raise Unsupported(f"cannot read {formula[at:at + 12]!r}")
        at = m.end()
        if m.lastgroup != "space":
            found.append((m.lastgroup, m.group()))
    return found


MAX_ROW = 1_048_576   # a sheet's last row
RANGE_CELLS = 20_000  # cells of one range: a gradebook's column is a few hundred
FORMULA_CHARS = 4000
DEPTH_MAX = 64        # nested parentheses and signs


def reference(text: str) -> Node:
    tab = None
    if "!" in text:
        tab, text = text.rsplit("!", 1)
        tab = tab[1:-1].replace("''", "'") if tab.startswith("'") else tab
    cells = [CELL.fullmatch(part) for part in text.split(":")]
    (c1, r1), *rest = [(m.group(1).upper(), int(m.group(2))) for m in cells]
    if not rest:
        return ("ref", tab, c1, r1)
    c2, r2 = rest[0]
    (c1, c2), (r1, r2) = sorted((c1, c2), key=col_index), sorted((r1, r2))
    if r2 > MAX_ROW or (col_index(c2) - col_index(c1) + 1) * (r2 - r1 + 1) > RANGE_CELLS:
        raise Unsupported("a range too big to count")  # D79: it is expanded cell by cell
    return ("range", tab, c1, r1, c2, r2)


# --- the parser ----------------------------------------------------------------------------------

FUNCTIONS = frozenset({"SUM", "AVERAGE", "COUNT", "COUNTA", "MIN", "MAX", "ROUND", "ROUNDUP", "ROUNDDOWN", "IF",
                       "IFERROR", "AND", "OR", "NOT", "ISNUMBER", "ISBLANK", "SUMIF", "ABS"})
LEVELS = [{"=", "<>", "<", ">", "<=", ">="}, {"&"}, {"+", "-"}, {"*", "/"}, {"^"}]


def parse(formula: str) -> Node:
    """`=...` into a tree. Unsupported for anything else, or a function this module does not compute."""
    if not formula.startswith("="):
        raise Unsupported("not a formula")
    if len(formula) > FORMULA_CHARS:
        raise Unsupported("a formula too long")
    toks = tokens(formula[1:])
    depth = deepest = signs = 0
    for kind, text in toks:  # the parser recurses per parenthesis and per sign in a row: Python's stack is short
        depth += (text == "(") - (text == ")") if kind == "op" else 0
        signs = signs + 1 if kind == "op" and text in "+-" else 0
        deepest = max(deepest, depth, signs)
    if deepest > DEPTH_MAX:
        raise Unsupported("a formula nested too deep")
    at = 0

    def peek() -> tuple[str, str] | None:
        return toks[at] if at < len(toks) else None

    def take(value: str | None = None) -> tuple[str, str]:
        nonlocal at
        tok = peek()
        if tok is None or (value is not None and tok[1] != value):
            raise Unsupported(f"expected {value or 'more'}")
        at += 1
        return tok

    def level(n: int) -> Node:
        if n == len(LEVELS):
            return unary()
        node = level(n + 1)
        while (tok := peek()) and tok[0] == "op" and tok[1] in LEVELS[n]:
            take()
            node = ("bin", tok[1], node, level(n + 1))
        return node

    def unary() -> Node:
        tok = peek()
        if tok and tok[0] == "op" and tok[1] in "+-" and tok[1]:
            take()
            inner = unary()
            return ("neg", inner) if tok[1] == "-" else inner
        node = primary()
        if (tok := peek()) and tok == ("op", "%"):
            take()
            node = ("bin", "/", node, ("num", 100.0))
        return node

    def primary() -> Node:
        kind, text = take()
        if kind == "num":
            return ("num", float(text))
        if kind == "str":
            return ("str", text[1:-1].replace('""', '"'))
        if kind == "bool":
            return ("bool", text.upper() == "TRUE")
        if kind == "ref":
            return reference(text)
        if kind == "name":
            name = text.upper().removeprefix("_XLFN.")
            if name not in FUNCTIONS:
                raise Unsupported(f"the function {name}")
            take("(")
            args = []
            if peek() != ("op", ")"):
                while True:
                    args.append(level(0))
                    if peek() in (("op", ","), ("op", ";")):
                        take()
                        continue
                    break
            take(")")
            return ("call", name, args)
        if (kind, text) == ("op", "("):
            node = level(0)
            take(")")
            return node
        raise Unsupported(f"unexpected {text!r}")

    tree = level(0)
    if at != len(toks):
        raise Unsupported(f"unexpected {toks[at][1]!r}")
    return tree


def refs(node: Node) -> list[Node]:
    """The references and ranges of a tree, in order."""
    if node[0] in ("ref", "range"):
        return [node]
    if node[0] == "bin":
        return refs(node[2]) + refs(node[3])
    if node[0] == "neg":
        return refs(node[1])
    if node[0] == "call":
        return [r for arg in node[2] for r in refs(arg)]
    return []


def cells(node: Node) -> list[tuple[str | None, str, int]]:
    """Every cell a reference or a range covers: (tab, column, row)."""
    if node[0] == "ref":
        return [(node[1], node[2], node[3])]
    _, tab, c1, r1, c2, r2 = node
    return [(tab, col_name(c), r) for r in range(r1, r2 + 1) for c in range(col_index(c1), col_index(c2) + 1)]


# --- evaluation ------------------------------------------------------------------------------------

Lookup = Callable[[str | None, str, int], Value]


def is_number(v: Value) -> bool:
    return isinstance(v, float | int) and not isinstance(v, bool)


def to_number(v: Value) -> float:
    if v is None:
        return 0.0
    if isinstance(v, bool):
        return float(v)
    if isinstance(v, int | float):
        return float(v)
    try:
        x = float(v.strip().replace(",", ".")) if v.strip() else 0.0
    except ValueError:
        raise CellError("#VALUE!") from None
    if not math.isfinite(x):  # "nan", "inf" are no numbers of a sheet
        raise CellError("#VALUE!")
    return x


def to_bool(v: Value) -> bool:
    if isinstance(v, str):
        if v.upper() in ("TRUE", "FALSE"):
            return v.upper() == "TRUE"
        raise CellError("#VALUE!")
    return bool(to_number(v))


def round_half_up(x: float, digits: int) -> float:
    """Excel's ROUND: half away from zero (Python's round is half to even)."""
    digits = max(-15, min(15, digits))
    q = 10.0 ** digits
    return math.copysign(math.floor(abs(x) * q + 0.5 + 1e-9) / q, x)


def wildcard(pattern: str) -> re.Pattern:
    out, escape = "", False
    for ch in pattern:
        if escape:
            out += re.escape(ch)
            escape = False
        elif ch == "~":
            escape = True
        elif ch == "*":
            out += ".*"
        elif ch == "?":
            out += "."
        else:
            out += re.escape(ch)
    return re.compile(out, re.IGNORECASE | re.DOTALL)


def criterion(spec: Value) -> Callable[[Value], bool]:
    """SUMIF's criteria: "*~*" (a header ending with *), ">=5", 3."""
    if is_number(spec) or isinstance(spec, bool):
        return lambda v: is_number(v) and float(v) == to_number(spec)
    spec = "" if spec is None else str(spec)
    m = re.match(r"(<>|<=|>=|<|>|=)?(.*)", spec, re.DOTALL)
    op, rest = m.group(1), m.group(2)
    try:
        target = float(rest.replace(",", "."))
    except ValueError:
        target = None
    if target is not None and op:
        return lambda v: is_number(v) and compare(op, float(v), target)
    if target is not None:
        return lambda v: is_number(v) and float(v) == target
    pattern = wildcard(rest)
    if op == "<>":
        return lambda v: not pattern.fullmatch("" if v is None else str(v))
    return lambda v: v is not None and pattern.fullmatch(str(v)) is not None


def compare(op: str, a, b) -> bool:
    return {"=": a == b, "<>": a != b, "<": a < b, ">": a > b, "<=": a <= b, ">=": a >= b}[op]


def comparable(a: Value, b: Value) -> tuple:
    """Excel orders numbers before text before booleans; an empty cell is 0 or "" by the other side."""
    if a is None:
        a = "" if isinstance(b, str) else False if isinstance(b, bool) else 0.0
    if b is None:
        b = "" if isinstance(a, str) else False if isinstance(a, bool) else 0.0
    rank = lambda v: 2 if isinstance(v, bool) else 1 if isinstance(v, str) else 0  # noqa: E731
    if rank(a) != rank(b):
        return rank(a), rank(b)
    if isinstance(a, str):
        return a.casefold(), b.casefold()
    return float(a), float(b)


def evaluate(node: Node, lookup: Lookup) -> Value:
    """The value of a tree, cells read through `lookup(tab, col, row)`. CellError for an error value."""
    kind = node[0]
    if kind in ("num", "str", "bool"):
        return node[1]
    if kind == "ref":
        return lookup(node[1], node[2], node[3])
    if kind == "range":
        raise CellError("#VALUE!")  # a range outside a function
    if kind == "neg":
        return -to_number(evaluate(node[1], lookup))
    if kind == "bin":
        op, a, b = node[1], evaluate(node[2], lookup), evaluate(node[3], lookup)
        if op == "&":
            return f"{'' if a is None else a}{'' if b is None else b}"
        if op in ("=", "<>", "<", ">", "<=", ">="):
            x, y = comparable(a, b)
            return compare(op, x, y)
        x, y = to_number(a), to_number(b)
        if op == "+":
            return x + y
        if op == "-":
            return x - y
        if op == "*":
            return x * y
        if op == "/":
            if y == 0:
                raise CellError("#DIV/0!")
            return x / y
        try:
            result = x ** y
        except (OverflowError, ZeroDivisionError):
            raise CellError("#NUM!") from None
        if isinstance(result, complex):  # a negative base to a fraction
            raise CellError("#NUM!")
        return result
    return call(node[1], node[2], lookup)


def values(args: list[Node], lookup: Lookup) -> list[Value]:
    """The values of the arguments, a range spread into its cells (those only skip text and blanks, as Excel's)."""
    found = []
    for arg in args:
        if arg[0] in ("ref", "range"):
            found += [("cell", lookup(*cell)) for cell in cells(arg)]
        else:
            found.append(("arg", evaluate(arg, lookup)))
    return found


def numbers(args: list[Node], lookup: Lookup) -> list[float]:
    out = []
    for source, v in values(args, lookup):
        if source == "cell":
            if is_number(v):
                out.append(float(v))
        elif v is not None:
            out.append(to_number(v))
    return out


def call(name: str, args: list[Node], lookup: Lookup) -> Value:
    def arg(i: int, default: Value = None) -> Value:
        return evaluate(args[i], lookup) if i < len(args) else default

    if name == "IF":
        return arg(1, True) if to_bool(arg(0)) else arg(2, False)
    if name == "IFERROR":
        try:
            return arg(0)
        except CellError:
            return arg(1, "")
    if name == "SUM":
        return sum(numbers(args, lookup))
    if name == "AVERAGE":
        found = numbers(args, lookup)
        if not found:
            raise CellError("#DIV/0!")
        return sum(found) / len(found)
    if name == "COUNT":
        return float(sum(1 for source, v in values(args, lookup)
                         if is_number(v) or (source == "arg" and isinstance(v, bool))))
    if name == "COUNTA":
        return float(sum(1 for _, v in values(args, lookup) if v is not None and v != ""))
    if name in ("MIN", "MAX"):
        found = numbers(args, lookup)
        return (min if name == "MIN" else max)(found) if found else 0.0
    if name in ("ROUND", "ROUNDUP", "ROUNDDOWN"):
        x, digits = to_number(arg(0)), max(-15, min(15, int(to_number(arg(1, 0.0)))))
        if name == "ROUND":
            return round_half_up(x, digits)
        q = 10.0 ** digits
        scaled = abs(x) * q
        scaled = math.ceil(scaled - 1e-9) if name == "ROUNDUP" else math.floor(scaled + 1e-9)
        return math.copysign(scaled / q, x)
    if name in ("AND", "OR"):
        found = [to_bool(v) for source, v in values(args, lookup) if not (source == "cell" and v is None)]
        return all(found) if name == "AND" else any(found)
    if name == "NOT":
        return not to_bool(arg(0))
    if name == "ISNUMBER":
        return is_number(arg(0))
    if name == "ISBLANK":
        return arg(0) is None
    if name == "ABS":
        return abs(to_number(arg(0)))
    if name == "SUMIF":
        if len(args) < 2 or args[0][0] not in ("ref", "range"):
            raise CellError("#VALUE!")
        test = criterion(arg(1))
        where = cells(args[0])
        add = cells(args[2]) if len(args) > 2 and args[2][0] in ("ref", "range") else where
        if len(args) > 2 and len(add) != len(where):  # Excel takes the sum range's corner and the criteria's shape
            first = add[0]
            c0, r0 = col_index(first[1]), first[2]
            cw, rw = col_index(where[0][1]), where[0][2]
            add = [(first[0], col_name(c0 + col_index(c) - cw), r0 + r - rw) for _, c, r in where]
        total = 0.0
        for cell, sum_cell in zip(where, add, strict=True):
            if test(lookup(*cell)):
                v = lookup(*sum_cell)
                if is_number(v):
                    total += float(v)
        return total
    raise Unsupported(f"the function {name}")


# --- written out --------------------------------------------------------------------------------

PRECEDENCE = {"=": 0, "<>": 0, "<": 0, ">": 0, "<=": 0, ">=": 0, "&": 1, "+": 2, "-": 2, "*": 3, "/": 3, "^": 4}


def number_text(x: float) -> str:
    return str(int(x)) if float(x).is_integer() else f"{x:g}"


def render(node: Node, name: Callable[[Node], str], parent: int = -1) -> str:
    """The formula written with names for its cells (`name` of a ref or a range): `0.35·КР1 + 0.35·КР2 + 0.3·ДЗ`."""
    kind = node[0]
    if kind == "num":
        return number_text(node[1])
    if kind == "str":
        return f"«{node[1]}»"
    if kind == "bool":
        return "TRUE" if node[1] else "FALSE"
    if kind in ("ref", "range"):
        return name(node)
    if kind == "neg":
        return "−" + render(node[1], name, 5)
    if kind == "bin":
        op, level = node[1], PRECEDENCE[node[1]]
        right_level = level + 1 if op in ("-", "/", "^") else level
        sign = {"*": "·", "/": " / ", "-": " − ", "+": " + ", "<>": " ≠ ", "<=": " ≤ ", ">=": " ≥ "}.get(op, f" {op} ")
        text = render(node[2], name, level) + sign + render(node[3], name, right_level)
        return f"({text})" if level < parent else text
    return f"{node[1]}({'; '.join(render(arg, name) for arg in node[2])})"
