"""The shelves of the web app (§8.6, stage 16.8, D67): the problem sheets with their PDFs and versions, the library's
raw material by course and the sorting of its inbox; every change is the connector's tool, journaled with its undo."""
# ruff: noqa: F811  (the sheets' fixtures come from test_sheets and are used as arguments)

import datetime as dt

import actions_app
import pytest
from aiohttp import FormData
from conftest import make_pdf
from test_http import env
from test_sheets import SHEET3, SHEET3_FIXED, batches, files, found, llm, sheets, topic  # noqa: F401
from test_web import FEED, IPHONE, ORIGIN, Seconds, call, log_in

from infomaster import clock
from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.collectors.ruz import parse
from infomaster.connector import Connector
from infomaster.library import Library
from infomaster.library.links import Links
from infomaster.processing.news import News
from infomaster.web import WebApp
from infomaster.web.views import Data

CALC = "Математический анализ"
PDF = b"%PDF-1.4 the lecture's slides"


def at(now, hour, minute=0):
    now.moment = dt.datetime(2026, 10, 20, hour, minute, tzinfo=clock.TZ).astimezone(dt.UTC)


async def build(aiohttp_client, store, budget, journal, schedule, merged, now, tmp_path, sheets, topic,
                library_on=True):
    schedule.ruz._sync(parse(FEED))
    schedule.subjects.set_manual(CALC, "Calculus", "Calc")
    schedule.subjects.set_manual("Algebra", "Algebra", "Alg")
    at(now, 15, 0)
    library, links = None, None
    if library_on:
        library = Library(store, tmp_path / "library", now=now)
        library.same_course = schedule.same_course
        sheets.library = library
        links = Links(store, library, env().public_url)
    sent, seconds = [], Seconds()

    async def send_code(text):
        sent.append(text)

    hse = HseApp(store, sheets.settings, env().public_url + CALLBACK_PATH)
    app = actions_app.app(env(), budget, journal, schedule.inbox, sheets.chat_settings, {"sheets": sheets.status},
                             schedule.subjects, schedule, hse, merged, sheets.settings, topic, library_links=links)
    connector = Connector(env(), store, News(store), now=now, seconds=seconds)
    connector.attach(app)
    webapp = WebApp(env(), store, send_code=send_code, lang=lambda: "en", now=seconds,
                    data=Data(schedule, merged, library, None, links, lang=lambda: "en"))
    webapp.connector = connector
    client = await aiohttp_client(webapp.attach(app))
    client.token = await log_in(client, sent, seconds)
    client.library, client.connector, client.sheets, client.topic = library, connector, sheets, topic
    client.links = links
    return client


@pytest.fixture
async def shelves(aiohttp_client, store, budget, journal, schedule, merged, now, tmp_path, sheets, topic):
    return await build(aiohttp_client, store, budget, journal, schedule, merged, now, tmp_path, sheets, topic)


@pytest.fixture
async def bare(aiohttp_client, store, budget, journal, schedule, merged, now, tmp_path, sheets, topic):
    return await build(aiohttp_client, store, budget, journal, schedule, merged, now, tmp_path, sheets, topic,
                       library_on=False)


async def ask(client, method, path, data=None, status=200):
    response = await call(client, method, "/shelf" + path, data, token=client.token)
    assert response.status == status, await response.text()
    return await response.json()


async def upload_sheet(client, data=SHEET3_FIXED, status=200, **fields):
    """A multipart upload; `data=None` sends no file."""
    form = FormData()
    for name, value in fields.items():
        form.add_field(name, value)
    if data is not None:
        form.add_field("file", data, filename="mine.pdf", content_type="application/pdf")
    response = await client.post("/web/api/shelf/sheets", data=form, headers={
        "Origin": ORIGIN, "User-Agent": IPHONE, "Cookie": f"im_session={client.token}"})
    assert response.status == status, await response.text()
    return await response.json()


async def two_sheets(client, batches, llm):
    await found(batches, llm, 1, SHEET3)
    await found(batches, llm, 2, make_pdf(["Sheet 4"]), number="4", name="list4.pdf")
    await client.topic.publish()


async def undo(client, change):
    answer = await (await call(client, "POST", f"/changes/{change['id']}/undo", {}, token=client.token)).json()
    return answer


