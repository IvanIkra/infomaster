import datetime as dt
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from telethon import errors
from telethon.tl import types

from uni_master import clock
from uni_master.collectors import telegram
from uni_master.collectors.files import NotNow
from uni_master.collectors.inbox import Inbox
from uni_master.settings import ChatSettings

CHANNEL = -1000000000010  # marked id of PeerChannel(10)


def message(text="", msg_id=5, media=None, entities=None, reply_to=None, date=None, **extra) -> types.Message:
    return types.Message(id=msg_id, peer_id=types.PeerChannel(10), date=date or clock.now_utc(), message=text,
                         media=media, entities=entities, reply_to=reply_to, **extra)


def document(mime: str, *attributes) -> types.MessageMediaDocument:
    return types.MessageMediaDocument(document=types.Document(
        id=1, access_hash=1, file_reference=b"", date=None, mime_type=mime, size=2048, dc_id=2,
        attributes=list(attributes)))


def test_hidden_links():
    text = "😀 Форма и ещё ссылка"
    entities = [types.MessageEntityTextUrl(offset=3, length=5, url="https://forms.gle/a"),  # 😀 is 2 UTF-16 units
                types.MessageEntityTextUrl(offset=15, length=6, url="https://b.ru")]
    assert telegram.message_text(message(text, entities=entities)) == \
        "😀 Форма (https://forms.gle/a) и ещё ссылка (https://b.ru)"
    assert telegram.message_text(message("просто текст")) == "просто текст"


def test_files():
    pdf = message(media=document("application/pdf", types.DocumentAttributeFilename("hw5.pdf")))
    assert telegram.message_files(pdf) == [{"kind": "pdf", "name": "hw5.pdf", "mime": "application/pdf",
                                            "size": 2048}]
    voice = message(media=document("audio/ogg", types.DocumentAttributeAudio(duration=3, voice=True)))
    assert telegram.message_files(voice)[0]["kind"] == "voice"
    photo = message(media=types.MessageMediaPhoto(photo=types.Photo(
        id=1, access_hash=1, file_reference=b"", date=None, dc_id=2,
        sizes=[types.PhotoSize(type="m", w=320, h=240, size=900),
               types.PhotoSizeProgressive(type="y", w=1280, h=960, sizes=[300, 5000])])))
    assert telegram.message_files(photo) == [{"kind": "photo", "mime": "image/jpeg", "size": 5000}]  # the largest
    assert telegram.message_files(message("текст")) == []
    # A link preview's picture or file is no file of the message.
    preview = types.WebPage(id=1, url="https://hse.ru", display_url="hse.ru", hash=0, photo=types.Photo(
        id=2, access_hash=1, file_reference=b"", date=None, dc_id=2, sizes=[types.PhotoSize(type="m", w=1, h=1,
                                                                                             size=900)]))
    assert telegram.message_files(message("https://hse.ru", media=types.MessageMediaWebPage(webpage=preview))) == []
    preview.document, preview.photo = document("application/pdf").document, None
    assert telegram.message_files(message("https://hse.ru", media=types.MessageMediaWebPage(webpage=preview))) == []


def test_forum_topics():
    in_topic = message("в теме", reply_to=types.MessageReplyHeader(reply_to_msg_id=40, forum_topic=True))
    assert (telegram.message_fields(in_topic)["topic_id"], telegram.message_fields(in_topic)["reply_to"]) == (40, None)
    reply = message("ответ", reply_to=types.MessageReplyHeader(reply_to_msg_id=55, reply_to_top_id=40,
                                                                forum_topic=True))
    assert (telegram.message_fields(reply)["topic_id"], telegram.message_fields(reply)["reply_to"]) == (40, 55)
    plain = message("ответ", reply_to=types.MessageReplyHeader(reply_to_msg_id=55))
    assert (telegram.message_fields(plain)["topic_id"], telegram.message_fields(plain)["reply_to"]) == (None, 55)


def test_forward():
    forwarded = message("пост", fwd_from=types.MessageFwdHeader(date=clock.now_utc(), from_id=types.PeerChannel(20),
                                                                channel_post=7, post_author="Деканат"))
    assert telegram.message_fields(forwarded)["forward"] == {"from": "Деканат", "chat_id": -1000000000020,
                                                             "msg_id": 7}


def test_chat_kind():
    assert telegram.chat_kind(types.User(id=1, bot=True)) == "bot"
    assert telegram.chat_kind(types.Chat(id=1, title="g", photo=None, participants_count=2, date=None,
                                         version=1)) == "group"
    channel = dict(id=1, title="c", photo=types.ChatPhotoEmpty(), date=None)
    assert telegram.chat_kind(types.Channel(**channel, broadcast=True)) == "channel"
    assert telegram.chat_kind(types.Channel(**channel, megagroup=True, forum=True)) == "forum"
    assert telegram.chat_kind(types.Channel(**channel, megagroup=True)) == "supergroup"


