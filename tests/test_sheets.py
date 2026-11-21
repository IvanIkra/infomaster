"""Листочки (§6.3, D14): the PDF of a message fetched for its batch, the sheet the model finds in it, its versions,
its reading within the limits and its posts in «Листочки»."""

import datetime as dt
from types import SimpleNamespace

import actions_app
import pytest
from conftest import make_pdf
from test_http import env

from infomaster import clock
from infomaster.collectors.files import Files
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import Class
from infomaster.library import Library
from infomaster.llm.budget import BudgetExceeded
from infomaster.llm.client import Document, Haiku
from infomaster.outputs.deadlines import DeadlineWatch
from infomaster.outputs.sheets import SheetPublisher
from infomaster.processing import pdf
from infomaster.processing.batches import Batches, Changes, check
from infomaster.processing.bot_sheets import BotSheets
from infomaster.processing.news import News
from infomaster.processing.sheets import Sheets, number_key, sheet_label, sheet_title
from infomaster.settings import ChatSettings, Settings

CHAT = -1001234567890
SHEET3 = make_pdf(["Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS", "Task 1. Prove that the series"])
SHEET3_FIXED = make_pdf(["Calculus. Sheet 3. Series\nHand in by 30.10 23:59 in the LMS",
                         "Task 1. Prove that the series converges"])
SCAN = make_pdf(["", ""])


class FakeLLM:
    """The batch answers in order; the readings of sheets ("pdf") too, from their own list."""

    available = True

    def __init__(self):
        self.answers, self.readings, self.calls = [], [], []

    async def json(self, task, system, prompt, schema, *, max_tokens, chat_id=None, documents=()):
        self.calls.append(SimpleNamespace(task=task, system=system, prompt=prompt, documents=list(documents)))
        answer = (self.answers if task == "batch" else self.readings).pop(0)
        if isinstance(answer, Exception):
            raise answer
        return answer


@pytest.fixture
def files(store, tmp_path):
    files = Files(store, tmp_path)
    files.remote, files.downloads = {}, []

    async def download(chat_id, msg_id, path):
        files.downloads.append(msg_id)
        if (chat_id, msg_id) not in files.remote:
            return False
        path.write_bytes(files.remote[chat_id, msg_id])
        return True

    files.download = download
    return files


@pytest.fixture
def llm():
    return FakeLLM()


@pytest.fixture
def sheets(store, now, schedule, files, llm):
    chat_settings = ChatSettings(store)
    schedule.inbox.put_chat(CHAT, "Матан", "supergroup", None, 0)
    chat_settings.update(CHAT, {"enabled": True, "study": True})
    return Sheets(store, Settings(store), chat_settings, schedule.inbox, files, schedule.deadlines, schedule.subjects,
                  llm, now=now)


@pytest.fixture
def batches(store, now, schedule, sheets, files, llm):
    batches = Batches(store, Settings(store), sheets.chat_settings, schedule.inbox, News(store), schedule.deadlines,
                      schedule, llm, now=now)
    batches.files, batches.sheets = files, sheets
    return batches


@pytest.fixture
def topic(sheets):
    return SheetPublisher(sheets)


def send(batches, msg_id, data: bytes | None, name="list3.pdf", text="", ago=dt.timedelta(minutes=30)):
    """A message of the chat, with a PDF the twin can download."""
    files = []
    if data is not None:
        batches.files.remote[CHAT, msg_id] = data
        files = [{"kind": "pdf", "name": name, "mime": "application/pdf", "size": len(data)}]
    batches.inbox.add(CHAT, msg_id, {"sender_id": 7, "sender_name": "Ассистент", "at": batches.now() - ago,
                                     "text": text, "files": files})


def sheet_item(msg_id, number="3", due="2026-10-29", course="Математический анализ"):
    return {"msg_ids": [msg_id], "news": {"full": f"Calc sheet {number} posted", "short": f"Calc sheet {number}",
                                          "importance": 5, "replaces": ""},
            "deadlines": [], "events": [], "sheets": [{"msg_id": msg_id, "course": course, "number": number,
                                                       "due": due}]}


async def parse(batches) -> Changes:
    changes = Changes()
    await batches.parse_chat(CHAT, changes)
    return changes


# --- local PDFs ------------------------------------------------------------------------------------

def test_pdf_locally(tmp_path):
    path = tmp_path / "s.pdf"
    path.write_bytes(SHEET3)
    extract = pdf.extract(path, 10)
    assert extract.pages == 2 and extract.first == "Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS"
    assert not pdf.scanned(extract.first) and pdf.scanned("") and pdf.scanned("Стр. 1")
    path.write_bytes(SCAN)
    assert pdf.extract(path, 1).texts == [""]
    head = tmp_path / "head.pdf"
    head.write_bytes(pdf.head(path, 1))
    assert pdf.extract(head, 5).pages == 1
    path.write_bytes(b"not a pdf")
    with pytest.raises(pdf.PdfError):
        pdf.extract(path, 1)

    assert pdf.diff("a\nb\nc", "a\nB\nc", 100) == "-b\n+B"
    assert pdf.diff("same", "same", 100) == ""
    assert pdf.diff("", "x\n" * 100, 10).endswith("…")


