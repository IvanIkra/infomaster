"""The library's tools of the MCP connector (§8.5, D58): raw material, the inbox, the knowledge base, files by links."""

import base64

import actions_app
import pytest
from test_connector import Seconds, call, connect, data, rpc, undo_last
from test_http import TWIN, env

from infomaster.collectors.hseapp import CALLBACK_PATH, HseApp
from infomaster.connector import Connector
from infomaster.library import Library
from infomaster.library import links as library_links
from infomaster.processing.news import News
from infomaster.settings import ChatSettings, Settings

ORIGIN = "https://infomaster.example.ts.net"
PNG = b"\x89PNG\r\n\x1a\n" + b"\x00" * 32


def add_class(store, uid: str, subject: str, start: str, kind: str):
    with store.db:
        store.db.execute("insert into classes(uid, subject, start, end, kind, room, seen_at) "
                         "values(?, ?, ?, ?, ?, '', ?)", (uid, subject, start, start[:11] + "23:00:00", kind, start))


@pytest.fixture
def library(store, tmp_path, now):
    add_class(store, "calc-1", "Calculus", "2026-10-19T09:30:00", "Лекция")
    add_class(store, "calc-2", "Calculus", "2026-10-20T11:10:00", "Семинар")
    add_class(store, "calc-3", "Calculus", "2026-10-27T09:30:00", "Лекция")  # not yet
    add_class(store, "alg-1", "Algebra", "2026-10-16T13:00:00", "Lecture")
    return Library(store, tmp_path, now=now)


def app_of(store, budget, journal, schedule, merged):
    hse = HseApp(store, Settings(store), env().public_url + CALLBACK_PATH)
    return actions_app.app(env(), budget, journal, schedule.inbox, ChatSettings(store), TWIN, schedule.subjects,
                              schedule, hse, merged, settings=budget.settings)


@pytest.fixture
def sent():
    return []


@pytest.fixture
def connector(store, budget, journal, schedule, merged, now, sent, library):
    app = app_of(store, budget, journal, schedule, merged)
    library_links.add_routes(app, library_links.Links(store, library, env().public_url))

    async def send_code(text):
        sent.append(text)

    connector = Connector(env(), store, News(store), send_code=send_code, now=now, seconds=Seconds())
    connector.attach(app)
    return connector


@pytest.fixture
async def client(aiohttp_client, connector):
    return await aiohttp_client(connector.app)


@pytest.fixture
async def token(client, sent):
    return (await connect(client, sent))["access_token"]


def path(url: str) -> str:
    assert url.startswith(ORIGIN)
    return url.removeprefix(ORIGIN)


def refused(result) -> str:
    assert result["isError"], result
    return result["content"][0]["text"]


def test_only_with_the_library(store, budget, journal, schedule, merged, now):
    plain = Connector(env(), store, News(store), now=now)
    plain.attach(app_of(store, budget, journal, schedule, merged))
    assert "list_library" not in plain.registry.tools


async def test_listing(client, token, connector):
    listed = (await (await rpc(client, token, "tools/list")).json())["result"]["tools"]
    tools = {tool["name"]: tool for tool in listed}
    names = {"list_library", "list_materials", "get_material", "list_classes", "list_documents", "get_document",
             "get_cheatsheet", "add_material", "sort_material", "update_material", "remove_material",
             "start_document_version", "publish_document_version"}
    assert names <= set(tools)
    assert tools["get_material"]["annotations"]["readOnlyHint"] is True
    assert tools["add_material"]["annotations"]["destructiveHint"] is False
    assert tools["sort_material"]["annotations"]["destructiveHint"] is True
    assert tools["remove_material"]["annotations"]["destructiveHint"] is True
    assert tools["publish_document_version"]["annotations"]["destructiveHint"] is False
    assert "NOT solved" in tools["start_document_version"]["description"]
    for name in names:
        assert tools[name]["inputSchema"]["additionalProperties"] is False
    state = connector.state()["tools"]
    assert state["read"] + state["write"] == len(listed)


