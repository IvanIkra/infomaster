"""PDFs behind links (§6.3, D59): a sheet often comes as a link, not as a file.

Teachers post a task as a Dropbox link in a Smart LMS letter, a Google Drive or Yandex Disk link in a chat, or a plain
URL of a PDF. A batch of a study chat or of the mailbox asks for the PDF behind the first such link of a message that
has no file of its own (collectors/files.py), and the model then sees its first page as it sees a sent PDF.

Only public links are followed, each by the address its service gives for a download: Dropbox by its content host
(dl.dropboxusercontent.com: some resolvers block www.dropbox.com), a Drive
file by `uc?export=download`, a Google document or presentation by its PDF export, a Yandex Disk file by the public
API (a folder is not a sheet and is skipped). Pages behind a login (Google Classroom, Smart LMS itself) cannot be
read and are not tried. What comes must start as a PDF does; anything else is no sheet.
"""

import re
from pathlib import Path
from urllib.parse import parse_qsl, quote, unquote, urlencode, urlsplit, urlunsplit

import aiohttp

from ..net import PrivateAddress, public_session

LINK = re.compile(r"https?://[^\s<>\"'«»\[\]]+")
TRAILING = ".,;:!?)»"
TIMEOUT = aiohttp.ClientTimeout(total=120, sock_read=60)
YANDEX_API = "https://cloud-api.yandex.net/v1/disk/public/resources"
DRIVE_FILE = re.compile(r"^/file/d/([\w-]{10,})")
GOOGLE_EXPORT = re.compile(r"^/(document|presentation|spreadsheets)/d/([\w-]{10,})")
EXPORTS = {"document": "/document/d/{id}/export?format=pdf", "presentation": "/presentation/d/{id}/export/pdf",
           "spreadsheets": "/spreadsheets/d/{id}/export?format=pdf"}
LOGIN_HOSTS = ("accounts.google.com", "classroom.google.com", "edu.hse.ru", "lms.hse.ru", "smartedu.hse.ru")
MAX_LINKS = 3                   # links of one message tried
DROPBOX_CONTENT = "dl.dropboxusercontent.com"  # serves a shared file without www.dropbox.com


class LinkError(RuntimeError):
    """The link could not be read now (the network, the service): it is tried again at a later run."""


def document_links(text: str) -> list[str]:
    """Links of a text that may lead to a PDF, in order, each once: links of services that need a login are left
    out."""
    found = []
    for match in LINK.finditer(text or ""):
        url = match[0].rstrip(TRAILING)
        host = urlsplit(url).netloc.lower().removeprefix("www.")
        if host and url not in found and not host.endswith(LOGIN_HOSTS) and downloadable(url):
            found.append(url)
    return found[:MAX_LINKS]


def downloadable(url: str) -> bool:
    """A link of a service this module reads, or a URL of a PDF."""
    parts = urlsplit(url)
    host = parts.netloc.lower().removeprefix("www.")
    if host.endswith("dropbox.com") and parts.path.startswith(("/s/", "/scl/fi/")):
        return True
    if host == "drive.google.com" and (DRIVE_FILE.match(parts.path) or parts.path == "/open" and "id=" in parts.query):
        return True
    if host == "docs.google.com" and GOOGLE_EXPORT.match(parts.path):
        return True
    if host in ("disk.yandex.ru", "disk.yandex.com", "yadi.sk", "disk.360.yandex.ru"):
        return True
    return parts.path.lower().endswith(".pdf")


def direct(url: str) -> str:
    """The address a PDF is downloaded from. A Yandex Disk link stays as it is: it needs the API (`fetch`)."""
    parts = urlsplit(url)
    host = parts.netloc.lower().removeprefix("www.")
    if host.endswith("dropbox.com"):  # its content host: www.dropbox.com is blocked by some resolvers
        query = [(key, value) for key, value in parse_qsl(parts.query, keep_blank_values=True) if key != "dl"]
        return urlunsplit(("https", DROPBOX_CONTENT, parts.path, urlencode([*query, ("dl", "1")]), ""))
    if host == "drive.google.com":
        found = DRIVE_FILE.match(parts.path)
        file_id = found[1] if found else dict(parse_qsl(parts.query)).get("id", "")
        return f"https://drive.google.com/uc?export=download&id={quote(file_id)}"
    if host == "docs.google.com" and (found := GOOGLE_EXPORT.match(parts.path)):
        return "https://docs.google.com" + EXPORTS[found[1]].format(id=found[2])
    return url


def file_name(response: aiohttp.ClientResponse, url: str) -> str:
    """The file's name: Content-Disposition, else the last part of the URL's path."""
    disposition = response.headers.get("Content-Disposition", "")
    found = re.search(r"filename\*=(?:UTF-8|utf-8)''([^;]+)", disposition) or \
        re.search(r'filename="?([^";]+)"?', disposition)
    name = unquote(found[1]).strip() if found else unquote(urlsplit(url).path.rsplit("/", 1)[-1]).strip()
    name = name or "document.pdf"
    return name if name.lower().endswith(".pdf") else name + ".pdf"


async def _save(session: aiohttp.ClientSession, url: str, path: Path, limit: int) -> str | None:
    """Download `url` into `path`; its name, or None when it is no PDF or too big."""
    async with session.get(url, allow_redirects=True) as response:
        if response.status in (401, 403, 404, 410):
            return None
        if response.status >= 400:
            raise LinkError(f"HTTP {response.status}")
        if (response.content_length or 0) > limit:
            return None
        size, head = 0, b""
        with path.open("wb") as out:
            async for chunk in response.content.iter_chunked(1 << 16):
                if not head:
                    head = chunk[:5]
                    if head != b"%PDF-":
                        return None  # a page, a login form, a Word file: no sheet
                size += len(chunk)
                if size > limit:
                    return None
                out.write(chunk)
        return file_name(response, str(response.url)) if size else None


async def _yandex(session: aiohttp.ClientSession, url: str) -> tuple[str, str] | None:
    """(download address, name) of a public Yandex Disk file; None for a folder or a file that is no PDF."""
    async with session.get(YANDEX_API, params={"public_key": url, "fields": "type,name,mime_type,file"}) as response:
        if response.status in (403, 404):
            return None
        if response.status >= 400:
            raise LinkError(f"Yandex Disk: HTTP {response.status}")
        about = await response.json()
    if about.get("type") != "file" or not about.get("file"):
        return None
    name = about.get("name") or "document.pdf"
    if about.get("mime_type") != "application/pdf" and not name.lower().endswith(".pdf"):
        return None
    return about["file"], name


async def fetch(url: str, path: Path, limit: int) -> str | None:
    """Files.fetch_url: the PDF behind a link into `path`; its name, or None when there is none (a folder, a page, a
    file that is no PDF, a link that needs a login). LinkError when the service or the network fails."""
    try:
        async with public_session(timeout=TIMEOUT, headers={"User-Agent": "infomaster"}) as session:
            host = urlsplit(url).netloc.lower().removeprefix("www.")
            if host in ("disk.yandex.ru", "disk.yandex.com", "yadi.sk", "disk.360.yandex.ru"):
                found = await _yandex(session, url)
                if found is None:
                    return None
                address, name = found
                return name if await _save(session, address, path, limit) else None
            return await _save(session, direct(url), path, limit)
    except PrivateAddress:
        return None  # a link into the service's own network is no sheet (D79)
    except (aiohttp.ClientError, TimeoutError) as e:
        raise LinkError(str(e) or type(e).__name__) from e
