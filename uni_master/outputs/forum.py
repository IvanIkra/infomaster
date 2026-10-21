"""The bot and its forum group (§8.3). Everything outgoing goes through the bot; the twin never writes (р. 15).

The bot must be an admin of the forum with the "Manage topics" right: it creates its topics itself and keeps
their ids in kv, so a deleted topic is created again on the next post.
"""

import asyncio
import logging
import re
from collections.abc import Awaitable, Callable, Coroutine
from pathlib import Path

from telethon import Button, TelegramClient, errors, events, functions
from telethon.tl import types

from .. import clock
from ..config import Env
from ..llm.budget import SOFT_SHARE, Budget, Level, Notice
from ..store import Store

log = logging.getLogger(__name__)

# key -> title; the other topics come with their stages
TOPICS = {"system": "Система", "news": "Новости", "deadlines": "Дедлайны", "schedule": "Расписание",
          "sheets": "Листочки", "grades": "Оценки", "summaries": "Сводки"}
# The bot's commands for Telegram's menu: name -> description
COMMANDS = {"summary": "Сводка чата: /summary [чат] [3h | 2d | 20.10-22.10]"}
RAISE_LIMIT = re.compile(rb"^llm:raise:(\d{4}-\d{2}-\d{2})$")  # the day is in the data: the button raises only that day
# Mini App screens a post can open (see app_button) -> what the bot says above the Mini App's button
SCREENS = {"hseapp": "Вход в HSE App X — в Mini App.", "chats": "Вход твинка — в Mini App, экран «Чаты»."}


def start_screen(text: str) -> str | None:
    """'/start hseapp' -> 'hseapp': the screen a deep link under a forum post asks for."""
    words = text.split()
    return words[1] if len(words) > 1 and words[1] in SCREENS else None