async def test_flow(client, token, connector, library):
    """A recording into the inbox by its link, sorted, transcribed; a lecture built from it, published, undone."""
    # A file: a draft with an upload link; not listed until its bytes are in.
    draft = data(await call(client, token, "add_material", {"file_name": "lecture 3.m4a", "author": "owner"}))
    assert draft["status"] == "uploading" and draft["kind"] == "audio" and draft["course"] is None
    assert draft["curl"] == f"curl --fail-with-body -T 'lecture 3.m4a' {draft['upload_url']}"
    assert "waiting for its upload" in draft["change"]["summary"]
    assert data(await call(client, token, "list_materials", {"inbox": True}))["items"] == []
    response = await client.put(path(draft["upload_url"]), data=b"m4a-bytes")
    assert response.status == 200 and (await response.json())["size"] == 9
    [audio] = data(await call(client, token, "list_materials", {"inbox": True}))["items"]
    assert (audio["id"], audio["kind"], audio["file"]["media_type"], audio["transcripts"]) == \
        (draft["id"], "audio", "audio/mp4", [])
    assert data(await call(client, token, "list_library"))["inbox"] == 1

    # Sorted into its class; the inbox is empty, the class counts it.
    sorted_ = data(await call(client, token, "sort_material", {"ids": [audio["id"]], "class_uid": "calc-1"}))
    assert (sorted_["course"], sorted_["day"]) == ("Calculus", "2026-10-19")
    assert sorted_["change"]["summary"] == "Sorted 1 item(s) into Calculus, lecture 2026-10-19"
    assert data(await call(client, token, "list_materials", {"inbox": True}))["items"] == []
    classes = data(await call(client, token, "list_classes", {"course": "calculus"}))
    assert [c["uid"] for c in classes["classes"]] == ["calc-2", "calc-1"]
    assert (classes["classes"][1]["items"], classes["classes"][1]["kind"]) == (1, "lecture")

    # Downloaded, transcribed locally, the transcript hangs on the recording.
    got = data(await call(client, token, "get_material", {"id": audio["id"]}))
    assert got["class"]["kind"] == "lecture" and "text" not in got and "image" not in got
    assert await (await client.get(path(got["download_url"]))).read() == b"m4a-bytes"
    transcript = data(await call(client, token, "add_material", {
        "text": "Today: limits. " * 4000, "kind": "transcript", "parent": audio["id"], "class_uid": "calc-1"}))
    got = data(await call(client, token, "get_material", {"id": audio["id"]}))
    assert [c["id"] for c in got["children"]] == [transcript["id"]] and got["transcripts"] == [transcript["id"]]
    long = data(await call(client, token, "get_material", {"id": transcript["id"], "max_chars": 1000}))
    assert len(long["text"]["text"]) == 1000 and long["text"]["next_offset"] == 1000
    assert long["text"]["total_chars"] == 60_000 and long["preview"].startswith("Today: limits.")
    rest = data(await call(client, token, "get_material", {"id": transcript["id"], "offset": 59_500}))
    assert len(rest["text"]["text"]) == 500 and rest["text"]["next_offset"] is None

    # A photo of the board comes inline.
    photo = data(await call(client, token, "add_material", {"file_name": "board.png", "class_uid": "calc-1"}))
    assert (await client.put(path(photo["upload_url"]), data=PNG)).status == 200
    result = await call(client, token, "get_material", {"id": photo["id"]})
    assert data(result)["image"] == "attached"
    assert result["content"][1]["mimeType"] == "image/png"
    assert base64.b64decode(result["content"][1]["data"]) == PNG
    assert len((await call(client, token, "get_material", {"id": photo["id"], "image": False}))["content"]) == 1

    # The lecture: the source inline, the PDF by its link, then published. Starting a draft is not journaled.
    journaled = len(connector.changes.recent())
    started = data(await call(client, token, "start_document_version", {
        "course": "Calculus", "kind": "lecture", "class_uid": "calc-1", "title": "Limits",
        "materials": [audio["id"], transcript["id"], photo["id"]], "source_text": r"\section{Limits}",
        "pdf_file": "build/limits.pdf"}))
    assert started["source_uploaded"] and set(started["missing"]) == {"pdf"} and "change" not in started
    assert started["missing"]["pdf"]["curl"].startswith("curl --fail-with-body -T build/limits.pdf ")
    assert len(connector.changes.recent()) == journaled
    assert "PDF is not uploaded" in refused(await call(client, token, "publish_document_version",
                                                       {"draft_id": started["draft_id"]}))
    assert (await client.put(path(started["missing"]["pdf"]["upload_url"]), data=b"%PDF-1.7 limits")).status == 200
    published = data(await call(client, token, "publish_document_version", {"draft_id": started["draft_id"]}))
    assert published["version"] == 1 and sorted(published["processed"]) == sorted(
        [audio["id"], transcript["id"], photo["id"]])
    assert published["change"]["summary"] == ("Published Calculus lecture 2026-10-19 v1: «Limits»; 3 item(s) marked "
                                              "processed")
    assert library.get(audio["id"])["status"] == "processed"
    assert await (await client.get(path(published["pdf_url"]))).read() == b"%PDF-1.7 limits"

    [doc] = data(await call(client, token, "list_documents", {"course": "Calculus"}))["documents"]
    assert (doc["kind"], doc["class_uid"], doc["version"]) == ("lecture", "calc-1", 1)
    classes = data(await call(client, token, "list_classes", {"course": "Calculus"}))
    assert classes["classes"][1]["documents"][0]["id"] == doc["id"] and classes["classes"][1]["new"] == 0
    got = data(await call(client, token, "get_document", {"course": "Calculus", "kind": "lecture",
                                                          "class_uid": "calc-1"}))
    assert got["source"]["text"] == r"\section{Limits}" and got["chosen"]["pdf_url"]
    assert got["chosen"]["materials"] == [audio["id"], transcript["id"], photo["id"]]
    assert "no version 2" in refused(await call(client, token, "get_document", {"doc_id": doc["id"], "version": 2}))

    # The owner undoes the publish: the version is gone, the items are new again.
    await undo_last(connector)
    assert library.documents("Calculus") == [] and library.get(audio["id"])["status"] == "new"
    assert data(await call(client, token, "list_documents"))["documents"] == []


