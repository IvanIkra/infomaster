"""The «Дедлайны» topic (§6.4, §8.3, р. 24): the merged list pinned, and a message for every new or moved deadline.

- The pinned list: every deadline the merge shows (processing/merge.py: the list Tablo and HA get), in its order, with
  its source's icon, the title, when it is due (сегодня, завтра, просрочен, else the weekday and the date) and the
  course as the source names it. The bot edits it in place, several messages in a row when it is long.
- A message for every deadline that joins the list or moves there, from any source. The list is compared with the
  one shown last (kv "deadlines.shown") by the row each deadline comes from (`Deadline.ref`), so a rename (a Midterm
  once the dictionary knows its course, an HSE App X label once Haiku answers) only edits the list, and a move of the
  chats or RUZ (a new row that moves the old one) is a move. A deadline its source withdrew (its message deleted or
  edited, its class gone, gone from HSE App X) gets «снят»; one that leaves the list otherwise (hidden, done in HSE
  App X, past) leaves quietly, and a copy of the same work and date taking the place of another is no news.
- Quiet: the first refresh (nothing shown yet) takes the list as it is, and so do the first poll of HSE App X and the
  first RUZ poll for what they bring, so a source joining floods nothing. The owner's own changes (HA, the Mini App,
  the buttons) only edit the list too.
- Under the message of a new or moved deadline «Скрыть» hides it exactly as HA's to-do lists do (an own deadline,
  «Удалить», is deleted); a hidden one gets «Вернуть». Only the owner may press them.

Several tasks change deadlines (the batches, the RUZ and HSE App X polls, the owner): one refresh runs at a time under
a lock and compares the list as it is then, so nothing is told twice. The marks move on at midnight.
"""

import asyncio
import datetime as dt
import html
import logging
import re
from collections.abc import Callable, Iterable

from telethon import Button

from .. import clock
from ..collectors.inbox import Inbox
from ..processing.merge import DeadlineList, same
from ..processing.todo import Deadline
from ..store import Store
from .feed import MESSAGE_LIMIT, Board, day, remember, source, split
from .forum import Forum

log = logging.getLogger(__name__)

TOPIC = "deadlines"
SHOWN_KEY = "deadlines.shown"   # the list as the forum showed it last: [{ref, title, due, source, course}]
BUTTON = re.compile(rb"^dl:(hide|show)$")  # the deadline is the post's: bot_posts.key by the message id
ICONS = {"hseapp": "🎓", "ical": "📅", "mail": "✉️", "tg": "💬", "ha": "🏠"}
WHY = {"ical": "пары больше нет в расписании", "hseapp": "его больше нет в HSE App X"}
WHY_MESSAGE = "сообщение удалено или исправлено"
MIDNIGHT_DELAY = 5              # seconds past midnight: today and tomorrow move on


def due_mark(due: dt.date, today: dt.date) -> str:
    days = (due - today).days
    if days < 0:
        return f"просрочен ({day(due.isoformat())})"
    if days == 0:
        return "сегодня"
    if days == 1:
        return "завтра"
    return day(due.isoformat())


def entry(d: Deadline) -> dict:
    return {"ref": d.ref, "title": d.title, "due": d.due.isoformat(), "source": d.source, "course": d.course}


def from_entry(e: dict) -> Deadline:
    return Deadline(e["title"], dt.date.fromisoformat(e["due"]), "study", e["source"], e["course"], ref=e["ref"])


def hide_button(d: Deadline) -> list[list]:
    return [[Button.inline("Удалить" if d.source == "ha" else "Скрыть", b"dl:hide")]]


