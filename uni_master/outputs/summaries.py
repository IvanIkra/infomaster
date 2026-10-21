"""The «Сводки» topic (§6.6, §8.3, р. 17, р. 20): the daily summary and /summary.

Daily: at `summary_time` (Moscow, 21:00 by default) every enabled chat with `summary` on gets a summary of what it said
since its last one, a message of its own in the topic. Chats where nothing of substance was said share one line. The
daily summary is an optional LLM task: above 80 % of the limit it is not made, and its messages wait for the next one
(as after a failed call; an answer the model cannot give for them is not retried). A slot missed while the service was
down is made up at the start; the very first start only remembers the slot.

/summary [chat] [period]: from the owner, in the chat with the bot or in the forum. The chat is a part of its name, or
the chat of a forwarded message the command answers; without one the bot shows the enabled chats as buttons. Without a
period the summary runs since the chat's last one. It goes to the topic like the daily one, and the bot's answer to
the command links to it. It is no optional task: it runs above 80 % of the limit, but a summary too big for what is
left is refused alone and stops no other call.
"""

import asyncio
import datetime as dt
import html
import logging
import re
from collections.abc import Callable

from telethon import Button, utils
from telethon.tl import types

from .. import clock
from ..collectors.inbox import Inbox, message_link
from ..llm.budget import BudgetExceeded, DoesNotFit
from ..llm.client import BadAnswer, LLMError
from ..processing.summaries import Collected, Period, PeriodError, Summaries, parse_period, span
from ..settings import ChatSettings, Settings
from ..store import Store
from .feed import MESSAGE_LIMIT, split
from .forum import Forum

log = logging.getLogger(__name__)

TOPIC = "summaries"
COMMAND = re.compile(r"^/summary(?:@(?P<bot>\w+))?(?:\s+(?P<args>.*))?$", re.DOTALL | re.IGNORECASE)
BUTTON = re.compile(rb"^sm:(-?\d+):([0-9hd.\-]*)$")
DONE = "summaries.done"   # the date of the last daily slot made
SLOT_DELAY = 5            # seconds past the summary time
CHAT_BUTTONS = 30
BUTTON_TITLE = 40         # characters of a chat's name on its button
QUOTE_PREFIX = 100        # characters of a forwarded text compared with the stored one
LIMIT_SPENT = ("Ежедневная сводка не собрана: лимит LLM. Сообщения войдут в следующую; по команде /summary сводка "
               "собирается, пока лимит не исчерпан целиком.")


def header(collected: Collected) -> str:
    return f"<b>{html.escape(collected.chat['title'])}</b> · {span(collected)}"


def point_line(collected: Collected, point: dict) -> str:
    link = message_link(collected.chat, point["msg_id"]) if point["msg_id"] else None
    return f"• {html.escape(point['text'])}" + (f' <a href="{html.escape(link)}">→</a>' if link else "")


def forwarded_chat(inbox: Inbox, message) -> int | None:
    """The stored chat a forwarded message comes from: a channel (or an anonymous admin of a group) is in the forward
    itself; for a person's message it is found by the time it was sent, and its text when two chats wrote then."""
    forward = getattr(message, "fwd_from", None)
    if forward is None:
        return None
    if isinstance(forward.from_id, types.PeerChannel):
        chat_id = utils.get_peer_id(forward.from_id)
        if inbox.chat(chat_id) is not None:
            return chat_id
    candidates = inbox.sent_at(forward.date)
    text = " ".join((message.message or "").split())[:QUOTE_PREFIX]
    chats = {c["chat_id"] for c in candidates if " ".join(c["text"].split()).startswith(text)} \
        or {c["chat_id"] for c in candidates}
    return chats.pop() if len(chats) == 1 else None


def split_args(args: str, now: dt.datetime) -> tuple[str, Period | None]:
    """'Матан 20.10 - 22.10' -> ('Матан', the period): the period is the end of the arguments, if any."""
    words = args.split()
    for take in (3, 2, 1):
        if len(words) >= take and (period := parse_period(" ".join(words[-take:]), now)) is not None:
            return " ".join(words[:-take]), period
    return " ".join(words), None


