"""The library (D58): raw material, the knowledge base, and the links its files move by."""

import datetime as dt

import pytest
from aiohttp import web

from infomaster import clock
from infomaster.library import Library, LibraryError, class_kind, guess_kind, safe_name
from infomaster.library import links as library_links
from infomaster.store import Store

NOW = dt.datetime(2026, 11, 20, 12, 0, tzinfo=clock.TZ)


def add_class(store: Store, uid: str, subject: str, start: str, kind: str = "Лекция", end: str | None = None):
    with store.db:
        store.db.execute("insert into classes(uid, subject, start, end, kind, room, seen_at) "
                         "values(?, ?, ?, ?, ?, '', ?)",
                         (uid, subject, start, end or start[:11] + "23:00:00", kind, start))


@pytest.fixture
def library(tmp_path):
    store = Store(tmp_path / "x.db")
    add_class(store, "calc-1", "Calculus", "2026-11-18T09:30:00", "Лекция")
    add_class(store, "calc-2", "Calculus", "2026-11-19T11:10:00", "Семинар")
    add_class(store, "calc-3", "Calculus", "2026-11-25T09:30:00", "Лекция")  # not yet
    add_class(store, "alg-1", "Algebra", "2026-11-20T11:10:00", "Lecture")
    lib = Library(store, tmp_path, now=lambda: NOW)
    lib.same_course = lambda a, b: bool(a and b and (a.casefold() == b.casefold() or {a, b} == {"Матан", "Calculus"}))
    return lib


async def stream(*parts: bytes):
    for part in parts:
        yield part


def test_helpers():
    assert guess_kind("image/jpeg") == "photo"
    assert guess_kind("audio/mp4") == "audio"
    assert guess_kind(None, "Lecture 3.PPTX") == "slides"
    assert guess_kind("application/pdf") == "file"
    assert safe_name("../a/b.pdf", "x") == "_a_b.pdf"
    assert safe_name("  ", "x") == "x"
    assert class_kind("Лекция") == "lecture" and class_kind("Практическое занятие") == "seminar"
    assert class_kind("Рубежный контроль") == ""


async def test_items_and_classes(library):
    # A photo of the board sent during the seminar belongs to it; the course's other name is the timetable's.
    photo = await library.add_stream(stream(b"jpeg", b"bytes"), name="board.jpg", media_type="image/jpeg",
                                     course="Матан", source="chat", chat_id=-5, msg_id=7,
                                     at=dt.datetime(2026, 11, 19, 12, 0, tzinfo=clock.TZ))
    row = library.get(photo)
    assert (row["course"], row["kind"], row["size"]) == ("Calculus", "photo", 9)
    assert row["class_uid"] is None  # no class was asked for: `find_class` is the caller's
    found = library.find_class("Матан", dt.datetime(2026, 11, 19, 12, 0, tzinfo=clock.TZ))
    assert found["uid"] == "calc-2" and found["kind"] == "seminar"
    assert library.find_class("Calculus", dt.datetime(2026, 11, 18, 9, 15, tzinfo=clock.TZ))["uid"] == "calc-1"
    assert library.find_class("Calculus", dt.datetime(2026, 11, 10, tzinfo=clock.TZ)) is None
    assert library.find_class("History", NOW) is None
    assert library.path(row).read_bytes() == b"jpegbytes"
    assert library.from_origin("chat", -5, 7)["id"] == photo

    note = library.add("text", source="miniapp", text="Limits, ε-δ", class_uid="calc-1")
    assert library.get(note)["course"] == "Calculus" and library.get(note)["day"] == "2026-11-18"
    with pytest.raises(LibraryError):
        library.add("text", source="miniapp", text="x", course="Algebra", class_uid="calc-1")
    inbox = library.add("link", source="bot", url="https://example.org/rec")
    assert library.inbox_count() == 1 and library.items(inbox=True)[0]["id"] == inbox

    classes = library.classes("Calculus")
    assert [c["uid"] for c in classes] == ["calc-2", "calc-1"]  # started ones, the last first
    assert classes[1]["items"] == 1 and classes[1]["kind"] == "lecture"

    old = library.update(inbox, course="Algebra", class_uid="alg-1")
    assert old == {"course": None, "class_uid": None}
    assert library.inbox_count() == 0 and library.get(inbox)["day"] == "2026-11-20"
    library.update(inbox, **old)
    assert library.inbox_count() == 1

    courses = {c["course"]: c for c in library.courses()}
    assert courses["Calculus"]["items"] == 2 and courses["Algebra"]["items"] == 0

    # The same bytes again are kept once on disk.
    again = await library.add_stream(stream(b"jpegbytes"), name="copy.JPG", media_type="image/jpeg", source="bot")
    assert library.get(again)["path"] == row["path"]