async def test_cheatsheet(client, token, connector):
    empty = data(await call(client, token, "get_cheatsheet", {"course": "Algebra"}))
    assert empty["cheatsheet"] is None and "start_document_version" in empty["hint"]
    assert "no PDF" in refused(await call(client, token, "start_document_version", {
        "course": "Algebra", "kind": "cheatsheet", "source_text": "x", "pdf_file": "x.pdf"}))
    started = data(await call(client, token, "start_document_version", {
        "course": "Algebra", "kind": "cheatsheet", "source_text": "# Notation\n- $A^T$: transpose"}))
    assert started["missing"] == {} and started["next"] == "call publish_document_version with draft_id"
    data(await call(client, token, "publish_document_version", {"draft_id": started["draft_id"]}))
    sheet = data(await call(client, token, "get_cheatsheet", {"course": "Algebra"}))
    assert sheet["version"] == 1 and sheet["cheatsheet"]["text"].startswith("# Notation")
    again = await call(client, token, "publish_document_version", {"draft_id": started["draft_id"]})
    assert "published already" in refused(again)

    # A lecture's draft without its source gives both links.
    lecture = data(await call(client, token, "start_document_version", {"course": "Algebra", "kind": "lecture",
                                                                        "class_uid": "alg-1"}))
    assert set(lecture["missing"]) == {"source", "pdf"} and "-T main.tex " in lecture["missing"]["source"]["curl"]


