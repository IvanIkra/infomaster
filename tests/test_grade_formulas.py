"""Stage 17.6: the own row's formulas and comments of a gradebook, and the calculator (names and texts made up).

The workbooks are written by openpyxl, which keeps no computed values: the formula cells are read without them, as a
Google export with a formula showing nothing would be, and the calculator counts them from their formulas."""

import io

import pytest
from openpyxl import Workbook
from openpyxl.comments import Comment
from test_grades import URL
from test_web_fixes import ask, fixer  # noqa: F401  (the fixture)

from infomaster.collectors.grades import TabView, read_views
from infomaster.processing import formulas as fx
from infomaster.processing.calc import Book, constants, referenced, short
from infomaster.processing.grades import VIEW, Grades, describe, extract

ME = "Тестов Тест"


def value(formula: str, cells: dict | None = None):
    cells = cells or {}
    return fx.evaluate(fx.parse(formula), lambda tab, col, row: cells.get(f"{tab + '!' if tab else ''}{col}{row}"))


# --- the formulas ------------------------------------------------------------------------------------------------

def test_the_formulas_of_the_gradebooks_count_as_excel_does():
    assert value("=0.35*A1 + 0.35*B1 + 0.3*C1", {"A1": 10.0, "B1": 8.0, "C1": 6.0}) == pytest.approx(8.1)
    assert value("=ROUND(2.5,0)") == 3 and value("=ROUND(-2.5,0)") == -3 and value("=ROUND(0.125,2)") == 0.13
    assert value("=ROUNDUP(5.001/21*10,2)") == 2.39 and value("=ROUNDDOWN(7.99,0)") == 7
    # the bonus problems are the ones whose header ends with a star: "*~*"
    row = {"C2": "1а", "D2": "1б", "E2": "2*", "C3": 1.0, "D3": 0.5, "E3": 1.0}
    assert value('=SUMIF($C$2:$E$2,"*~*",C3:E3)', row) == 1.0
    assert value("=sum(C3:E3)-SUMIF($C$2:$E$2,\"*~*\",C3:E3)", row) == 1.5
    assert value('=SUMIF(C3:E3,">=1")', row) == 2.0
    # blanks: no average, no count, and an IFERROR or IF gives the sheet's ""
    assert value('=IFERROR(ROUND(AVERAGE(A1,B1),2),"")') == ""
    assert value('=IF(COUNT(A1:C1)=0,"",MIN(10,A1/10))', {"B1": 3.0}) == 0
    assert value('=IF(AND($A5<>"",$B5<>""),SUM(C5:D5),"")', {"A5": "x", "B5": "y", "C5": 2.0, "D5": 3.0}) == 5
    # a retake replaces the exam when there is one
    retake = '=IF(OR(ISNUMBER(D1),ISNUMBER(E1)),IF(ISNUMBER(E1),E1,D1)*0.5,"")'
    assert value(retake, {"D1": 4.0}) == 2.0 and value(retake, {"D1": 4.0, "E1": 8.0}) == 4.0 and value(retake) == ""
    assert value("='ДЗ-1А'!L59 + 'It''s'!A1", {"ДЗ-1А!L59": 2.0, "It's!A1": 1.0}) == 3.0
    assert value("=1/3") == pytest.approx(1 / 3) and value("=-A1^2", {"A1": 3.0}) == 9 and value("=50%") == 0.5
    with pytest.raises(fx.CellError):
        value("=A1/B1", {"A1": 1.0})
    for unknown in ("=VLOOKUP(A1,B1:C9,2)", "=A:A", "=SUM(A1", "not a formula"):
        with pytest.raises(fx.Unsupported):
            fx.parse(unknown)


def test_a_formula_is_written_with_the_names_of_its_cells():
    names = {"C": "КР1", "D": "КР2", "F": "ДЗ"}
    tree = fx.parse("=ROUND(0.35*C2+0.35*D2+0.3*F2,0)")
    assert fx.render(tree, lambda node: names[node[2]]) == "ROUND(0.35·КР1 + 0.35·КР2 + 0.3·ДЗ; 0)"
    assert fx.render(fx.parse("=10*(A1+B1)/6"), lambda node: node[2]) == "10·(A + B) / 6"
    assert short("7 / 1а") == "1а" and short("Score") == "Score" and short("5") == "5"


# --- read from the file -------------------------------------------------------------------------------------------

