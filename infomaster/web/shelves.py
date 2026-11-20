"""The shelves of the web app (§8.6, stage 16.8, D67): the problem sheets with their PDFs and versions, the library's
raw material by course, and the sorting of the library's inbox.

The screens read through the sheets' store and the library; every change is the connector's write tool
(`Connector.act`), so it is checked once, journaled with `via` web and undone from the journal like the others:
`edit_sheet`, `join_sheet`, `delete_sheet`, `sort_material`, `update_material`, `remove_material`. An own sheet's PDF
is a multipart upload that no tool can take; it is kept as the Mini App keeps it and journaled by hand, with the undo
that deletes it again.

Two names of a course meet here. A sheet keeps the course as the model named it and is matched to a lens by the
course's key (`Data.key`); the library keeps the timetable's name (`Library.course`). The app speaks keys: `course_name`
turns a key into a name before a tool is called, and `Data.key` turns a name into a key on the way out.

A sheet's PDF has three homes, tried in this order: the library's copy of it (made when the sheet was read, kept for
good, opened by a signed link), the file the sheets keep for 14 days (streamed by `GET /shelf/sheets/{id}/file` behind
the session), and none, when both are gone.
"""

import datetime as dt
import json
import logging
from functools import partial

from aiohttp import web

from .. import clock
from ..collectors.files import UPLOAD_MAX_SIZE
from ..connector.tools import ToolError
from ..i18n import json_default, reason
from ..outputs import miniapp

log = logging.getLogger(__name__)

SHEET_COURSE_LIMIT = 60
SHEET_NUMBER_LIMIT = 20
ITEMS = 500          # of a course's page or of the inbox
CLASSES = 40         # the classes a course's item can be moved to
PREVIEW = 600        # characters of a text on a list
CANDIDATES = 30      # the sheets «It is a newer version of…» offers
FIELD = 400          # bytes of a form field


class Shelves:
    def __init__(self, web_app):
        self.web = web_app  # the WebApp: its data, language and connector

    def routes(self) -> list[tuple]:
        return [("GET", "", home),
                ("GET", "/sheets", sheets),
                ("POST", "/sheets", add_sheet),
                ("GET", "/sheets/{sheet_id}", sheet),
                ("PATCH", "/sheets/{sheet_id}", edit_sheet),
                ("DELETE", "/sheets/{sheet_id}", delete_sheet),
                ("POST", "/sheets/{sheet_id}/join", join_sheet),
                ("GET", "/sheets/{sheet_id}/file", sheet_file),
                ("GET", "/raw", raw),
                ("GET", "/inbox", inbox),
                ("GET", "/classes", classes),
                ("GET", "/items/{item_id}", item),
                ("PATCH", "/items/{item_id}", update_item),
                ("DELETE", "/items/{item_id}", remove_item),
                ("POST", "/sort", sort)]


SHELVES_KEY = web.AppKey("shelves", Shelves)


# --- what the handlers lean on ------------------------------------------------------------------------------------

def _web(request: web.Request):
    return request.app[SHELVES_KEY].web


def _data(request: web.Request):
    data = _web(request).data
    if data is None:
        raise web.HTTPServiceUnavailable(text="the screens are not wired")
    return data


def _library(request: web.Request):
    library = _data(request).library
    if library is None:
        raise web.HTTPNotFound(text="the library is off")
    return library


def _topic(request: web.Request):
    topic = request.app.get(miniapp.SHEETS_KEY)
    if topic is None:
        raise web.HTTPNotFound(text="the sheets are off")
    return topic


def _id(request: web.Request, name: str) -> int:
    try:
        return int(request.match_info[name])
    except ValueError:
        raise web.HTTPNotFound() from None


async def _body(request: web.Request) -> dict:
    try:
        data = await request.json()
    except (json.JSONDecodeError, UnicodeDecodeError):
        raise web.HTTPBadRequest(text="JSON expected") from None
    if not isinstance(data, dict):
        raise web.HTTPBadRequest(text="a JSON object expected")
    return data


def _answer(request: web.Request, data) -> web.Response:
    lang = _web(request).lang()
    return web.json_response(data, dumps=partial(json.dumps, default=json_default(lang)))


async def _act(request: web.Request, tool: str, arguments: dict) -> dict:
    """A change through the connector's tool, journaled: {id, summary, undo, note}."""
    connector = _web(request).connector
    if connector is None or tool not in connector.registry.tools:
        raise web.HTTPServiceUnavailable(text="changes are not wired")
    try:
        change_id, output = await connector.act(tool, arguments)
    except ToolError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return {"id": change_id, "summary": output.summary, "undo": output.undo is not None, "note": output.note}


