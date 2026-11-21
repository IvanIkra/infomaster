"""The bot (§8.3, D69). The web app (§8.6) took the place of the forum and the Mini App; the bot stays for two jobs:

- it sends the owner's login codes of the web app and the connector to the owner's private chat (`send`);
- it takes material for the library (D58): a private message of the owner that is no command (a forward, a photo, a
  file, a recording, a link, a note) goes to the library's inbox (`on_material`), and the bot says so, quietly; a PDF
  that is a sheet goes to the sheets with its caption instead (D73, processing/bot_sheets.py). Its file is downloaded
  by the bot itself, straight to disk.

/start answers with the web app's address, for an old link of the forum or the calendar too. Anyone else's messages
are ignored. The twin never writes (D15).
"""

import asyncio
import logging
from collections.abc import Awaitable, Callable, Coroutine
from pathlib import Path

from telethon import Button, TelegramClient, errors, events, functions
from telethon.tl import types

from ..collectors.telegram import message_files, message_forward, message_text
from ..config import Env
from ..i18n import tr

log = logging.getLogger(__name__)


class Bot:
    def __init__(self, env: Env, session: Path, lang: Callable[[], str]):
        self.env, self.session, self.lang = env, session, lang
        self.client = TelegramClient(str(session), env.api_id, env.api_hash)
        # The intake of the owner's private messages (processing/bot_sheets.py): the message as the twin's rows have
        # it and a download of its file into a path; where it went ("sheet", "known", "inbox"), or None for nothing.
        self.on_material: Callable[[dict, Callable[[Path], Awaitable[bool]]], Awaitable[str | None]] | None = None
        self._album: int | None = None  # the album answered last: one answer for all its messages
        self._tasks: set[asyncio.Task] = set()

    def tr(self, en: str, ru: str) -> str:
        return tr(self.lang(), en, ru)

    @property
    def app_url(self) -> str:
        return f"{self.env.public_url}/web/"

    async def start(self) -> None:
        await self.client.start(bot_token=self.env.bot_token)
        session_file = self.session.with_name(self.session.name + ".session")
        if session_file.exists():
            session_file.chmod(0o600)
        self.client.add_event_handler(self._on_start, events.NewMessage(pattern=r"^/start\b",
                                                                        func=lambda e: e.is_private))
        self.client.add_event_handler(self._on_private, events.NewMessage(incoming=True, func=lambda e: e.is_private))
        try:  # the Mini App's menu button and the old commands are taken away
            await self.client(functions.bots.SetBotMenuButtonRequest(user_id=types.InputUserEmpty(),
                                                                     button=types.BotMenuButtonDefault()))
            await self.client(functions.bots.ResetBotCommandsRequest(scope=types.BotCommandScopeDefault(),
                                                                     lang_code=""))
        except errors.RPCError as e:
            log.info("the bot's menu not reset: %s", e)

    async def send(self, text: str) -> None:
        """A message to the owner's private chat: a login code. It fails until the owner has pressed Start there."""
        await self.client.send_message(self.env.owner_id, text, parse_mode=None)

    def spawn(self, coro: Coroutine) -> None:
        """Run a task in the background from synchronous code; failures go to the log."""
        task = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._task_done)

    async def stop(self) -> None:
        for task in list(self._tasks):
            task.cancel()
        await self.client.disconnect()

    async def _on_start(self, event: events.NewMessage.Event) -> None:
        if event.sender_id != self.env.owner_id:
            return
        try:  # the Mini App's button was set for the owner alone: taken away for them too
            await self.client(functions.bots.SetBotMenuButtonRequest(user_id=await event.get_input_sender(),
                                                                     button=types.BotMenuButtonDefault()))
        except (ValueError, errors.RPCError) as e:
            log.info("the owner's menu button not reset: %s", e)
        url = self.app_url
        await event.respond(self.tr(f"infomaster is a web app: {url}", f"infomaster — веб-приложение: {url}"),
                            buttons=[Button.url(self.tr("Open", "Открыть"), url)], link_preview=False)

    async def _on_private(self, event: events.NewMessage.Event) -> None:
        """A private message of the owner that is no command: into the library's inbox (D58). Each message of an
        album is an item of its own; the album gets one answer."""
        if event.sender_id != self.env.owner_id or self.on_material is None or event.raw_text.startswith("/"):
            return
        message = event.message
        album = message.grouped_id
        first = album is None or album != self._album
        self._album = album  # before any await: the album's other messages come at once
        fields = {"chat_id": event.chat_id, "msg_id": message.id, "at": message.date, "text": message_text(message),
                  "files": message_files(message), "forward": message_forward(message)}

        async def download(path: Path) -> bool:
            return await message.download_media(file=str(path)) is not None

        try:
            saved = await self.on_material(fields, download)
        except ValueError as e:  # the library refuses it (LibraryError): too big, empty
            answer = self.tr(f"Not saved: {e}.", f"Не сохранено: {e}.")
        except Exception:
            log.exception("message %d to the bot did not go to the library", message.id)
            answer = self.tr("Not saved: something went wrong, see the log.",
                             "Не сохранено: что-то пошло не так, подробности в журнале.")
        else:
            if saved is None or (saved not in ("sheet", "known") and not first):
                return
            answer = {"sheet": self.tr("Saved as a sheet: it is on the Sheets shelf.",
                                       "Сохранено как листочек: он на полке «Листочки»."),
                      "known": self.tr("This sheet is on the shelf already.", "Этот листочек уже на полке.")
                      }.get(saved) or self.tr("Saved to the inbox.", "Сохранено во входящие.")
        await event.respond(answer, silent=True)

    def _task_done(self, task: asyncio.Task) -> None:
        self._tasks.discard(task)
        if not task.cancelled() and task.exception() is not None:
            log.error("a background task failed", exc_info=task.exception())
