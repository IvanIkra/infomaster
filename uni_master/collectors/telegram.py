"""The twin: a separate Telegram account that reads chats and channels (§5.1). It never writes anywhere (р. 15).

Every chat of the account is listed in `chats`, a new one disabled (р. 11). Messages are stored only from enabled
chats: new ones live, edits, deletions and pins as they happen. After a restart each enabled chat is caught up from
its `last_id`; edits and deletions made while the service was down are not caught up. A file (a PDF, an image) is
downloaded only when a batch asks for it (collectors/files.py).

The account logs in and out in the Mini App, screen «Чаты»: the phone, the code Telegram sends to the account, the
cloud password if it has one (`send_code`, `sign_in`, `logout`). The service runs on without it and starts reading
as soon as the login is done, without a restart. A terminal works too: `python -m uni_master login` (see `login`).
"""

import asyncio
import logging
import re
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from telethon import TelegramClient, errors, events, helpers, utils
from telethon.tl import types
from telethon.tl.custom import Message

from .. import clock
from ..config import Env
from ..i18n import Phrase
from ..settings import ChatSettings
from .files import NotNow
from .inbox import RETENTION, Inbox

log = logging.getLogger(__name__)

QUOTE_LIMIT = 500  # characters of a replied message that is not stored itself
LOGIN_HINT = "docker compose run --rm uni-master python -m uni_master login"


class LoginError(ValueError):
    """A step of the login that did not go; the text, a Phrase, is for the owner in the Mini App."""


@dataclass
class Pending:
    """A login under way: the client that asked for the code, until it signs in or is dropped."""
    client: TelegramClient
    phone: str
    code_hash: str
    step: str = "code"  # code, password


def session_file(session: Path) -> Path:
    return session.with_name(session.name + ".session")