@pytest.fixture
def twin(store, tmp_path):
    inbox, chat_settings = Inbox(store), ChatSettings(store)
    notices = []
    twin = telegram.Twin(SimpleNamespace(), inbox, chat_settings, tmp_path / "twin", notices.append)
    twin.notices = notices
    inbox.put_chat(CHANNEL, "Матан", "supergroup", None, 4)
    return twin


def live(msg: types.Message, sender_name="Староста", replied=None) -> SimpleNamespace:
    msg.get_sender = AsyncMock(return_value=types.User(id=7, first_name=sender_name))
    msg.get_reply_message = AsyncMock(return_value=replied)
    return SimpleNamespace(chat_id=CHANNEL, message=msg, get_chat=AsyncMock())


async def test_live_messages(twin):
    await twin._on_new(live(message("до включения", msg_id=5)))
    assert not twin.inbox.has(CHANNEL, 5) and twin.inbox.chat(CHANNEL)["last_id"] == 5  # disabled: only moves on

    twin.chat_settings.update(CHANNEL, {"enabled": True})
    old = message("Консультация в пятницу в R405")
    reply = message("А во сколько?", msg_id=6, reply_to=types.MessageReplyHeader(reply_to_msg_id=2))
    await twin._on_new(live(reply, replied=old))
    stored = twin.inbox.message(CHANNEL, 6)
    assert stored["sender_name"] == "Староста" and stored["reply_quote"] == "Консультация в пятницу в R405"
    assert twin.inbox.chat(CHANNEL)["last_id"] == 6

    await twin._on_new(live(message("", msg_id=7)))  # nothing to keep: no text, no file
    assert not twin.inbox.has(CHANNEL, 7)


async def test_new_chat_is_announced_and_off(twin):
    event = live(message("привет", msg_id=3))
    event.chat_id = -1000000000099
    event.get_chat = AsyncMock(return_value=types.Channel(id=99, title="Новости ВШЭ", photo=types.ChatPhotoEmpty(),
                                                          date=None, broadcast=True, username="hse"))
    await twin._on_new(event)
    chat = twin.inbox.chat(-1000000000099)
    assert (chat["kind"], chat["username"], chat["last_id"]) == ("channel", "hse", 3)
    assert not twin.inbox.has(-1000000000099, 3) and "Новости ВШЭ" in twin.notices[0]


async def test_edit_and_pin(twin):
    twin.chat_settings.update(CHANNEL, {"enabled": True})
    await twin._on_new(live(message("ДЗ до 22.10", msg_id=6)))
    edited = message("ДЗ до 24.10", msg_id=6, edit_date=clock.now_utc())
    await twin._on_edit(SimpleNamespace(chat_id=CHANNEL, message=edited))
    assert twin.inbox.message(CHANNEL, 6)["text"] == "ДЗ до 24.10"

    older = live(message("старое, но правленое", msg_id=3, date=clock.now_utc() - dt.timedelta(days=2)))
    await twin._on_edit(older)
    assert twin.inbox.has(CHANNEL, 3)
    ancient = live(message("совсем старое", msg_id=2, date=clock.now_utc() - dt.timedelta(days=30)))
    await twin._on_edit(ancient)
    assert not twin.inbox.has(CHANNEL, 2)

    await twin._on_pin(types.UpdatePinnedChannelMessages(channel_id=10, messages=[6], pts=1, pts_count=1,
                                                         pinned=True))
    assert twin.inbox.message(CHANNEL, 6)["pinned"] is True


async def test_no_session_needs_login(twin):
    assert await twin.start() is False
    assert twin.status()["login_needed"] is True and "login" in twin.notices[0]


async def test_download(twin, tmp_path):
    """Files.download: the message's own file; NotNow when it cannot be done now, False when it is gone."""
    path = tmp_path / "f.part"
    with pytest.raises(NotNow):  # not logged in
        await twin.download(CHANNEL, 5, path)
    photo = message(media=types.MessageMediaPhoto(photo=types.Photo(
        id=1, access_hash=1, file_reference=b"", date=None, dc_id=2, sizes=[])))
    photo.download_media = AsyncMock(return_value=str(path))
    preview = message("https://hse.ru", media=types.MessageMediaWebPage(webpage=types.WebPage(
        id=1, url="https://hse.ru", display_url="hse.ru", hash=0, photo=photo.media.photo)))
    found = {5: photo, 6: preview}
    twin.client = SimpleNamespace(is_connected=lambda: True,
                                  get_messages=AsyncMock(side_effect=lambda chat_id, ids: found.get(ids)))
    assert await twin.download(CHANNEL, 5, path) is True
    assert await twin.download(CHANNEL, 6, path) is False  # a link preview is no file of the message
    assert await twin.download(CHANNEL, 7, path) is False  # deleted
    twin.client.get_messages = AsyncMock(side_effect=errors.FloodWaitError(None, capture=30))
    with pytest.raises(NotNow):
        await twin.download(CHANNEL, 5, path)
    twin.client.get_messages = AsyncMock(side_effect=errors.ChannelPrivateError(None))
    with pytest.raises(errors.ChannelPrivateError):  # closed to the twin: final, Files keeps it as the file's error
        await twin.download(CHANNEL, 5, path)