async def test_undo_sort_remove_update(client, token, connector, library):
    a = data(await call(client, token, "add_material", {"text": "Notes on rings", "title": "Rings"}))["id"]
    b = data(await call(client, token, "add_material", {"url": "https://example.org/board",
                                                        "class_uid": "calc-2"}))["id"]
    assert library.get(b)["kind"] == "link" and library.get(b)["course"] == "Calculus"

    # Two items from different places into one course; the undo puts each back where it was.
    data(await call(client, token, "sort_material", {"ids": [a, b], "course": "Algebra"}))
    assert {library.get(i)["course"] for i in (a, b)} == {"Algebra"} and library.get(b)["class_uid"] is None
    await undo_last(connector)
    assert (library.get(a)["course"], library.get(b)["class_uid"]) == (None, "calc-2")

    # Back to the inbox, and undone.
    data(await call(client, token, "sort_material", {"ids": [b], "course": None}))
    assert library.inbox_count() == 2
    await undo_last(connector)
    assert library.get(b)["class_uid"] == "calc-2"

    # A class of another course is refused, and nothing moved.
    text = refused(await call(client, token, "sort_material", {"ids": [a, b], "course": "Algebra",
                                                               "class_uid": "calc-1"}))
    assert "is of Calculus" in text and library.get(a)["course"] is None

    changed = data(await call(client, token, "update_material", {"id": a, "title": "Ideals", "status": "processed"}))
    assert changed["change"]["summary"] == "Changed «Rings»: title → Ideals, status → processed"
    await undo_last(connector)
    assert (library.get(a)["title"], library.get(a)["status"]) == ("Rings", "new")

    # Removed with its transcript; the undo brings both back.
    t = data(await call(client, token, "add_material", {"text": "words", "kind": "transcript", "parent": b}))["id"]
    removed = data(await call(client, token, "remove_material", {"id": b}))
    assert removed["with"] == [t] and library.get(b) is None and library.get(t) is None
    await undo_last(connector)
    assert library.get(b) is not None and library.get(t) is not None

    # Several at once (the recording and its transcript among them): one change, and one undo brings all back.
    many = data(await call(client, token, "remove_material", {"ids": [a, b, t]}))
    assert [r["removed"] for r in many["removed"]] == [a, b] and many["removed"][1]["with"] == [t]
    assert all(library.get(i) is None for i in (a, b, t))
    await undo_last(connector)
    assert all(library.get(i) is not None for i in (a, b, t))

    # An upload that never came: removed, nothing to undo; its link takes nothing then.
    draft = data(await call(client, token, "add_material", {"file_name": "rec.mp3"}))
    gone = data(await call(client, token, "remove_material", {"id": draft["id"]}))
    assert gone["change"]["undo"].startswith("cannot be undone")
    assert (await client.put(path(draft["upload_url"]), data=b"late")).status == 400


async def test_refusals(client, token):
    assert "unknown" in refused(await call(client, token, "add_material", {"text": "x", "colour": "red"}))
    assert "give text" in refused(await call(client, token, "add_material", {"title": "nothing"}))
    assert "not a file with the others" in refused(await call(client, token, "add_material", {
        "file_name": "a.pdf", "text": "x"}))
    assert "one of" in refused(await call(client, token, "add_material", {"text": "x", "kind": "poem"}))
    assert "no class nope" in refused(await call(client, token, "add_material", {"text": "x", "class_uid": "nope"}))
    assert "expected integer" in refused(await call(client, token, "get_material", {"id": "1"}))
    assert "no item 99" in refused(await call(client, token, "get_material", {"id": 99}))
    assert "ids: 1 to" in refused(await call(client, token, "sort_material", {"ids": [], "course": "Algebra"}))
    assert "give course" in refused(await call(client, token, "sort_material", {"ids": [99]}))
    assert "give id" in refused(await call(client, token, "remove_material", {}))
    assert "give id" in refused(await call(client, token, "remove_material", {"id": 1, "ids": [1]}))
    assert "no item 99" in refused(await call(client, token, "remove_material", {"ids": [99]}))
    assert "needs class_uid" in refused(await call(client, token, "start_document_version", {
        "course": "Calculus", "kind": "lecture"}))
    assert "one of" in refused(await call(client, token, "start_document_version", {
        "course": "Calculus", "kind": "essay"}))
    assert "required" in refused(await call(client, token, "publish_document_version", {}))
    assert "give doc_id" in refused(await call(client, token, "get_document", {"course": "Calculus"}))
    assert "inbox or a course" in refused(await call(client, token, "list_materials", {"inbox": True,
                                                                                         "course": "Algebra"}))