class Twin:
    def __init__(self, env: Env, inbox: Inbox, chat_settings: ChatSettings, session: Path,
                 notify: Callable[[Phrase], None]):
        self.env, self.inbox, self.chat_settings = env, inbox, chat_settings
        self.session = session
        self.notify = notify  # a line for the System topic
        self.notify_login: Callable[[Phrase], None] | None = None  # the same, with a button to the login screen
        self.client: TelegramClient | None = None
        self.ok_at: str | None = None
        self.error: str | Phrase | None = None
        self.login_needed = False
        self.account: dict | None = None  # id, name, username of the logged-in account
        self.changed = asyncio.Event()     # set when the client changes: a login or a logout (app.py watches it)
        self._pending: Pending | None = None
        self._login_lock = asyncio.Lock()

    def status(self) -> dict:
        login = {"step": self._pending.step, "phone": masked(self._pending.phone)} if self._pending else None
        return {"ok_at": self.ok_at, "error": self.error, "login_needed": self.login_needed,
                "account": self.account, "login": login}

    async def start(self) -> bool:
        """Connect, list the chats and catch up; False when the account is not logged in (the service runs on)."""
        if not session_file(self.session).exists():
            return self._needs_login(Phrase("the twin has not logged in to Telegram yet",
                                            "твинк ещё не входил в Telegram"))
        self.client = TelegramClient(str(self.session), self.env.api_id, self.env.api_hash)
        await self.client.connect()
        if not await self.client.is_user_authorized():
            await self.client.disconnect()
            self.client = None
            return self._needs_login(Phrase("the twin's session is no longer valid",
                                            "сессия твинка больше не действует"))
        await self._listen()
        return True

    async def _listen(self) -> None:
        """The client is logged in: remember the account, take live updates and catch up."""
        session_file(self.session).chmod(0o600)
        self.account = describe(await self.client.get_me())
        self.login_needed, self.error = False, None
        # Live updates first: what arrives during the catch-up is stored once either way.
        self.client.add_event_handler(self._on_new, events.NewMessage())
        self.client.add_event_handler(self._on_edit, events.MessageEdited())
        self.client.add_event_handler(self._on_delete, events.MessageDeleted())
        self.client.add_event_handler(self._on_pin, events.Raw((types.UpdatePinnedMessages,
                                                                types.UpdatePinnedChannelMessages)))
        self.client.add_event_handler(self._on_title, events.ChatAction(func=lambda e: e.new_title))
        await self._sync()
        self.ok_at = clock.local_iso(clock.now_utc())

    async def stop(self) -> None:
        await self._drop_pending()
        if self.client is not None:
            await self.client.disconnect()

    def _needs_login(self, why: Phrase) -> bool:
        self.login_needed, self.error = True, why
        log.warning("%s; log in in the Mini App or with: %s", why, LOGIN_HINT)
        (self.notify_login or self.notify)(Phrase(
            f"The chats are not read: {why.en}.\nLog in in the Mini App, screen «Chats» (or on the server: "
            f"{LOGIN_HINT}).",
            f"Чаты не читаются: {why.ru}.\nВойти — в Mini App, экран «Чаты» (или на сервере: {LOGIN_HINT})."))
        return False

    # --- login and logout from the Mini App ---------------------------------------

    async def send_code(self, phone: str) -> None:
        """Ask Telegram for a login code; it comes to the account's other sessions or by SMS."""
        phone = "+" + re.sub(r"\D", "", phone)
        if len(phone) < 8:
            raise LoginError(Phrase("the twin's phone number is needed in the international format: +7…",
                                    "нужен номер телефона твинка в международном формате: +7…"))
        async with self._login_lock:
            if self.client is not None:
                raise LoginError(Phrase("the twin is logged in already; to change the account, log out first",
                                        "твинк уже вошёл; чтобы сменить аккаунт, сначала выйдите"))
            await self._drop_pending()
            client = TelegramClient(str(self.session), self.env.api_id, self.env.api_hash)
            try:
                await client.connect()
                sent = await client.send_code_request(phone)
            except errors.PhoneNumberInvalidError:
                await client.disconnect()
                raise LoginError(Phrase("Telegram does not know this number", "Telegram не знает такого номера")) \
                    from None
            except errors.FloodWaitError as e:
                await client.disconnect()
                raise LoginError(Phrase(f"Telegram asks to wait {e.seconds} s before a new code",
                                        f"Telegram просит подождать {e.seconds} с перед новым кодом")) from None
            except (errors.RPCError, ConnectionError, OSError) as e:
                await client.disconnect()
                raise LoginError(Phrase(f"Telegram did not answer: {e}", f"Telegram не ответил: {e}")) from None
            self._pending = Pending(client, phone, sent.phone_code_hash)

    async def sign_in(self, code: str | None = None, password: str | None = None) -> None:
        """The code, then the cloud password if the account has one (the step tells which is asked)."""
        async with self._login_lock:
            pending = self._pending
            if pending is None:
                raise LoginError(Phrase("ask for a code first", "сначала запросите код"))
            try:
                if pending.step == "code":
                    code = re.sub(r"\D", "", code or "")
                    if not code:
                        raise LoginError(Phrase("enter the code from Telegram", "введите код из Telegram"))
                    await pending.client.sign_in(pending.phone, code, phone_code_hash=pending.code_hash)
                else:
                    if not password:
                        raise LoginError(Phrase("enter the cloud password", "введите облачный пароль"))
                    await pending.client.sign_in(password=password)
            except errors.SessionPasswordNeededError:
                pending.step = "password"
                return
            except errors.PhoneCodeInvalidError:
                raise LoginError(Phrase("wrong code", "неверный код")) from None
            except errors.PhoneCodeExpiredError:
                await self._drop_pending()
                raise LoginError(Phrase("the code expired or was shared in Telegram: ask for a new one",
                                        "код устарел или им поделились в Telegram — запросите новый")) from None
            except errors.PasswordHashInvalidError:
                raise LoginError(Phrase("wrong cloud password", "неверный облачный пароль")) from None
            except errors.FloodWaitError as e:
                raise LoginError(Phrase(f"Telegram asks to wait {e.seconds} s",
                                        f"Telegram просит подождать {e.seconds} с")) from None
            except (errors.RPCError, ConnectionError, OSError) as e:
                raise LoginError(Phrase(f"Telegram did not answer: {e}", f"Telegram не ответил: {e}")) from None
            me = await pending.client.get_me()
            self._pending = None
            if me.bot or me.id == self.env.owner_id:
                await pending.client.log_out()  # the twin is its own account (р. 15)
                raise LoginError(Phrase("this is no twin: a separate account is needed, not a bot and not your own",
                                        "это не твинк: нужен отдельный аккаунт, а не бот и не ваш основной"))
            self.client = pending.client
            self.changed.set()
            try:
                await self._listen()
            except (errors.RPCError, ConnectionError, OSError) as e:  # logged in; the next start catches up
                log.warning("the first sync after the login failed: %s", e)
                self.error = Phrase(f"logged in, but the chats did not catch up: {e}",
                                    f"вход выполнен, но чаты не догнались: {e}")
            name = self.account["name"] if self.account else me.id
            self.notify(Phrase(f"The twin logged in: {name}. The chats are read.",
                               f"Твинк вошёл: {name}. Чаты читаются."))

    async def cancel_login(self) -> None:
        async with self._login_lock:
            await self._drop_pending()

    async def logout(self) -> None:
        """End the session on Telegram's side and forget it here; the chats and their settings stay."""
        async with self._login_lock:
            if self.client is None:
                raise LoginError(Phrase("the twin is not logged in anyway", "твинк и так не вошёл"))
            client, self.client, name = self.client, None, (self.account or {}).get("name", "?")
            self.changed.set()  # first: the watcher must not take the disconnect for a lost connection
            try:
                await client.log_out()
            except (errors.RPCError, ConnectionError, OSError) as e:
                log.warning("logging the twin out failed: %s", e)
                await client.disconnect()
            session_file(self.session).unlink(missing_ok=True)
            self.account = None
            self.login_needed, self.error = True, Phrase("you logged out of the twin's account",
                                                         "вы вышли из аккаунта твинка")
            self.notify(Phrase(f"The twin logged out of {name}. The chats are not read until it logs in again.",
                               f"Твинк вышел из аккаунта {name}. Чаты не читаются, пока он снова не войдёт."))

    async def _drop_pending(self) -> None:
        if self._pending is not None:
            pending, self._pending = self._pending, None
            await pending.client.disconnect()

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
                    self.error = Phrase(f"could not catch up with «{known['title']}»: {e}",
                                        f"не удалось догнать «{known['title']}»: {e}")
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
        self.notify(Phrase(f"A new chat in the twin's account: «{chat['title']}» ({chat['kind']}). It is off; it can "
                           "be enabled in the Mini App.",
                           f"Новый чат в аккаунте твинка: «{chat['title']}» ({chat['kind']}). Он выключен, включить "
                           "можно в Mini App."))

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

def describe(me: types.User) -> dict:
    return {"id": me.id, "name": utils.get_display_name(me) or str(me.id), "username": me.username}


def masked(phone: str) -> str:
    """+79161234567 -> +7916•••••67: enough to recognise it on the screen."""
    return phone[:5] + "•" * max(0, len(phone) - 7) + phone[-2:]


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
        raise SystemExit("this is no twin: a separate account is needed, not a bot and not TG_OWNER_ID")
    session_file(session).chmod(0o600)
    print(f"The twin logged in: {utils.get_display_name(me)} (id {me.id}). Restart the service.")
    await client.disconnect()