@pytest.mark.parametrize(("a", "b", "same"), [
    ("hw5.pdf", "hw5_v2_final.pdf", True),
    ("ДЗ3.pdf", "ДЗ3_исправленный (1).pdf", True),
    ("Листок 3 ряды.pdf", "листок 3 ряды новый.pdf", True),
    ("Sheet_2.1.pdf", "sheet-2.1-upd.pdf", True),
    ("hw5.pdf", "hw6.pdf", False),
    ("matan_list3.pdf", "algebra_list3.pdf", False),
])
def test_similar_names(a, b, same):
    assert pdf.similar_names(a, b) is same


def test_keys():
    assert number_key("№ 3") == "3" and number_key("ДЗ-2.1") == "дз-2.1"


def test_a_title_instead_of_a_number():
    assert sheet_label("Calc", "3") == "Calc sheet 3" and sheet_label("Calc", "") == "Calc sheet"
    assert sheet_label("Alg", "Test 1 problems") == "Alg: Test 1 problems"
    assert sheet_label("", "Test 1 problems") == "Test 1 problems"
    assert sheet_title("№ 4b") == "Sheet № 4b" and sheet_title("Exam variant") == "Exam variant"
    assert sheet_title("") == "Sheet"


# --- the files ------------------------------------------------------------------------------------

async def test_files_are_fetched_once(batches, files, now):
    send(batches, 1, SHEET3)
    message = batches.inbox.message(CHAT, 1)
    row = await files.fetch(message)
    assert files.downloads == [1] and row["pages"] == 2 and row["first_page"].startswith("Calculus. Sheet 3")
    assert files.path(row).read_bytes() == SHEET3 and row["path"] == f"files/{row['sha256']}.pdf"
    assert (await files.fetch(message))["id"] == row["id"] and files.downloads == [1]  # stored: no second download

    # The message's file was replaced: its new one is fetched.
    files.remote[CHAT, 1] = SHEET3_FIXED
    batches.inbox.edit(CHAT, 1, "", [{"kind": "pdf", "name": "list3.pdf", "size": len(SHEET3_FIXED)}], now())
    fixed = await files.fetch(batches.inbox.message(CHAT, 1))
    assert files.downloads == [1, 1] and fixed["sha256"] != row["sha256"]

    send(batches, 2, None)  # a message without a PDF
    assert await files.fetch(batches.inbox.message(CHAT, 2)) is None
    batches.inbox.add(CHAT, 3, {"at": now(), "text": "", "files": [{"kind": "pdf", "name": "gone.pdf", "size": 5}]})
    failed = await files.fetch(batches.inbox.message(CHAT, 3))  # deleted before the batch came
    assert failed["sha256"] is None and failed["error"] == "the message or its file is gone"
    assert await files.fetch(batches.inbox.message(CHAT, 3)) == failed and files.downloads == [1, 1, 3]
    batches.inbox.add(CHAT, 4, {"at": now(), "text": "", "files": [{"kind": "pdf", "name": "book.pdf",
                                                                    "size": 80 * 1024 * 1024}]})
    assert await files.fetch(batches.inbox.message(CHAT, 4)) is None  # a book is not downloaded
    files.download = None  # the twin is not logged in
    send(batches, 5, SHEET3, name="other.pdf")
    assert await files.fetch(batches.inbox.message(CHAT, 5)) is None

    assert files.prune(now() + dt.timedelta(days=15)) == 2
    assert files.path(fixed) is None and list((files.directory).iterdir()) == []


# --- the batch -------------------------------------------------------------------------------------

async def test_batch_reads_the_first_page_and_finds_the_sheet(batches, llm, sheets):
    send(batches, 1, SHEET3, text="Новый листок")
    send(batches, 2, None, text="а когда сдавать?")
    llm.answers = [{"items": [sheet_item(1), {**sheet_item(2), "news": None}]}]  # 2 has no PDF: no sheet
    changes = await parse(batches)

    prompt = llm.calls[0].prompt
    assert '  PDF "list3.pdf", 2 pages, first page:\n  | Calculus. Sheet 3. Series\n  | Hand in by 29.10' in prompt
    assert llm.calls[0].documents == []
    [sheet] = sheets.waiting()
    assert changes.sheets == [sheet["id"]]
    assert (sheet["msg_id"], sheet["name"], sheet["course"], sheet["number"], sheet["due"]) == \
        (1, "list3.pdf", "Математический анализ", "3", "2026-10-29")

    # The same file again (forwarded, posted twice) is no new sheet.
    send(batches, 3, SHEET3, name="list3 (1).pdf")
    llm.answers = [{"items": [sheet_item(3)]}]
    assert (await parse(batches)).sheets == [] and len(sheets.waiting()) == 1


async def test_batch_attaches_a_scan(batches, llm, sheets):
    send(batches, 1, SCAN, name="scan.pdf")
    llm.answers = [{"items": [sheet_item(1, number="")]}]
    await parse(batches)
    assert '  PDF "scan.pdf", 2 pages: a scan, its first page is attached as document 1' in llm.calls[0].prompt
    [document] = llm.calls[0].documents
    assert document.pages == 1 and document.data.startswith(b"%PDF")


def test_check_keeps_sheets_of_read_pdfs():
    answer = {"items": [{"msg_ids": [1], "news": None, "deadlines": [], "events": [],
                         "sheets": [{"msg_id": 1, "course": " Алгебра ", "number": " 2 ", "due": "2026-13-01"},
                                    {"msg_id": 1, "course": "", "number": "2", "due": ""}]}]}
    assert check(answer, {1}) == []  # no PDF read: nothing
    [item] = check(answer, {1}, {1})
    assert item["sheets"] == [{"msg_id": 1, "course": "", "number": "2", "due": None}]  # one per message


