"""The «Sheets» topic, «Листочки» in Russian (§6.3, §8.3, р. 14): every sheet as its file, with a caption.

The caption: the course (its English title) and the number, the due date, the message it came from with a link, the
course's hashtag (#Calc), and with `pdf_content` the model's summary. A newer version replaces the file and the
caption in the first version's message (the post stays where it is, with its replies) and answers it «Sheet
updated», with what changed when the chat's PDFs are read. If that message is gone, the version is posted anew. The
captions are in the forum's language (р. 41), and so are the summary and the changes the model wrote for them.

The sheets are posted after every batch run, oldest first; one that fails stays waiting for the next run. The owner's
joining of two sheets in the Mini App goes through here too, under the same lock: the newer sheet's own post is
deleted, and the older one's message takes its file.
"""

import asyncio
import datetime as dt
import html
import logging
import re
from collections.abc import Callable

from .. import clock
from ..collectors.inbox import Inbox
from ..processing.sheets import Sheets
from ..store import Store
from .feed import day, remember, source
from .forum import Forum

log = logging.getLogger(__name__)

TOPIC = "sheets"
CAPTION_LIMIT = 1024  # characters of a caption; the HTML is counted whole, so the visible text always fits


def hashtag(abbr: str | None) -> str:
    """'Calc' -> '#Calc'; a tag holds letters, digits and _ only."""
    tag = re.sub(r"\W+", "_", abbr or "").strip("_")
    return f"#{tag}" if tag and not tag.isdigit() else ""


class SheetTopic:
    def __init__(self, forum: Forum, store: Store, inbox: Inbox, sheets: Sheets,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.db, self.inbox, self.sheets, self.now = forum, store.db, inbox, sheets, now
        self.files = sheets.files
        self._lock = asyncio.Lock()

    async def publish(self) -> None:
        """After every batch run: the sheets waiting for the forum."""
        async with self._lock:
            for waiting in self.sheets.waiting():
                try:
                    sheet = await self.sheets.prepare(waiting["id"])
                    if sheet is not None:
                        await self._post(sheet)
                except Exception:
                    log.exception("posting sheet %d failed, it waits for the next run", waiting["id"])

    async def join(self, sheet_id: int, previous_id: int) -> None:
        """The Mini App: a sheet posted as new is a newer version of another. ValueError when it cannot be."""
        async with self._lock:
            sheet, own_post = await self.sheets.join(sheet_id, previous_id)
            await self._post(sheet)
            if own_post is not None and own_post != sheet["post_id"]:
                try:
                    await self.forum.delete([own_post])
                except Exception as e:
                    log.warning("cannot delete the post of sheet %d: %s", sheet_id, e)
                with self.db:
                    self.db.execute("delete from bot_posts where kind = 'sheet' and msg_id = ?", (own_post,))

    # --- posting ------------------------------------------------------------------------------

    async def _post(self, sheet: dict) -> None:
        """A prepared sheet to the forum: a message of its own, or the first version's message edited."""
        path = self.files.by_hash(sheet["sha256"])
        if path is None:
            self.sheets.failed(sheet["id"], self.sheets.file_gone())
            return
        first = self.sheets.first(sheet)
        caption = self.caption(sheet, first)
        if sheet["previous"] is not None and first["post_id"] is not None and \
                await self.forum.edit_file(first["post_id"], path, sheet["name"], caption):
            self.sheets.posted(sheet["id"], first["post_id"])
            reply = await self.forum.post(TOPIC, self._updated(sheet), html=True, reply_to=first["post_id"])
            remember(self.db, TOPIC, reply, "sheet_update", sheet["id"], self.now())
            return
        msg_id = await self.forum.post_file(TOPIC, path, sheet["name"], caption)
        self.sheets.posted(sheet["id"], msg_id)
        remember(self.db, TOPIC, msg_id, "sheet", sheet["id"], self.now())
        if sheet["previous"] is not None:  # the first version's message is gone: this post carries the sheet now
            with self.db:
                self.db.execute("update sheets set post_id = ? where id in (select id from sheets where post_id = ?)",
                                (msg_id, first["post_id"]))

    def caption(self, sheet: dict, first: dict) -> str:
        subjects, t = self.sheets.subjects, self.forum.tr
        what = html.escape(subjects.title(sheet["course"])) if sheet["course"] else t("Sheet", "Листок")
        if sheet["number"]:
            what += t(" · sheet ", " · листок ") + html.escape(sheet["number"])
        version = t(f" (version {sheet['version']})", f" (версия {sheet['version']})")
        lines = [f"📄 <b>{what}</b>" + (version if sheet["version"] > 1 else "")]
        if sheet["due"]:
            lines.append(t("Due: ", "Срок: ") + day(sheet["due"], self.forum.lang))
        tag = hashtag(subjects.abbr(sheet["course"]))
        lines.append(" · ".join(part for part in (self._source(sheet), tag) if part))
        text = "\n".join(lines)
        if summary := first["summary"]:
            room = CAPTION_LIMIT - len(text) - 2
            summary = html.escape(summary)
            if len(summary) > room:
                summary = summary[:max(0, room - 1)].rsplit(" ", 1)[0] + "…"
            text += f"\n\n{summary}"
        return text

    def _updated(self, sheet: dict) -> str:
        name = html.escape(sheet["name"])
        lines = ["🔄 " + self.forum.tr(f"Sheet updated: version {sheet['version']}, file «{name}»",
                                       f"Листок обновлён: версия {sheet['version']}, файл «{name}»")]
        if sheet["changes"]:
            lines.append(html.escape(sheet["changes"]))
        lines.append(self._source(sheet))
        return "\n".join(line for line in lines if line)

    def _source(self, sheet: dict) -> str:
        return source(self.inbox, {"source": "tg", "chat_id": sheet["chat_id"], "msg_ids": [sheet["msg_id"]]},
                      self.forum.lang)