def _name(request: web.Request, key) -> str | None:
    """The course's name for a key of the lenses; 400 for a key that is none."""
    if key in (None, ""):
        return None
    if not isinstance(key, str):
        raise web.HTTPBadRequest(text="course is a key of the lenses")
    name = _data(request).course_name(key)
    if name is None:
        raise web.HTTPBadRequest(text="no such course")
    return name


def _library_course(request: web.Request, key) -> str | None:
    """The library's course of a key: its timetable name; None when the key names no course."""
    name = _name(request, key)
    return _library(request).course(name) if name else None


# --- the sheets ---------------------------------------------------------------------------------------------------

def _entry(request: web.Request, row: dict) -> dict:
    """A line of the sheets' listing as the screen has it: the course by its key."""
    data = _data(request)
    key = data.key(row["course"])
    return {"id": row["id"], "first": row["first"], "versions": row["versions"], "status": row["status"],
            "error": row["error"], "course": key, "course_title": data.title(key), "number": row["number"],
            "name": row["name"], "due": row["due"], "due_assumed": row["due_assumed"], "chat": row["chat"],
            "own": row["own"], "created_at": row["created_at"], "updated_at": row["updated_at"],
            "summary": row["summary"], "file": row["file"]}


def _file_url(request: web.Request, version: dict) -> str | None:
    """Where a version's PDF opens, or None when it is gone: the library's copy first, then the kept file."""
    data, topic = _data(request), _topic(request)
    if data.library is not None and data.links is not None:
        found = data.library.from_origin("sheet", version["chat_id"], version["msg_id"], version["sha256"])
        if found is not None and data.library.path(found) is not None:
            return data.links.material(found)
    if topic.files.by_hash(version["sha256"]) is None:
        return None
    if data.links is not None and data.links.kept is not None:  # signed: it opens outside the session too (D69)
        return data.links.kept_pdf(version["sha256"], version["name"])
    return f"/web/api/shelf/sheets/{version['id']}/file"


async def home(request: web.Request) -> web.Response:
    """The shelves' entry: how many sheets, what waits in the inbox, and the courses with raw material."""
    data, library, topic = _data(request), _data(request).library, request.app.get(miniapp.SHEETS_KEY)
    courses = []
    if library is not None:
        for course in library.courses():
            key = data.key(course["course"])
            if key and (course["items"] or course["documents"]):
                courses.append({"key": key, "title": data.title(key), "items": course["items"], "new": course["new"],
                                "documents": course["documents"]})
    return _answer(request, {
        "library": library is not None, "inbox": library.inbox_count() if library is not None else 0,
        "sheets": len(topic.sheets.listing()) if topic is not None else None, "courses": courses})


async def sheets(request: web.Request) -> web.Response:
    """?course=<key>: the sheets of a course, or all of them; the newest first. Those of no known course come with
    all of them."""
    topic, key = _topic(request), " ".join(request.query.get("course", "").split()) or None
    listed = [_entry(request, row) for row in topic.sheets.listing()]
    if key:
        listed = [entry for entry in listed if entry["course"] == key]
    return _answer(request, {"course": key, "title": _data(request).title(key), "sheets": listed,
                             "courses": _data(request).courses()})


async def sheet(request: web.Request) -> web.Response:
    """A sheet with every version and its PDF, and the sheets it may become a newer version of."""
    topic = _topic(request)
    found = topic.sheets.get(_id(request, "sheet_id"))
    if found is None:
        raise web.HTTPNotFound(text="no such sheet")
    first = topic.sheets.first(found)
    entry = next((e for e in map(partial(_entry, request), topic.sheets.listing()) if e["first"] == first["id"]), None)
    if entry is None:
        raise web.HTTPNotFound(text="no such sheet")
    chain = topic.sheets.versions(first) if first["status"] == "posted" else [first]
    versions = [{"id": v["id"], "version": v["version"], "name": v["name"], "created_at": v["created_at"],
                 "changes": v["changes"], "file_url": _file_url(request, v)} for v in chain]
    # «It is a newer version of…» works only for a posted sheet without versions: the others are the candidates
    candidates = []
    if entry["status"] == "posted" and entry["versions"] == 1:
        others = [e for e in map(partial(_entry, request), topic.sheets.listing())
                  if e["status"] == "posted" and e["first"] != first["id"]]
        others.sort(key=lambda e: e["course"] != entry["course"])
        candidates = [{"id": e["id"], "course": e["course"], "course_title": e["course_title"],
                       "number": e["number"], "name": e["name"], "versions": e["versions"]}
                      for e in others[:CANDIDATES]]
    return _answer(request, {**entry, "versions_list": versions, "candidates": candidates,
                             "courses": _data(request).courses()})