# --- publishing ----------------------------------------------------------------------------------

async def found(batches, llm, msg_id, data, **sheet) -> None:
    send(batches, msg_id, data, name=sheet.pop("name", "list3.pdf"))
    llm.answers.append({"items": [sheet_item(msg_id, **sheet)]})
    await parse(batches)


async def test_published_and_versions(batches, llm, sheets, topic, schedule):
    await schedule.subjects.resolve(["Математический анализ"])
    await found(batches, llm, 1, SHEET3)
    await topic.publish()
    first = sheets.get(1)
    assert (first["status"], first["version"], first["previous"]) == ("posted", 1, None)
    assert first["text"].startswith("Calculus. Sheet 3") and llm.readings == []  # pdf_content is off: no reading

    # The same number again: a newer version.
    await found(batches, llm, 2, SHEET3_FIXED, name="list3_fixed.pdf", due="2026-10-30")
    await topic.publish()
    second = sheets.get(2)
    assert (second["status"], second["previous"], second["version"]) == ("posted", 1, 2)

    # Another number is another sheet; so is a sheet older than 30 days.
    await found(batches, llm, 3, make_pdf(["Sheet 4"]), number="4")
    await topic.publish()
    assert sheets.get(3)["previous"] is None

    await found(batches, llm, 4, make_pdf(["Sheet 3 again"]), name="list3_v3.pdf")
    await topic.publish()
    assert (sheets.get(4)["previous"], sheets.get(4)["version"]) == (2, 3)
    assert [s["versions"] for s in sheets.listing()] == [1, 3]


async def test_same_message_new_file_is_a_version(batches, llm, sheets, topic, now):
    await found(batches, llm, 1, SHEET3, number="")
    await topic.publish()
    batches.files.remote[CHAT, 1] = SHEET3_FIXED
    batches.inbox.edit(CHAT, 1, "", [{"kind": "pdf", "name": "totally_other.pdf", "size": len(SHEET3_FIXED)}],
                       now() + dt.timedelta(minutes=1))
    llm.answers.append({"items": [sheet_item(1, number="", course="")]})
    await parse(batches)
    await topic.publish()
    assert sheets.get(2)["previous"] == 1


async def test_reading_the_whole_sheet(batches, llm, sheets, topic, schedule):
    sheets.chat_settings.update(CHAT, {"pdf_content": True})
    sheets.settings.set("pdf_pages", 1)
    sheets.settings.set("language", "ru")  # the summary is written in the app's language (D41)
    await schedule.subjects.resolve(["Математический анализ"])
    llm.readings = [{"summary": "Ряды: 6 задач, сдать в LMS.",
                     "deadlines": [{"label": "Calc sheet 3", "due": "2026-10-29"},
                                   {"label": "Calc defence", "due": "2026-10-31"},
                                   {"label": "a label far too long for Tablo", "due": "2026-10-31"},
                                   {"label": "Calc", "due": "31.10"}]}]
    await found(batches, llm, 1, SHEET3)
    await topic.publish()
    reading = llm.calls[-1]
    assert reading.task == "pdf" and reading.documents == []
    assert "- summary: in Russian" in reading.system and 'no "в этом листке"' in reading.system
    assert "Course: Математический анализ, abbreviation Calc" in reading.prompt
    assert "Text of the sheet (1 of 2 pages):\n--- page 1 ---\nCalculus. Sheet 3" in reading.prompt
    assert "Task 1" not in reading.prompt  # past the page limit
    assert sheets.get(1)["summary"] == "Ряды: 6 задач, сдать в LMS."
    today = clock.local_day(batches.now())
    saved = {d["label"]: d for d in batches.deadlines.upcoming(today)}
    assert set(saved) == {"Calc sheet 3", "Calc defence"}
    assert saved["Calc defence"]["sheet_id"] == 1 and saved["Calc defence"]["msg_ids"] == [1]
    assert batches.deadlines.made_of("tg", CHAT, 1) == []  # an edit of the message's text keeps them

    # A newer version: the model sees only the changed lines.
    llm.readings = [{"changes": "Срок сдвинут на 30.10, в задаче 1 уточнено условие."}]
    await found(batches, llm, 2, SHEET3_FIXED, name="list3_new.pdf")
    await topic.publish()
    describing = llm.calls[-1]
    assert describing.task == "pdf" and "Changed lines:\n-Hand in by 29.10 23:59 in the LMS" in describing.prompt
    assert "Output changes: in Russian" in describing.system and "Изменилось только оформление." in describing.system
    assert "+Task 1. Prove that the series converges" in describing.prompt
    assert sheets.get(2)["changes"] == "Срок сдвинут на 30.10, в задаче 1 уточнено условие."
    assert sheets.get(1)["summary"] == "Ряды: 6 задач, сдать в LMS."  # the first version's summary stays


async def test_a_scan_is_read_as_pages_and_the_limit_is_kept(batches, llm, sheets, topic):
    sheets.chat_settings.update(CHAT, {"pdf_content": True})
    sheets.settings.set("pdf_tokens", 4000)  # one page of a scan
    llm.readings = [BudgetExceeded("optional LLM tasks are off above 80% of the daily limit")]
    await found(batches, llm, 1, SCAN, name="scan.pdf")
    await topic.publish()
    reading = llm.calls[-1]
    assert [d.pages for d in reading.documents] == [1] and "first 1 of 2 pages are attached" in reading.prompt
    assert sheets.get(1)["status"] == "posted" and sheets.get(1)["summary"] is None  # published all the same


