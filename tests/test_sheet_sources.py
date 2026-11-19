"""Sheets that come as no PDF of a chat (D59): behind a link, as a letter's attachment; and the downloads that fail."""

import datetime as dt
from email.message import EmailMessage
from email.utils import format_datetime

import pytest
from aiohttp import web
from test_sheets import CHAT, SHEET3, SHEET3_FIXED, batches, files, llm, parse, send, sheet_item, sheets  # noqa: F401

from infomaster.collectors import links
from infomaster.collectors.files import MAX_TRIES, pdf_file
from infomaster.collectors.inbox import MAILBOX
from infomaster.collectors.links import LinkError, direct, document_links
from infomaster.collectors.mail import Mail
from infomaster.collectors.mail import parse as parse_letter
from infomaster.collectors.telegram import message_files
from infomaster.processing.batches import Changes
from infomaster.processing.sheets import origin
from infomaster.settings import Settings

DROPBOX = "https://www.dropbox.com/scl/fi/abc123/Sheet-3.pdf?rlkey=k9&dl=0"


def test_document_links():
    text = (f"Слайды и ДЗ: {DROPBOX}. Задание в Classroom: https://classroom.google.com/c/XYZ/a/1 и "
            "https://accounts.google.com/AccountChooser?continue=x, папка https://disk.yandex.ru/d/AbCdEf, "
            "https://drive.google.com/file/d/1AbCdEfGhIjK/view?usp=sharing, (https://example.org/hw/list3.pdf), "
            "https://example.org/page")
    assert document_links(text) == [DROPBOX, "https://disk.yandex.ru/d/AbCdEf",
                                     "https://drive.google.com/file/d/1AbCdEfGhIjK/view?usp=sharing"]  # three at most
    assert document_links("https://example.org/hw/list3.pdf») и всё") == ["https://example.org/hw/list3.pdf"]
    assert document_links("https://edu.hse.ru/mod/forum/discuss.php?d=1") == []
    assert direct(DROPBOX) == "https://www.dropbox.com/scl/fi/abc123/Sheet-3.pdf?rlkey=k9&dl=1"
    assert direct("https://drive.google.com/file/d/1AbCdEfGhIjK/view") == \
        "https://drive.google.com/uc?export=download&id=1AbCdEfGhIjK"
    assert direct("https://docs.google.com/document/d/1AbCdEfGhIjK/edit") == \
        "https://docs.google.com/document/d/1AbCdEfGhIjK/export?format=pdf"


async def test_save_takes_only_a_pdf(aiohttp_server, tmp_path):
    async def serve(request):
        name = request.match_info["name"]
        if name == "missing.pdf":
            raise web.HTTPNotFound()
        if name == "broken.pdf":
            raise web.HTTPServiceUnavailable()
        body = SHEET3 if name != "page.pdf" else b"<html>sign in</html>"
        return web.Response(body=body, headers={"Content-Disposition": "attachment; filename*=UTF-8''%D0%9B%D0%B8"
                                                                       "%D1%81%D1%82%D0%BE%D0%BA%203.pdf"})
    app = web.Application()
    app.router.add_get("/{name}", serve)
    server = await aiohttp_server(app)
    path = tmp_path / "x.part"
    async with links.aiohttp.ClientSession() as session:
        assert await links._save(session, str(server.make_url("/sheet.pdf")), path, 10**6) == "Листок 3.pdf"
        assert path.read_bytes() == SHEET3
        assert await links._save(session, str(server.make_url("/page.pdf")), path, 10**6) is None
        assert await links._save(session, str(server.make_url("/sheet.pdf")), path, 100) is None  # too big
        assert await links._save(session, str(server.make_url("/missing.pdf")), path, 10**6) is None
        with pytest.raises(LinkError):
            await links._save(session, str(server.make_url("/broken.pdf")), path, 10**6)


def link_message(batches, msg_id, text):  # noqa: F811
    batches.inbox.add(CHAT, msg_id, {"sender_id": 7, "sender_name": "Ассистент",
                                     "at": batches.now() - dt.timedelta(minutes=30), "text": text, "files": []})


@pytest.fixture
def remote_links(files):  # noqa: F811
    files.links, files.asked = {}, []

    async def fetch_url(url, path, limit):
        files.asked.append(url)
        found = files.links.get(url)
        if isinstance(found, Exception):
            raise found
        if found is None:
            return None
        path.write_bytes(found)
        return "Sheet-3.pdf"

    files.fetch_url = fetch_url
    return files.links


async def test_a_sheet_behind_a_link(batches, llm, sheets, files, remote_links):  # noqa: F811
    remote_links[DROPBOX] = SHEET3
    link_message(batches, 1, f"Новый листок: {DROPBOX}")
    llm.answers = [{"items": [sheet_item(1)]}]
    changes = await parse(batches)
    assert f'  PDF "Sheet-3.pdf", 2 pages, downloaded from the link {DROPBOX}, first page:' in llm.calls[0].prompt
    [sheet] = sheets.waiting()
    assert changes.sheets == [sheet["id"]] and sheet["name"] == "Sheet-3.pdf"
    row = await files.fetch_link(batches.inbox.message(CHAT, 1))
    assert row["url"] == DROPBOX and files.asked == [DROPBOX]  # kept: not downloaded again