async def last_via(client):
    return client.connector.changes.recent()[0]["via"]


def local(url):
    """A signed link of the service as the path the test client asks."""
    return url.removeprefix(env().public_url)


async def chunks(*parts):
    for part in parts:
        yield part


# --- the entry and the lens --------------------------------------------------------------------------------------

async def test_home_counts_and_the_feed_marks_the_inbox(shelves, batches, llm):
    await two_sheets(shelves, batches, llm)
    shelves.library.add("text", source="web", text="a loose note", title="Note")
    shelves.library.add("text", source="web", text="Series", title="Series", course=CALC)
    home = await ask(shelves, "GET", "")
    assert (home["library"], home["inbox"], home["sheets"]) == (True, 1, 2)
    assert [(c["key"], c["title"], c["items"]) for c in home["courses"]] == [("Calc", "Calculus", 3)]  # +the sheets
    feed = await (await call(shelves, "GET", "/feed", token=shelves.token)).json()
    assert feed["inbox"] == 1
    lens = await (await call(shelves, "GET", "/course?key=Calc", token=shelves.token)).json()
    assert lens["sheets"] == {"count": 2}


async def test_the_library_may_be_off(bare):
    home = await ask(bare, "GET", "")
    assert (home["library"], home["inbox"], home["courses"]) == (False, 0, [])
    for path in ("/inbox", "/raw?course=Calc", "/classes?course=Calc"):
        assert (await ask(bare, "GET", path, status=404))["error"] == "the library is off"
    assert (await (await call(bare, "GET", "/feed", token=bare.token)).json())["inbox"] == 0
    assert (await ask(bare, "GET", "/sheets"))["sheets"] == []  # the sheets do not need it


# --- the sheets ---------------------------------------------------------------------------------------------------

async def test_sheets_by_course_and_a_sheet_card(shelves, batches, llm):
    await two_sheets(shelves, batches, llm)
    listed = await ask(shelves, "GET", "/sheets")
    keys = ("id", "course", "course_title", "number", "versions", "file")
    assert [tuple(s[k] for k in keys) for s in listed["sheets"]] \
        == [(2, "Calc", "Calculus", "4", 1, True), (1, "Calc", "Calculus", "3", 1, True)]
    assert {"key": "Calc", "title": "Calculus"} in listed["courses"]
    assert [s["id"] for s in (await ask(shelves, "GET", "/sheets?course=Calc"))["sheets"]] == [2, 1]
    assert (await ask(shelves, "GET", "/sheets?course=Alg"))["sheets"] == []

    card = await ask(shelves, "GET", "/sheets/1")
    assert (card["id"], card["name"], card["status"], card["own"]) == (1, "list3.pdf", "posted", False)
    [version] = card["versions_list"]
    assert version["version"] == 1 and version["file_url"].startswith(env().public_url + "/library/")
    served = await shelves.get(local(version["file_url"]))  # the library's copy, by its signed link
    assert served.status == 200 and await served.read() == SHEET3
    assert [c["id"] for c in card["candidates"]] == [2]
    await ask(shelves, "GET", "/sheets/99", status=404)


async def test_a_sheets_pdf_has_three_homes(shelves, batches, llm):
    await two_sheets(shelves, batches, llm)
    [copy] = [row for row in shelves.library.items(course=CALC)
              if row["source"] == "sheet" and row["name"] == "list3.pdf"]
    shelves.library.remove(copy["id"])  # the library's copy is gone: the kept file by a signed link (D69)
    links = shelves.links
    links.kept = shelves.sheets.files.by_hash
    [version] = (await ask(shelves, "GET", "/sheets/1"))["versions_list"]
    assert version["file_url"].startswith(env().public_url + "/library/") and version["file_url"].endswith("list3.pdf")
    served = await shelves.get(local(version["file_url"]))  # no session needed: Safari opens it from the iPhone app
    assert served.status == 200 and await served.read() == SHEET3
    assert served.headers["Content-Type"] == "application/pdf" and "inline" in served.headers["Content-Disposition"]
    links.kept = None  # without the signed route, it answers behind the session
    [version] = (await ask(shelves, "GET", "/sheets/1"))["versions_list"]
    assert version["file_url"] == "/web/api/shelf/sheets/1/file"
    response = await call(shelves, "GET", "/shelf/sheets/1/file", token=shelves.token)
    assert response.status == 200 and await response.read() == SHEET3
    assert "inline" in response.headers["Content-Disposition"]
    assert (await call(shelves, "GET", "/shelf/sheets/1/file")).status == 401  # not without the session
    shelves.sheets.files.by_hash(shelves.sheets.get(1)["sha256"]).unlink()  # 14 days passed
    [version] = (await ask(shelves, "GET", "/sheets/1"))["versions_list"]
    assert version["file_url"] is None
    await ask(shelves, "GET", "/sheets/1/file", status=404)


