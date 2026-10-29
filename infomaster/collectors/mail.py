"""The university mailbox over IMAP (§5.2, D6, D30): the port of Tablo's sources/mail.py (§6.2).

Only the @edu.hse.ru box (Yandex 360), with an app password from .env (MAIL_USER, MAIL_PASSWORD; without them the
source is off and says so). INBOX is opened read-only and letters are fetched with BODY.PEEK, so nothing is marked as
read. A letter is new when its UID is above the last one seen (kv "mail.state"); the first poll, or one after
UIDVALIDITY changed, takes only letters of the last `news_ttl_hours` (24 h). imaplib blocks, so a poll runs in a
thread, 60 s per socket operation; one every 5 minutes.

A letter becomes a message (source "mail") of the mailbox's pseudo chat (`MAILBOX`): the sender, the subject, the
names of the attachments and the body without HTML, quoted replies and the signature, cleaned and cut at 8000
characters. The batches parse it with the chats (processing/batches.py): every letter is study, and the mailbox comes
after the study chats and before the others (D30). Its msg_id is the UID plus an offset that grows past every
msg_id given before whenever UIDVALIDITY changes, so a rebuilt box never collides with letters kept from the old
one. The letters and the state are saved in one transaction: a letter is stored once. One broken letter does not
stop the others.

The password never leaves this module: an error is told without it. The System topic hears once when the box has
failed for 3 hours, and once when it works again.
"""

import asyncio
import datetime as dt
import email
import email.policy
import imaplib
import json
import logging
import re
from collections.abc import Callable
from dataclasses import dataclass
from email.message import EmailMessage
from email.utils import parsedate_to_datetime
from html.parser import HTMLParser

from .. import clock
from ..config import Env
from ..i18n import Phrase
from ..settings import Settings
from ..store import Store
from .inbox import Inbox

log = logging.getLogger(__name__)

STATE_KEY = "mail.state"    # {"uidvalidity", "last": the highest UID seen, "base": msg_id = base + UID}
FOLDER = "INBOX"
POLL_EVERY = 5 * 60         # seconds (§6.2)
TIMEOUT = 60                # seconds, each socket operation
MAX_BODY = 8000             # characters of a letter's text (§6.2)
STALE_AFTER = dt.timedelta(hours=3)  # failing longer than this is told to the System topic
HIDDEN = "***"
MONTHS = ("Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec")
NOT_SET = "MAIL_USER and MAIL_PASSWORD are not set"

# A line that starts the quoted letter in a reply: everything from it down is cut.
REPLY_HEADER = re.compile(
    r"^\s*("
    r"On .{5,200} wrote:"
    r"|.{5,200} (пишет|написал|написала|написал\(а\)):"
    r"|.{0,120}<[^<>\s]+@[^<>\s]+>:"  # Yandex: 18.10.2026, 12:00, "Name" <a@b.ru>:
    r"|-{2,}\s*(Original Message|Исходное сообщение)\s*-{2,}"
    r"|_{10,}"  # Outlook puts a line of underscores above "От: … Отправлено: …"
    r")\s*$",
    re.IGNORECASE,
)
SIGNATURE = re.compile(r"^-- ?$")
INVISIBLE = re.compile("[­͏​-‏⁠-⁤﻿]")  # preheader padding in mailings


# --- letter to text -------------------------------------------------------------


