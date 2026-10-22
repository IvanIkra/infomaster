"""Листочки (§6.3, р. 14): the PDF of a message fetched for its batch, the sheet the model finds in it, its versions,
its reading within the limits and its posts in «Листочки»."""

import datetime as dt
from types import SimpleNamespace

import pytest
from conftest import make_pdf
from test_http import env, init_data

from infomaster import clock
from infomaster.collectors.files import Files
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.llm.budget import BudgetExceeded
from infomaster.llm.client import Document, Haiku
from infomaster.outputs import miniapp
from infomaster.outputs.sheets import SheetTopic, hashtag
from infomaster.processing import pdf
from infomaster.processing.batches import Batches, Changes, check
from infomaster.processing.news import News
from infomaster.processing.sheets import Sheets, number_key
from infomaster.settings import ChatSettings, Settings

CHAT = -1001234567890
SHEET3 = make_pdf(["Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS", "Task 1. Prove that the series"])
SHEET3_FIXED = make_pdf(["Calculus. Sheet 3. Series\nHand in by 30.10 23:59 in the LMS",
                         "Task 1. Prove that the series converges"])
SCAN = make_pdf(["", ""])


class FakeLLM:
    """The batch answers in order; the readings of sheets ("pdf") too, from their own list."""

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
def topic(forum, store, sheets, now):
    return SheetTopic(forum, store, sheets.inbox, sheets, now=now)


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
    assert hashtag("Calc") == "#Calc" and hashtag("Rus Hist") == "#Rus_Hist" and hashtag(None) == ""


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


# --- the forum -------------------------------------------------------------------------------------

async def found(batches, llm, msg_id, data, **sheet) -> None:
    send(batches, msg_id, data, name=sheet.pop("name", "list3.pdf"))
    llm.answers.append({"items": [sheet_item(msg_id, **sheet)]})
    await parse(batches)


async def test_posts_and_versions(batches, llm, sheets, topic, forum, schedule):
    await schedule.subjects.resolve(["Математический анализ"])
    await found(batches, llm, 1, SHEET3)
    await topic.publish()
    [post] = forum.files
    assert forum.files[post] == (SHEET3, "list3.pdf")
    assert forum.messages[post][1] == ('📄 <b>Calculus · листок 3</b>\nСрок: чт 29.10\n'
                                       '<a href="https://t.me/c/1234567890/1">Матан</a> · #Calc')
    first = sheets.get(1)
    assert (first["status"], first["post_id"], first["version"]) == ("posted", post, 1)
    assert first["text"].startswith("Calculus. Sheet 3") and llm.readings == []  # pdf_content is off: no reading

    # The same number again: the first message takes the new file, a reply tells it.
    await found(batches, llm, 2, SHEET3_FIXED, name="list3_fixed.pdf", due="2026-10-30")
    await topic.publish()
    assert list(forum.files) == [post] and forum.files[post] == (SHEET3_FIXED, "list3_fixed.pdf")
    assert forum.messages[post][1].startswith("📄 <b>Calculus · листок 3</b> (версия 2)\nСрок: пт 30.10")
    [reply] = forum.replies
    assert forum.replies[reply] == post
    assert forum.messages[reply][1] == ('🔄 Листок обновлён: версия 2, файл «list3_fixed.pdf»\n'
                                        '<a href="https://t.me/c/1234567890/2">Матан</a>')
    second = sheets.get(2)
    assert (second["previous"], second["version"], second["post_id"]) == (1, 2, post)

    # Another number is another sheet; so is a sheet older than 30 days.
    await found(batches, llm, 3, make_pdf(["Sheet 4"]), number="4")
    await topic.publish()
    assert sheets.get(3)["previous"] is None and len(forum.files) == 2

    # The first message was deleted by hand: the next version is posted anew and carries the sheet.
    forum.messages.pop(post)
    await found(batches, llm, 4, make_pdf(["Sheet 3 again"]), name="list3_v3.pdf")
    await topic.publish()
    third = sheets.get(4)
    assert third["version"] == 3 and third["post_id"] not in (None, post)
    assert {sheets.get(i)["post_id"] for i in (1, 2, 4)} == {third["post_id"]}
    assert [s["versions"] for s in sheets.listing()] == [1, 3]


async def test_same_message_new_file_is_a_version(batches, llm, sheets, topic, forum, now):
    await found(batches, llm, 1, SHEET3, number="")
    await topic.publish()
    batches.files.remote[CHAT, 1] = SHEET3_FIXED
    batches.inbox.edit(CHAT, 1, "", [{"kind": "pdf", "name": "totally_other.pdf", "size": len(SHEET3_FIXED)}],
                       now() + dt.timedelta(minutes=1))
    llm.answers.append({"items": [sheet_item(1, number="", course="")]})
    await parse(batches)
    await topic.publish()
    assert sheets.get(2)["previous"] == 1 and len(forum.files) == 1