async def sheet_file(request: web.Request) -> web.StreamResponse:
    """A version's PDF from the file the sheets keep (14 days), behind the session."""
    topic = _topic(request)
    found = topic.sheets.get(_id(request, "sheet_id"))
    path = topic.files.by_hash(found["sha256"]) if found else None
    if path is None:
        raise web.HTTPNotFound(text="the file is gone")
    name = (found["name"] or "sheet.pdf").encode("ascii", "replace").decode().replace('"', "_").replace("?", "_")
    return web.FileResponse(path, headers={"Content-Type": "application/pdf",
                                           "Content-Disposition": f'inline; filename="{name}"',
                                           "Cache-Control": "private, max-age=3600"})


def _newest(request: web.Request, sheet_id: int) -> int:
    """The id the tools and the listing use for a sheet: its newest version's."""
    topic = _topic(request)
    found = topic.sheets.get(sheet_id)
    if found is None:
        raise web.HTTPNotFound(text="no such sheet")
    chain = topic.sheets.versions(topic.sheets.first(found))
    return chain[-1]["id"] if chain and topic.sheets.first(found)["status"] == "posted" else found["id"]


async def edit_sheet(request: web.Request) -> web.Response:
    """Some of {course: a key or "", number, due: YYYY-MM-DD or null, summary}: the connector's edit_sheet."""
    values, sheet_id = await _body(request), _newest(request, _id(request, "sheet_id"))
    if not values or set(values) - {"course", "number", "due", "summary"}:
        raise web.HTTPBadRequest(text="expected some of course, number, due, summary")
    arguments = {name: value for name, value in values.items() if name != "course"}
    if "course" in values:
        arguments["course"] = _name(request, values["course"]) or ""
    change = await _act(request, "edit_sheet", {"sheet_id": sheet_id, **arguments})
    return _answer(request, {"change": change, "id": sheet_id})


async def join_sheet(request: web.Request) -> web.Response:
    """{previous: a sheet's id}: the sheet is a newer version of it; the connector's join_sheet, which cannot be
    undone."""
    values, sheet_id = await _body(request), _id(request, "sheet_id")
    previous = values.get("previous")
    if isinstance(previous, bool) or not isinstance(previous, int):
        raise web.HTTPBadRequest(text="expected {previous: id}")
    change = await _act(request, "join_sheet", {"sheet_id": sheet_id, "previous": previous})
    return _answer(request, {"change": change, "id": previous})


async def delete_sheet(request: web.Request) -> web.Response:
    """An own sheet with its versions: the connector's delete_sheet, which cannot be undone."""
    change = await _act(request, "delete_sheet", {"sheet_id": _id(request, "sheet_id")})
    return _answer(request, {"change": change})


async def _field(part) -> str:
    data = b""
    while chunk := await part.read_chunk(FIELD):
        data += chunk
        if len(data) > FIELD:
            raise web.HTTPBadRequest(text=f"{part.name}: up to {FIELD} bytes")
    return data.decode(errors="replace").strip()


async def add_sheet(request: web.Request) -> web.Response:
    """Multipart: course (a key), number, due, then the PDF `file`: an own sheet, posted at once. Journaled by hand,
    with the undo that deletes it."""
    topic = _topic(request)
    try:
        reader = await request.multipart()
    except (AssertionError, ValueError, KeyError):
        raise web.HTTPBadRequest(text="multipart/form-data expected") from None
    fields, content, name = {}, None, None
    while (part := await reader.next()) is not None:
        if part.filename is None:
            if part.name in ("course", "number", "due"):
                fields[part.name] = await _field(part)
            continue
        if content is not None:
            raise web.HTTPBadRequest(text="one PDF at a time")
        name, content = part.filename, bytearray()
        while chunk := await part.read_chunk(1 << 16):
            content += chunk
            if len(content) > UPLOAD_MAX_SIZE:
                raise web.HTTPBadRequest(text="the file is too big")
    if not content:
        raise web.HTTPBadRequest(text="choose a PDF")
    number = " ".join(fields.get("number", "").split())
    if len(number) > SHEET_NUMBER_LIMIT:
        raise web.HTTPBadRequest(text=f"number: up to {SHEET_NUMBER_LIMIT} characters")
    due = fields.get("due") or None
    try:
        due = dt.date.fromisoformat(due).isoformat() if due else None
    except ValueError:
        raise web.HTTPBadRequest(text="the due date is a date like 2026-10-22") from None
    course = _name(request, fields.get("course"))
    try:
        file = await topic.files.keep(bytes(content), name or "sheet.pdf", clock.now_utc())
    except ValueError as e:
        raise web.HTTPBadRequest(text=reason(e, _web(request).lang())) from None
    sheet_id = topic.sheets.add_own(file, course, number, due, clock.now_utc())
    connector = _web(request).connector
    summary = f"Added the own sheet «{file['name'] or name}»"
    undo = [{"tool": "_remove_own_sheet", "arguments": {"sheet_id": sheet_id}}] \
        if connector is not None and "_remove_own_sheet" in connector.registry.ops else None
    change_id = connector.journal("add_sheet", {"sheet_id": sheet_id}, summary, undo) if connector else None
    try:
        await topic.publish()
    except Exception:  # the forum takes the sheet at its next run
        log.exception("the own sheet %d is not posted yet", sheet_id)
    return _answer(request, {"id": sheet_id, "change": {"id": change_id, "summary": summary,
                                                        "undo": undo is not None, "note": ""}})