class Forum:
    def __init__(self, env: Env, store: Store, budget: Budget, session: Path):
        self.env, self.store, self.budget = env, store, budget
        self.session = session
        self.client = TelegramClient(str(session), env.api_id, env.api_hash)
        self._forum: types.TypeInputPeer | None = None
        self.username: str | None = None  # the bot's, for links that open its chat
        self._topics_lock = asyncio.Lock()
        self._tasks: set[asyncio.Task] = set()

    @property
    def miniapp_url(self) -> str:
        return f"{self.env.public_url}/app/"

    def screen_url(self, screen: str) -> str:
        """The Mini App opened on one of its screens."""
        return f"{self.miniapp_url}?screen={screen}"

    def app_button(self, text: str, screen: str) -> types.KeyboardInlineButton | None:
        """A button under a forum post that leads to a Mini App screen. Web App buttons work only in the chat with
        the bot, so this one opens that chat with /start <screen>, and the bot answers with the Mini App's button.
        None until the bot has started."""
        if not self.username:
            return None
        return Button.url(text, f"https://t.me/{self.username}?start={screen}")

    async def start(self) -> None:
        await self.client.start(bot_token=self.env.bot_token)
        session_file = self.session.with_name(self.session.name + ".session")
        if session_file.exists():
            session_file.chmod(0o600)
        self._forum = await self.client.get_input_entity(self.env.forum_id)
        self.username = (await self.client.get_me()).username
        self.client.add_event_handler(self._on_start, events.NewMessage(pattern=r"^/start\b",
                                                                        func=lambda e: e.is_private))
        self.client.add_event_handler(self._on_raise, events.CallbackQuery(data=RAISE_LIMIT))
        await self._set_menu_button(self.env.owner_id)
        try:
            await self.client(functions.bots.SetBotCommandsRequest(
                scope=types.BotCommandScopeDefault(), lang_code="",
                commands=[types.BotCommand(name, about) for name, about in COMMANDS.items()]))
        except errors.RPCError as e:
            log.info("bot commands not set: %s", e)

    def on_button(self, pattern: re.Pattern, handler: Callable[[events.CallbackQuery.Event], Awaitable[None]]) -> None:
        """A callback button under the bot's posts; the handler checks who pressed it."""
        self.client.add_event_handler(handler, events.CallbackQuery(data=pattern))

    def on_command(self, pattern: re.Pattern, handler: Callable[[events.NewMessage.Event], Awaitable[None]]) -> None:
        """A command of the owner, in the chat with the bot or in the forum; anyone else's is ignored."""
        self.client.add_event_handler(handler, events.NewMessage(pattern=pattern, func=self._owner_here))

    def _owner_here(self, event: events.NewMessage.Event) -> bool:
        return event.sender_id == self.env.owner_id and (event.is_private or event.chat_id == self.env.forum_id)

    async def stop(self) -> None:
        for task in list(self._tasks):
            task.cancel()
        await self.client.disconnect()

    async def post(self, topic: str, text: str, buttons=None, *, html: bool = False,
                   reply_to: int | None = None) -> int:
        """Send to a topic; returns the message id. With `html` the text is HTML and links get no preview; with
        `reply_to` it answers that message of the topic."""
        for attempt in (1, 2):
            topic_id = await self._topic(topic)
            try:
                message = await self.client.send_message(self._forum, text, reply_to=reply_to or topic_id,
                                                         buttons=buttons, **self._html(html))
                return message.id
            except errors.TopicDeletedError:
                if attempt == 2:
                    raise
                log.warning("topic %r was deleted, creating it again", topic)
                await self._forget_topic(topic)
        raise AssertionError("unreachable")

    async def post_file(self, topic: str, path: Path, name: str, caption: str) -> int:
        """Send a file under its own name to a topic, with an HTML caption; returns the message id."""
        for attempt in (1, 2):
            topic_id = await self._topic(topic)
            try:
                message = await self.client.send_file(
                    self._forum, str(path), caption=caption, parse_mode="html", reply_to=topic_id, force_document=True,
                    attributes=[types.DocumentAttributeFilename(name)])
                return message.id
            except errors.TopicDeletedError:
                if attempt == 2:
                    raise
                log.warning("topic %r was deleted, creating it again", topic)
                await self._forget_topic(topic)
        raise AssertionError("unreachable")

    async def edit_file(self, msg_id: int, path: Path, name: str, caption: str) -> bool:
        """Replace a message's file and caption; False when the message is gone."""
        try:
            await self.client.edit_message(self._forum, msg_id, caption, file=str(path), parse_mode="html",
                                           force_document=True, attributes=[types.DocumentAttributeFilename(name)])
        except errors.MessageNotModifiedError:
            pass
        except (errors.MessageIdInvalidError, errors.MessageEditTimeExpiredError):
            return False
        return True

    async def edit(self, msg_id: int, text: str, *, html: bool = False) -> bool:
        """Replace a message's text; False when the message is gone (deleted by hand, or with its topic)."""
        try:
            await self.client.edit_message(self._forum, msg_id, text, **self._html(html))
        except errors.MessageNotModifiedError:
            pass
        except (errors.MessageIdInvalidError, errors.MessageEditTimeExpiredError):
            return False
        return True

    async def delete(self, msg_ids: list[int]) -> None:
        if msg_ids:
            await self.client.delete_messages(self._forum, msg_ids)

    async def pin(self, msg_id: int) -> None:
        try:
            await self.client.pin_message(self._forum, msg_id, notify=False)
        except errors.RPCError as e:  # without the right to pin the list still works, just unpinned
            log.warning("cannot pin message %d: %s", msg_id, e)

    @staticmethod
    def _html(html: bool) -> dict:
        return {"parse_mode": "html", "link_preview": False} if html else {}

    def spawn(self, coro: Coroutine) -> None:
        """Run a post in the background from synchronous code; failures go to the log."""
        task = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._task_done)

    def on_budget(self, notice: Notice) -> None:
        """Budget.notify: the limit's warnings go to the System topic (§7)."""
        if notice.level is Level.HARD:
            text = (f"Дневной лимит LLM исчерпан: ${notice.spent:.2f} из ${notice.limit:.2f}.\n"
                    "Вызовы стоят до полуночи по Москве, сообщения копятся в очереди.")
        else:
            text = (f"Расход LLM: ${notice.spent:.2f} из ${notice.limit:.2f}, это {SOFT_SHARE:.0%} дневного лимита.\n"
                    "Отключены необязательные задачи: картинки, чтение PDF целиком, ежедневная сводка.")
        self.spawn(self.post("system", text, buttons=[self._raise_button()]))

    # --- topics -----------------------------------------------------------------

    async def _topic(self, key: str) -> int:
        async with self._topics_lock:
            saved = self._saved_topics()
            if key not in saved["ids"]:
                saved["ids"][key] = await self._create_topic(TOPICS[key])
                self.store.put_json("forum.topics", saved)
            return saved["ids"][key]

    async def _forget_topic(self, key: str) -> None:
        async with self._topics_lock:
            saved = self._saved_topics()
            saved["ids"].pop(key, None)
            self.store.put_json("forum.topics", saved)

    def _saved_topics(self) -> dict:
        saved = self.store.get_json("forum.topics")
        if not saved or saved["forum_id"] != self.env.forum_id:  # another group: its topics are not ours
            saved = {"forum_id": self.env.forum_id, "ids": {}}
        return saved

    async def _create_topic(self, title: str) -> int:
        result = await self.client(functions.messages.CreateForumTopicRequest(peer=self._forum, title=title))
        for update in result.updates:
            message = getattr(update, "message", None)
            if isinstance(message, types.MessageService) and isinstance(message.action,
                                                                        types.MessageActionTopicCreate):
                log.info("created topic %r: %d", title, message.id)
                return message.id  # the topic's id is the id of the message that opened it
        raise RuntimeError(f"Telegram did not return the new topic {title!r}")

    # --- the owner's side -------------------------------------------------------

    def _raise_button(self) -> types.KeyboardInlineButton:
        step = self.budget.settings.get("llm_daily_limit_usd")
        day = clock.local_day(clock.now_utc()).isoformat()
        return Button.inline(f"+${step:.2f} на сегодня", f"llm:raise:{day}".encode())

    async def _on_raise(self, event: events.CallbackQuery.Event) -> None:
        if event.sender_id != self.env.owner_id:
            await event.answer("Нет доступа", alert=True)
            return
        day = event.pattern_match.group(1).decode()
        if day != clock.local_day(clock.now_utc()).isoformat():
            await event.answer("Эта кнопка за прошлый день, лимит уже обновился", alert=True)
            await event.edit(buttons=None)
            return
        limit = self.budget.raise_today(self.budget.settings.get("llm_daily_limit_usd"))
        await event.answer(f"Лимит на сегодня: ${limit:.2f}")
        message = await event.get_message()
        await event.edit(f"{message.text}\n\nЛимит на сегодня поднят до ${limit:.2f}.", buttons=None)

    async def _on_start(self, event: events.NewMessage.Event) -> None:
        if event.sender_id != self.env.owner_id:
            return
        await self._set_menu_button(await event.get_input_sender())
        screen = start_screen(event.raw_text)  # from a button under a forum post
        url = self.screen_url(screen) if screen else self.miniapp_url
        open_app = types.KeyboardInlineButton("Открыть", types.InlineButtonTypeWebView(url))
        await event.respond(SCREENS[screen] if screen else "Настройки — в Mini App.", buttons=[open_app])

    async def _set_menu_button(self, user) -> None:
        """The button next to the message field in the chat with the bot, for the owner only."""
        try:
            await self.client(functions.bots.SetBotMenuButtonRequest(
                user_id=user, button=types.BotMenuButton("Настройки", self.miniapp_url)))
        except (ValueError, errors.RPCError) as e:
            log.info("menu button not set (%s); send /start to the bot", e)

    def _task_done(self, task: asyncio.Task) -> None:
        self._tasks.discard(task)
        if not task.cancelled() and task.exception() is not None:
            log.error("background post failed", exc_info=task.exception())
