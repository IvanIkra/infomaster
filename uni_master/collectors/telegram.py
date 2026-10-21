"""The twin: a separate Telegram account that reads chats and channels (§5.1). It never writes anywhere (р. 15).

Every chat of the account is listed in `chats`, a new one disabled (р. 11). Messages are stored only from enabled
chats: new ones live, edits, deletions and pins as they happen. After a restart each enabled chat is caught up from
its `last_id`; edits and deletions made while the service was down are not caught up. A file (a PDF, an image) is
downloaded only when a batch asks for it (collectors/files.py).

The account is logged in once from a terminal: `python -m uni_master login` (see `login`).
"""

import logging
from collections.abc import Callable
from pathlib import Path

from telethon import TelegramClient, errors, events, helpers, utils
from telethon.tl import types
from telethon.tl.custom import Message

from .. import clock
from ..config import Env
from ..settings import ChatSettings
from .files import NotNow
from .inbox import RETENTION, Inbox

log = logging.getLogger(__name__)

QUOTE_LIMIT = 500  # characters of a replied message that is not stored itself
LOGIN_HINT = ("docker compose run --rm uni-master python -m uni_master login, "
              "затем docker compose restart uni-master")


def session_file(session: Path) -> Path:
    return session.with_name(session.name + ".session")