class FakeClient:
    """Telethon's client for the login: the code 12345, the cloud password pw when `password` is set."""
    password = False
    me = types.User(id=777, first_name="Twin", username="twin_acc")
    made: list["FakeClient"] = []

    def __init__(self, session, api_id, api_hash):
        self.file = telegram.session_file(telegram.Path(session))
        self.connected, self.handlers = False, []
        FakeClient.made.append(self)

    async def connect(self):
        self.file.touch()
        self.connected = True

    async def disconnect(self):
        self.connected = False

    async def send_code_request(self, phone):
        if phone == "+70000000000":
            raise errors.PhoneNumberInvalidError(request=None)
        return SimpleNamespace(phone_code_hash="hash")

    async def sign_in(self, phone=None, code=None, phone_code_hash=None, password=None):
        if password is None:
            assert phone_code_hash == "hash"
            if code != "12345":
                raise errors.PhoneCodeInvalidError(request=None)
            if self.password:
                raise errors.SessionPasswordNeededError(request=None)
        elif password != "pw":
            raise errors.PasswordHashInvalidError(request=None)

    async def get_me(self):
        return self.me

    async def log_out(self):
        self.file.unlink()
        self.connected = False

    def add_event_handler(self, handler, event):
        self.handlers.append(handler)

    async def iter_dialogs(self):
        return
        yield


@pytest.fixture
def fake_client(monkeypatch):
    FakeClient.made, FakeClient.password = [], False
    monkeypatch.setattr(telegram, "TelegramClient", FakeClient)
    return FakeClient


async def test_login_and_logout(twin, fake_client):
    twin.env = SimpleNamespace(api_id=1, api_hash="h", owner_id=42)
    with pytest.raises(telegram.LoginError, match="сначала запросите код"):
        await twin.sign_in(code="12345")
    with pytest.raises(telegram.LoginError, match="не знает"):
        await twin.send_code("+7 000 000-00-00")
    await twin.send_code("+7 (916) 123-45-67")
    assert twin.status()["login"] == {"step": "code", "phone": "+7916•••••67"}
    with pytest.raises(telegram.LoginError, match="неверный код"):
        await twin.sign_in(code="11111")
    assert twin.client is None and twin.status()["login"]["step"] == "code"  # one more try with the same code

    await twin.sign_in(code="1 2 3 4 5")
    status = twin.status()
    assert status["account"] == {"id": 777, "name": "Twin", "username": "twin_acc"} and status["login"] is None
    assert status["login_needed"] is False and status["error"] is None and twin.changed.is_set()
    assert twin.client.handlers and "Твинк вошёл: Twin" in twin.notices[-1]
    assert telegram.session_file(twin.session).stat().st_mode & 0o777 == 0o600
    with pytest.raises(telegram.LoginError, match="уже вошёл"):
        await twin.send_code("+79161234567")

    twin.changed.clear()
    await twin.logout()
    assert twin.client is None and not telegram.session_file(twin.session).exists() and twin.changed.is_set()
    assert twin.status()["login_needed"] is True and twin.status()["account"] is None
    assert "вышел из аккаунта Twin" in twin.notices[-1]
    with pytest.raises(telegram.LoginError, match="и так не вошёл"):
        await twin.logout()


async def test_login_with_a_cloud_password(twin, fake_client):
    twin.env = SimpleNamespace(api_id=1, api_hash="h", owner_id=42)
    fake_client.password = True
    await twin.send_code("+79161234567")
    await twin.sign_in(code="12345")
    assert twin.status()["login"]["step"] == "password" and twin.client is None
    with pytest.raises(telegram.LoginError, match="неверный облачный пароль"):
        await twin.sign_in(password="nope")
    await twin.sign_in(password="pw")
    assert twin.status()["account"]["name"] == "Twin"

    # A new code drops the pending login; cancelling drops it too.
    await twin.logout()
    await twin.send_code("+79161234567")
    await twin.send_code("+79161234567")
    assert [c.connected for c in fake_client.made[-2:]] == [False, True]
    await twin.cancel_login()
    assert twin.status()["login"] is None and not fake_client.made[-1].connected


async def test_the_owners_account_is_no_twin(twin, fake_client):
    twin.env = SimpleNamespace(api_id=1, api_hash="h", owner_id=777)
    await twin.send_code("+79161234567")
    with pytest.raises(telegram.LoginError, match="это не твинк"):
        await twin.sign_in(code="12345")
    assert twin.client is None and not telegram.session_file(twin.session).exists()