async def test_editing_a_sheet_puts_it_in_the_lens_and_undoes(shelves, batches, llm):
    await two_sheets(shelves, batches, llm)
    changed = await ask(shelves, "PATCH", "/sheets/1", {"course": "Alg", "number": "3b", "due": "2026-11-02",
                                                        "summary": "Series, 6 tasks."})
    assert changed["change"]["undo"] and await last_via(shelves) == "web"
    assert [s["id"] for s in (await ask(shelves, "GET", "/sheets?course=Alg"))["sheets"]] == [1]  # the lens follows
    assert [s["id"] for s in (await ask(shelves, "GET", "/sheets?course=Calc"))["sheets"]] == [2]
    lens = await (await call(shelves, "GET", "/course?key=Alg", token=shelves.token)).json()
    assert lens["sheets"] == {"count": 1}
    [edited] = [s for s in (await ask(shelves, "GET", "/sheets"))["sheets"] if s["id"] == 1]
    assert (edited["number"], edited["due"], edited["due_assumed"], edited["summary"]) == \
        ("3b", "2026-11-02", False, "Series, 6 tasks.")
    await undo(shelves, changed["change"])
    assert [s["id"] for s in (await ask(shelves, "GET", "/sheets?course=Calc"))["sheets"]] == [2, 1]

    assert (await ask(shelves, "PATCH", "/sheets/1", {"course": "Nope"}, status=400))["error"] == "no such course"
    await ask(shelves, "PATCH", "/sheets/1", {}, status=400)
    await ask(shelves, "PATCH", "/sheets/1", {"oops": 1}, status=400)
    await ask(shelves, "PATCH", "/sheets/1", {"due": "2.11"}, status=400)
    await ask(shelves, "PATCH", "/sheets/99", {"number": "1"}, status=404)
    cleared = await ask(shelves, "PATCH", "/sheets/1", {"course": "", "due": None})
    assert cleared["change"]["undo"]
    [none] = [s for s in (await ask(shelves, "GET", "/sheets"))["sheets"] if s["id"] == 1]
    assert (none["course"], none["due"]) == (None, None)


async def test_joining_a_sheet_to_another_cannot_be_undone(shelves, batches, llm):
    await two_sheets(shelves, batches, llm)
    joined = await ask(shelves, "POST", "/sheets/2/join", {"previous": 1})
    assert joined["change"]["undo"] is False and joined["change"]["note"] and await last_via(shelves) == "web"
    assert [(s["id"], s["first"], s["versions"]) for s in (await ask(shelves, "GET", "/sheets"))["sheets"]] \
        == [(2, 1, 2)]
    card = await ask(shelves, "GET", "/sheets/2")  # the card of any version shows the chain
    assert [v["version"] for v in card["versions_list"]] == [1, 2] and card["candidates"] == []
    await ask(shelves, "POST", "/sheets/2/join", {"previous": "1"}, status=400)
    await ask(shelves, "POST", "/sheets/2/join", {"previous": 1}, status=400)  # it has versions already
    await ask(shelves, "POST", "/sheets/9/join", {"previous": 1}, status=400)


