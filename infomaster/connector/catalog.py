"""infomaster's tools for the MCP connector (§8.5): what Claude reads and changes, as the web app does.

Reads give the screens' data: the merged deadlines, the timetable, the grades, the news, the stored messages, the
sheets, the dictionary, the settings. Writes are the owner's actions (actions.py), made through their handlers,
each with the steps that undo it. Not here, on purpose (D57): the secrets of .env, the twin's and HSE App X's logins,
the LLM limit and the model prices (raising the limit is the owner's alone). The library's tools (stage 14: raw
material, the inbox, the knowledge base, files moved by signed links) are in library.py.
"""

import asyncio
import datetime as dt

from .. import actions, clock
from ..collectors.grades import sheet_id
from ..collectors.inbox import MAILBOX
from ..i18n import say
from ..outputs.api import SOURCES_KEY
from ..processing.events import IN_FORCE, OWN
from ..processing.grades import OWN_LAYOUT
from ..processing.pdf import PdfError, first_page_jpeg
from ..processing.schedule import DEFAULT_LENGTH, all_day, local
from ..processing.todo import CATEGORIES, uid
from ..settings import BATCH_HOURS, CHAT_SPECS, PUSH_KINDS, SPECS, SettingsError
from .tools import UNTRUSTED, Context, Output, Registry, Tool, ToolError, call_action, schema

DATE = {"type": "string", "description": "a date, YYYY-MM-DD"}
MOMENT = {"type": "string", "description": "Moscow local time, YYYY-MM-DDTHH:MM"}
SHEET_TEXT_CHARS = 20_000


def need(ctx: Context, key):
    value = ctx.app.get(key)
    if value is None:
        raise ToolError("this part of infomaster is not running")
    return value


def today(ctx: Context) -> dt.date:
    return clock.local_day(ctx.services.now())


# --- reads ----------------------------------------------------------------------------------------

def attention(ctx: Context, statuses: dict) -> list[str]:
    """What is broken and needs the owner, as «Machine» lists it under «Needs attention»."""
    found = []
    twin = statuses.get("telegram")
    if twin is not None and not twin.get("account"):
        found.append("The twin is not logged in: no chat is read (the web app: Machine → Twin and chats).")
    elif twin and twin.get("error"):
        found.append(f"The twin: {twin['error']}")
    mail = statuses.get("mail") or {}
    if mail.get("configured") and mail.get("error"):
        found.append(f"The mail is not read: {mail['error']}")
    if (statuses.get("hseapp") or {}).get("login_needed"):
        found.append("HSE App X needs a login (the web app: Machine → HSE App X).")
    if (ruz := statuses.get("ruz") or {}).get("error"):
        found.append(f"RUZ is not read: {ruz['error']}")
    env = ctx.app[actions.ENV_KEY]
    if not (env.calendar_secret or env.calendar_public_secret):
        found.append("The iCal feeds are off: CALENDAR_SECRET or CALENDAR_PUBLIC_SECRET is not set in .env.")
    for sid, sheet in ((statuses.get("grades") or {}).get("sheets") or {}).items():
        if sheet.get("error"):
            found.append(f"A gradebook is not read ({sheet.get('title') or sid}): {sheet['error']}")
    for url, page in ((statuses.get("wiki") or {}).get("pages") or {}).items():
        if page.get("error"):
            found.append(f"A course wiki page is not read ({page.get('title') or url}): {page['error']}")
    if (images := statuses.get("images") or {}).get("error"):
        found.append(f"Reading images failed: {images['error']}")
    level = ctx.app[actions.BUDGET_KEY].status().get("level")
    if level == "hard":
        found.append("The LLM limit of the day is spent: calls wait until midnight.")
    elif level not in (None, "ok"):
        found.append("Over 80 % of the LLM limit of the day is spent.")
    return found


async def get_status(ctx: Context, args: dict) -> dict:
    statuses = {name: status() for name, status in ctx.app[SOURCES_KEY].items() if name != "summaries"}
    return {"needs_attention": attention(ctx, statuses), "sources": statuses,
            "llm": ctx.app[actions.BUDGET_KEY].status()}


async def list_deadlines(ctx: Context, args: dict) -> dict:
    state = await call_action(ctx.app, actions.deadlines)
    return {"untrusted": UNTRUSTED, "today": state["today"], "items": state["items"], "own": state["own"],
            "hseapp": state["hseapp"]}


async def list_hidden_deadlines(ctx: Context, args: dict) -> dict:
    return {"hidden": (await call_action(ctx.app, actions.deadlines))["hidden"]}


async def get_timetable(ctx: Context, args: dict) -> dict:
    timetable = ctx.app[actions.SCHEDULE_KEY]
    try:
        start = dt.date.fromisoformat(args["start"]) if args.get("start") else today(ctx)
    except ValueError:
        raise ToolError("start: a date, YYYY-MM-DD") from None
    days = args.get("days", 7)
    agenda = timetable.agenda(start, days)
    return {"untrusted": UNTRUSTED, "today": today(ctx).isoformat(), "start": start.isoformat(), "days": days,
            "items": agenda["items"], "hidden_events": agenda["hidden"],
            "ruz": ctx.app[SOURCES_KEY]["ruz"]() if "ruz" in ctx.app[SOURCES_KEY] else None}


async def list_grades(ctx: Context, args: dict) -> dict:
    state = await call_action(ctx.app, actions.grade_sheets)
    return {**state, "new": need(ctx, actions.GRADES_KEY).new()}


async def get_gradebook(ctx: Context, args: dict) -> dict:
    return await call_action(ctx.app, actions.gradebook, sid=args["sheet_id"])


def chat_title(ctx: Context, chat_id: int) -> str | None:
    if chat_id == MAILBOX:
        return "mail"
    chat = ctx.app[actions.INBOX_KEY].chat(chat_id)
    return chat["title"] if chat else None


async def list_news(ctx: Context, args: dict) -> dict:
    settings = need(ctx, actions.SETTINGS_KEY)
    hours = args.get("hours") or settings.get("news_ttl_hours")
    items = ctx.services.news.live(ctx.services.now(), hours)
    return {"untrusted": UNTRUSTED, "hours": hours, "items": [
        {"id": item["id"], "source": item["source"], "chat_id": item["chat_id"],
         "chat": chat_title(ctx, item["chat_id"]), "msg_ids": item["msg_ids"], "posted_at": item["posted_at"],
         "study": bool(item["study"]), "importance": item["importance"], "full": item["full"], "short": item["short"]}
        for item in items]}


async def hide_news(ctx: Context, args: dict) -> Output:
    news = ctx.services.news
    item = news.get(args["news_id"])
    if item is None:
        raise ToolError("no such news item: take the id from list_news")
    with news.db:
        if not news.hide(item["id"]):
            raise ToolError("it is hidden already")
    return Output({"hidden": item["id"]}, f"Hid the news «{item['short'] or item['full'][:60]}»",
                  [step("_unhide_news", news_id=item["id"])])


async def unhide_news(ctx: Context, args: dict) -> None:
    with ctx.services.news.db:
        ctx.services.news.unhide(args["news_id"])


async def search_messages(ctx: Context, args: dict) -> dict:
    for name in ("since", "until"):
        if args.get(name):
            try:
                dt.date.fromisoformat(args[name])
            except ValueError:
                raise ToolError(f"{name}: a date, YYYY-MM-DD") from None
    found = ctx.app[actions.INBOX_KEY].search(args.get("text", ""), args.get("chat_id"), args.get("since"),
                                              args.get("until"), args.get("limit", 30))
    return {"untrusted": UNTRUSTED, "kept_days": 14, "messages": [
        {"chat_id": m["chat_id"], "chat": chat_title(ctx, m["chat_id"]), "msg_id": m["msg_id"], "at": m["at"],
         "sender": m["sender_name"], "subject": m["subject"], "text": m["text"][:2000],
         "cut": len(m["text"]) > 2000, "files": [f.get("name") or f.get("kind") for f in m["files"]],
         "edited": m["edited_at"] is not None} for m in found]}