class Twin:
    def __init__(self, env: Env, inbox: Inbox, chat_settings: ChatSettings, session: Path,
                 notify: Callable[[str], None]):
        self.env, self.inbox, self.chat_settings = env, inbox, chat_settings
        self.session = session
        self.notify = notify  # a line for the System topic
        self.client: TelegramClient | None = None
        self.ok_at: str | None = None
        self.error: str | None = None
        self.login_needed = False

    def status(self) -> dict:
        return {"ok_at": self.ok_at, "error": self.error, "login_needed": self.login_needed}

    async def start(self) -> bool:
        """Connect, list the chats and catch up; False when the account is not logged in (the service runs on)."""
        if not session_file(self.session).exists():
            return self._needs_login("твинк ещё не входил в Telegram")
        self.client = TelegramClient(str(self.session), self.env.api_id, self.env.api_hash)
        await self.client.connect()
        if not await self.client.is_user_authorized():
            await self.client.disconnect()
            self.client = None
            return self._needs_login("сессия твинка больше не действует")
        session_file(self.session).chmod(0o600)

        # Live updates first: what arrives during the catch-up is stored once either way.
        self.client.add_event_handler(self._on_new, events.NewMessage())
        self.client.add_event_handler(self._on_edit, events.MessageEdited())
        self.client.add_event_handler(self._on_delete, events.MessageDeleted())
        self.client.add_event_handler(self._on_pin, events.Raw((types.UpdatePinnedMessages,
                                                                types.UpdatePinnedChannelMessages)))
        self.client.add_event_handler(self._on_title, events.ChatAction(func=lambda e: e.new_title))
        await self._sync()
        self.ok_at = clock.local_iso(clock.now_utc())
        return True

    async def stop(self) -> None:
        if self.client is not None:
            await self.client.disconnect()

    def _needs_login(self, why: str) -> bool:
        self.login_needed, self.error = True, why
        log.warning("%s; log in with: %s", why, LOGIN_HINT)
        self.notify(f"Чаты не читаются: {why}.\nВойти на сервере: {LOGIN_HINT}.")
        return False

    # --- chats and the catch-up -------------------------------------------------

    async def _sync(self) -> None:
        first = not self.inbox.chats()  # the very first listing: no news about every chat
        enabled = self.chat_settings.enabled_ids()
        added = caught_up = 0
        async for dialog in self.client.iter_dialogs():
            chat_id, top = dialog.id, dialog.message.id if dialog.message else 0
            known = self.inbox.chat(chat_id)
            if self.inbox.put_chat(chat_id, chat_title(dialog.entity), chat_kind(dialog.entity),
                                   getattr(dialog.entity, "username", None), top):
                added += 1
                if not first:
                    self._announce(chat_id)
            elif chat_id in enabled and top > known["last_id"]:
                try:
                    caught_up += await self._catch_up(dialog.input_entity, chat_id, known["last_id"])
                except errors.RPCError as e:  # one chat closed to the twin must not stop the others
                    log.warning("catch-up of %s failed: %s", chat_id, e)
                    self.error = f"не удалось догнать «{known['title']}»: {e}"
                    continue  # last_id stays: the next start tries again
            self.inbox.advance(chat_id, top)
        log.info("twin: %d new chats, %d messages caught up, %d chats enabled", added, caught_up, len(enabled))

    async def _catch_up(self, entity, chat_id: int, last_id: int) -> int:
        oldest = clock.now_utc() - RETENTION  # older ones would be pruned at once
        stored = 0
        async for message in self.client.iter_messages(entity, min_id=last_id, reverse=True):
            if message.date >= oldest:
                stored += await self._store(message, chat_id)
        return stored

    def _announce(self, chat_id: int) -> None:
        chat = self.inbox.chat(chat_id)
        self.notify(f"Новый чат в аккаунте твинка: «{chat['title']}» ({chat['kind']}). "
                    "Он выключен, включить можно в Mini App.")

    # --- live updates -----------------------------------------------------------

    async def _on_new(self, event: events.NewMessage.Event) -> None:
        chat_id = event.chat_id
        if self.inbox.chat(chat_id) is None:
            chat = await event.get_chat()
            self.inbox.put_chat(chat_id, chat_title(chat), chat_kind(chat), getattr(chat, "username", None),
                                event.message.id)
            self._announce(chat_id)
        elif self.chat_settings.enabled(chat_id):
            await self._store(event.message, chat_id)
        self.inbox.advance(chat_id, event.message.id)
        self.ok_at = clock.local_iso(clock.now_utc())

    async def _on_edit(self, event: events.MessageEdited.Event) -> None:
        message: Message = event.message
        if not self.chat_settings.enabled(event.chat_id):
            return
        at = message.edit_date or clock.now_utc()
        if not self.inbox.edit(event.chat_id, message.id, message_text(message), message_files(message), at) \
                and not self.inbox.has(event.chat_id, message.id) and message.date >= clock.now_utc() - RETENTION:
            await self._store(message, event.chat_id)  # an older message edited: its new text is news

    async def _on_delete(self, event: events.MessageDeleted.Event) -> None:
        self.inbox.delete(event.deleted_ids, event.chat_id, clock.now_utc())

    async def _on_pin(self, update: types.UpdatePinnedMessages | types.UpdatePinnedChannelMessages) -> None:
        peer = update.peer if isinstance(update, types.UpdatePinnedMessages) else types.PeerChannel(update.channel_id)
        self.inbox.pin(utils.get_peer_id(peer), update.messages, update.pinned)

    async def _on_title(self, event: events.ChatAction.Event) -> None:
        self.inbox.rename_chat(event.chat_id, event.new_title)

    # --- files --------------------------------------------------------------------

    async def download(self, chat_id: int, msg_id: int, path: Path) -> bool:
        """Files.download: the file of a message into `path`; False when the message or its file is gone. NotNow when
        the twin is offline or the download failed on the way: the message waits for its next batch. Any other
        Telegram error (the chat is closed to the twin) is final for the file."""
        if self.client is None or not self.client.is_connected():
            raise NotNow("the twin is not connected to Telegram")
        try:
            message = await self.client.get_messages(chat_id, ids=msg_id)
            if message is None or not own_media(message):
                return False
            return await message.download_media(file=str(path)) is not None
        except (errors.FloodError, errors.ServerError, ConnectionError, TimeoutError) as e:
            raise NotNow(f"the download failed on the way: {e}") from e

    async def _store(self, message: Message, chat_id: int) -> bool:
        """Store one message of an enabled chat; False when there is nothing to keep or it is stored already."""
        if not isinstance(message, types.Message) or self.inbox.has(chat_id, message.id):
            return False
        fields = message_fields(message)
        if not fields["text"] and not fields["files"]:
            return False  # polls, locations, dice: nothing for the batches
        sender = await message.get_sender()
        fields["sender_name"] = message.post_author or (utils.get_display_name(sender) if sender else None)
        if fields["reply_to"] and not self.inbox.has(chat_id, fields["reply_to"]):
            replied = await message.get_reply_message()
            if replied is not None:
                fields["reply_quote"] = message_text(replied)[:QUOTE_LIMIT] or None
        return self.inbox.add(chat_id, message.id, fields)