async def test_an_own_sheet_is_added_with_an_undo_and_deleted_without(shelves, batches, llm):
    await found(batches, llm, 1, SHEET3)
    await shelves.topic.publish()
    added = await upload_sheet(shelves, course="Alg", number=" 7 ", due="2026-11-05")
    assert added["change"]["undo"] and await last_via(shelves) == "web"
    [mine] = [s for s in (await ask(shelves, "GET", "/sheets?course=Alg"))["sheets"]]
    assert (mine["id"], mine["own"], mine["number"], mine["due"], mine["status"]) == (added["id"], True, "7",
                                                                                      "2026-11-05", "posted")
    assert shelves.sheets.get(added["id"])["course"] == "Algebra"
    await undo(shelves, added["change"])
    assert (await ask(shelves, "GET", "/sheets?course=Alg"))["sheets"] == []

    again = await upload_sheet(shelves, course="Alg")
    deleted = await ask(shelves, "DELETE", f"/sheets/{again['id']}")
    assert deleted["change"]["undo"] is False and deleted["change"]["note"]
    assert [s["id"] for s in (await ask(shelves, "GET", "/sheets"))["sheets"]] == [1]
    await ask(shelves, "DELETE", "/sheets/1", status=400)  # a chat's sheet is not the owner's to delete

    await upload_sheet(shelves, data=None, status=400, number="1")  # no file
    await upload_sheet(shelves, status=400, course="Nope")
    await upload_sheet(shelves, status=400, due="2.11")
    await upload_sheet(shelves, data=b"not a pdf", status=400)


# --- raw material and the inbox -----------------------------------------------------------------------------------

async def test_sorting_the_inbox_and_the_raw_shelf(shelves):
    library = shelves.library
    note = library.add("text", source="web", text="Series converge", title="Series")
    photo = library.add("photo", source="web", title="Board")
    inbox = await ask(shelves, "GET", "/inbox")
    assert sorted(i["id"] for i in inbox["items"]) == [note, photo]
    assert {"key": "Calc", "title": "Calculus"} in inbox["courses"]

    classes = (await ask(shelves, "GET", "/classes?course=Calc"))["classes"]
    assert classes and {"uid", "start", "kind", "items", "new"} <= set(classes[0])
    uid = classes[0]["uid"]

    first = await ask(shelves, "POST", "/sort", {"ids": [note], "course": "Calc"})
    assert first["change"]["undo"] and await last_via(shelves) == "web"
    second = await ask(shelves, "POST", "/sort", {"ids": [photo], "class_uid": uid})
    assert [i["id"] for i in (await ask(shelves, "GET", "/inbox"))["items"]] == []
    page = await ask(shelves, "GET", "/raw?course=Calc")
    assert (page["course"], page["title"]) == ("Calc", "Calculus")
    assert [i["id"] for i in page["items"]] == [note]
    [placed] = [c for c in page["classes"] if c["items_list"]]
    assert placed["uid"] == uid and [i["id"] for i in placed["items_list"]] == [photo]
    assert placed["items_list"][0]["course"] == "Calc" and (placed["items"], placed["new"]) == (1, 1)

    await undo(shelves, second["change"])
    await undo(shelves, first["change"])
    assert sorted(i["id"] for i in (await ask(shelves, "GET", "/inbox"))["items"]) == [note, photo]
    back = await ask(shelves, "POST", "/sort", {"ids": [note], "course": "Calc"})
    assert back["change"]["undo"]
    assert (await ask(shelves, "POST", "/sort", {"ids": [note], "course": None}))["change"]["undo"]  # to the inbox
    assert note in [i["id"] for i in (await ask(shelves, "GET", "/inbox"))["items"]]

    await ask(shelves, "POST", "/sort", {"ids": [note], "course": "Nope"}, status=400)
    await ask(shelves, "POST", "/sort", {"ids": [note], "class_uid": "nope"}, status=400)
    await ask(shelves, "POST", "/sort", {"ids": [999], "course": "Calc"}, status=400)
    await ask(shelves, "POST", "/sort", {"ids": [], "course": "Calc"}, status=400)
    await ask(shelves, "POST", "/sort", {"ids": [note]}, status=400)
    await ask(shelves, "GET", "/raw?course=Nope", status=400)
    await ask(shelves, "GET", "/raw", status=400)