def gradebook() -> bytes:
    """Like the discrete maths gradebook: problems with bonus ones starred, two tests, a summary with the grade."""
    wb = Workbook()
    hw = wb.active
    hw.title = "ДЗ-1"
    hw.append(["Max", None, 3, 1])
    hw.append(["Группа", "Студент", "1а", "1б", "2*", "Балл", "Балл*"])
    hw.append([1, "Петров Пётр", 1, 1, 0, "=SUM(C3:E3)-G3", '=SUMIF($C$2:$E$2,"*~*",C3:E3)'])
    hw.append([1, ME, 1, 0.5, 1, "=SUM(C4:E4)-G4", '=SUMIF($C$2:$E$2,"*~*",C4:E4)'])
    hw["D4"].comment = Comment("пример стоит проверить подстановкой", "Преподаватель Первый")
    hw["F4"].comment = Comment("в целом хорошо", "Преподаватель Первый")
    hw["C3"].comment = Comment("a classmate's comment", "Преподаватель Первый")
    for title, points in (("КР1", [4, 3, 0]), ("КР2", [None, None, None])):  # the second not written yet
        ws = wb.create_sheet(title)
        ws.append(["Группа", "Студент", "1", "2", "3", title])
        ws.append([1, "Петров Пётр", 5, 5, 5, "=10*SUM(C2:E2)/15"])
        ws.append([1, ME, *points, '=IF($A3<>"",10*SUM(C3:E3)/15,"")'])
    wb["КР1"]["E3"].comment = Comment("ход решения ясен, ответ не получен", "Преподаватель Второй")
    total = wb.create_sheet("Итоги")
    total.append(["Группа", "Студент", "КР1", "КР2", "ДЗ", "НК", "Экзамен обязателен", "Чужое"])
    total.append([1, ME, "='КР1'!F3", "='КР2'!F3", "=10*'ДЗ-1'!F4/'ДЗ-1'!$C$1", "=ROUND(0.35*C2+0.35*D2+0.3*E2,0)",
                  "=IF(F2<4,1,0)", "='ДЗ-1'!F3"])
    buf = io.BytesIO()
    wb.save(buf)
    return buf.getvalue()


LAYOUTS = {"ДЗ-1": {"header_rows": [1, 2], "items": [{"column": "F", "label": "HW 1", "max": 3, "bonus_column": "G"}]},
           "КР1": {"header_rows": [1], "items": [{"column": "F", "label": "Test 1", "max": 10, "bonus_column": None}]},
           "КР2": {"header_rows": [1], "items": [{"column": "F", "label": "Test 2", "max": 10, "bonus_column": None}]},
           "Итоги": {"skip": True, "header_rows": [1], "items": []}}


# What Google's export would have computed in the own row (openpyxl writes none).
COMPUTED = {"ДЗ-1": {"F": 1.5, "G": 1}, "КР1": {"F": 4.67}, "КР2": {"F": 0},
            "Итоги": {"C": 4.67, "D": 0, "E": 5, "F": 3, "G": 1, "H": 2}}


def computed(views: list[TabView]) -> list[TabView]:
    return [TabView(v.tab, v.rows, {**v.mine, **COMPUTED[v.tab]}, v.name_col, v.own_row, v.graded, v.formulas,
                    v.notes) for v in views]


def kept(views) -> dict:
    wanted = referenced(views)
    return {"course": "DM", "tabs": [describe(v, LAYOUTS[v.tab], wanted.get(v.tab, ())) for v in views],
            "constants": constants(views)}


def test_the_own_rows_formulas_and_comments_are_read_and_no_one_elses():
    views = {v.tab: v for v in read_views(gradebook(), ME)}
    assert views["ДЗ-1"].formulas == {"F": "=SUM(C4:E4)-G4", "G": '=SUMIF($C$2:$E$2,"*~*",C4:E4)'}
    assert views["ДЗ-1"].notes == {"D": "пример стоит проверить подстановкой", "F": "в целом хорошо"}
    assert views["КР1"].notes == {"E": "ход решения ясен, ответ не получен"} and views["КР2"].notes == {}
    # the header's cells the formulas read are kept, a classmate's row is not (and its formula is not counted)
    assert constants(list(views.values())) == {"ДЗ-1!C2": "1а", "ДЗ-1!D2": "1б", "ДЗ-1!E2": "2*", "ДЗ-1!C1": "3"}
    book = Book(kept(list(views.values())))
    assert book.value("Итоги", "H") is None and book.value("Итоги", "E") == 5
    # a formula cell without a value has its column, under its header
    assert [c["column"] for c in describe(views["КР1"], LAYOUTS["КР1"])["columns"]] == ["A", "C", "D", "E", "F"]