async def get_message(ctx: Context, args: dict) -> dict:
    found = await call_action(ctx.app, actions.message, query={"chat": args["chat_id"], "msg": args["msg_id"]})
    return {"untrusted": UNTRUSTED, **found}


async def list_chats(ctx: Context, args: dict) -> dict:
    state = await call_action(ctx.app, actions.chats)
    for chat in state["chats"]:
        chat["settings"] = {name: value for name, value in chat["settings"].items() if name in CHAT_PROPS}
    return {"untrusted": UNTRUSTED, **state}


async def list_chat_senders(ctx: Context, args: dict) -> dict:
    return {"senders": await call_action(ctx.app, actions.senders, chat_id=args["chat_id"])}


async def list_sheets(ctx: Context, args: dict) -> dict:
    return {"untrusted": UNTRUSTED, **await call_action(ctx.app, actions.sheets)}


async def get_sheet(ctx: Context, args: dict) -> Output:
    topic = need(ctx, actions.SHEETS_KEY)
    sheets = topic.sheets
    sheet = sheets.get(args["sheet_id"])
    if sheet is None:
        raise ToolError("no such sheet")
    first = sheets.first(sheet)
    text = sheet["text"] or ""
    data = {"untrusted": UNTRUSTED, "id": sheet["id"], "name": sheet["name"], "course": sheet["course"],
            "title": ctx.app[actions.SUBJECTS_KEY].title(sheet["course"]) if sheet["course"] else None,
            "number": sheet["number"], "due": sheet["due"], "status": sheet["status"],
            "chat": chat_title(ctx, sheet["chat_id"]), "summary": first["summary"], "changes": sheet["changes"],
            "versions": [{"id": s["id"], "name": s["name"], "created_at": s["created_at"]}
                         for s in sheets.versions(first)],
            "text": text[:SHEET_TEXT_CHARS], "text_cut": len(text) > SHEET_TEXT_CHARS,
            "file_kept": topic.files.by_hash(sheet["sha256"]) is not None}
    images = []
    if args.get("first_page_image"):
        path = topic.files.by_hash(sheet["sha256"])
        try:
            image = await asyncio.to_thread(first_page_jpeg, path) if path is not None else None
        except PdfError:
            image = None
        if image is not None:
            images.append((image, "image/jpeg"))
        data["first_page_image"] = "attached" if image else \
            "not available: the file is kept 14 days, and only a scanned page (one JPEG) can be given"
    return Output(data, images=images)


async def list_courses(ctx: Context, args: dict) -> dict:
    return await call_action(ctx.app, actions.subjects)


async def get_settings(ctx: Context, args: dict) -> dict:
    settings = need(ctx, actions.SETTINGS_KEY)
    budget = ctx.app[actions.BUDGET_KEY]
    data = {"general": {name: settings.get(name) for name in actions.GENERAL if name != "llm_prices"},
            "batch_hours_choices": list(BATCH_HOURS),
            "reminders": {key: settings.get(name) for key, name in actions.REMINDER_FIELDS.items()},
            "images_daily": settings.get("images_daily"),
            "pdf": {name: settings.get(name) for name in actions.PDF_LIMITS},
            "grades": {key: settings.get(name) for key, name in actions.GRADE_SETTINGS.items() if key != "sheets"},
            "hseapp_enabled": settings.get("hseapp_enabled"),
            "notifications": {key: settings.get(name) for key, name in NOTIFICATION_NAMES.items()},
            "llm": {**budget.status(), "daily_limit_usd": settings.get("llm_daily_limit_usd"),
                    "prices": settings.get("llm_prices")},
            "not_here": "the secrets of .env, the twin's and HSE App X's logins and the LLM limit are the owner's, "
                        "in the web app («Machine»)"}
    return data


# --- writes: deadlines ----------------------------------------------------------------------------

def own_keys(ctx: Context) -> set[str]:
    return {d.key for d in ctx.app[actions.DEADLINES_KEY].todo.own()}


def listed(ctx: Context, item_uid: str):
    merged = ctx.app[actions.DEADLINES_KEY]
    d = merged.find(item_uid) or next((own for own in merged.todo.own() if own.key == item_uid), None)
    if d is None:
        raise ToolError("no such deadline in the list: take the uid from list_deadlines")
    return d


def step(tool: str, **arguments) -> dict:
    return {"tool": tool, "arguments": arguments}


async def add_deadline(ctx: Context, args: dict) -> Output:
    before = own_keys(ctx)
    body = {"title": args["title"], "due": args["due"], "category": args.get("category", "study"),
            **({"course": args["course"]} if args.get("course") else {})}
    await call_action(ctx.app, actions.add_deadline, body)
    new = sorted(own_keys(ctx) - before)
    return Output({"uid": new[0] if new else None, **body}, f"Added the deadline «{body['title']}», due {body['due']}",
                  [step("delete_deadline", uid=key) for key in new])


async def edit_deadline(ctx: Context, args: dict) -> Output:
    d = listed(ctx, args["uid"])
    body = {name: args[name] for name in ("title", "due", "category") if name in args}
    if not body:
        raise ToolError("give at least one of title, due, category")
    before = own_keys(ctx)
    await call_action(ctx.app, actions.edit_deadline, body, uid=args["uid"])
    summary = f"Changed the deadline «{d.title}» ({d.due.isoformat()}): " + \
        ", ".join(f"{name} → {value}" for name, value in body.items())
    if d.source == "ha":
        return Output({"uid": d.key, **body}, summary,
                      [step("edit_deadline", uid=d.key, title=d.title, due=d.due.isoformat(), category=d.category)])
    new = sorted(own_keys(ctx) - before)
    if not new and d.assumed:  # a sheet's assumed date given a real one: the chats' deadline itself moved (§6.3)
        return Output({"uid": None, **body}, summary + " (the sheet's real date now)", None,
                      "the sheet's date is real now: change it again to move it")
    return Output({"uid": new[0] if new else None, "hidden_original": uid(d), **body},
                  summary + " (the source's one is hidden, an own copy stands for it)",
                  [*(step("delete_deadline", uid=key) for key in new), step("unhide_deadline", uid=uid(d))])


REASON_WORDS = {"done": "as done", "not_mine": "as not the owner's", "duplicate": "as a duplicate"}


async def hide_deadline(ctx: Context, args: dict) -> Output:
    d = listed(ctx, args["uid"])
    if d.source == "ha":
        raise ToolError("this is an own deadline: delete it with delete_deadline")
    reason, kept = args.get("reason"), args.get("kept")
    if reason == "duplicate":
        if not kept:
            raise ToolError("a duplicate needs `kept`: the uid of the deadline that stays")
        if kept == args["uid"]:
            raise ToolError("a deadline cannot be a duplicate of itself")
        listed(ctx, kept)
    if reason is None:
        await call_action(ctx.app, actions.delete_deadline, uid=args["uid"])
    else:
        ctx.app[actions.DEADLINES_KEY].remove(d, reason, kept)
    words = f" {REASON_WORDS[reason]}" if reason else ""
    return Output({"hidden": uid(d), "reason": reason},
                  f"Hid the deadline «{d.title}» ({d.due.isoformat()}) of {d.source}{words}",
                  [step("unhide_deadline", uid=uid(d))])


def deadline_row(ctx: Context, d) -> dict | None:
    """The chats', mail's or RUZ's row of a deadline of the list; None for HSE App X and own ones."""
    if not d.ref.startswith("dl:") or not d.ref[3:].isdigit():
        return None
    return ctx.app[actions.DEADLINES_KEY].deadlines.get(int(d.ref[3:]))