async def test_remove_restore_and_prune(library, tmp_path):
    audio = await library.add_stream(stream(b"m4a"), name="rec.m4a", media_type="audio/mp4", source="miniapp",
                                     class_uid="calc-1")
    transcript = library.add("transcript", source="connector", text="today: limits", parent=audio)
    assert [c["id"] for c in library.children(audio)] == [transcript]
    library.remove(audio)
    assert library.get(audio) is None and library.get(transcript) is None
    library.restore(audio)
    assert library.get(transcript) is not None
    library.remove(audio)
    library.now = lambda: NOW + dt.timedelta(days=31)
    assert library.prune() == 2
    assert not list((tmp_path / "library" / "raw").rglob("*.m4a"))
    with pytest.raises(LibraryError):
        library.restore(audio)


async def test_drafts(library):
    draft = library.add("audio", source="connector", class_uid="calc-1", status="uploading",
                        file={"name": "rec.m4a", "media_type": "audio/mp4"})
    assert library.get(draft) is None and library.items() == []
    row = await library.fill(draft, stream(b"abc"))
    assert row["status"] == "new" and row["size"] == 3
    with pytest.raises(LibraryError):
        await library.fill(draft, stream(b"x"))
    stale = library.add("file", source="connector", status="uploading")
    library.now = lambda: NOW + dt.timedelta(days=2)
    library.prune()
    assert library.get(stale, drafts=True) is None and library.get(draft) is not None


async def test_knowledge(library):
    photo = library.add("text", source="miniapp", text="board", class_uid="calc-1")
    with pytest.raises(LibraryError):
        library.draft("Calculus", "lecture")  # a lecture needs its class
    with pytest.raises(LibraryError):
        library.draft("Calculus", "book", class_uid="calc-1")
    with pytest.raises(LibraryError):
        library.draft("Algebra", "lecture", class_uid="calc-1")
    draft = library.draft("Матан", "lecture", class_uid="calc-1", note="first", materials=[photo])
    assert draft["version"] is None and draft["course"] == "Calculus"
    with pytest.raises(LibraryError):
        library.publish(draft["id"])
    await library.put_text(draft["id"], "source", r"\section{Limits}")
    with pytest.raises(LibraryError):
        library.publish(draft["id"])  # no PDF yet
    with pytest.raises(LibraryError):
        await library.put_text(draft["id"], "pdf", "not a pdf")
    await library.put(draft["id"], "pdf", stream(b"%PDF-1.7 ..."))
    first = library.publish(draft["id"])
    assert first["version"] == 1 and library.get(photo)["status"] == "processed"
    assert library.read_source(first) == r"\section{Limits}"

    second = library.draft("Calculus", "lecture", class_uid="calc-1", title="Limits")
    await library.put_text(second["id"], "source", "v2")
    await library.put(second["id"], "pdf", stream(b"%PDF-2"))
    library.publish(second["id"])
    doc = library.find_document("Calculus", "lecture", "calc-1")
    assert [v["version"] for v in doc["versions"]] == [2, 1] and doc["title"] == "Limits"
    with pytest.raises(LibraryError):
        library.unpublish(first["id"])  # a newer one came after it
    library.unpublish(second["id"])
    assert [v["version"] for v in library.find_document("Calculus", "lecture", "calc-1")["versions"]] == [1]

    cheat = library.draft("Calculus", "cheatsheet")
    await library.put_text(cheat["id"], "source", "# Notation\n- ε for epsilon")
    assert library.publish(cheat["id"])["version"] == 1  # no PDF needed
    assert library.part_path(library.version(cheat["id"]), "source").suffix == ".md"
    assert [d["kind"] for d in library.documents("Calculus")] == ["cheatsheet", "lecture"]
    assert library.usage()["documents"] == 2