def test_the_calculator_counts_the_grade_and_what_is_needed():
    book = Book(kept(computed(read_views(gradebook(), ME))))
    targets = book.targets()
    assert book.default_target(targets) == "Итоги!F"  # the grade, not the flag on top of it
    assert book.written("Итоги", "F") == "ROUND(0.35·КР1 + 0.35·КР2 + 0.3·ДЗ; 0)"
    assert book.written("ДЗ-1", "F") == "SUM(1а…2*) − HW 1*" and book.written("Итоги", "E") == "10·ДЗ-1: HW 1 / 3"
    assert book.value("Итоги", "F") == 3  # 0.35·4.67 + 0 + 0.3·5 = 3.13
    inputs = {i["cell"]: i for i in book.inputs("Итоги!F", {})}
    assert list(inputs) == ["КР1!F", "КР2!F", "ДЗ-1!F"]  # the works, not their problems
    assert inputs["КР1!F"]["sheet"] == pytest.approx(4.67, abs=0.01) and inputs["ДЗ-1!F"]["max"] == 3
    assert book.value("Итоги", "F", {"КР2!F": 8.0}) == 6
    # for a 6: 0.35·x ≥ 5.5 − 1.63 − 1.5, at least 6.77 on the second test
    assert book.solve("Итоги!F", "КР2!F", 6, {}) == {"need": 6.77, "reached": True, "max": 10.0}
    assert book.value("Итоги", "F", {"КР2!F": 6.77}) == 6 and book.value("Итоги", "F", {"КР2!F": 6.76}) == 5
    assert book.solve("Итоги!F", "КР2!F", 9, {}) == {"need": None, "reached": False, "max": 10.0}
    assert book.solve("Итоги!F", "КР2!F", 3, {})["need"] == 0
    # a test not written yet: its problems are inputs, empty, under their headers; the group's number is none
    assert [(i["cell"], i["name"], i["sheet"]) for i in book.inputs("КР2!F", {})] == [
        ("КР2!C", "1", None), ("КР2!D", "2", None), ("КР2!E", "3", None)]
    assert book.value("КР2", "F", {"КР2!C": 5.0, "КР2!D": 4.0}) == 6


# --- the web app ------------------------------------------------------------------------------------------------------

async def test_the_grade_card_and_the_calculator_in_the_web_app(fixer):  # noqa: F811
    fixer.settings.set("grades_sheets", [{"url": URL, "label": "DM"}])
    views = computed(read_views(gradebook(), ME))
    view = kept(views)
    view["how"] = {"Итоги!F": {"en": "0.35 × Test 1 + 0.35 × Test 2 + 0.3 × homework, rounded", "ru": "…"}}
    fixer.grades.store.put_json(VIEW + "abc123", view)
    items = []
    for v in views:
        items += extract("abc123", "DM", v, LAYOUTS[v.tab], len(items))
    fixer.grades.record("abc123", "DM", items)
    grades = (await ask(fixer, "GET", "/course?key=DM"))["grades"]
    hw = next(w for w in grades["works"] if w["tab"] == "ДЗ-1")
    assert hw["note"] == "в целом хорошо"
    assert [(p["header"], p["note"]) for p in hw["parts"]] == [
        ("1а", None), ("1б", "пример стоит проверить подстановкой"), ("2*", None)]  # no group's number
    assert hw["how"] == {"text": None, "formula": "SUM(1а…2*) − HW 1*"}
    book = grades["books"][0]
    assert book["calc"]
    grade = next(c for c in book["summaries"][0]["cells"] if c["header"] == "НК")
    assert grade["how"]["text"].startswith("0.35 × Test 1")

    calc = await ask(fixer, "POST", "/course/calc", {"key": "DM", "book": "abc123", "values": {"КР2!F": 8},
                                                     "solve": {"cell": "КР2!F", "goal": 6}})
    assert (calc["target"], calc["value"]) == ("Итоги!F", 6)
    assert next(i for i in calc["inputs"] if i["cell"] == "КР2!F")["changed"]
    assert calc["solve"] == {"cell": "КР2!F", "goal": 6, "need": 6.77, "reached": True, "max": 10.0}
    # a cell that is no input of the target is not taken; a target or a gradebook it has not is not found
    calc = await ask(fixer, "POST", "/course/calc", {"key": "DM", "book": "abc123", "values": {"ДЗ-1!C": 99}})
    assert calc["value"] == 3
    await ask(fixer, "POST", "/course/calc", {"key": "DM", "book": "abc123", "target": "Итоги!Z"}, status=404)
    await ask(fixer, "POST", "/course/calc", {"key": "DM", "book": "nope"}, status=404)
    await ask(fixer, "POST", "/course/calc", {"key": "DM", "book": "abc123", "values": {"x": "8"}}, status=400)


class Explainer:
    available = True

    def __init__(self):
        self.calls = 0

    async def json(self, task, system, prompt, schema, *, max_tokens):
        self.calls += 1
        return {"cells": [{"cell": line.split(" | ")[0], "en": "how " + line.split(" | ")[0], "ru": "как"}
                          for line in prompt.splitlines()]}


async def test_the_formulas_are_explained_once_while_they_stay(store, now):
    from infomaster.settings import Settings
    llm = Explainer()
    grades = Grades(store, Settings(store), llm, now=now)
    view = kept(read_views(gradebook(), ME))
    how = await grades.explain("abc123", view)
    assert how["Итоги!F"] == {"en": "how Итоги!F", "ru": "как"} and "КР1!E" not in how  # a formula only
    assert await grades.explain("abc123", view) == how and llm.calls == 1
    view["tabs"][0]["formulas"]["F"] = "=SUM(C4:D4)"
    await grades.explain("abc123", view)
    assert llm.calls == 2