async def confirm_deadline_date(ctx: Context, args: dict) -> Output:
    """A sheet's assumed date (§6.3) is right: it is the sheet's real date now, and the mark goes."""
    d = listed(ctx, args["uid"])
    row = deadline_row(ctx, d)
    if not d.assumed or row is None:
        raise ToolError("this deadline's date is not an assumed one")
    sheet = None
    if row["sheet_id"] is not None and ctx.app.get(actions.SHEETS_KEY) is not None:
        sheet = ctx.app[actions.SHEETS_KEY].sheets.get(row["sheet_id"])
    if sheet is not None:  # the sheet's way: the sheet's date given, the same one
        await call_action(ctx.app, actions.edit_deadline, {"due": d.due.isoformat()}, uid=args["uid"])
    else:
        deadlines = ctx.app[actions.DEADLINES_KEY].deadlines
        with deadlines.db:
            deadlines.confirm(row["id"])
    return Output({"uid": args["uid"], "due": d.due.isoformat()},
                  f"Confirmed the date of «{d.title}»: {d.due.isoformat()}",
                  [step("_assume_again", deadline_id=row["id"], sheet_id=sheet["id"] if sheet else None,
                        due_at=sheet["due_at"] if sheet else None)],
                  "the sheet keeps the date as confirmed" if sheet else "")


async def assume_again(ctx: Context, args: dict) -> None:
    """The undo of confirm_deadline_date: the date is an assumed one again and follows the timetable."""
    deadlines = ctx.app[actions.DEADLINES_KEY].deadlines
    with deadlines.db:
        row = deadlines.get(args["deadline_id"])
        if row is None or row["moved_by"] is not None or row["gone_at"] is not None:
            raise ToolError("the deadline has changed since")
        deadlines.db.execute("update deadlines set assumed = 1 where id = ?", (row["id"],))
        if args.get("sheet_id") is not None:
            deadlines.db.execute("update sheets set due_assumed = 1, due_at = coalesce(?, due_at) where id = ?",
                                 (args.get("due_at"), args["sheet_id"]))


async def set_deadline_course(ctx: Context, args: dict) -> Output:
    """The course of a deadline whose chat or letter named none (or named it wrong)."""
    d = listed(ctx, args["uid"])
    row = deadline_row(ctx, d)
    if row is None:
        raise ToolError("only a deadline of a chat, a letter or RUZ has a course to set")
    course = " ".join(args["course"].split())
    if not course:
        raise ToolError("course is empty")
    deadlines = ctx.app[actions.DEADLINES_KEY].deadlines
    with deadlines.db:
        deadlines.rename(row["id"], row["label"], course)
    return Output({"uid": args["uid"], "course": course}, f"Set the course of «{d.title}» to {course}",
                  [step("_set_course_back", deadline_id=row["id"], course=row["course"])])


async def set_course_back(ctx: Context, args: dict) -> None:
    deadlines = ctx.app[actions.DEADLINES_KEY].deadlines
    row = deadlines.get(args["deadline_id"])
    if row is None:
        raise ToolError("the deadline is gone")
    with deadlines.db:
        deadlines.rename(row["id"], row["label"], args.get("course"))


async def delete_deadline(ctx: Context, args: dict) -> Output:
    d = listed(ctx, args["uid"])
    if d.source != "ha":
        raise ToolError("this deadline comes from a source: hide it with hide_deadline")
    await call_action(ctx.app, actions.delete_deadline, uid=args["uid"])
    return Output({"deleted": d.key}, f"Deleted the own deadline «{d.title}» ({d.due.isoformat()})",
                  [step("add_deadline", title=d.title, due=d.due.isoformat(), category=d.category)])


async def unhide_deadline(ctx: Context, args: dict) -> Output:
    merged = ctx.app[actions.DEADLINES_KEY]
    row = next((row for row in merged.todo.hidden_rows() if row["uid"] == args["uid"]), None)
    if row is None:
        raise ToolError("no such hidden deadline: take the uid from list_hidden_deadlines")
    await call_action(ctx.app, actions.unhide_deadline, uid=args["uid"])
    back = merged.find(args["uid"]) is not None
    return Output({"unhidden": args["uid"], "back_in_list": back},
                  f"Restored the hidden deadline «{row['title']}» ({row['due']})",
                  [step("hide_deadline", uid=args["uid"])] if back else None,
                  "" if back else "its source no longer has it, so it is not back in the list")


# --- writes: the timetable ---------------------------------------------------------------------

def newest_own(ctx: Context, after: int, correction: bool = False) -> int | None:
    timetable = ctx.app[actions.SCHEDULE_KEY]
    row = timetable.db.execute(f"select id from events where id > ? and source = ? and class_uid is "
                               f"{'not ' if correction else ''}null order by id desc limit 1", (after, OWN)).fetchone()
    return row["id"] if row else None


def last_event_id(ctx: Context) -> int:
    return ctx.app[actions.SCHEDULE_KEY].db.execute("select coalesce(max(id), 0) n from events").fetchone()["n"]


def event_fields(event: dict) -> dict:
    """An event's fields as add_event and edit_event take them."""
    end = None
    if not all_day(event["start"]):
        end = event["end"] or (local(event["start"]) + DEFAULT_LENGTH).isoformat()
    return {"title": event["title"], "kind": event["kind"], "start": event["start"], "end": end,
            "room": event["room"] or "", "cancelled": bool(event["cancelled"])}


def current_event(ctx: Context, event_id: int) -> dict:
    event = ctx.app[actions.SCHEDULE_KEY].events.current(event_id)
    if event is None or event["gone_at"] is not None:
        raise ToolError("no such event: take the id from get_timetable (items[].event.id or hidden_events[].id)")
    return event


async def add_event(ctx: Context, args: dict) -> Output:
    body = {"title": args["title"], "kind": args.get("kind", "Event"), "start": args["start"],
            "end": args.get("end"), "room": args.get("room", ""), "required": args.get("required", True)}
    if args.get("cancelled"):
        body["cancelled"] = True
    after = last_event_id(ctx)
    await call_action(ctx.app, actions.add_event, body)
    new = newest_own(ctx, after)
    return Output({"event_id": new, **body}, f"Added the event «{body['title']}» at {body['start']}",
                  [step("remove_event", event_id=new)] if new else None)


async def edit_event(ctx: Context, args: dict) -> Output:
    event = current_event(ctx, args["event_id"])
    if event["class_uid"] is not None:
        raise ToolError("this is a class of RUZ: change it with correct_class")
    body = {name: args[name] for name in ("title", "kind", "start", "end", "room", "cancelled") if name in args}
    if not body:
        raise ToolError("give at least one of title, kind, start, end, room, cancelled")
    if "start" in body and "end" not in body:
        body["end"] = None
    after = last_event_id(ctx)
    await call_action(ctx.app, actions.update_event, body, event_id=event["id"])
    new = newest_own(ctx, after)
    summary = f"Changed the event «{event['title']}» ({event['start']}): " + \
        ", ".join(f"{name} → {value}" for name, value in body.items())
    if event["source"] == OWN:
        before = event_fields(event)
        return Output({"event_id": new, **body}, summary,
                      [step("edit_event", event_id=new, **{name: before[name] for name in body})] if new else None)
    return Output({"event_id": new, "hidden_original": event["id"], **body},
                  summary + " (the chat's one is hidden, an own copy stands for it)",
                  [step("remove_event", event_id=new)] if new else None)


async def set_event_options(ctx: Context, args: dict) -> Output:
    event = current_event(ctx, args["event_id"])
    body = {name: args[name] for name in ("required", "hidden") if name in args}
    if not body:
        raise ToolError("give required, hidden or both")
    await call_action(ctx.app, actions.update_event, body, event_id=event["id"])
    after = ctx.app[actions.SCHEDULE_KEY].events.current(event["id"])
    words = {"required": ("in the main calendar", "in the optional calendar"), "hidden": ("hidden", "shown again")}
    return Output({"event_id": after["id"] if after else event["id"], **body},
                  f"«{event['title']}» ({event['start']}): " + ", ".join(
                      words[name][0 if value else 1] for name, value in body.items()),
                  [step("set_event_options", event_id=after["id"] if after else event["id"],
                        **{name: bool(event[name]) for name in body})])