class DeadlineTopic:
    def __init__(self, forum: Forum, store: Store, inbox: Inbox, merged: DeadlineList, owner_id: int,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.store, self.db, self.inbox = forum, store, store.db, inbox
        self.merged, self.owner_id, self.now = merged, owner_id, now
        self._lock = asyncio.Lock()
        self._quiet: set[str] = set()

    @property
    def pinned(self) -> Board:
        return Board(self.forum, self.db, TOPIC, "deadline_board", self.now)

    # --- what changes the list ------------------------------------------------------------

    def quiet(self, refs: Iterable[str]) -> None:
        """Deadlines a source brought that are no news: the next refresh takes them into the list without a message.
        Call it right after they are saved, before any await, so no refresh sees them first."""
        self._quiet |= set(refs)

    def changed(self, quiet: set[str]) -> None:
        """DeadlineList.on_change: the owner's change shows in the list, quietly."""
        self.quiet(quiet)
        self.forum.spawn(self.refresh())

    async def run(self) -> None:
        """At the start (the very first one takes the list as it is), then every midnight: the marks move on, past
        deadlines leave, own and hidden rows long past are removed."""
        while True:
            try:
                self.merged.prune()
                await self.refresh()
            except Exception:
                log.exception("refreshing the deadlines failed")
            await asyncio.sleep(self._seconds_to_midnight())

    async def refresh(self) -> None:
        """Compare the list with the one shown last, post what is news, bring the pinned list up to date."""
        async with self._lock:
            shown = self.merged.visible()
            before = self.store.get_json(SHOWN_KEY)
            quiet = set(self._quiet)  # what came before this comparison; later ones wait for the next refresh
            self._quiet -= quiet
            self.store.put_json(SHOWN_KEY, [entry(d) for d in shown])  # first: a failing post is not told twice
            if before is not None:
                for text, d, ref in self.messages(before, shown, quiet):
                    try:
                        await self._post(text, d, ref)
                    except Exception:
                        log.exception("posting a deadline failed")
            await self.pinned.sync(self.board(shown))

    # --- the pinned list ---------------------------------------------------------------------

    def board(self, shown: list[Deadline]) -> list[str]:
        today = clock.local_day(self.now())
        lines = []
        for d in shown:
            line = f"{ICONS.get(d.source, '•')} <b>{html.escape(d.title)}</b> — {due_mark(d.due, today)}"
            if d.name:
                line += f" · {html.escape(d.name)}"
            lines.append(line)
        return split("<b>Дедлайны</b>\n", lines or ["Дедлайнов нет."], MESSAGE_LIMIT)

    # --- the messages --------------------------------------------------------------------------

    def messages(self, before: list[dict], shown: list[Deadline],
                 quiet: set[str]) -> list[tuple[str, Deadline | None, str]]:
        """(text, the deadline for the button or None, its ref) of every change worth a message, in list order."""
        refs = {d.ref for d in shown}
        was = {e["ref"]: e for e in before if e["ref"] in refs}  # a shown deadline -> as it was shown last
        lost = [e for e in before if e["ref"] not in refs]
        for e in list(lost):  # a move of the chats or RUZ: the old row points at the new one
            newer = self._newer(e["ref"])
            if newer in refs and newer not in was:
                was[newer] = e
                lost.remove(e)
        joined = [d for d in shown if d.ref not in was]
        for d in list(joined):  # the same work on the same date in another row takes the place: no news
            e = next((e for e in lost if same(d, from_entry(e))), None)
            if e is not None:
                joined.remove(d)
                lost.remove(e)

        result = []
        new = {d.ref for d in joined}
        for d in shown:
            if d.ref in quiet:
                continue
            if d.ref in new:
                result.append((self._new(d), d, d.ref))
            elif d.ref in was and was[d.ref]["due"] != d.due.isoformat():  # the same date: a rename, no news
                result.append((self._moved(was[d.ref], d), d, d.ref))
        today = clock.local_day(self.now()).isoformat()
        for e in lost:
            if e["ref"] not in quiet and e["due"] >= today and (why := self._withdrawn(e)):
                result.append((self._gone(e, why), None, e["ref"]))
        return result

    def _newer(self, ref: str) -> str | None:
        """The ref of the row that moved a row of the chats, mail or RUZ, or None."""
        if not ref.startswith("dl:"):
            return None
        row = self.merged.deadlines.latest(int(ref[3:]))
        return f"dl:{row['id']}" if row is not None and f"dl:{row['id']}" != ref else None

    def _withdrawn(self, e: dict) -> str | None:
        """Why the source withdrew a deadline; None when it left the list for another reason."""
        ref = e["ref"]
        if ref.startswith("dl:"):
            row = self.merged.deadlines.get(int(ref[3:]))
            if row is not None and row["gone_at"] is not None and row["moved_by"] is None:
                return WHY.get(row["source"], WHY_MESSAGE)
        elif ref.startswith("hseapp:"):
            item_id = ref.removeprefix("hseapp:")
            if all(item["id"] != item_id for item in self.merged.hseapp.current() + self.merged.hseapp.done()):
                return WHY["hseapp"]
        return None

    def _new(self, d: Deadline) -> str:
        lines = [f"🆕 <b>{html.escape(d.title)}</b> — {day(d.due.isoformat())}"]
        if d.name:
            lines.append(html.escape(d.name))
        if d.source == "ical":
            lines.append("Рубежный контроль в РУЗ")
        return "\n".join(lines + self._origin(d.ref, new=True))

    def _moved(self, e: dict, d: Deadline) -> str:
        lines = [f"↪️ <b>{html.escape(d.title)}</b> перенесён: {day(e['due'])} → {day(d.due.isoformat())}"]
        return "\n".join(lines + self._origin(d.ref))

    def _gone(self, e: dict, why: str) -> str:
        return "\n".join([f"✖️ <b>{html.escape(e['title'])}</b> ({day(e['due'])}) снят: {why}"]
                         + self._origin(e["ref"]))

    def _origin(self, ref: str, new: bool = False) -> list[str]:
        """Where the deadline came from: the message of a chat or the subject of a letter, HSE App X with the Russian
        title."""
        if ref.startswith("dl:"):
            row = self.merged.deadlines.get(int(ref[3:]))
            if row is not None and row["chat_id"] is not None:
                return [source(self.inbox, row)]
        elif ref.startswith("hseapp:"):
            item_id = ref.removeprefix("hseapp:")
            item = next((item for item in self.merged.hseapp.current() if item["id"] == item_id), None)
            if new and item is not None and item["title"] != item["label"]:
                return [f"HSE App X: «{html.escape(item['title'])}»"]
            return ["HSE App X"]
        elif new:
            return ["Свой дедлайн"]
        return []

    async def _post(self, text: str, d: Deadline | None, ref: str) -> None:
        msg_id = await self.forum.post(TOPIC, text, buttons=hide_button(d) if d is not None else None, html=True)
        row_id = int(ref[3:]) if ref.startswith("dl:") else None
        remember(self.db, TOPIC, msg_id, "deadline", row_id, self.now(), key=ref if d is not None else None)

    # --- the buttons -------------------------------------------------------------------------

    async def on_button(self, event) -> None:
        """«Скрыть», «Удалить» or «Вернуть» under a deadline's message (Telethon's CallbackQuery)."""
        if event.sender_id != self.owner_id:
            await event.answer("Нет доступа", alert=True)
            return
        action = event.pattern_match.group(1).decode()
        post = self.db.execute("select key from bot_posts where topic = ? and kind = 'deadline' and msg_id = ?",
                               (TOPIC, event.message_id)).fetchone()
        d = self.merged.by_ref(post["key"]) if post is not None and post["key"] else None
        if d is None:
            await event.answer("Этого дедлайна больше нет в списке", alert=True)
            await event.edit(buttons=None)
            return
        if action == "show":
            self.merged.restore(d)
            await event.answer("Снова в списке")
            await event.edit(buttons=hide_button(d))
            return
        if d.source == "ha":
            self.merged.remove(d)
            await event.answer("Удалён")
            await event.edit(buttons=None)
            return
        if self.merged.is_hidden(d):
            await event.answer("Уже скрыт")
        else:
            self.merged.remove(d)
            await event.answer("Скрыт: в списке, в Tablo и в HA его больше нет")
        await event.edit(buttons=[[Button.inline("Вернуть", b"dl:show")]])

    def _seconds_to_midnight(self) -> float:
        local = self.now().astimezone(clock.TZ)
        midnight = (local + dt.timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0)
        return (midnight - local).total_seconds() + MIDNIGHT_DELAY
