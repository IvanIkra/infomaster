"""The corrections of the web app (§8.6, stage 16.9, D68): moving, cancelling and changing a class, changing, hiding
and deleting an event, the main or the optional calendar, the hidden deadlines and events with «Restore», and
correcting grades.

What stands behind a class or an event comes with its card (`Data.fix`), with every field as it is now. The changes
are the connector's write tools, journaled with `via` web and their undo, as in the Mini App's timetable:

- a RUZ class: `correct_class` moves, cancels or renames it and puts it in the main or the optional calendar; the
  tool resets a field it is not given, so the app sends all of them; «As it was» takes the own correction back
  (`remove_event`); the chat's correction of the class is ignored or applied again with `set_event_options`
  (hidden), which moves the class's Midterm with it;
- an event: `edit_event` changes it (a chat's one is hidden and an own copy stands for it), `set_event_options` hides
  it or picks its calendar, `remove_event` deletes an own one or takes an own copy back;
- the hidden lists: `unhide_deadline`, and `set_event_options` with hidden false;
- a grade: `change_grade`, and `delete_grade`, which gives a sheet's grade back as the sheet has it and deletes one
  added by hand.
"""

from aiohttp import web

from ..connector.tools import ToolError

CLASS_FIELDS = ("start", "end", "room", "cancelled", "kind", "required", "title")
EVENT_FIELDS = ("title", "kind", "start", "end", "room", "cancelled")


class Fixes:
    def __init__(self, web_app):
        self.web = web_app  # the WebApp: its data and connector

    def routes(self) -> list[tuple]:
        return [("POST", "/class", fix_class),
                ("POST", "/event", fix_event),
                ("POST", "/options", options),
                ("POST", "/remove", remove),
                ("GET", "/hidden", hidden),
                ("POST", "/hidden/deadline", unhide_deadline),
                ("POST", "/grade", grade),
                ("POST", "/grade/reset", reset_grade)]


FIXES_KEY = web.AppKey("fixes", Fixes)


# --- what the handlers lean on ------------------------------------------------------------------------------------

def _data(request: web.Request):
    data = request.app[FIXES_KEY].web.data
    if data is None:
        raise web.HTTPServiceUnavailable(text="the screens are not wired")
    return data


async def _body(request: web.Request) -> dict:
    from . import body  # the module that mounts this one
    return await body(request)


async def _act(request: web.Request, tool: str, arguments: dict) -> web.Response:
    """A change through the connector's tool, journaled; the answer is the change, for its undo."""
    connector = request.app[FIXES_KEY].web.connector
    if connector is None or tool not in connector.registry.tools:
        raise web.HTTPServiceUnavailable(text="changes are not wired")
    try:
        change_id, output = await connector.act(tool, arguments)
    except ToolError as e:
        raise web.HTTPBadRequest(text=str(e)) from None
    return web.json_response({"change": {"id": change_id, "summary": output.summary,
                                         "undo": output.undo is not None, "note": output.note}})


def _uid(values: dict) -> str:
    uid = values.get("uid")
    if not isinstance(uid, str) or not uid.strip():
        raise web.HTTPBadRequest(text="uid expected")
    return uid.strip()


def _event_id(values: dict) -> int:
    event_id = values.get("id")
    if isinstance(event_id, bool) or not isinstance(event_id, int):
        raise web.HTTPBadRequest(text="id: the event's id expected")
    return event_id


def _fix(request: web.Request, uid: str) -> dict:
    entry = next((e for e in _data(request).schedule.entries() if e.uid == uid), None)
    found = _data(request).fix(entry) if entry is not None else None
    if found is None:
        raise web.HTTPNotFound(text="no such class or event")
    return found


# --- classes and events -------------------------------------------------------------------------------------------

async def fix_class(request: web.Request) -> web.Response:
    """{uid, start, end, room, cancelled, kind, required, title?}: the owner's correction of a RUZ class, every field
    as the card gave it with the owner's changes; the connector's correct_class."""
    values = await _body(request)
    uid = _uid(values)
    if _fix(request, uid)["of"] != "class":
        raise web.HTTPBadRequest(text="this is an event, not a class of RUZ")
    if set(values) - {"uid", *CLASS_FIELDS}:
        raise web.HTTPBadRequest(text="expected uid and " + ", ".join(CLASS_FIELDS))
    return await _act(request, "correct_class",
                      {"class_uid": uid, **{name: values[name] for name in CLASS_FIELDS if name in values}})


async def fix_event(request: web.Request) -> web.Response:
    """{uid, some of title, kind, start, end, room, cancelled}: a change of an event; a chat's one is hidden and an own
    copy stands for it. The connector's edit_event."""
    values = await _body(request)
    found = _fix(request, _uid(values))
    if found["of"] != "event":
        raise web.HTTPBadRequest(text="a class of RUZ is changed as a class")
    fields = {name: values[name] for name in EVENT_FIELDS if name in values}
    if not fields or set(values) - {"uid", *EVENT_FIELDS}:
        raise web.HTTPBadRequest(text="expected uid and some of " + ", ".join(EVENT_FIELDS))
    return await _act(request, "edit_event", {"event_id": found["id"], **fields})


async def options(request: web.Request) -> web.Response:
    """{id, required?, hidden?}: an event hidden or shown again, in the main or the optional calendar; a chat's
    correction of a class ignored or applied again. The connector's set_event_options."""
    values = await _body(request)
    flags = {name: values[name] for name in ("required", "hidden") if name in values}
    if not flags or set(values) - {"id", "required", "hidden"}:
        raise web.HTTPBadRequest(text="expected {id, required?, hidden?}")
    return await _act(request, "set_event_options", {"event_id": _event_id(values), **flags})


async def remove(request: web.Request) -> web.Response:
    """{id}: an own event deleted, an own correction or an own copy taken back. The connector's remove_event."""
    return await _act(request, "remove_event", {"event_id": _event_id(await _body(request))})


# --- the hidden lists ---------------------------------------------------------------------------------------------

async def hidden(request: web.Request) -> web.Response:
    """The hidden deadlines and the hidden events from today on."""
    return web.json_response(_data(request).hidden())


async def unhide_deadline(request: web.Request) -> web.Response:
    """{uid}: a hidden deadline back in the list. The connector's unhide_deadline."""
    return await _act(request, "unhide_deadline", {"uid": _uid(await _body(request))})


# --- grades -------------------------------------------------------------------------------------------------------

async def grade(request: web.Request) -> web.Response:
    """{id, value, label?}: a grade changed by the owner. The connector's change_grade."""
    values = await _body(request)
    grade_id = values.get("id")
    if not isinstance(grade_id, str) or not grade_id:
        raise web.HTTPBadRequest(text="id: the grade's id expected")
    if set(values) - {"id", "value", "label"} or "value" not in values:
        raise web.HTTPBadRequest(text="expected {id, value, label?}")
    arguments = {"grade_id": grade_id, "value": values["value"]}
    if values.get("label"):
        arguments["label"] = values["label"]
    return await _act(request, "change_grade", arguments)


async def reset_grade(request: web.Request) -> web.Response:
    """{id}: a sheet's grade back as the sheet has it; a grade added by hand deleted. The connector's delete_grade."""
    grade_id = (await _body(request)).get("id")
    if not isinstance(grade_id, str) or not grade_id:
        raise web.HTTPBadRequest(text="id: the grade's id expected")
    return await _act(request, "delete_grade", {"grade_id": grade_id})