async def remove_event(ctx: Context, args: dict) -> Output:
    event = current_event(ctx, args["event_id"])
    if event["source"] != OWN:
        raise ToolError("only own events, own copies and own class corrections are removed; hide a chat's event "
                        "with set_event_options")
    await call_action(ctx.app, actions.delete_event, event_id=event["id"])
    fields = event_fields(event)
    if event["class_uid"] is not None:
        undo = step("correct_class", class_uid=event["class_uid"], start=fields["start"], end=fields["end"],
                    room=fields["room"], cancelled=fields["cancelled"], kind=fields["kind"],
                    required=bool(event["required"]), **({"title": event["title"]} if event["title"] else {}))
        return Output({"removed": event["id"]}, f"Took back the own change of the class at {event['start']}", [undo])
    if event["copy_of"]:
        row = ctx.app[actions.SCHEDULE_KEY].db.execute(
            f"select id from events where uid = ? and {IN_FORCE} order by id desc limit 1",
            (event["copy_of"],)).fetchone()
        return Output({"removed": event["id"]}, f"Took back the own change of «{event['title']}»: the chat's one is "
                      "back", [step("edit_event", event_id=row["id"], **fields)] if row else None,
                      "" if row else "the chat's event is gone")
    return Output({"removed": event["id"]}, f"Deleted the own event «{event['title']}» ({event['start']})",
                  [step("add_event", **fields, required=bool(event["required"]))])


async def correct_class(ctx: Context, args: dict) -> Output:
    timetable = ctx.app[actions.SCHEDULE_KEY]
    earlier = timetable.events.own_correction(args["class_uid"])
    body = {"class_uid": args["class_uid"], "start": args["start"], "end": args["end"], "room": args.get("room", ""),
            "cancelled": args.get("cancelled", False),
            **{name: args[name] for name in ("title", "kind", "required") if name in args}}
    await call_action(ctx.app, actions.correct_class, body)
    now = timetable.events.own_correction(args["class_uid"])
    what = "Cancelled" if body["cancelled"] else "Changed"
    summary = f"{what} the class {args['class_uid'][:12]}…: {body['start']}–{body['end']}" + \
        (f", {body['room']}" if body["room"] else "")
    if earlier is None:
        return Output({"correction_id": now["id"] if now else None, **body}, summary,
                      [step("remove_event", event_id=now["id"])] if now else None)
    fields = event_fields(earlier)
    return Output({"correction_id": now["id"] if now else None, **body}, summary, [step(
        "correct_class", class_uid=args["class_uid"], start=fields["start"], end=fields["end"], room=fields["room"],
        cancelled=fields["cancelled"], kind=fields["kind"], required=bool(earlier["required"]),
        **({"title": earlier["title"]} if earlier["title"] else {}))])


# --- writes: sheets, chats, the dictionary ------------------------------------------------------

async def edit_sheet(ctx: Context, args: dict) -> Output:
    sheets = need(ctx, actions.SHEETS_KEY).sheets
    sheet = sheets.get(args["sheet_id"])
    if sheet is None:
        raise ToolError("no such sheet")
    body = {name: args[name] for name in ("course", "number", "due", "summary") if name in args}
    if not body:
        raise ToolError("give at least one of course, number, due, summary")
    before = {**sheet, "summary": sheets.first(sheet)["summary"]}
    await call_action(ctx.app, actions.edit_sheet, body, sheet_id=sheet["id"])
    old = {name: (before[name] if before[name] is not None else ("" if name != "due" else None)) for name in body}
    return Output({"sheet_id": sheet["id"], **body}, f"Changed the sheet «{sheet['name']}»: " + ", ".join(body),
                  [step("edit_sheet", sheet_id=sheet["id"], **old)])


async def delete_sheet(ctx: Context, args: dict) -> Output:
    """An own sheet with all its versions; a chat's sheet is not the owner's to delete."""
    topic = need(ctx, actions.SHEETS_KEY)
    sheet = topic.sheets.get(args["sheet_id"])
    if sheet is None:
        raise ToolError("no such sheet")
    if not await topic.remove(sheet["id"]):
        raise ToolError("it is not an own sheet: only the sheets the owner uploaded can be deleted")
    return Output({"deleted": sheet["id"]}, f"Deleted the own sheet «{sheet['name']}»", None,
                  "upload the file again to bring it back")


async def remove_own_sheet(ctx: Context, args: dict) -> None:
    """The undo of an own sheet's upload."""
    if not await need(ctx, actions.SHEETS_KEY).remove(args["sheet_id"]):
        raise ToolError("the sheet is gone already")


async def join_sheet(ctx: Context, args: dict) -> Output:
    """A sheet posted as new becomes a newer version of another one; the file replaces the older one's."""
    topic = need(ctx, actions.SHEETS_KEY)
    sheet, older = topic.sheets.get(args["sheet_id"]), topic.sheets.get(args["previous"])
    if sheet is None or older is None:
        raise ToolError("no such sheet")
    try:
        await topic.join(sheet["id"], older["id"])
    except ValueError as e:
        raise ToolError(say(e.args[0], "en") if e.args else "it cannot be joined") from None
    return Output({"sheet_id": sheet["id"], "previous": older["id"]},
                  f"Joined the sheet «{sheet['name']}» to «{older['name']}» as its newer version", None,
                  "the two are one sheet now")


async def update_chat_settings(ctx: Context, args: dict) -> Output:
    chat_id = args["chat_id"]
    chat = ctx.app[actions.INBOX_KEY].chat(chat_id)
    if chat is None:
        raise ToolError("no such chat: take the id from list_chats")
    body = {name: value for name, value in args.items() if name != "chat_id"}
    if not body:
        raise ToolError("give at least one setting")
    before = ctx.app[actions.CHAT_SETTINGS_KEY].all(chat_id)
    await call_action(ctx.app, actions.update_chat, body, chat_id=chat_id)
    return Output({"chat_id": chat_id, **body}, f"Chat «{chat['title']}»: " + ", ".join(
        f"{name} → {value}" for name, value in body.items()),
        [step("update_chat_settings", chat_id=chat_id, **{name: before[name] for name in body})])


async def set_course(ctx: Context, args: dict) -> Output:
    subjects = ctx.app[actions.SUBJECTS_KEY]
    before = subjects.get(args["name"])
    entry = await call_action(ctx.app, actions.put_subject, {name: args[name] for name in ("name", "title", "abbr")})
    undo = step("_restore_course", name=entry["name"], title=before.title, abbr=before.abbr, manual=before.manual) \
        if before is not None else step("delete_course", name=entry["name"])
    return Output(entry, f"Dictionary: «{entry['name']}» → {entry['title']} ({entry['abbr']})", [undo])


async def delete_course(ctx: Context, args: dict) -> Output:
    subjects = ctx.app[actions.SUBJECTS_KEY]
    before = subjects.get(args["name"])
    await call_action(ctx.app, actions.delete_subject, {"name": args["name"]})
    return Output({"deleted": args["name"]}, f"Dictionary: deleted «{args['name']}»",
                  [step("_restore_course", name=args["name"], title=before.title, abbr=before.abbr,
                        manual=before.manual)] if before is not None else None)


async def restore_course(ctx: Context, args: dict) -> None:
    ctx.app[actions.SUBJECTS_KEY].restore(args["name"], args["title"], args["abbr"], args["manual"])


# --- writes: settings ---------------------------------------------------------------------------

