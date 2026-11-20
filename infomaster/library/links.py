"""Links to the library's files (D58): a file goes in and out by a signed link, never inside a tool's call.

Claude Code on the owner's machine moves a lecture's recording or a built PDF with `curl`; the web app opens a PDF
the same way. A link names one file and one direction and expires; it is signed with a key kept in the database, so
the service's restart keeps the links it gave. On the public port, next to the web app:
- GET /library/<token>/<name>: a material's file, a version's source or PDF, or a sheet's PDF the sheets keep for 14
  days (D69: it opens outside the app's session, as an iPhone's Home Screen app opens a link in Safari);
- PUT /library/<token>: the bytes of a draft material or of a draft version's source or PDF (POST too).
"""

import base64
import hashlib
import hmac
import json
import secrets
import time
from collections.abc import Callable
from pathlib import Path
from urllib.parse import quote

from aiohttp import web

from ..store import Store
from . import Library, LibraryError

SECRET_KEY = "library_link_secret"
GET_TTL = 24 * 3600       # seconds a download link works
PUT_TTL = 6 * 3600        # an upload link
INLINE = ("image/", "audio/", "video/", "text/", "application/pdf")


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def _unb64(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


class Links:
    def __init__(self, store: Store, library: Library, public_url: str, seconds: Callable[[], float] = time.time):
        self.library, self.public_url, self.seconds = library, public_url.rstrip("/"), seconds
        key = store.get_json(SECRET_KEY)
        if key is None:
            key = secrets.token_hex(32)
            store.put_json(SECRET_KEY, key)
        self.key = bytes.fromhex(key)
        self.kept: Callable[[str], Path | None] | None = None  # a sheet's PDF by its hash, while the sheets keep it

    def _sign(self, payload: dict) -> str:
        body = _b64(json.dumps(payload, separators=(",", ":")).encode())
        mac = hmac.new(self.key, body.encode(), hashlib.sha256).digest()[:18]
        return f"{body}.{_b64(mac)}"

    def check(self, token: str) -> dict | None:
        """The payload of a valid, unexpired link; None otherwise."""
        body, _, mac = token.partition(".")
        try:
            expected = hmac.new(self.key, body.encode(), hashlib.sha256).digest()[:18]
            if not hmac.compare_digest(_unb64(mac), expected):
                return None
            payload = json.loads(_unb64(body))
        except ValueError:
            return None
        return payload if isinstance(payload, dict) and payload.get("e", 0) > self.seconds() else None

    # --- making links -------------------------------------------------------------------------

    def material(self, row: dict, ttl: int = GET_TTL) -> str:
        """The download link of a material's file."""
        token = self._sign({"a": "get", "m": row["id"], "e": int(self.seconds() + ttl)})
        return f"{self.public_url}/library/{token}/{quote(row.get('name') or 'file')}"

    def material_upload(self, item_id: int, ttl: int = PUT_TTL) -> str:
        return f"{self.public_url}/library/{self._sign({'a': 'put', 'm': item_id, 'e': int(self.seconds() + ttl)})}"

    def version(self, version: dict, part: str, ttl: int = GET_TTL) -> str:
        """The download link of a version's source or PDF."""
        token = self._sign({"a": "get", "v": version["id"], "p": part, "e": int(self.seconds() + ttl)})
        return f"{self.public_url}/library/{token}/{quote(file_name(version, part))}"

    def kept_pdf(self, sha256: str, name: str | None, ttl: int = GET_TTL) -> str | None:
        """The download link of a sheet's PDF the sheets keep (Files.by_hash); None when it is gone."""
        if self.kept is None or self.kept(sha256) is None:
            return None
        token = self._sign({"a": "get", "k": sha256, "e": int(self.seconds() + ttl)})
        return f"{self.public_url}/library/{token}/{quote(name or 'sheet.pdf')}"

    def version_upload(self, version_id: int, part: str, ttl: int = PUT_TTL) -> str:
        token = self._sign({"a": "put", "v": version_id, "p": part, "e": int(self.seconds() + ttl)})
        return f"{self.public_url}/library/{token}"


def file_name(version: dict, part: str) -> str:
    """A version's file as it is served: «<title> v<n>.pdf»."""
    suffix = ".pdf" if part == "pdf" else (".md" if version.get("kind") == "cheatsheet" else ".tex")
    title = "".join(c if c.isalnum() or c in " -_.,()" else "_" for c in version.get("title") or "document").strip()
    number = f" v{version['version']}" if version.get("version") else ""
    return f"{title[:100]}{number}{suffix}"


def disposition(name: str, media_type: str) -> str:
    kind = "inline" if media_type.startswith(INLINE) else "attachment"
    ascii_name = name.encode("ascii", "replace").decode().replace('"', "_").replace("?", "_")
    return f"{kind}; filename=\"{ascii_name}\"; filename*=UTF-8''{quote(name)}"


LINKS_KEY = web.AppKey("library_links", Links)
LIBRARY_KEY = web.AppKey("library", Library)


async def download(request: web.Request) -> web.StreamResponse:
    links = request.app[LINKS_KEY]
    payload = links.check(request.match_info["token"])
    if payload is None or payload.get("a") != "get":
        raise web.HTTPNotFound(text="the link is wrong or expired")
    library = links.library
    if "m" in payload:
        row = library.get(payload["m"])
        path = library.path(row) if row else None
        if path is None:
            raise web.HTTPNotFound(text="the file is gone")
        name, media_type = row["name"] or "file", row["media_type"] or "application/octet-stream"
    elif "k" in payload:
        path = links.kept(str(payload["k"])) if links.kept is not None else None
        if path is None:
            raise web.HTTPNotFound(text="the file is gone")
        name, media_type = request.match_info.get("name") or "sheet.pdf", "application/pdf"
    else:
        version = library.version(payload.get("v", 0))
        path = library.part_path(version, payload.get("p", "")) if version else None
        if path is None:
            raise web.HTTPNotFound(text="the file is gone")
        name = file_name(version, payload["p"])
        media_type = "application/pdf" if payload["p"] == "pdf" else "text/plain; charset=utf-8"
    return web.FileResponse(path, headers={"Content-Type": media_type,
                                           "Content-Disposition": disposition(name, media_type.split(";")[0]),
                                           "Cache-Control": "private, max-age=3600"})


async def upload(request: web.Request) -> web.Response:
    links = request.app[LINKS_KEY]
    payload = links.check(request.match_info["token"])
    if payload is None or payload.get("a") != "put":
        raise web.HTTPNotFound(text="the link is wrong or expired")
    chunks = request.content.iter_chunked(1 << 16)
    try:
        if "m" in payload:
            row = await links.library.fill(payload["m"], chunks)
            result = {"material": row["id"], "size": row["size"], "sha256": row["sha256"]}
        else:
            version = await links.library.put(payload["v"], payload.get("p", ""), chunks)
            result = {"version_draft": version["id"], "part": payload["p"], "uploaded": True}
    except LibraryError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return web.json_response(result)


def add_routes(app: web.Application, links: Links) -> None:
    """The links' routes; the library and its links under LIBRARY_KEY and LINKS_KEY for the web app and the tools."""
    app[LINKS_KEY], app[LIBRARY_KEY] = links, links.library
    app.router.add_get("/library/{token}/{name}", download)
    app.router.add_get("/library/{token}", download)
    app.router.add_put("/library/{token}", upload)
    app.router.add_post("/library/{token}", upload)
