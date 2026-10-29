"""The «Новости» topic (§8.3, D23, D24), and the pinned lists the forum keeps edited in place.

«Новости»: a pinned list of every live news item, `full`, in rank order, with its source: the chat with a link to the
message, or for a letter its subject (letters have no link, §8.3). The bot rebuilds it after each batch run and edits
it in place; a list longer than one Telegram message takes several in a row. A new item with importance 4 or 5 also
gets a short message of its own, which the next rebuild deletes. Under it «Скрыть» hides the item for good (only the
owner may press it): it leaves the list at once, and the API later.

The deadlines have their own topic (outputs/deadlines.py), built from the merged list.

The list is read from `bot_posts`, brought up to date in Telegram and only then written back, so its rebuild and the
alerts it drops run under a lock: the batches and the owner's buttons rebuild it. What the bot posted is kept in
`bot_posts`.
"""

import asyncio
import datetime as dt
import hashlib
import html
import logging
import re
from collections.abc import Callable

from telethon import Button

from .. import clock
from ..collectors.inbox import Inbox, message_link
from ..i18n import tr
from ..processing.batches import Changes
from ..processing.news import News
from ..settings import Settings
from ..store import Store
from .forum import Forum

log = logging.getLogger(__name__)

MESSAGE_LIMIT = 4096   # characters; the HTML is counted whole, so the visible text always fits
ALERT_IMPORTANCE = 4
ICONS = {"channel": "📢", "user": "👤", "bot": "👤"}  # chats and groups: 💬
MAIL_ICON = "✉️"
WEEKDAYS = {"en": ("Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"), "ru": ("пн", "вт", "ср", "чт", "пт", "сб", "вс")}
BUTTON = re.compile(rb"^nw:hide:(\d+)$")