async def test_reading_the_whole_sheet(batches, llm, sheets, topic, forum, schedule):
    sheets.chat_settings.update(CHAT, {"pdf_content": True})
    sheets.settings.set("pdf_pages", 1)
    sheets.settings.set("forum_language", "ru")  # the summary is written for the forum (р. 41)
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
    [post] = forum.files
    assert forum.messages[post][1].endswith("#Calc\n\nРяды: 6 задач, сдать в LMS.")
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
    reply = max(forum.replies)
    assert "\nСрок сдвинут на 30.10, в задаче 1 уточнено условие.\n" in forum.messages[reply][1]
    assert forum.messages[post][1].endswith("Ряды: 6 задач, сдать в LMS.")  # the first version's summary stays


async def test_a_scan_is_read_as_pages_and_the_limit_is_kept(batches, llm, sheets, topic, forum):
    sheets.chat_settings.update(CHAT, {"pdf_content": True})
    sheets.settings.set("pdf_tokens", 4000)  # one page of a scan
    llm.readings = [BudgetExceeded("optional LLM tasks are off above 80% of the daily limit")]
    await found(batches, llm, 1, SCAN, name="scan.pdf")
    await topic.publish()
    reading = llm.calls[-1]
    assert [d.pages for d in reading.documents] == [1] and "first 1 of 2 pages are attached" in reading.prompt
    [post] = forum.files  # posted without a summary all the same
    assert sheets.get(1)["summary"] is None and "\n\n" not in forum.messages[post][1]


async def test_a_failed_post_waits_and_a_lost_file_fails(batches, llm, sheets, topic, forum):
    await found(batches, llm, 1, SHEET3)

    async def down(*args):
        raise ConnectionError("Telegram is down")

    forum.post_file, working = down, forum.post_file
    await topic.publish()
    assert sheets.get(1)["status"] == "new" and sheets.status() == {"waiting": 1, "error": None}
    forum.post_file = working
    await topic.publish()
    assert sheets.get(1)["status"] == "posted"

    await found(batches, llm, 2, make_pdf(["Sheet 9"]), number="9")
    batches.files.by_hash(sheets.get(2)["sha256"]).unlink()
    await topic.publish()
    assert sheets.get(2)["status"] == "failed" and sheets.get(2)["error"] == "the file is deleted already"


async def test_owner_joins_a_sheet_to_an_older_one(batches, llm, sheets, topic, forum):
    await found(batches, llm, 1, SHEET3, number="3")
    await found(batches, llm, 2, SHEET3_FIXED, number="3а", name="ряды.pdf")  # not recognised: posted as new
    await topic.publish()
    first, own = sheets.get(1)["post_id"], sheets.get(2)["post_id"]
    assert first != own and sheets.get(2)["previous"] is None

    with pytest.raises(ValueError, match="the same sheet"):
        await topic.join(1, 1)
    await topic.join(2, 1)
    assert own in forum.deleted and list(forum.files) == [first] and forum.files[first][0] == SHEET3_FIXED
    joined = sheets.get(2)
    assert (joined["previous"], joined["version"], joined["post_id"]) == (1, 2, first)
    assert forum.replies[max(forum.replies)] == first
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


# --- the Mini App ------------------------------------------------------------------------------------

async def test_miniapp_screen(aiohttp_client, batches, llm, sheets, topic, budget, journal, store, schedule, merged):
    await found(batches, llm, 1, SHEET3)
    await found(batches, llm, 2, make_pdf(["Sheet 4"]), number="4", name="list4.pdf")
    await topic.publish()
    hse = HseApp(store, sheets.settings, env().public_url + CALLBACK_PATH)
    app = miniapp.create_app(env(), budget, journal, sheets.inbox, sheets.chat_settings, {"sheets": sheets.status},
                             schedule.subjects, schedule, hse, merged, sheets.settings, topic)
    client = await aiohttp_client(app)
    owner = {"Authorization": "tma " + init_data()}
    assert (await client.get("/app/api/sheets")).status == 401

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

    assert (await client.post("/app/api/sheets/2/join", json={"previous": "1"}, headers=owner)).status == 400
    assert (await client.post("/app/api/sheets/9/join", json={"previous": 1}, headers=owner)).status == 400
    state = await (await client.post("/app/api/sheets/2/join", json={"previous": 1}, headers=owner)).json()
    assert [(s["id"], s["first"], s["versions"]) for s in state["sheets"]] == [(2, 1, 2)]