async def test_an_item_card_its_changes_and_its_file(shelves):
    library = shelves.library
    new = await library.add_stream(chunks(PDF), name="slides.pdf", media_type="application/pdf", source="web",
                                   author="owner", course=CALC)
    text = library.add("text", source="web", text="Long " * 400, title="Notes", course=CALC)
    card = await ask(shelves, "GET", f"/items/{new}")
    assert (card["kind"], card["course"], card["course_title"], card["status"]) == ("file", "Calc", "Calculus", "new")
    assert card["file_url"].startswith(env().public_url + "/library/") and card["children"] == []
    served = await shelves.get(local(card["file_url"]))
    assert await served.read() == PDF
    whole = await ask(shelves, "GET", f"/items/{text}")
    assert len(whole["text"]) == 2000 and whole["text_cut"] is False and whole["file_url"] is None
    cut = [i for i in (await ask(shelves, "GET", "/raw?course=Calc"))["items"] if i["id"] == text][0]
    assert len(cut["text"]) == 600 and cut["text_cut"] is True  # the list shows a preview
    await ask(shelves, "GET", "/items/999", status=404)

    renamed = await ask(shelves, "PATCH", f"/items/{new}", {"title": "Slides 1", "note": "week 1"})
    assert renamed["change"]["undo"] and await last_via(shelves) == "web"
    assert (await ask(shelves, "GET", f"/items/{new}"))["title"] == "Slides 1"
    await undo(shelves, renamed["change"])
    assert (await ask(shelves, "GET", f"/items/{new}"))["title"] != "Slides 1"
    await ask(shelves, "PATCH", f"/items/{new}", {"status": "processed"})
    assert (await ask(shelves, "GET", f"/items/{new}"))["status"] == "processed"
    await ask(shelves, "PATCH", f"/items/{new}", {"kind": "photo"}, status=400)
    await ask(shelves, "PATCH", f"/items/{new}", {}, status=400)

    removed = await ask(shelves, "DELETE", f"/items/{new}")
    assert removed["change"]["undo"]
    await ask(shelves, "GET", f"/items/{new}", status=404)
    await undo(shelves, removed["change"])
    assert (await ask(shelves, "GET", f"/items/{new}"))["id"] == new

    # The inbox's «Delete» of several: one change, and its undo brings them all back.
    other = (await ask(shelves, "GET", "/inbox"))["items"]
    ids = [new] + [i["id"] for i in other if i["id"] != new][:1]
    many = await ask(shelves, "POST", "/remove", {"ids": ids})
    assert many["change"]["undo"]
    for item_id in ids:
        await ask(shelves, "GET", f"/items/{item_id}", status=404)
    await undo(shelves, many["change"])
    for item_id in ids:
        assert (await ask(shelves, "GET", f"/items/{item_id}"))["id"] == item_id
    await ask(shelves, "POST", "/remove", {"ids": []}, status=400)


async def test_everything_is_behind_the_session_and_the_origin(shelves):
    for method, path in (("GET", "/shelf"), ("GET", "/shelf/sheets"), ("GET", "/shelf/inbox"),
                         ("POST", "/shelf/sort"), ("POST", "/shelf/remove")):
        assert (await call(shelves, method, path, {} if method == "POST" else None)).status == 401
    wrong = await call(shelves, "POST", "/shelf/sort", {"ids": [1], "course": "Calc"}, token=shelves.token,
                       origin="https://evil.example")
    assert wrong.status == 403


async def test_the_lens_links_every_version_of_a_document(shelves):
    async def chunks(data):
        yield data

    for number in (1, 2):
        draft = shelves.library.draft(CALC, "book", note=f"version {number}")
        await shelves.library.put_text(draft["id"], "source", f"v{number}")
        await shelves.library.put(draft["id"], "pdf", chunks(f"%PDF-{number}".encode()))
        shelves.library.publish(draft["id"])
    response = await call(shelves, "GET", "/course?key=Calc", token=shelves.token)
    [book] = (await response.json())["documents"]
    assert [v["version"] for v in book["versions"]] == [2, 1] and book["url"] == book["versions"][0]["url"]
    assert [v["note"] for v in book["versions"]] == ["version 2", "version 1"]
    for version, body in zip(book["versions"], (b"%PDF-2", b"%PDF-1"), strict=True):
        served = await shelves.get(local(version["url"]))  # each version opens by a link of its own (D69)
        assert served.status == 200 and await served.read() == body
    machine = await (await call(shelves, "GET", "/machine", token=shelves.token)).json()
    assert machine["library"]["documents"] == 1 and machine["library"]["bytes"] > 0  # its disk, in «Machine»