def digest(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()[:16]


def split(header: str, lines: list[str], limit: int) -> list[str]:
    """Lines into as few messages as fit, the header on top of the first."""
    chunks, chunk = [], header
    for line in lines:
        if len(chunk) + 1 + len(line) > limit:
            chunks.append(chunk)
            chunk = line
        else:
            chunk = f"{chunk}\n{line}"
    return [*chunks, chunk]


def day(due: str, lang: str) -> str:
    """'2026-10-22' -> 'Thu 22.10' (the date is day.month in both languages)."""
    date = dt.date.fromisoformat(due)
    return f"{WEEKDAYS[lang][date.weekday()]} {date:%d.%m}"


def source(inbox: Inbox, row: dict, lang: str) -> str:
    """Where a news item, deadline or event (`row`: source, chat_id, msg_ids) came from: the chat's name with a link
    to the message where the platform has one; for a letter its subject (§8.3), while the letter is kept."""
    if row["source"] == "mail":
        subject = inbox.subject(row["msg_ids"])
        letter = tr(lang, "letter", "письмо")
        return f"{letter} «{html.escape(subject)}»" if subject else letter
    chat = inbox.chat(row["chat_id"]) if row["chat_id"] is not None else None
    if chat is None:
        return "?"
    title = html.escape(chat["title"])
    link = message_link(chat, row["msg_ids"][0]) if row["msg_ids"] else None
    return f'<a href="{html.escape(link)}">{title}</a>' if link else title


def icon(inbox: Inbox, row: dict) -> str:
    """The source's icon in the news list: a letter, a channel, a person, else a chat."""
    if row["source"] == "mail":
        return MAIL_ICON
    return ICONS.get((inbox.chat(row["chat_id"]) or {}).get("kind"), "💬")


def remember(db, topic: str, msg_id: int, kind: str, ref: int | None, at: dt.datetime,
             text_digest: str | None = None, key: str | None = None) -> None:
    """A post of the bot in `bot_posts` (§8.3)."""
    with db:
        db.execute("insert into bot_posts(topic, msg_id, kind, ref, key, digest, at) values(?, ?, ?, ?, ?, ?, ?)",
                   (topic, msg_id, kind, ref, key, text_digest, clock.local_iso(at)))


def posts(db, kind: str) -> list[dict]:
    return [dict(row) for row in db.execute("select * from bot_posts where kind = ? order by id", (kind,))]


async def forget(forum: Forum, db, rows: list[dict]) -> None:
    """Delete the messages and their rows; a message deleted already is no reason to stop."""
    if not rows:
        return
    try:
        await forum.delete([row["msg_id"] for row in rows])
    except Exception as e:
        log.warning("cannot delete bot messages %s: %s", [row["msg_id"] for row in rows], e)
    with db:
        db.executemany("delete from bot_posts where id = ?", [(row["id"],) for row in rows])


class Board:
    """A list pinned in a topic and edited in place, several messages in a row when it is long. The caller holds
    its own lock: the messages are read from `bot_posts`, brought up to date and only then written back."""

    def __init__(self, forum: Forum, db, topic: str, kind: str, now: Callable[[], dt.datetime]):
        self.forum, self.db, self.topic, self.kind, self.now = forum, db, topic, kind, now

    async def sync(self, chunks: list[str]) -> bool:
        """Bring the list to `chunks`; True if anything in it changed."""
        rows = posts(self.db, self.kind)
        if [row["digest"] for row in rows] == [digest(chunk) for chunk in chunks]:
            return False
        for row, chunk in zip(rows, chunks, strict=False):
            if row["digest"] == digest(chunk):
                continue
            if not await self.forum.edit(row["msg_id"], chunk, html=True):
                # A message of the list is gone (deleted by hand or with its topic): the list starts anew.
                await forget(self.forum, self.db, rows)
                await self._post(chunks, 0)
                return True
            with self.db:
                self.db.execute("update bot_posts set digest = ? where id = ?", (digest(chunk), row["id"]))
        await self._post(chunks, len(rows))
        await forget(self.forum, self.db, rows[len(chunks):])
        return True

    async def _post(self, chunks: list[str], start: int) -> None:
        for i in range(start, len(chunks)):
            msg_id = await self.forum.post(self.topic, chunks[i], html=True)
            remember(self.db, self.topic, msg_id, self.kind, None, self.now(), digest(chunks[i]))
            if i == 0:
                await self.forum.pin(msg_id)


class Feed:
    def __init__(self, forum: Forum, store: Store, inbox: Inbox, news: News, settings: Settings, owner_id: int,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.db, self.inbox = forum, store.db, inbox
        self.news, self.settings, self.owner_id, self.now = news, settings, owner_id, now
        self._lock = asyncio.Lock()  # one rebuild of the list at a time, with its alerts

    @property
    def pinned(self) -> Board:
        return Board(self.forum, self.db, "news", "news_board", self.now)

    async def publish(self, changes: Changes) -> None:
        """Batches.publish: called after every run, so the list also drops items that are no longer live."""
        async with self._lock:
            await self._rebuild()
            for news_id in changes.news:
                item = self.news.get(news_id)
                if item and item["replaced_by"] is None and item["gone_at"] is None and not item["hidden"] \
                        and item["importance"] >= ALERT_IMPORTANCE:
                    text = f"🔔 {html.escape(item['full'])}\n{source(self.inbox, item, self.forum.lang)}"
                    hide = [[Button.inline(self.forum.tr("Hide", "Скрыть"), f"nw:hide:{news_id}".encode())]]
                    self._remember(await self.forum.post("news", text, buttons=hide, html=True), "news_alert",
                                   news_id)

    # --- the news list ------------------------------------------------------------------

    def board(self) -> list[str]:
        """The messages of the list as they should be now."""
        lines, lang = [], self.forum.lang
        for item in self.news.live(self.now(), self.settings.get("news_ttl_hours")):
            text = html.escape(item["full"])
            if item["importance"] >= 5:
                text = f"<b>{text}</b>"
            lines.append(f"{icon(self.inbox, item)} {text} · {source(self.inbox, item, lang)}")
        return split(tr(lang, "<b>News</b>\n", "<b>Новости</b>\n"),
                     lines or [tr(lang, "No live news.", "Живых новостей нет.")], MESSAGE_LIMIT)

    async def _rebuild(self) -> None:
        """The list as it should be; a change deletes the alerts, whose items are on it already. Under the lock."""
        if await self.pinned.sync(self.board()):
            await forget(self.forum, self.db, posts(self.db, "news_alert"))

    # --- the button under an alert --------------------------------------------------------

    async def on_button(self, event) -> None:
        """«Скрыть» under an alert (Telethon's CallbackQuery): the item leaves the list for good."""
        if event.sender_id != self.owner_id:
            await event.answer(self.forum.tr("No access", "Нет доступа"), alert=True)
            return
        news_id = int(event.pattern_match.group(1))
        item = self.news.get(news_id)
        if item is None or item["replaced_by"] is not None or item["gone_at"] is not None:
            await event.answer(self.forum.tr("This news is gone: another one replaced it or the message was deleted",
                                             "Этой новости больше нет: её заменила другая или сообщение удалено"),
                               alert=True)
            await event.edit(buttons=None)
            return
        async with self._lock:  # hidden under the lock: a rebuild running now must not see it half done
            with self.db:
                self.news.hide(news_id)
            # Only this alert goes: the others stay until a rebuild of the batches, as they would without the hiding.
            await self.pinned.sync(self.board())
            await forget(self.forum, self.db, [row for row in posts(self.db, "news_alert") if row["ref"] == news_id])
        await event.answer(self.forum.tr("Hidden: it is off the news list", "Скрыто: в списке новостей её больше нет"))

    # --- helpers --------------------------------------------------------------------------

    def _remember(self, msg_id: int, kind: str, ref: int | None) -> None:
        remember(self.db, "news", msg_id, kind, ref, self.now())