# --- the library: raw material and the inbox ----------------------------------------------------------------------

def _view(request: web.Request, row: dict, whole: bool = False) -> dict:
    """An item for the screen: its file by a signed link, a text cut to a preview unless `whole`, its course by key."""
    data = _data(request)
    text = row["text"] or ""
    view = {name: row[name] for name in ("id", "kind", "title", "author", "url", "name", "media_type", "size", "status",
                                         "day", "created_at", "class_uid", "note", "parent", "source")}
    key = data.key(row["course"])
    view.update(course=key, course_title=data.title(key), text=text if whole else text[:PREVIEW],
                text_cut=not whole and len(text) > PREVIEW,
                file_url=data.links.material(row) if data.links is not None and data.library.path(row) else None)
    return view


def _class(row: dict) -> dict:
    return {"uid": row["uid"], "start": row["start"], "end": row["end"], "kind": row["kind"],
            "items": row.get("items", 0), "new": row.get("new", 0)}


async def raw(request: web.Request) -> web.Response:
    """?course=<key>: the raw material of a course by class, and what has no class."""
    library, key = _library(request), request.query.get("course")
    course = _library_course(request, key)
    if course is None:
        raise web.HTTPBadRequest(text="which course?")
    started = library.classes(course)
    by_class: dict[str, list] = {c["uid"]: [] for c in started}
    loose = []
    for row in library.items(course=course, limit=ITEMS):
        by_class.get(row["class_uid"], loose).append(_view(request, row))
    data = _data(request)
    return _answer(request, {"course": key, "title": data.title(key),
                             "classes": [{**_class(c), "items_list": by_class[c["uid"]]} for c in started],
                             "items": loose})


async def inbox(request: web.Request) -> web.Response:
    """What came to the library without a course, the newest first, and the courses to sort it into."""
    library, data = _library(request), _data(request)
    return _answer(request, {"items": [_view(request, row) for row in library.items(inbox=True, limit=ITEMS)],
                             "courses": data.courses()})


async def classes(request: web.Request) -> web.Response:
    """?course=<key>: the started classes of a course, the last first: where an item can be moved to."""
    course = _library_course(request, request.query.get("course"))
    found = _library(request).classes(course) if course else []
    return _answer(request, {"course": request.query.get("course"), "classes": [_class(c) for c in found[:CLASSES]]})


async def item(request: web.Request) -> web.Response:
    """One item with its whole text, the class it is at and the items that hang on it (a recording's transcripts)."""
    library = _library(request)
    row = library.get(_id(request, "item_id"))
    if row is None:
        raise web.HTTPNotFound(text="no such item")
    placed = library.class_of(row["class_uid"]) if row["class_uid"] else None
    return _answer(request, {**_view(request, row, whole=True),
                             "class": {"uid": placed["uid"], "start": placed["start"], "kind": placed["kind"]}
                             if placed else None,
                             "children": [_view(request, child) for child in library.children(row["id"])],
                             "courses": _data(request).courses()})


async def update_item(request: web.Request) -> web.Response:
    """Some of {title, note, status: new or processed}: the connector's update_material."""
    values, item_id = await _body(request), _id(request, "item_id")
    if not values or set(values) - {"title", "note", "status"}:
        raise web.HTTPBadRequest(text="expected some of title, note, status")
    return _answer(request, {"change": await _act(request, "update_material", {"id": item_id, **values})})


async def remove_item(request: web.Request) -> web.Response:
    """The connector's remove_material: the item and its transcripts leave every list, and come back with the undo."""
    return _answer(request, {"change": await _act(request, "remove_material", {"id": _id(request, "item_id")})})


async def sort(request: web.Request) -> web.Response:
    """{ids, class_uid} or {ids, course: a key, or null for back to the inbox}: the connector's sort_material."""
    values = await _body(request)
    ids = values.get("ids")
    if not isinstance(ids, list) or not ids or not all(isinstance(i, int) and not isinstance(i, bool) for i in ids):
        raise web.HTTPBadRequest(text="expected {ids: [item ids]}")
    arguments: dict = {"ids": ids}
    if values.get("class_uid"):
        arguments["class_uid"] = values["class_uid"]
    elif "course" in values:
        arguments["course"] = _name(request, values["course"])
    else:
        raise web.HTTPBadRequest(text="give class_uid or course")
    return _answer(request, {"change": await _act(request, "sort_material", arguments)})