async def setting_change(ctx: Context, names: dict[str, str], args: dict, handler, tool: str, what: str) -> Output:
    """A change of general settings through the action `handler`; `names`: the argument -> the setting."""
    settings = need(ctx, actions.SETTINGS_KEY)
    if not args:
        raise ToolError("give at least one setting")
    before = {key: settings.get(names[key]) for key in args}
    await call_action(ctx.app, handler, dict(args))
    return Output(dict(args), f"{what}: " + ", ".join(f"{key} → {value}" for key, value in args.items()),
                  [step(tool, **before)])


GENERAL_NAMES = {name: name for name in ("batch_hours", "news_ttl_hours", "language",
                                         "about_me", "wiki_pages")}


async def update_general_settings(ctx: Context, args: dict) -> Output:
    return await setting_change(ctx, GENERAL_NAMES, args, actions.update_general, "update_general_settings",
                                "General settings")


async def update_reminders(ctx: Context, args: dict) -> Output:
    need(ctx, actions.REMINDERS_KEY)
    return await setting_change(ctx, actions.REMINDER_FIELDS, args, actions.update_reminders, "update_reminders",
                                "Deadline reminders")


async def update_images(ctx: Context, args: dict) -> Output:
    return await setting_change(ctx, {"daily": "images_daily"}, args, actions.update_images, "update_images",
                                "Images read a day")


async def update_pdf_limits(ctx: Context, args: dict) -> Output:
    need(ctx, actions.SHEETS_KEY)
    return await setting_change(ctx, {name: name for name in actions.PDF_LIMITS}, args, actions.update_sheets,
                                "update_pdf_limits", "Reading whole PDFs")


NOTIFICATION_NAMES = {"kinds": "push_kinds", "quiet": "push_quiet"}


async def update_notifications(ctx: Context, args: dict) -> Output:
    """The web app's notifications (§8.6, D65): which kinds come, and the quiet hours."""
    settings = need(ctx, actions.SETTINGS_KEY)
    if not args:
        raise ToolError("give kinds, quiet or both")
    try:
        checked = {key: SPECS[NOTIFICATION_NAMES[key]][1](value) for key, value in args.items()}
    except SettingsError as e:
        raise ToolError(str(e)) from None
    before = {key: settings.get(NOTIFICATION_NAMES[key]) for key in checked}
    for key, value in checked.items():
        settings.set(NOTIFICATION_NAMES[key], value)
    shown = {key: ", ".join(value) if isinstance(value, list) else value for key, value in checked.items()}
    return Output(checked, "Notifications: " + ", ".join(f"{key} → {value or 'none'}" for key, value in shown.items()),
                  [step("update_notifications", **before)])


# --- writes: grades -----------------------------------------------------------------------------

FORGOTTEN = "the grades are read again from scratch: own changes of the sheet's grades and its tab layouts are gone"


async def update_grade_settings(ctx: Context, args: dict) -> Output:
    grades = need(ctx, actions.GRADES_KEY)
    names = {"student": "grades_student", "window_days": "grades_window_days"}
    if not args:
        raise ToolError("give student, window_days or both")
    before = {key: grades.settings.get(names[key]) for key in args}
    await call_action(ctx.app, actions.update_grades, dict(args))
    renamed = "student" in args and args["student"] != before["student"]
    return Output(dict(args), "Grades: " + ", ".join(f"{key} → {value}" for key, value in args.items()),
                  [step("update_grade_settings", **before)], f"every sheet: {FORGOTTEN}" if renamed else "")


def gradebooks(ctx: Context) -> list[dict]:
    return [dict(sheet) for sheet in need(ctx, actions.GRADES_KEY).settings.get("grades_sheets")]


def gradebook_of(ctx: Context, sid: str) -> dict:
    found = next((sheet for sheet in gradebooks(ctx) if sheet_id(sheet["url"]) == sid), None)
    if found is None:
        raise ToolError("no such gradebook: take the id from list_grades")
    return found


async def add_gradebook(ctx: Context, args: dict) -> Output:
    new = {"url": args["url"], "label": args.get("label", ""), **({"name": args["name"]} if args.get("name") else {})}
    await call_action(ctx.app, actions.update_grades, {"sheets": [*gradebooks(ctx), new]})
    sid = sheet_id(args["url"])
    return Output({"sheet_id": sid, **new}, f"Added the gradebook {new.get('name') or new['label'] or sid}",
                  [step("remove_gradebook", sheet_id=sid)])


async def remove_gradebook(ctx: Context, args: dict) -> Output:
    sheet = gradebook_of(ctx, args["sheet_id"])
    await call_action(ctx.app, actions.update_grades, {"sheets": [s for s in gradebooks(ctx)
                                                               if sheet_id(s["url"]) != args["sheet_id"]]})
    return Output({"removed": args["sheet_id"]}, f"Removed the gradebook {sheet.get('name') or sheet['label']}",
                  [step("add_gradebook", url=sheet["url"], label=sheet["label"],
                        **({"name": sheet["name"]} if sheet.get("name") else {}))], FORGOTTEN)


async def update_gradebook(ctx: Context, args: dict) -> Output:
    sheet = gradebook_of(ctx, args["sheet_id"])
    body = {name: args[name] for name in ("name", "label") if name in args}
    if not body:
        raise ToolError("give name, label or both")
    await call_action(ctx.app, actions.update_gradebook, body, sid=args["sheet_id"])
    return Output({"sheet_id": args["sheet_id"], **body}, f"Gradebook {sheet.get('name') or sheet['label']}: " +
                  ", ".join(f"{name} → {value}" for name, value in body.items()),
                  [step("update_gradebook", sheet_id=args["sheet_id"], **{name: sheet.get(name, "")
                                                                          for name in body})])


async def read_gradebook_now(ctx: Context, args: dict) -> Output:
    await call_action(ctx.app, actions.read_gradebook, sid=args["sheet_id"])
    return Output({"reading": args["sheet_id"], "hint": "call get_gradebook in a minute to see what was read"},
                  f"Asked to read the gradebooks now ({args['sheet_id']})", None, "nothing to undo")


def own_layout(ctx: Context, sid: str, tab: str) -> dict | None:
    return need(ctx, actions.GRADES_KEY).store.get_json(f"{OWN_LAYOUT}{sid}|{tab}")


async def set_tab_layout(ctx: Context, args: dict) -> Output:
    before = own_layout(ctx, args["sheet_id"], args["tab"])
    await call_action(ctx.app, actions.set_layout, {"skip": args["skip"], "works": args["works"]},
                   sid=args["sheet_id"], tab=args["tab"])
    undo = step("set_tab_layout", sheet_id=args["sheet_id"], tab=args["tab"], skip=before["skip"],
                works=before["items"]) if before else step("reset_tab_layout", sheet_id=args["sheet_id"],
                                                           tab=args["tab"])
    return Output({"sheet_id": args["sheet_id"], "tab": args["tab"], "skip": args["skip"]},
                  f"Laid out the tab «{args['tab']}» by hand: " + ("skipped" if args["skip"] else
                                                                  f"{len(args['works'])} work(s)"), [undo])


async def reset_tab_layout(ctx: Context, args: dict) -> Output:
    before = own_layout(ctx, args["sheet_id"], args["tab"])
    await call_action(ctx.app, actions.clear_layout, sid=args["sheet_id"], tab=args["tab"])
    return Output({"sheet_id": args["sheet_id"], "tab": args["tab"]},
                  f"Gave the tab «{args['tab']}» back to Haiku",
                  [step("set_tab_layout", sheet_id=args["sheet_id"], tab=args["tab"], skip=before["skip"],
                        works=before["items"])] if before else None, "" if before else "it had no own layout")