# --- Telegram objects to rows -----------------------------------------------------

def chat_kind(entity) -> str:
    if isinstance(entity, types.User):
        return "bot" if entity.bot else "user"
    if isinstance(entity, types.Chat | types.ChatForbidden):
        return "group"
    if getattr(entity, "broadcast", False):
        return "channel"
    return "forum" if getattr(entity, "forum", False) else "supergroup"


def chat_title(entity) -> str:
    return utils.get_display_name(entity) or str(utils.get_peer_id(entity))


def message_fields(message: Message) -> dict:
    """What a message carries for the batches, without the sender (that may need a request)."""
    topic_id, reply_to = None, message.reply_to_msg_id
    header = message.reply_to
    if isinstance(header, types.MessageReplyHeader) and header.forum_topic:
        # In a forum topic every message "replies" to the topic; a real reply also has the topic as its top.
        topic_id = header.reply_to_top_id or header.reply_to_msg_id
        reply_to = header.reply_to_msg_id if header.reply_to_top_id else None
    return {
        "sender_id": message.sender_id,
        "sender_name": None,
        "at": message.date,
        "text": message_text(message),
        "topic_id": topic_id,
        "reply_to": reply_to,
        "reply_quote": None,
        "forward": message_forward(message),
        "grouped_id": message.grouped_id,
        "files": message_files(message),
        "pinned": bool(message.pinned),
    }


def message_text(message: Message) -> str:
    """The plain text with hidden links spelled out: "форма (https://…)"."""
    text = message.message or ""
    links = [e for e in message.entities or () if isinstance(e, types.MessageEntityTextUrl)]
    if not links:
        return text
    text = helpers.add_surrogate(text)  # entity offsets count UTF-16 units
    for entity in sorted(links, key=lambda e: e.offset + e.length, reverse=True):
        end = entity.offset + entity.length
        text = f"{text[:end]} ({entity.url}){text[end:]}"
    return helpers.del_surrogate(text)


def own_media(message: Message) -> bool:
    """A photo or a document of the message itself. Telethon's `photo` and `document` also give the picture or the
    file of a link preview, which tells nothing the link does not and often arrives later by an edit."""
    media = message.media
    return isinstance(media, types.MessageMediaPhoto) and isinstance(media.photo, types.Photo) \
        or isinstance(media, types.MessageMediaDocument) and isinstance(media.document, types.Document)


def message_files(message: Message) -> list[dict]:
    if not own_media(message):
        return []
    if message.photo:
        return [{"kind": "photo", "mime": "image/jpeg", "size": message.file.size}]  # its largest size
    kind = next((k for k in ("sticker", "voice", "video_note", "gif", "video", "audio") if getattr(message, k)),
                "pdf" if message.file.mime_type == "application/pdf" else "document")
    return [{"kind": kind, "name": message.file.name, "mime": message.file.mime_type, "size": message.file.size}]


def message_forward(message: Message) -> dict | None:
    header = message.fwd_from
    if header is None:
        return None
    source = (message.forward.chat or message.forward.sender) if message.forward else None
    return {"from": header.from_name or header.post_author or (utils.get_display_name(source) if source else None),
            "chat_id": utils.get_peer_id(header.from_id) if header.from_id else None,
            "msg_id": header.channel_post}


# --- the one-time login -------------------------------------------------------------

async def login(env: Env, session: Path) -> None:
    """Log the twin in from a terminal: phone, code, the cloud password if there is one."""
    client = TelegramClient(str(session), env.api_id, env.api_hash)
    await client.start()
    me = await client.get_me()
    if me.bot or me.id == env.owner_id:
        await client.log_out()  # the twin is its own account (р. 15): not the bot, not the owner's
        raise SystemExit("это не твинк: нужен отдельный аккаунт, а не бот и не TG_OWNER_ID")
    session_file(session).chmod(0o600)
    print(f"Твинк вошёл: {utils.get_display_name(me)} (id {me.id}). Перезапустите сервис.")
    await client.disconnect()