class _HtmlText(HTMLParser):
    """Visible text with line breaks at block tags; quoted replies (blockquote, gmail_quote) are left out."""

    BLOCK = {"p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "table", "ul", "ol", "hr"}
    SKIP = {"script", "style", "head", "title"}

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.parts: list[str] = []
        self._skip: list[str] = []  # open tags whose content is dropped

    def handle_starttag(self, tag, attrs):
        if self._skip:
            if tag == self._skip[-1]:
                self._skip.append(tag)
            return
        classes = dict(attrs).get("class") or ""
        if tag in self.SKIP or tag == "blockquote" or "gmail_quote" in classes or "yandex_quote" in classes:
            if tag not in ("br", "hr"):
                self._skip.append(tag)
            return
        if tag in self.BLOCK:
            self.parts.append("\n")

    def handle_startendtag(self, tag, attrs):
        if not self._skip and tag in ("br", "hr"):
            self.parts.append("\n")

    def handle_endtag(self, tag):
        if self._skip:
            if tag == self._skip[-1]:
                self._skip.pop()
            return
        if tag in self.BLOCK:
            self.parts.append("\n")

    def handle_data(self, data):
        if not self._skip:
            self.parts.append(data)


def html_to_text(html: str) -> str:
    parser = _HtmlText()
    parser.feed(html)
    parser.close()
    return "".join(parser.parts)


def strip_quotes(text: str) -> str:
    """Drop '>' lines, the quoted letter below a reply header and the signature below '-- '."""
    kept = []
    for line in text.splitlines():
        if REPLY_HEADER.match(line) or SIGNATURE.match(line.rstrip()):
            break
        if not line.lstrip().startswith(">"):
            kept.append(line)
    return "\n".join(kept)


def tidy(text: str) -> str:
    text = INVISIBLE.sub("", text)
    lines = [" ".join(line.replace("\xa0", " ").split()) for line in text.splitlines()]
    return re.sub(r"\n{3,}", "\n\n", "\n".join(lines)).strip()


def _part_text(part) -> str:
    try:
        return part.get_content()
    except (LookupError, UnicodeDecodeError, KeyError):  # unknown or lying charset
        payload = part.get_payload(decode=True) or b""
        return payload.decode("utf-8", errors="replace")


def body_text(msg: EmailMessage) -> str:
    """Plain text of the letter: text/plain if it says something, else the HTML part as text."""
    plain = msg.get_body(preferencelist=("plain",))
    html = msg.get_body(preferencelist=("html",))
    text = tidy(strip_quotes(_part_text(plain))) if plain is not None else ""
    if len(text) < 20 and html is not None:
        text = tidy(strip_quotes(html_to_text(_part_text(html))))
    return text


@dataclass(frozen=True)
class Letter:
    uid: int
    sender: str                 # display name, else the address
    subject: str
    date: dt.datetime           # aware
    text: str
    files: tuple[str, ...] = ()

    def fields(self) -> dict:
        """The message the batches read: the text cut at MAX_BODY, the attachments by name."""
        return {"sender_id": None, "sender_name": self.sender or "?", "at": self.date, "text": self.text[:MAX_BODY],
                "files": [file_entry(name) for name in self.files], "subject": self.subject}


def file_entry(name: str) -> dict:
    """An attachment as the chats' files are listed: only the name; mail attachments are not downloaded."""
    kind = "pdf" if name.lower().endswith(".pdf") else "document"
    return {"kind": kind, "name": name, "mime": None, "size": None}


def parse(raw: bytes, uid: int = 0, fallback_date: dt.datetime | None = None) -> Letter:
    msg = email.message_from_bytes(raw, policy=email.policy.default)
    sender = ""
    try:
        addresses = msg["From"].addresses if msg["From"] else ()
        if addresses:
            sender = addresses[0].display_name or addresses[0].addr_spec
    except (AttributeError, IndexError, ValueError):
        sender = str(msg.get("From", ""))
    try:
        date = parsedate_to_datetime(str(msg["Date"]))
        if date.tzinfo is None:
            date = date.replace(tzinfo=dt.UTC)
    except (TypeError, ValueError):
        date = fallback_date or clock.now_utc()
    files = tuple(p.get_filename() for p in msg.iter_attachments() if p.get_filename())
    return Letter(uid, " ".join(sender.split()), " ".join(str(msg.get("Subject", "")).split()), date,
                  body_text(msg), files)


# --- IMAP -----------------------------------------------------------------------


def _imap_date(day: dt.date) -> str:
    return f"{day.day:02d}-{MONTHS[day.month - 1]}-{day.year}"


def fetch(host: str, port: int, user: str, password: str, folder: str, uidvalidity: int | None,
          last: int | None, since: dt.date) -> tuple[int, int, list[tuple[int, bytes]]]:
    """Blocking. Returns (uidvalidity, highest UID in the folder, [(uid, raw letter)]) of letters to read."""
    with imaplib.IMAP4_SSL(host, port, timeout=TIMEOUT) as imap:
        imap.login(user, password)
        typ, _ = imap.select(f'"{folder}"', readonly=True)
        if typ != "OK":
            raise imaplib.IMAP4.error(f"cannot open folder {folder}")
        current = int(imap.response("UIDVALIDITY")[1][0])
        typ, data = imap.status(f'"{folder}"', "(UIDNEXT)")
        top = int(re.search(rb"UIDNEXT (\d+)", data[0])[1]) - 1
        if current == uidvalidity and last is not None:
            if top <= last:
                return current, top, []
            criteria = f"UID {last + 1}:*"
        else:
            criteria = f"SINCE {_imap_date(since)}"
        typ, data = imap.uid("SEARCH", None, criteria)
        uids = sorted(int(u) for u in (data[0] or b"").split())
        if current == uidvalidity and last is not None:
            uids = [u for u in uids if u > last]  # "n:*" always matches the newest letter
        letters = []
        for uid in uids:
            typ, data = imap.uid("FETCH", str(uid), "(BODY.PEEK[])")
            raw = next((d[1] for d in data if isinstance(d, tuple)), None)
            if typ == "OK" and raw:
                letters.append((uid, raw))
        return current, max([top] + uids), letters


# --- the source ---------------------------------------------------------------------


class Mail:
    def __init__(self, env: Env, store: Store, settings: Settings, inbox: Inbox,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.host, self.port, self.user, self._password = env.mail_host, env.mail_port, env.mail_user, \
            env.mail_password
        self.store, self.db, self.settings, self.inbox, self.now = store, store.db, settings, inbox, now
        self.fetcher: Callable = fetch  # replaced in tests
        self.notify: Callable[[Phrase], None] | None = None  # a line for the System topic
        self.ok_at: str | None = None
        self.error: str | None = None if self.configured else NOT_SET
        self._failing_since: dt.datetime | None = None
        self._told = False

    @property
    def configured(self) -> bool:
        return bool(self.user and self._password)

    def status(self) -> dict:
        """Never the password. `last`: the highest UID seen; `letters`: those stored (14 days)."""
        state = self.store.get_json(STATE_KEY) or {}
        return {"configured": self.configured, "ok_at": self.ok_at, "error": self.error, "last": state.get("last"),
                "letters": self.inbox.letters()}

    async def run(self) -> None:
        if not self.configured:
            log.info("%s: no mail", NOT_SET)
            return
        while True:
            try:
                await self.poll()
            except Exception:
                log.exception("mail poll failed")
            await asyncio.sleep(POLL_EVERY)

    async def poll(self) -> int:
        """One look into the box; returns how many letters were stored."""
        state = self.store.get_json(STATE_KEY) or {}
        since = clock.local_day(self.now() - self._ttl())
        try:
            uidvalidity, top, letters = await asyncio.to_thread(
                self.fetcher, self.host, self.port, self.user, self._password, FOLDER,
                state.get("uidvalidity"), state.get("last"), since)
        except (imaplib.IMAP4.error, OSError, ValueError, TypeError, IndexError) as e:
            self._failed(self._hide(f"{type(e).__name__}: {e}"))
            return 0
        first = uidvalidity != state.get("uidvalidity") or state.get("last") is None
        saved = self.take(state, uidvalidity, top, letters, first)
        self._recovered()
        if saved:
            log.info("mail: %d new letters", saved)
        return saved

    def take(self, state: dict, uidvalidity: int, top: int, letters: list[tuple[int, bytes]], first: bool) -> int:
        """Store the letters, on a first poll only those of the last day, and the new state in the same transaction.
        Returns how many were stored."""
        base = self._base(state, uidvalidity)
        cutoff = self.now() - self._ttl()
        rows = []
        for uid, raw in letters:
            try:
                letter = parse(raw, uid, fallback_date=self.now())
            except Exception as e:  # one broken letter must not block the rest
                log.warning("mail: cannot read letter %s: %s", uid, e)
                continue
            if first and letter.date < cutoff:
                continue
            rows.append((base + uid, letter.fields()))
        new_state = {"uidvalidity": uidvalidity, "last": top, "base": base}
        with self.db:
            saved = sum(self.inbox.add_letter(msg_id, fields) for msg_id, fields in rows)
            self.db.execute("insert into kv(key, value) values(?, ?) "
                            "on conflict(key) do update set value = excluded.value",
                            (STATE_KEY, json.dumps(new_state)))
        return saved

    def _base(self, state: dict, uidvalidity: int) -> int:
        """The offset of msg_ids under this UIDVALIDITY: kept while it stays, past every msg_id given when it
        changes (the UIDs of a rebuilt box start anew)."""
        if state.get("uidvalidity") == uidvalidity:
            return state.get("base", 0)
        given = state.get("base", 0) + (state.get("last") or 0) if state.get("uidvalidity") is not None else 0
        return max(given, self.inbox.newest_letter())

    def _ttl(self) -> dt.timedelta:
        return dt.timedelta(hours=self.settings.get("news_ttl_hours"))

    def _hide(self, text: str) -> str:
        """An error's text without the password (the user name is no secret)."""
        return text.replace(self._password, HIDDEN) if self._password else text

    def _failed(self, error: str) -> None:
        self.error = error
        log.warning("mail poll failed: %s", error)
        now = self.now()
        self._failing_since = self._failing_since or now
        if not self._told and now - self._failing_since >= STALE_AFTER:
            self._told = True
            if self.notify is not None:
                since = self._failing_since.astimezone(clock.TZ)
                self.notify(Phrase(f"The mailbox is unavailable since {since:%d.%m %H:%M}: {error}.\nNew letters are "
                                   "not read; the read ones stay in the news and the deadlines.",
                                   f"Почта недоступна с {since:%d.%m %H:%M}: {error}.\n"
                                   "Новые письма не читаются; прочитанные остаются в новостях и дедлайнах."))

    def _recovered(self) -> None:
        if self._told and self.notify is not None:
            self.notify(Phrase("The mailbox is available again.", "Почта снова доступна."))
        self.ok_at, self.error = clock.local_iso(self.now()), None
        self._failing_since, self._told = None, False