def grade_before(ctx: Context, grade_id: str) -> dict | None:
    """A grade as it is: one added by hand ({course, label, value, own}), or a sheet's with the owner's change."""
    grades = need(ctx, actions.GRADES_KEY)
    for grade in grades.manual():
        if grade["id"] == grade_id:
            return {**grade, "own": True}
    for sheet in gradebooks(ctx):
        for grade in grades.items(sheet_id(sheet["url"])):
            if grade["id"] == grade_id:
                return {**grade, "own": False}
    return None


async def add_grade(ctx: Context, args: dict) -> Output:
    grades = need(ctx, actions.GRADES_KEY)
    before = {grade["id"] for grade in grades.manual()}
    body = {name: args[name] for name in ("course", "label", "value")}
    await call_action(ctx.app, actions.add_grade, body)
    new = sorted({grade["id"] for grade in grades.manual()} - before)
    return Output({"grade_id": new[0] if new else None, **body},
                  f"Added the grade {body['course']} {body['label']}: {body['value']}",
                  [step("delete_grade", grade_id=key) for key in new])


async def change_grade(ctx: Context, args: dict) -> Output:
    before = grade_before(ctx, args["grade_id"])
    if before is None:
        raise ToolError("no such grade: take the id from list_grades")
    body = {name: args[name] for name in ("value", "label", "course") if name in args}
    await call_action(ctx.app, actions.change_grade, body, grade=args["grade_id"])
    if before["own"]:
        undo = step("change_grade", grade_id=args["grade_id"], value=before["value"], label=before["label"],
                    course=before["course"])
    elif before["edited"]:
        undo = step("change_grade", grade_id=args["grade_id"], value=before["value"], label=before["label"])
    else:
        undo = step("delete_grade", grade_id=args["grade_id"])
    return Output({"grade_id": args["grade_id"], **body}, f"Changed the grade {before['label']}: "
                  f"{before['value']} → {body['value']}", [undo])


async def delete_grade(ctx: Context, args: dict) -> Output:
    before = grade_before(ctx, args["grade_id"])
    if before is None:
        raise ToolError("no such grade: take the id from list_grades")
    await call_action(ctx.app, actions.drop_grade, grade=args["grade_id"])
    if before["own"]:
        return Output({"deleted": args["grade_id"]}, f"Deleted the grade {before['label']} ({before['value']})",
                      [step("add_grade", course=before["course"], label=before["label"], value=before["value"])]
                      if before["course"] else None, "" if before["course"] else "it had no course")
    return Output({"reset": args["grade_id"]}, f"The grade {before['label']} is back as the sheet has it",
                  [step("change_grade", grade_id=args["grade_id"], value=before["value"], label=before["label"])]
                  if before["edited"] else None, "" if before["edited"] else "it was not changed")


# --- the catalog --------------------------------------------------------------------------------

UID = {"type": "string", "description": "the deadline's uid, as list_deadlines gives it (items[].uid or own[].uid)"}
EVENT_ID = {"type": "integer", "description": "the event's id, from get_timetable: items[].event.id, or "
                                              "hidden_events[].id"}
GRADEBOOK = {"type": "string", "description": "the gradebook's id, from list_grades: sheets[].id"}
TAB = {"type": "string", "description": "the tab's name, from get_gradebook: tabs[].tab"}
GRADE_VALUE = {"type": "number", "minimum": 0, "maximum": 10, "description": "the grade, 0 to 10"}
EVENT_PROPS = {
    "title": {"type": "string", "maxLength": actions.EVENT_TITLE_LIMIT, "description": "short English title"},
    "kind": {"type": "string", "maxLength": actions.KIND_LIMIT,
             "description": "the type: Event, Consultation, Exam, Lecture, Seminar…"},
    "start": {"type": "string", "description": "Moscow local time YYYY-MM-DDTHH:MM, or a date YYYY-MM-DD for an "
                                               "all-day event (then no end)"},
    "end": {"type": ["string", "null"], "description": "Moscow local time YYYY-MM-DDTHH:MM, after the start; "
                                                       "null for an all-day event"},
    "room": {"type": "string", "maxLength": actions.ROOM_LIMIT, "description": "a room or a meeting link"},
    "cancelled": {"type": "boolean"},
}
CHAT_PROPS = {
    "enabled": {"type": "boolean", "description": "read the chat at all"},
    "study": {"type": "boolean", "description": "a study chat: its news counts as study"},
    "filter": {"type": "string", "enum": ["all", "important"], "description": "important: only the people listed in "
                                                                              "`people` are read"},
    "people": {"type": "array", "items": {"type": "integer"},
               "description": "Telegram user ids of the important people (list_chat_senders gives them)"},
    "images": {"type": "boolean", "description": "Haiku reads photos and screenshots of the chat"},
    "pdf_content": {"type": "boolean", "description": "PDFs of the chat are read whole (problem sheets)"},
    "urgent": {"type": "boolean", "description": "a new message starts a batch at once"},
    "batch_hours": {"type": ["integer", "null"], "enum": [None, *BATCH_HOURS],
                    "description": "the chat's own batch period in hours; null: the general one"},
    "note": {"type": "string", "maxLength": 300, "description": "what the chat is about, for the models"},
}
assert set(CHAT_PROPS) <= set(CHAT_SPECS)