async def test_a_failure_waits_and_a_lost_file_fails(batches, llm, sheets, topic):
    await found(batches, llm, 1, SHEET3)

    async def down(sheet_id):
        raise ConnectionError("the twin is down")

    sheets.prepare, working = down, sheets.prepare
    await topic.publish()
    assert sheets.get(1)["status"] == "new" and sheets.status() == {"waiting": 1, "error": None}
    sheets.prepare = working
    await topic.publish()
    assert sheets.get(1)["status"] == "posted"

    await found(batches, llm, 2, make_pdf(["Sheet 9"]), number="9")
    batches.files.by_hash(sheets.get(2)["sha256"]).unlink()
    await topic.publish()
    assert sheets.get(2)["status"] == "failed" and sheets.get(2)["error"] == "the file is deleted already"


async def test_owner_joins_a_sheet_to_an_older_one(batches, llm, sheets, topic):
    await found(batches, llm, 1, SHEET3, number="3")
    await found(batches, llm, 2, SHEET3_FIXED, number="3а", name="ряды.pdf")  # not recognised: published as new
    await topic.publish()
    assert sheets.get(2)["previous"] is None

    with pytest.raises(ValueError, match="the same sheet"):
        await topic.join(1, 1)
    batches.files.by_hash(sheets.get(2)["sha256"]).unlink()  # its file is gone: it is joined all the same
    await topic.join(2, 1)
    joined = sheets.get(2)
    assert (joined["previous"], joined["version"], joined["status"]) == (1, 2, "posted")
    with pytest.raises(ValueError, match="has versions already"):
        await topic.join(1, 2)
    assert [(s["id"], s["first"], s["versions"]) for s in sheets.listing()] == [(2, 1, 2)]


# --- the LLM client -----------------------------------------------------------------------------------