async def test_a_link_is_tried_again_then_given_up(batches, files, remote_links):  # noqa: F811
    remote_links[DROPBOX] = LinkError("HTTP 503")
    link_message(batches, 1, DROPBOX)
    message = batches.inbox.message(CHAT, 1)
    for tries in range(1, MAX_TRIES + 1):
        row = await files.fetch_link(message)
        assert row["tries"] == tries and row["sha256"] is None and "HTTP 503" in row["error"]
    await files.fetch_link(message)
    assert len(files.asked) == MAX_TRIES  # given up

    link_message(batches, 2, "https://disk.yandex.ru/d/folder")  # a folder: no PDF, not asked again
    row = await files.fetch_link(batches.inbox.message(CHAT, 2))
    assert row["error"] == "no PDF behind the links" and row["tries"] == MAX_TRIES
    link_message(batches, 3, "просто текст")
    assert await files.fetch_link(batches.inbox.message(CHAT, 3)) is None


async def test_links_only_in_study_chats(batches, llm, sheets, files, remote_links):  # noqa: F811
    sheets.chat_settings.update(CHAT, {"study": False})
    remote_links[DROPBOX] = SHEET3
    link_message(batches, 1, f"смотрите {DROPBOX}")
    llm.answers = [{"items": []}]
    await parse(batches)
    assert files.asked == [] and "downloaded from the link" not in llm.calls[0].prompt


async def test_a_failed_download_is_tried_again(batches, files):  # noqa: F811
    send(batches, 1, SHEET3)
    message = batches.inbox.message(CHAT, 1)
    download = files.download

    async def refused(chat_id, msg_id, path):
        files.downloads.append(msg_id)
        raise RuntimeError("CHANNEL_PRIVATE")
    files.download = refused
    assert (await files.fetch(message))["tries"] == 1
    files.download = download  # the next run: Telegram gives it
    row = await files.fetch(message)
    assert row["sha256"] and row["error"] is None and files.downloads == [1, 1]


def test_a_pdf_sent_as_a_document():
    assert pdf_file({"files": [{"kind": "document", "name": "HW 3.PDF", "mime": "application/octet-stream"}]})
    assert pdf_file({"files": [{"kind": "document", "name": "x", "mime": "application/x-pdf"}]})
    assert pdf_file({"files": [{"kind": "document", "name": "a.docx", "mime": "application/msword"}]}) is None

    class File:
        name, mime_type, size = "Листок 4.pdf", "application/octet-stream", 10

    class Message:
        media = photo = sticker = voice = video_note = gif = video = audio = None
        file = File()
    message = Message()
    from telethon import types
    message.media = types.MessageMediaDocument(document=types.Document(
        id=1, access_hash=1, file_reference=b"", date=None, mime_type="application/octet-stream", size=10, dc_id=1,
        attributes=[]))
    assert message_files(message)[0]["kind"] == "pdf"


async def test_an_old_pdf_of_another_chat_is_read(batches, llm, sheets):  # noqa: F811
    sheets.chat_settings.update(CHAT, {"study": False})
    send(batches, 1, SHEET3, ago=dt.timedelta(days=3))  # after a pause of the twin
    send(batches, 2, None, text="старые новости", ago=dt.timedelta(days=3))
    llm.answers = [{"items": [sheet_item(1)]}]
    await parse(batches)
    assert '"list3.pdf"' in llm.calls[0].prompt and "старые новости" not in llm.calls[0].prompt
    assert len(sheets.waiting()) == 1


def letter(at: dt.datetime, pdf: bytes | None) -> bytes:
    msg = EmailMessage()
    msg["From"] = '"Петров Иван" <petrov@hse.ru>'
    msg["Subject"] = "Листок 3"
    msg["Date"] = format_datetime(at)
    msg.set_content("Добрый день! Новый листок во вложении.")
    if pdf is not None:
        msg.add_attachment(pdf, maintype="application", subtype="octet-stream", filename="list3.pdf")
    return msg.as_bytes()


async def test_a_sheet_in_a_letter(store, now, batches, llm, sheets, files):  # noqa: F811
    assert parse_letter(letter(now(), SHEET3)).pdf == ("list3.pdf", SHEET3)
    mail = Mail(__import__("test_mail").env(), store, Settings(store), batches.inbox, now=now)
    mail.files = files

    def fetcher(*args):
        return 555, 7, [(7, letter(now() - dt.timedelta(hours=1), SHEET3_FIXED))]
    mail.fetcher = fetcher
    assert await mail.poll() == 1
    message = batches.inbox.message(MAILBOX, 7, "mail")
    row = await files.fetch(message)
    assert row["sha256"] and row["first_page"].startswith("Calculus. Sheet 3") and files.downloads == []

    llm.answers = [{"items": [sheet_item(7)]}]
    changes = Changes()
    await batches.parse_chat(MAILBOX, changes)
    assert '  PDF "list3.pdf", 2 pages, first page:' in llm.calls[0].prompt
    [sheet] = sheets.waiting()
    assert origin(sheet) == "mail" and sheet["chat_id"] == MAILBOX and changes.sheets == [sheet["id"]]
    assert sheets.listing()[0]["chat"] == "✉ Листок 3"