def register(registry: Registry, app) -> None:
    """The tools of the parts of infomaster this app runs with."""
    has = {key: app.get(key) is not None for key in (actions.SETTINGS_KEY, actions.SHEETS_KEY, actions.GRADES_KEY,
                                                     actions.REMINDERS_KEY)}

    def read(name, title, description, properties, run, required=()):
        registry.add(Tool(name, title, description, schema(properties, required), run))

    def write(name, title, description, properties, run, required=(), destructive=True, idempotent=False):
        registry.add(Tool(name, title, description, schema(properties, required), run, write=True,
                          destructive=destructive, idempotent=idempotent))

    read("get_status", "Status and what needs attention",
         "What is broken and needs the owner (needs_attention), every source's status (the twin, mail, RUZ, HSE App X, "
         "sheets, grades, images) and today's LLM spending against the limit.", {}, get_status)
    read("list_deadlines", "Deadlines",
         "The one merged list of deadlines (HSE App X, RUZ midterms, chats, mail, own ones), as the web app and Tablo "
         "show it, in screen order. Each item has a uid (for the change tools), title, due date, category "
         "(study/personal), source, course, overdue, and for chats and mail the chat and the message (chat, msg) it "
         "came from. `own` lists the owner's own deadlines.", {}, list_deadlines)
    read("list_hidden_deadlines", "Hidden deadlines",
         "Deadlines of the sources the owner hid for good; unhide_deadline brings one back.", {},
         list_hidden_deadlines)
    read("get_timetable", "Timetable",
         "The timetable as the calendar feeds have it: RUZ classes with their changes, events from chats and mail, own "
         "events, by day. Each item has uid, title, start, end, type, room, required (main or optional calendar), "
         "cancelled, source, and `event`: the event or correction behind it (its id is what edit_event, "
         "set_event_options and remove_event take). hidden_events: events hidden from the feeds.",
         {"start": {**DATE, "description": "the first day, YYYY-MM-DD; today when left out"},
          "days": {"type": "integer", "minimum": 1, "maximum": 60, "description": "how many days, 7 by default"}},
         get_timetable)
    read("list_news", "News",
         "Live news parsed from the chats and mail, ranked: importance 1-5, study or not, full and short text, and the "
         "chat and message ids it was made of.",
         {"hours": {"type": "integer", "minimum": 1, "maximum": 336,
                    "description": "how old at most, in hours; by default how long news lives (news_ttl_hours)"}},
         list_news)
    read("search_messages", "Search messages",
         "Raw messages of the enabled chats and letters of the mailbox (chat_id 0), newest first. They are kept 14 "
         "days. Filter by text (any case), chat and days.",
         {"text": {"type": "string", "description": "words the text or the letter's subject contains"},
          "chat_id": {"type": "integer", "description": "one chat (list_chats gives the ids; 0 is the mailbox)"},
          "since": {**DATE, "description": "from this day, YYYY-MM-DD"},
          "until": {**DATE, "description": "to this day included, YYYY-MM-DD"},
          "limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "30 by default"}},
         search_messages)
    read("get_message", "One message",
         "A message as the twin read it (chat_id 0: a letter), with the message it answers, its link and its chat; "
         "the page a source link of a deadline or event opens (D50).",
         {"chat_id": {"type": "integer"}, "msg_id": {"type": "integer"}}, get_message, ("chat_id", "msg_id"))
    read("list_chats", "Chats and their settings",
         "Every chat of the twin's account with its settings (enabled, study, filter, people, images, pdf_content, "
         "urgent, batch_hours, note), stored message counts, and the twin's, mail's and images' status.", {},
         list_chats)
    read("list_chat_senders", "People of a chat", "Who wrote in a chat, most active first: ids for `people`.",
         {"chat_id": {"type": "integer"}}, list_chat_senders, ("chat_id",))
    read("list_courses", "Course dictionary",
         "The course dictionary: a source's course name → short English title and abbreviation (the prefix of "
         "deadline labels). manual: set by the owner, Haiku never overwrites it.", {}, list_courses)
    if has[actions.SETTINGS_KEY]:
        read("get_settings", "Settings",
             "Every setting «Machine» shows, without the secrets: batches, news life, languages, what the student "
             "told the models (about_me), reminders, images a day, whole-PDF limits, grades, the LLM limit and "
             "spending (read only).", {}, get_settings)
    if has[actions.SETTINGS_KEY] and has[actions.SHEETS_KEY]:
        read("list_sheets", "Problem sheets",
             "Problem sheets found in the chats or uploaded, newest first, with course, number, due date, summary, "
             "versions, and the limits of reading a PDF whole.", {}, list_sheets)
        read("get_sheet", "One problem sheet",
             "A sheet's details and the text of its pages as read locally (empty for a scan). With "
             "first_page_image, a scanned first page comes as an image while the file is kept (14 days).",
             {"sheet_id": {"type": "integer", "description": "from list_sheets: sheets[].id"},
              "first_page_image": {"type": "boolean"}}, get_sheet, ("sheet_id",))
    if has[actions.GRADES_KEY]:
        read("list_grades", "Grades",
             "The gradebooks (Google Sheets) with every grade read, 0-10, the grades added by hand, and `new`: the "
             "new grades of the window.", {}, list_grades)
        read("get_gradebook", "One gradebook",
             "A gradebook's last read and every tab with its layout: which column is which work, the own row's "
             "cells, and whose the layout is (Haiku's or the owner's).", {"sheet_id": GRADEBOOK}, get_gradebook,
             ("sheet_id",))

    write("hide_news", "Hide a news item",
          "Take a news item off the news list, the web app and Tablo; it stays hidden when its message is edited.",
          {"news_id": {"type": "integer", "description": "from list_news: items[].id"}}, hide_news, ("news_id",))
    registry.op("_unhide_news", unhide_news)
    write("add_deadline", "Add a deadline", "A new own deadline.",
          {"title": {"type": "string", "maxLength": actions.TITLE_LIMIT}, "due": DATE,
           "category": {"type": "string", "enum": list(CATEGORIES), "description": "study by default"},
           "course": {"type": "string", "maxLength": actions.COURSE_KEY_LIMIT,
                      "description": "the course's abbreviation (list_courses), for its lens in the web app"}},
          add_deadline, ("title", "due"), destructive=False)
    write("edit_deadline", "Change a deadline",
          "Change the title, due date or list of a deadline. An own one changes; one from a source is hidden and an "
          "own copy with the change takes its place.",
          {"uid": UID, "title": {"type": "string", "maxLength": actions.TITLE_LIMIT}, "due": DATE,
           "category": {"type": "string", "enum": list(CATEGORIES)}}, edit_deadline, ("uid",))
    write("hide_deadline", "Hide a deadline",
          "Hide a deadline of a source (a chat, mail, HSE App X, RUZ) for good, even if the source sends it again. "
          "reason, if known: done (handed in), not_mine (not the owner's work), duplicate (the same work as `kept`, "
          "which stays).",
          {"uid": UID, "reason": {"type": "string", "enum": ["done", "not_mine", "duplicate"]},
           "kept": {**UID, "description": "of a duplicate: the uid of the deadline that stays"}},
          hide_deadline, ("uid",))
    write("confirm_deadline_date", "Confirm an assumed date",
          "A sheet's due date the service assumed (assumed: true, its course's next seminar) is right: it becomes "
          "the real date and stops following the timetable.", {"uid": UID}, confirm_deadline_date, ("uid",),
          destructive=False)
    write("set_deadline_course", "Set a deadline's course",
          "The course of a deadline from a chat, a letter or RUZ, for one that has none or a wrong one. course: a "
          "name of the course dictionary (list_courses) or the timetable's.",
          {"uid": UID, "course": {"type": "string", "maxLength": 200}}, set_deadline_course, ("uid", "course"),
          destructive=False)
    registry.op("_assume_again", assume_again)
    registry.op("_set_course_back", set_course_back)
    write("delete_deadline", "Delete an own deadline", "Delete one of the owner's own deadlines.", {"uid": UID},
          delete_deadline, ("uid",))
    write("unhide_deadline", "Restore a hidden deadline", "Bring back a hidden deadline, if its source still has it.",
          {"uid": {"type": "string", "description": "from list_hidden_deadlines: hidden[].uid"}}, unhide_deadline,
          ("uid",), destructive=False)
    write("add_event", "Add an event", "A new own event in the timetable and the calendar feeds.",
          {**EVENT_PROPS, "required": {"type": "boolean", "description": "the main calendar (true, default) or the "
                                                                           "optional one"}},
          add_event, ("title", "start"), destructive=False)
    write("edit_event", "Change an event",
          "Change an event of a chat, the mail or the owner (not a RUZ class: correct_class). Give start and end "
          "together. A chat's event is hidden and an own copy with the change takes its place.",
          {"event_id": EVENT_ID, **EVENT_PROPS}, edit_event, ("event_id",))
    write("set_event_options", "Hide or move an event between calendars",
          "hidden: out of the feeds (true) or back (false); a hidden correction of a class is not applied. required: "
          "the main calendar (true) or the optional one (false); a class is always in the main one.",
          {"event_id": EVENT_ID, "required": {"type": "boolean"}, "hidden": {"type": "boolean"}},
          set_event_options, ("event_id",), idempotent=True)
    write("remove_event", "Remove an own event",
          "Delete an own event, take back an own change of a class (the chat's change or RUZ applies again) or take "
          "back an own copy of a chat's event (the chat's one is back). A chat's event is hidden instead, with "
          "set_event_options.", {"event_id": EVENT_ID}, remove_event, ("event_id",))
    write("correct_class", "Move, cancel or change a class",
          "The owner's change of a RUZ class: a new time, room, title, type or calendar, or cancelled. It stands "
          "over RUZ and the chats until removed with remove_event.",
          {"class_uid": {"type": "string", "description": "the class's uid, from get_timetable: items[].uid of a "
                                                          "class (source ruz)"},
           "start": MOMENT, "end": MOMENT, "room": EVENT_PROPS["room"],
           "cancelled": {"type": "boolean", "description": "false by default"},
           "title": EVENT_PROPS["title"], "kind": EVENT_PROPS["kind"], "required": {"type": "boolean"}},
          correct_class, ("class_uid", "start", "end"))
    write("update_chat_settings", "Change a chat's settings",
          "Change some settings of a chat; the others stay. Turning a chat on makes its messages read from now.",
          {"chat_id": {"type": "integer", "description": "from list_chats"}, **CHAT_PROPS}, update_chat_settings,
          ("chat_id",), idempotent=True)
    write("set_course", "Set a dictionary entry",
          "Add or change a course in the dictionary by hand: the source's name, a short English title, an "
          "abbreviation of at most 6 characters. A manual entry is never overwritten by Haiku.",
          {"name": {"type": "string", "description": "the course as the sources name it"},
           "title": {"type": "string"}, "abbr": {"type": "string", "maxLength": 6}},
          set_course, ("name", "title", "abbr"), idempotent=True)
    write("delete_course", "Delete a dictionary entry",
          "Drop a course from the dictionary; Haiku names it again when a source brings it.",
          {"name": {"type": "string"}}, delete_course, ("name",))
    registry.op("_restore_course", restore_course)
    if has[actions.SETTINGS_KEY]:
        write("update_general_settings", "Change general settings",
              "Batches every N hours, how long news lives, the language of the app (en, ru), "
              "what the student tells the models about themselves (group, subgroup, electives), and the course wiki "
              "pages whose new sheets and homework come as notices in the mailbox (the whole list, addresses).",
              {"batch_hours": {"type": "integer", "enum": list(BATCH_HOURS)},
               "news_ttl_hours": {"type": "integer", "minimum": 1, "maximum": 168},
               "language": {"type": "string", "enum": ["en", "ru"]},
               "about_me": {"type": "string", "maxLength": 500},
               "wiki_pages": {"type": "array", "items": {"type": "string"}}}, update_general_settings,
              idempotent=True)
        write("update_images", "Change images read a day",
              "How many images a day Haiku reads in chats with images on; 0 reads none.",
              {"daily": {"type": "integer", "minimum": 0, "maximum": 500}}, update_images, ("daily",),
              idempotent=True)
        write("update_notifications", "Change the web app's notifications",
              "Which kinds the web app pushes to the owner's devices (deadlines: new and moved; reminders; failures: "
              "a source failed or needs a login; llm: the daily limit spent), and the quiet hours (Moscow, "
              "HH:MM-HH:MM, '' for none) when they come without a sound.",
              {"kinds": {"type": "array", "items": {"type": "string", "enum": list(PUSH_KINDS)}},
               "quiet": {"type": "string", "description": "HH:MM-HH:MM, or ''"}}, update_notifications,
              idempotent=True)
    if has[actions.SETTINGS_KEY] and has[actions.REMINDERS_KEY]:
        write("update_reminders", "Change deadline reminders",
              "Deadline reminders pushed to the web app: on or off, the eve's time and the day's time (Moscow, HH:MM).",
              {"enabled": {"type": "boolean"}, "eve": {"type": "string", "description": "HH:MM"},
               "day": {"type": "string", "description": "HH:MM"}}, update_reminders, idempotent=True)
    if has[actions.SETTINGS_KEY] and has[actions.SHEETS_KEY]:
        write("edit_sheet", "Change a problem sheet",
              "The course, number, due date or summary of a sheet; the caption of its message follows. An empty "
              "course or a null due clears it.",
              {"sheet_id": {"type": "integer"}, "course": {"type": "string"}, "number": {"type": "string"},
               "due": {"type": ["string", "null"], "description": "YYYY-MM-DD, or null"},
               "summary": {"type": "string"}}, edit_sheet, ("sheet_id",))
        write("delete_sheet", "Delete an own sheet",
              "An own sheet (one the owner uploaded) with every version. It cannot be "
              "undone; a chat's sheet is refused.", {"sheet_id": {"type": "integer"}}, delete_sheet, ("sheet_id",))
        write("join_sheet", "Join a sheet to another as its newer version",
              "A sheet that was posted as new becomes the newer version of another sheet (sheet_id of list_sheets, "
              "the newer one first). It cannot be undone.",
              {"sheet_id": {"type": "integer", "description": "the sheet that becomes a newer version"},
               "previous": {"type": "integer", "description": "the sheet it updates"}}, join_sheet,
              ("sheet_id", "previous"), idempotent=False)
        registry.op("_remove_own_sheet", remove_own_sheet)
        write("update_pdf_limits", "Change the limits of whole PDFs",
              "For chats with pdf_content: how many pages and tokens of one PDF are read, whichever comes first.",
              {"pdf_pages": {"type": "integer", "minimum": 1, "maximum": 50},
               "pdf_tokens": {"type": "integer", "minimum": 1000, "maximum": 200000}}, update_pdf_limits,
              idempotent=True)
    if has[actions.GRADES_KEY]:
        write("update_grade_settings", "Change grade settings",
              "student: the name the own row is found by in every gradebook (changing it reads them all again from "
              "scratch); window_days: how long a grade counts as new.",
              {"student": {"type": "string"}, "window_days": {"type": "integer", "minimum": 1, "maximum": 60}},
              update_grade_settings)
        write("add_gradebook", "Add a gradebook",
              "A teacher's Google Sheet open to anyone with the link; label: the course's short name in front of its "
              "grades (empty: the start of the sheet's title); name: the owner's name for it.",
              {"url": {"type": "string", "description": "https://docs.google.com/spreadsheets/d/…"},
               "label": {"type": "string"}, "name": {"type": "string"}}, add_gradebook, ("url",), destructive=False)
        write("remove_gradebook", "Remove a gradebook", "Stop reading a gradebook; its grades are forgotten.",
              {"sheet_id": GRADEBOOK}, remove_gradebook, ("sheet_id",))
        write("update_gradebook", "Rename a gradebook", "The owner's name of a gradebook, or its course label.",
              {"sheet_id": GRADEBOOK, "name": {"type": "string"}, "label": {"type": "string"}}, update_gradebook,
              ("sheet_id",), idempotent=True)
        write("read_gradebook_now", "Read the gradebooks now", "Read the gradebooks at once instead of in 30 minutes.",
              {"sheet_id": GRADEBOOK}, read_gradebook_now, ("sheet_id",), destructive=False, idempotent=True)
        write("set_tab_layout", "Lay out a gradebook tab",
              "The owner's layout of a tab, over Haiku's: skip it, or which column of the own row is which work. "
              "get_gradebook shows the columns.",
              {"sheet_id": GRADEBOOK, "tab": TAB, "skip": {"type": "boolean"},
               "works": {"type": "array", "items": {
                   "type": "object", "required": ["column", "label", "max", "bonus_column"],
                   "additionalProperties": False, "properties": {
                       "column": {"type": "string", "description": "a column letter of the own row"},
                       "label": {"type": "string", "description": "the work: HW 1, Test 2"},
                       "max": {"type": "number", "description": "points out of, above 0"},
                       "bonus_column": {"type": ["string", "null"]}}}}},
              set_tab_layout, ("sheet_id", "tab", "skip", "works"), idempotent=True)
        write("reset_tab_layout", "Give a tab back to Haiku", "Drop the owner's layout of a tab.",
              {"sheet_id": GRADEBOOK, "tab": TAB}, reset_tab_layout, ("sheet_id", "tab"))
        write("add_grade", "Add a grade by hand", "A grade the gradebooks do not have.",
              {"course": {"type": "string"}, "label": {"type": "string", "maxLength": actions.GRADE_LABEL_LIMIT},
               "value": GRADE_VALUE}, add_grade, ("course", "label", "value"), destructive=False)
        write("change_grade", "Change a grade",
              "Change a grade added by hand, or correct a gradebook's grade (the correction holds until the sheet "
              "changes the grade itself).",
              {"grade_id": {"type": "string", "description": "from list_grades: manual[].id or sheets[].grades[].id"},
               "value": GRADE_VALUE, "label": {"type": "string", "maxLength": actions.GRADE_LABEL_LIMIT},
               "course": {"type": "string"}}, change_grade, ("grade_id", "value"), idempotent=True)
        write("delete_grade", "Delete or reset a grade",
              "Delete a grade added by hand, or bring a corrected gradebook grade back to the sheet's value.",
              {"grade_id": {"type": "string"}}, delete_grade, ("grade_id",))