async def test_documents_go_before_the_prompt(budget, journal, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test")
    usage = SimpleNamespace(input_tokens=3000, output_tokens=10, cache_creation_input_tokens=0,
                            cache_read_input_tokens=0)
    calls = []

    async def create(**kwargs):
        calls.append(kwargs)
        return SimpleNamespace(usage=usage, stop_reason="end_turn", stop_details=None,
                               content=[SimpleNamespace(type="text", text='{"x": 1}')])

    llm = Haiku(budget, journal)
    llm._client = SimpleNamespace(messages=SimpleNamespace(create=create))
    await llm.json("pdf", "sys", "read it", {"type": "object"}, documents=[Document(b"%PDF-1.4", 2)])
    [document, text] = calls[0]["messages"][0]["content"]
    assert document == {"type": "document", "source": {"type": "base64", "media_type": "application/pdf",
                                                       "data": "JVBERi0xLjQ="}}
    assert text == {"type": "text", "text": "read it"}

    budget.settings.set("llm_daily_limit_usd", 0.004)  # a page is reserved as 3000 tokens: 200 pages do not fit
    with pytest.raises(BudgetExceeded):
        await llm.json("pdf", "sys", "read it", {"type": "object"}, documents=[Document(b"%PDF", 200)])


async def test_a_deleted_message_withdraws_the_deadlines_of_its_sheet(batches, llm, sheets, topic, now):
    sheets.chat_settings.update(CHAT, {"pdf_content": True})
    llm.readings = [{"summary": "", "deadlines": [{"label": "Calc defence", "due": "2026-10-31"}]}]
    await found(batches, llm, 1, SHEET3)
    await topic.publish()
    batches.inbox.delete([1], CHAT, now())
    changes = await parse(batches)
    [gone] = changes.gone
    assert batches.deadlines.get(gone)["sheet_id"] == 1


# --- the web app ------------------------------------------------------------------------------------

async def test_actions_screen(aiohttp_client, batches, llm, sheets, topic, budget, journal, store, schedule, merged):
    await found(batches, llm, 1, SHEET3)
    await found(batches, llm, 2, make_pdf(["Sheet 4"]), number="4", name="list4.pdf")
    await topic.publish()
    hse = HseApp(store, sheets.settings, env().public_url + CALLBACK_PATH)
    app = actions_app.app(env(), budget, journal, sheets.inbox, sheets.chat_settings, {"sheets": sheets.status},
                             schedule.subjects, schedule, hse, merged, settings=sheets.settings, sheets=topic)
    client = await aiohttp_client(app)
    owner = {}

    state = await (await client.get("/app/api/sheets", headers=owner)).json()
    assert state["limits"] == {"pdf_pages": 10, "pdf_tokens": 30000}
    assert state["status"] == {"waiting": 0, "error": None}
    assert [(s["id"], s["name"], s["number"], s["versions"], s["file"], s["chat"]) for s in state["sheets"]] == \
        [(2, "list4.pdf", "4", 1, True, "Матан"), (1, "list3.pdf", "3", 1, True, "Матан")]

    bad = await client.patch("/app/api/sheets", json={"pdf_pages": 5, "pdf_tokens": 10}, headers=owner)
    assert bad.status == 400 and sheets.settings.get("pdf_pages") == 10  # neither is saved
    assert (await client.patch("/app/api/sheets", json={"oops": 1}, headers=owner)).status == 400
    state = await (await client.patch("/app/api/sheets", json={"pdf_pages": 5}, headers=owner)).json()
    assert state["limits"]["pdf_pages"] == 5


async def test_owner_changes_a_sheet(aiohttp_client, batches, llm, sheets, topic, budget, journal,
                                            store, schedule, merged):
    """The owner changes a sheet's fields; adding, deleting and joining are the shelves' (test_web_shelves)."""
    await found(batches, llm, 1, SHEET3)
    await topic.publish()
    hse = HseApp(store, sheets.settings, env().public_url + CALLBACK_PATH)
    client = await aiohttp_client(actions_app.app(env(), budget, journal, sheets.inbox, sheets.chat_settings,
                                                     {"sheets": sheets.status}, schedule.subjects, schedule, hse,
                                                     merged, settings=sheets.settings, sheets=topic))
    owner = {}

    change = {"course": "Линейная алгебра", "number": " 3b ", "due": "2026-11-02", "summary": "Series, 6 tasks."}
    [listed] = (await (await client.patch("/app/api/sheets/1", json=change, headers=owner)).json())["sheets"]
    assert (listed["number"], listed["due"], listed["summary"]) == ("3b", "2026-11-02", "Series, 6 tasks.")
    state = await (await client.patch("/app/api/sheets/1", json={"due": "", "summary": ""}, headers=owner)).json()
    assert (state["sheets"][0]["due"], state["sheets"][0]["summary"]) == (None, None)
    for bad in ({}, {"number": 5}, {"due": "2.11"}, {"oops": 1}, {"summary": "x" * 601}):
        assert (await client.patch("/app/api/sheets/1", json=bad, headers=owner)).status == 400
    assert (await client.patch("/app/api/sheets/99", json={"number": "1"}, headers=owner)).status == 404


# --- the assumed due date (§6.3) ---------------------------------------------------------------------

def lesson(n, subject, start, kind="Seminar", cancelled=False) -> Class:
    """A class of the fictional RUZ feed: `start` is "MM-DD HH:MM" in 2026, 80 minutes long."""
    begin = dt.datetime.fromisoformat(f"2026-{start.replace(' ', 'T')}")
    return Class(f"30000{n}@ruz.example", subject, begin, begin + dt.timedelta(minutes=80), kind, "R101", None,
                 cancelled)


TIMETABLE = [
    lesson(1, "Математический анализ", "10-21 13:00", "Лекция"),
    lesson(2, "Calculus", "10-22 09:30", "Seminar"),
    lesson(3, "Математический анализ", "10-29 09:30", "Проведение семинаров (студенты)"),
    lesson(4, "Algebra", "10-21 11:10", "Ongoing interim assessment"),  # never a sheet's class
    lesson(5, "Algebra", "10-23 13:00", "Лекция"),
]


@pytest.fixture
async def timetable(schedule, sheets, batches):
    """RUZ classes of Calculus (lectures and seminars) and Algebra (no seminars); the sheets follow them."""
    sheets.schedule = schedule
    schedule.followers.append(sheets.follow)
    schedule.ruz._sync(TIMETABLE)
    await schedule.subjects.resolve(["Математический анализ", "Calculus", "Алгебра", "Algebra"])
    return schedule


async def posted(batches, llm, msg_id, data, **sheet) -> None:
    """A sheet found by a whole batch run: the run ends by syncing the deadlines that follow the timetable."""
    send(batches, msg_id, data, name=sheet.pop("name", "list3.pdf"))
    llm.answers.append({"items": [sheet_item(msg_id, **sheet)]})
    await batches.run_chats([CHAT])


def assumed_rows(batches) -> list[dict]:
    return [d for d in batches.deadlines.upcoming(clock.local_day(batches.now())) if d["assumed"]]


async def test_a_sheet_without_a_date_is_due_at_the_next_seminar(batches, llm, sheets, topic, timetable,
                                                                  merged, store, now):
    watch = DeadlineWatch(store, merged, now=now)
    told = []
    watch.notify = lambda d, was: told.append((d.title, d.due.isoformat(), d.assumed, was))
    await watch.refresh()  # the first look is quiet
    await posted(batches, llm, 1, SHEET3, due="")  # posted Tue 11:30: the lecture of Wed is no seminar
    sheet = sheets.get(1)
    assert (sheet["due"], sheet["due_at"], sheet["due_assumed"]) == ("2026-10-22", "2026-10-22T09:30:00", 1)
    [row] = assumed_rows(batches)
    assert (row["label"], row["due"], row["sheet_id"], row["msg_ids"], row["source"]) == (
        "Calc sheet 3", "2026-10-22", 1, [1], "tg")
    [item] = [i for i in merged.items() if i["title"].startswith("Calc")]
    assert (item["title"], item["due"], item["assumed"]) == ("Calc sheet 3", "2026-10-22", True)

    await topic.publish()
    await watch.refresh()
    assert ("Calc sheet 3", "2026-10-22", True, None) in told  # pushed as new, the date marked assumed

    # The batch model sees the date as assumed.
    send(batches, 2, None, text="ок")
    llm.answers.append({"items": []})
    await batches.run_chats([CHAT])
    assert f'- d{row["id"]}: "Calc sheet 3" 2026-10-22 assumed' in llm.calls[-1].prompt


async def test_a_cancelled_seminar_is_skipped(batches, llm, sheets, timetable, now):
    timetable.ruz._sync([*TIMETABLE[:1], lesson(2, "Calculus", "10-22 09:30", cancelled=True), *TIMETABLE[2:]])
    await posted(batches, llm, 1, SHEET3, due="")
    assert sheets.get(1)["due"] == "2026-10-29" and assumed_rows(batches)[0]["due"] == "2026-10-29"

    # A chat cancels that one too: nothing within 21 days, no date (and no deadline) until a class comes.
    uid = TIMETABLE[2].uid
    item = timetable.ruz.by_uid(uid)
    with timetable.db:
        timetable.events.add({"uid": uid, "class_uid": uid, "source": "tg", "chat_id": CHAT, "msg_ids": [5],
                              "course": item["subject"], "title": "", "kind": item["kind"], "start": item["start"],
                              "end": item["end"], "room": item["room"], "required": True, "cancelled": True}, now())
    changes = timetable.sync_deadlines()
    sheet = sheets.get(1)
    assert (sheet["due"], sheet["due_assumed"]) == (None, 1) and assumed_rows(batches) == [] and changes.gone
    timetable.ruz._sync([*TIMETABLE[:1], lesson(2, "Calculus", "10-22 09:30", cancelled=True), *TIMETABLE[2:],
                         lesson(6, "Calculus", "11-05 09:30")])
    timetable.sync_deadlines()
    assert sheets.get(1)["due"] == "2026-11-05" and assumed_rows(batches)[0]["due"] == "2026-11-05"


async def test_a_course_without_seminars_takes_its_next_class(batches, llm, sheets, timetable):
    await posted(batches, llm, 1, SHEET3, due="", course="Алгебра", number="1", name="alg1.pdf")
    sheet = sheets.get(1)
    assert (sheet["due"], sheet["due_at"]) == ("2026-10-23", "2026-10-23T13:00:00")  # not the interim assessment
    assert [d["label"] for d in assumed_rows(batches)] == ["Alg sheet 1"]

    await posted(batches, llm, 2, make_pdf(["Sheet 1"]), due="", course="История", number="1", name="h.pdf")
    assert (sheets.get(2)["due"], sheets.get(2)["due_assumed"]) == (None, 1)  # no classes at all: no date


async def test_a_real_date_wins(batches, llm, sheets, topic, timetable):
    await posted(batches, llm, 1, SHEET3)  # the first page gives the date: nothing assumed
    assert (sheets.get(1)["due"], sheets.get(1)["due_assumed"]) == ("2026-10-29", 0) and assumed_rows(batches) == []

    # A later message moves the assumed deadline: the sheet takes the date, the mark goes.
    await posted(batches, llm, 2, make_pdf(["Sheet 4"]), due="", number="4", name="list4.pdf")
    [row] = assumed_rows(batches)
    send(batches, 3, None, text="Листок 4 сдать до пятницы")
    llm.answers.append({"items": [{"msg_ids": [3], "news": None, "events": [], "sheets": [], "deadlines": [
        {"course": "Математический анализ", "label": "Calc sheet 4", "due": "2026-10-23", "moves": f"d{row['id']}"}]}]})
    await batches.run_chats([CHAT])
    sheet = sheets.get(2)
    assert (sheet["due"], sheet["due_assumed"], sheet["due_at"]) == ("2026-10-23", 0, None)
    newer = batches.deadlines.latest(row["id"])
    assert (newer["due"], newer["assumed"]) == ("2026-10-23", 0) and assumed_rows(batches) == []

    # The same work on the assumed date, told without a move: it is real now.
    await posted(batches, llm, 4, make_pdf(["Sheet 5"]), due="", number="5", name="list5.pdf")
    [row] = assumed_rows(batches)
    send(batches, 5, None, text="Листок 5 к семинару в четверг")
    llm.answers.append({"items": [{"msg_ids": [5], "news": None, "events": [], "sheets": [], "deadlines": [
        {"course": "Математический анализ", "label": "Calc sheet 5", "due": row["due"], "moves": ""}]}]})
    await batches.run_chats([CHAT])
    assert batches.deadlines.get(row["id"])["assumed"] == 0 and sheets.get(3)["due_assumed"] == 0

    # The sheet's own pages give a date: it replaces the assumed one.
    await topic.publish()
    sheets.chat_settings.update(CHAT, {"pdf_content": True})
    llm.readings = [{"summary": "", "deadlines": [{"label": "Calc sheet 6", "due": "2026-10-30"}]}]
    await posted(batches, llm, 6, make_pdf(["Sheet 6"]), due="", number="6", name="list6.pdf")
    [sheet] = [s for s in sheets.waiting() if s["msg_id"] == 6]
    assert sheet["due_assumed"] == 1 and assumed_rows(batches)[0]["due"] == "2026-10-22"
    await topic.publish()
    sheet = sheets.get(sheet["id"])
    assert (sheet["due"], sheet["due_assumed"]) == ("2026-10-30", 0) and assumed_rows(batches) == []


async def test_the_owner_clears_the_mark(aiohttp_client, batches, llm, sheets, topic, budget, journal, store,
                                         timetable, merged):
    sheets.on_change = merged.on_change  # the owner's changes are no news
    await posted(batches, llm, 1, SHEET3, due="")
    await posted(batches, llm, 2, make_pdf(["Sheet 4"]), due="", number="4", name="list4.pdf")
    await topic.publish()
    hse = HseApp(store, sheets.settings, env().public_url + CALLBACK_PATH)
    client = await aiohttp_client(actions_app.app(env(), budget, journal, sheets.inbox, sheets.chat_settings,
                                                     {"sheets": sheets.status}, timetable.subjects, timetable, hse,
                                                     merged, settings=sheets.settings, sheets=topic))
    owner = {}
    listed = {s["id"]: s for s in (await (await client.get("/app/api/sheets", headers=owner)).json())["sheets"]}
    assert (listed[1]["due_assumed"], listed[1]["due_at"]) == (True, "2026-10-22T09:30:00")
    items = (await (await client.get("/app/api/deadlines", headers=owner)).json())["items"]
    assert [(d["title"], d["assumed"]) for d in items if d["title"].startswith("Calc")] == [
        ("Calc sheet 3", True), ("Calc sheet 4", True)]

    # The description alone keeps the date assumed; a date makes it real.
    await client.patch("/app/api/sheets/1", json={"summary": "Series."}, headers=owner)
    assert sheets.get(1)["due_assumed"] == 1
    await client.patch("/app/api/sheets/1", json={"due": "2026-10-25"}, headers=owner)
    sheet = sheets.get(1)
    assert (sheet["due"], sheet["due_assumed"]) == ("2026-10-25", 0)
    [real] = [d for d in batches.deadlines.upcoming(clock.local_day(batches.now())) if d["sheet_id"] == 1]
    assert (real["due"], real["assumed"]) == ("2026-10-25", 0) and f"dl:{real['id']}" in merged.changes[-1]

    # A new date for the assumed deadline itself: it moves and stays the chat's, the sheet takes the date.
    [d] = [d for d in items if d["title"] == "Calc sheet 4"]
    body = {"title": "Calc sheet 4", "due": "2026-10-26", "category": "study"}
    state = await (await client.patch(f"/app/api/deadlines/{d['uid']}", json=body, headers=owner)).json()
    [moved] = [i for i in state["items"] if i["title"] == "Calc sheet 4"]
    assert (moved["due"], moved["assumed"], moved["source"]) == ("2026-10-26", False, "tg") and state["own"] == []
    assert (sheets.get(2)["due"], sheets.get(2)["due_assumed"]) == ("2026-10-26", 0)
    timetable.ruz._sync([*TIMETABLE[:1], lesson(2, "Calculus", "10-22 11:10"), *TIMETABLE[2:]])
    timetable.sync_deadlines()
    assert sheets.get(2)["due"] == "2026-10-26"  # a real date does not follow the timetable


async def test_the_assumed_date_follows_the_timetable(batches, llm, sheets, topic, timetable, now):
    await posted(batches, llm, 1, SHEET3, due="")
    await topic.publish()
    [first] = assumed_rows(batches)

    # RUZ moves the seminar to Friday: the date follows.
    diff = timetable.ruz._sync([*TIMETABLE[:1], lesson(2, "Calculus", "10-23 11:10"), *TIMETABLE[2:]])
    changes = await timetable.after_poll(diff)
    [row] = assumed_rows(batches)
    assert (row["due"], batches.deadlines.get(first["id"])["moved_by"]) == ("2026-10-23", row["id"])
    assert changes.moved == [(first["id"], row["id"])] and sheets.get(1)["due_at"] == "2026-10-23T11:10:00"

    # The owner moves the class in the web app: the date follows the feeds as they are.
    await timetable.correct_class(TIMETABLE[1].uid, "2026-10-24T10:00:00", "2026-10-24T11:20:00", "R101", False)
    assert assumed_rows(batches)[0]["due"] == "2026-10-24"

    # Once the class has started, the date stays.
    now.moment = dt.datetime(2026, 10, 24, 10, 30, tzinfo=clock.TZ).astimezone(dt.UTC)
    await timetable.correct_class(TIMETABLE[1].uid, "2026-10-25T10:00:00", "2026-10-25T11:20:00", "R101", False)
    assert sheets.get(1)["due"] == "2026-10-24"


async def test_a_new_version_takes_the_assumed_date_over(batches, llm, sheets, topic, timetable, now):
    await posted(batches, llm, 1, SHEET3, due="")
    await topic.publish()
    [first] = assumed_rows(batches)
    now.moment = dt.datetime(2026, 10, 22, 12, 0, tzinfo=clock.TZ).astimezone(dt.UTC)  # after Thursday's seminar
    await posted(batches, llm, 2, SHEET3_FIXED, due="", name="list3_fixed.pdf")
    await topic.publish()
    assert sheets.get(2)["previous"] == 1
    [row] = assumed_rows(batches)
    assert (row["sheet_id"], row["due"]) == (2, "2026-10-29")
    assert batches.deadlines.latest(first["id"])["id"] == row["id"]  # one deadline, moved

    # Its message deleted withdraws the deadline: the sheet has no date any more and nothing comes back.
    batches.inbox.delete([2], CHAT, now())
    await batches.run_chats([CHAT])
    assert assumed_rows(batches) == [] and (sheets.get(2)["due"], sheets.get(2)["due_assumed"]) == (None, 0)


# --- the library (D58) -------------------------------------------------------------------------------

async def test_sheets_go_to_the_library(batches, llm, sheets, topic, schedule, store, files, tmp_path):
    schedule.ruz._sync([lesson(9, "Математический анализ", "10-20 10:00", "Семинар")])  # the sheet is sent at 11:30
    library = Library(store, tmp_path, now=batches.now)
    library.same_course = schedule.same_course
    sheets.library = library

    await found(batches, llm, 1, SHEET3)
    await topic.publish()
    [item] = library.items(kind="sheet")
    assert (item["source"], item["chat_id"], item["msg_id"], item["title"], item["author"]) == (
        "sheet", CHAT, 1, "Sheet 3", "Ассистент")
    assert (item["course"], item["class_uid"], item["name"], item["day"]) == (
        "Математический анализ", "300009@ruz.example", "list3.pdf", "2026-10-20")
    assert library.path(item).read_bytes() == SHEET3 and item["media_type"] == "application/pdf"
    await sheets.prepare(1)  # prepared again (a post that failed): taken once
    assert len(library.items(kind="sheet")) == 1

    # The same message with a new file is a new version: its file is taken too. A sheet of no course and no number
    # waits in the inbox.
    files.remote[CHAT, 1] = SHEET3_FIXED
    batches.inbox.edit(CHAT, 1, "", [{"kind": "pdf", "name": "list3_fixed.pdf", "size": len(SHEET3_FIXED)}],
                       batches.now() + dt.timedelta(minutes=1))
    llm.answers.append({"items": [sheet_item(1, number="", course="")]})
    await parse(batches)
    await topic.publish()
    newer = next(i for i in library.items(kind="sheet") if i["id"] != item["id"])
    assert (newer["title"], newer["course"], newer["class_uid"]) == ("Sheet", None, None)
    assert library.path(newer).read_bytes() == SHEET3_FIXED and library.inbox_count() == 1


# --- a PDF sent to the bot (D73) -----------------------------------------------------------------------

OWNER = 4242


def to_bot(msg_id, data: bytes, name="hw5.pdf", text=""):
    """The owner's private message to the bot with a PDF, and its download."""
    message = {"chat_id": OWNER, "msg_id": msg_id, "at": clock.now_utc(), "text": text, "forward": None,
               "files": [{"kind": "pdf", "name": name, "mime": "application/pdf", "size": len(data)}]}
    downloads = []

    async def download(path):
        downloads.append(path)
        path.write_bytes(data)
        return True

    return message, download, downloads


@pytest.fixture
def bot_sheets(store, tmp_path, topic):
    topic.sheets.library = Library(store, tmp_path / "library")  # as the app links them
    taken = BotSheets(topic, topic.sheets.library)
    taken.journaled = []
    taken.journal = lambda tool, arguments, summary, undo: taken.journaled.append((tool, arguments, undo))
    taken.can_undo = lambda op: op == "_remove_own_sheet"
    return taken


async def test_a_sheet_sent_to_the_bot_is_a_sheet_with_its_caption(bot_sheets, llm, sheets):
    caption = "ДЗ 5 по матану, сдать до пятницы"
    message, download, downloads = to_bot(1, SHEET3, text=caption)
    llm.readings = [{"sheet": True, "course": "Математический анализ", "number": "5", "due": "2026-10-30"}]
    assert await bot_sheets.take(message, download) == "sheet"
    [call] = llm.calls
    assert call.task == "pdf" and caption in call.prompt and "Calculus. Sheet 3" in call.prompt and not call.documents
    [listed] = sheets.listing()
    assert (listed["own"], listed["course"], listed["number"], listed["due"], listed["caption"], listed["status"]) == (
        True, "Математический анализ", "5", "2026-10-30", caption, "posted")
    assert f"The owner's caption: {caption}" in sheets._context(sheets.get(listed["id"]))
    assert bot_sheets.journaled == [("add_sheet", {"sheet_id": listed["id"]},
                                     [{"tool": "_remove_own_sheet", "arguments": {"sheet_id": listed["id"]}}])]
    assert len(downloads) == 1 and not downloads[0].exists()  # downloaded once, the part removed
    # In the library as a sheet, not as an item of the inbox too.
    assert bot_sheets.library.items(inbox=True) == [] and len(bot_sheets.library.items(kind="sheet")) == 1

    again, download, _ = to_bot(2, SHEET3, name="copy.pdf")
    assert await bot_sheets.take(again, download) == "known" and len(llm.calls) == 1 and len(sheets.listing()) == 1


async def test_a_pdf_that_is_no_sheet_goes_to_the_inbox(bot_sheets, llm, sheets):
    slides, download, downloads = to_bot(1, make_pdf(["Lecture 4. Series"]), name="lec4.pdf", text="slides")
    llm.readings = [{"sheet": False, "course": "", "number": "", "due": ""}]
    assert await bot_sheets.take(slides, download) == "inbox"
    [item] = bot_sheets.library.items(inbox=True)
    assert item["name"] == "lec4.pdf" and bot_sheets.library.path(item).read_bytes() == make_pdf(["Lecture 4. Series"])
    assert len(downloads) == 1 and sheets.listing() == [] and bot_sheets.journaled == []

    # No answer from the model: the inbox as before; a scan goes to it as its first page.
    scan, download, _ = to_bot(2, SCAN, name="scan.pdf")
    llm.readings = [BudgetExceeded("the limit")]
    assert await bot_sheets.take(scan, download) == "inbox"
    assert len(llm.calls[-1].documents) == 1 and len(bot_sheets.library.items(inbox=True)) == 2

    # Not a PDF: no model at all.
    note = {"chat_id": OWNER, "msg_id": 3, "at": clock.now_utc(), "text": "a note", "forward": None, "files": []}
    assert await bot_sheets.take(note, download) == "inbox" and len(llm.calls) == 2