@pytest.fixture
async def http(library, aiohttp_client):
    app = web.Application()
    links = library_links.Links(library.store, library, "https://example.test")
    library_links.add_routes(app, links)
    return await aiohttp_client(app), links


async def test_links(library, http):
    client, links = http
    item = await library.add_stream(stream(b"%PDF-slides"), name="Слайды 1.pdf", media_type="application/pdf",
                                    source="bot")
    url = links.material(library.get(item))
    path = url.removeprefix("https://example.test")
    response = await client.get(path)
    assert response.status == 200 and await response.read() == b"%PDF-slides"
    assert "inline" in response.headers["Content-Disposition"]
    assert (await client.get(path.replace("/library/", "/library/x"))).status == 404
    assert (await client.put(path.rsplit("/", 1)[0], data=b"x")).status == 404  # a download link uploads nothing

    links.seconds = lambda: 10**10  # later than every link
    assert (await client.get(path)).status == 404
    links.seconds = __import__("time").time

    draft = library.add("audio", source="connector", status="uploading", file={"name": "rec.m4a"})
    upload = links.material_upload(draft).removeprefix("https://example.test")
    response = await client.put(upload, data=b"audio-bytes")
    assert response.status == 200 and (await response.json())["size"] == 11
    assert (await client.put(upload, data=b"again")).status == 400

    version = library.draft("Calculus", "book")
    for part, data in (("source", b"\\documentclass{book}"), ("pdf", b"%PDF-book")):
        response = await client.put(links.version_upload(version["id"], part).removeprefix("https://example.test"),
                                    data=data)
        assert response.status == 200
    published = library.publish(version["id"])
    response = await client.get(links.version(published, "pdf").removeprefix("https://example.test"))
    assert await response.read() == b"%PDF-book"
    assert "Calculus v1.pdf" in response.headers["Content-Disposition"]


async def test_a_renamed_course_is_one_course(library):
    """RUZ's feed went from Russian to English: the old classes keep the old name, and both are one course."""
    add_class(library.store, "calc-0", "Математический анализ", "2026-10-07T09:30:00", "Семинар")
    subjects = {"Математический анализ": "Calculus", "Calculus": "Calculus", "Матан": "Calculus"}
    library.same_course = lambda a, b: bool(a and b and subjects.get(a, a) == subjects.get(b, b))
    assert library.course("Математический анализ") == "Calculus" and library.course("Матан") == "Calculus"
    assert sorted(library.names("Матан")) == ["Calculus", "Математический анализ"]
    item = library.add("text", source="miniapp", text="old notes", course="Математический анализ", class_uid="calc-0")
    assert library.get(item)["course"] == "Calculus"
    assert "calc-0" in [c["uid"] for c in library.classes("Calculus")]
    assert [c["course"] for c in library.courses()].count("Calculus") == 1
    assert "Математический анализ" not in [c["course"] for c in library.courses()]
    assert library.find_class("Матан", dt.datetime(2026, 10, 7, 10, 0, tzinfo=clock.TZ))["uid"] == "calc-0"
    draft = library.draft("Математический анализ", "seminar", class_uid="calc-0")
    assert draft["course"] == "Calculus"


def test_a_sheet_finds_its_seminar(library):
    """A sheet sent the night before its seminar belongs to that seminar, not to the lecture before it."""
    add_class(library.store, "calc-lec", "Calculus", "2026-11-17T14:40:00", "Лекция")
    night = dt.datetime(2026, 11, 19, 0, 25, tzinfo=clock.TZ)
    assert library.find_class("Calculus", night, seminar=True)["uid"] == "calc-2"
    assert library.find_class("Calculus", night)["uid"] == "calc-1"  # the lecture before it
    assert library.find_class("Algebra", night, seminar=True) is None  # no seminar, no class started yet