class SummaryTopic:
    def __init__(self, forum: Forum, store: Store, inbox: Inbox, chat_settings: ChatSettings, summaries: Summaries,
                 settings: Settings, owner_id: int, forum_id: int, now: Callable[[], dt.datetime] = clock.now_utc):
        self.forum, self.store, self.inbox, self.chat_settings = forum, store, inbox, chat_settings
        self.summaries, self.settings, self.owner_id, self.forum_id, self.now = \
            summaries, settings, owner_id, forum_id, now
        self.ok_at: str | None = None
        self.error: str | None = None
        self._woken = asyncio.Event()

    def status(self) -> dict:
        return {"ok_at": self.ok_at, "error": self.error, "done": self.store.get_json(DONE),
                "time": self.settings.get("summary_time")}

    # --- the daily summary ----------------------------------------------------------------

    def reschedule(self) -> None:
        """The summary time changed: the loop counts its sleep anew."""
        self._woken.set()

    async def run(self) -> None:
        if self.store.get_json(DONE) is None:
            self.store.put_json(DONE, self._slot().date().isoformat())
        while True:
            self._woken.clear()
            slot = self._slot().date().isoformat()
            if slot > self.store.get_json(DONE):
                try:
                    await self.daily()
                except Exception:
                    log.exception("the daily summary failed")
                self.store.put_json(DONE, slot)
            try:
                await asyncio.wait_for(self._woken.wait(), self._seconds_to_slot())
            except TimeoutError:
                pass

    async def daily(self) -> None:
        quiet, failed, spent = [], [], False
        for chat_id in self.summaries.daily_chats():
            collected = self.summaries.collect(chat_id, None)
            if collected is None or not collected.seen:
                continue
            title = html.escape(collected.chat["title"])
            try:
                points = await self.summaries.summarize(collected, asked=False)
            except BudgetExceeded as e:  # the rest would be refused too
                log.info("the daily summary waits for the LLM limit: %s", e)
                spent = True
                break
            except BadAnswer as e:  # the same messages would fail again: they are left out
                failed.append(f"«{title}»: {html.escape(str(e))}, её сообщения пропущены")
                self.summaries.mark(collected)
                continue
            except LLMError as e:
                failed.append(f"«{title}»: {html.escape(str(e))}, сообщения войдут в следующую")
                continue
            if points:
                try:
                    await self.publish(collected)
                except Exception as e:  # the next chats still get theirs; this one's messages wait
                    log.exception("posting the summary of %s failed", chat_id)
                    failed.append(f"«{title}»: не отправлена ({html.escape(str(e) or type(e).__name__)}), "
                                  "сообщения войдут в следующую")
                    continue
            else:
                quiet.append(f"«{title}»")
            self.summaries.mark(collected)
        if quiet:
            await self.forum.post(TOPIC, "Ничего важного: " + ", ".join(quiet) + ".", html=True)
        if failed:
            await self.forum.post(TOPIC, "Сводка не собрана:\n" + "\n".join(failed), html=True)
        if spent:
            await self.forum.post(TOPIC, LIMIT_SPENT, html=True)
        self.ok_at = clock.local_iso(self.now())
        self.error = "лимит LLM" if spent else "; ".join(failed) or None

    def _slot(self) -> dt.datetime:
        """The latest summary time that has come, Moscow local."""
        local = self.now().astimezone(clock.TZ).replace(tzinfo=None)
        hour, minute = map(int, self.settings.get("summary_time").split(":"))
        today = local.replace(hour=hour, minute=minute, second=0, microsecond=0)
        return today if local >= today else today - dt.timedelta(days=1)

    def _seconds_to_slot(self) -> float:
        local = self.now().astimezone(clock.TZ).replace(tzinfo=None)
        return (self._slot() + dt.timedelta(days=1) - local).total_seconds() + SLOT_DELAY

    async def publish(self, collected: Collected) -> int:
        """Post a summary to the topic, in several messages when long; the id of the first."""
        first = None
        for text in split(header(collected), [point_line(collected, p) for p in collected.points], MESSAGE_LIMIT):
            msg_id = await self.forum.post(TOPIC, text, html=True)
            first = first or msg_id
        return first

    # --- /summary -------------------------------------------------------------------------

    async def on_command(self, event) -> None:
        """Forum.on_command: the owner's /summary [chat] [period]. Replies are HTML: the bot's default is Markdown,
        which would read the chat's name or the query."""
        bot, username = event.pattern_match.group("bot"), getattr(self.forum, "username", None)
        if bot and username and bot.casefold() != username.casefold():
            return  # /summary@another_bot in the forum
        try:
            query, period = split_args(event.pattern_match.group("args") or "", self.now())
        except PeriodError as e:
            await self._reply(event, html.escape(str(e)))
            return
        label = period.label if period else ""
        if event.is_reply:
            replied = await event.get_reply_message()
            if getattr(replied, "fwd_from", None) is not None:
                chat_id = forwarded_chat(self.inbox, replied)
                if chat_id is None:
                    await self._reply(event, "Не нашёл, из какого чата это сообщение: чат должен быть включён, а "
                                             "сообщение — моложе 14 дней. Можно назвать чат: /summary "
                                             "&lt;часть названия&gt;.")
                    return
                await self._answer(event, chat_id, period)
                return
        chats = self._chats()
        if not chats:
            await self._reply(event, "Включённых чатов нет: их включают в Mini App.")
            return
        if not query:
            await self._reply(event, "Сводка какого чата?", self._buttons(chats, label))
            return
        needle = query.casefold()
        found = [chat for chat in chats if chat["title"].casefold() == needle] or \
            [chat for chat in chats if needle in chat["title"].casefold()]
        if len(found) == 1:
            await self._answer(event, found[0]["id"], period)
        elif found:
            await self._reply(event, "Какой из них?", self._buttons(found, label))
        else:
            await self._reply(event, f"Среди включённых чатов нет «{html.escape(query)}».",
                              self._buttons(chats, label))

    async def on_button(self, event) -> None:
        """A chat chosen under the bot's question."""
        if event.sender_id != self.owner_id:
            await event.answer("Нет доступа", alert=True)
            return
        chat_id, label = int(event.pattern_match.group(1)), event.pattern_match.group(2).decode()
        chat = self.inbox.chat(chat_id)
        if chat is None:
            await event.answer("Чата больше нет", alert=True)
            return
        await event.answer()
        await event.edit(f"Собираю сводку «{html.escape(chat['title'])}»…", buttons=None, parse_mode="html")
        try:
            period = parse_period(label, self.now()) if label else None
        except PeriodError as e:
            text = html.escape(str(e))
        else:
            text = await self.make(chat_id, period)
        await event.edit(text, parse_mode="html", link_preview=False)

    async def _answer(self, event, chat_id: int, period: Period | None) -> None:
        chat = self.inbox.chat(chat_id)
        if chat is None:
            await self._reply(event, "Чата больше нет.")
            return
        progress = await self._reply(event, f"Собираю сводку «{html.escape(chat['title'])}»…")
        await progress.edit(await self.make(chat_id, period), parse_mode="html", link_preview=False)

    @staticmethod
    async def _reply(event, text: str, buttons=None):
        return await event.reply(text, buttons=buttons, parse_mode="html", link_preview=False)

    async def make(self, chat_id: int, period: Period | None) -> str:
        """A summary asked for, posted to the topic; returns the bot's answer, HTML. Whatever fails is told in it,
        so the answer never stays at «Собираю сводку…»."""
        try:
            return await self._make(chat_id, period)
        except Exception as e:
            log.exception("the summary of %s failed", chat_id)
            return f"Сводка не собрана: {html.escape(str(e) or type(e).__name__)}."

    async def _make(self, chat_id: int, period: Period | None) -> str:
        collected = self.summaries.collect(chat_id, period)
        if collected is None:
            return "Чата больше нет."
        title = html.escape(collected.chat["title"])
        if not collected.chosen:
            self.summaries.mark(collected)
            return f"В «{title}» с прошлой сводки читать нечего." if period is None else \
                f"В «{title}» за {span(collected)} читать нечего."
        try:
            points = await self.summaries.summarize(collected, asked=True)
        except DoesNotFit:
            return "Сводка не собрана: не хватает остатка дневного лимита LLM. Промежуток короче может поместиться."
        except BudgetExceeded:
            return "Сводка не собрана: дневной лимит LLM исчерпан."
        except LLMError as e:
            return f"Сводка не собрана: {html.escape(str(e))}."
        if not points:
            self.summaries.mark(collected)
            return f"В «{title}» за {span(collected)} ничего важного (сообщений: {len(collected.chosen)})."
        msg_id = await self.publish(collected)
        self.summaries.mark(collected)
        link = message_link({"id": self.forum_id, "kind": "supergroup", "username": None}, msg_id)
        return f'Сводка «{title}» — <a href="{html.escape(link)}">в теме «Сводки»</a>.'

    def _chats(self) -> list[dict]:
        """Enabled chats for the buttons: those with the daily summary first, then study ones, then by name."""
        enabled = self.chat_settings.enabled_ids()
        daily = self.chat_settings.flagged_ids("summary")
        study = self.chat_settings.flagged_ids("study")
        chats = [chat for chat in self.inbox.chats() if chat["id"] in enabled]
        chats.sort(key=lambda chat: (chat["id"] not in daily, chat["id"] not in study, chat["title"].casefold()))
        return chats

    @staticmethod
    def _buttons(chats: list[dict], label: str) -> list[list]:
        return [[Button.inline(chat["title"][:BUTTON_TITLE], f"sm:{chat['id']}:{label}".encode())]
                for chat in chats[:CHAT_BUTTONS]]
