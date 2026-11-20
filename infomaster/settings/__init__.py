"""General and chat settings (§5.1, §10): kept in SQLite, edited in the Mini App. A setting never set has its default.

General settings arrive with the stages that use them. Chat settings are all here from the start, so the Mini App can
set a chat up once; the twin uses `enabled`, the batches `study`, `filter`, `people`, `urgent`, `batch_hours` and
`images` and `note`, the sheets `pdf_content`. A saved setting no longer listed here (the summaries' `summary_time`
and `summary`, removed with them, D55) stays in its table and is never read.
"""

import copy
import json
import re
from collections.abc import Callable
from typing import Any
from urllib.parse import unquote, urlsplit

from ..i18n import LANGUAGES, Phrase
from ..store import Store


class SettingsError(ValueError):
    pass


def _money(value: Any) -> float:
    if isinstance(value, bool) or not isinstance(value, int | float) or value < 0:
        raise SettingsError(f"expected a non-negative number of dollars, got {value!r}")
    return float(value)


def _positive_money(value: Any) -> float:
    value = _money(value)
    if value == 0:
        raise SettingsError("the limit must be above zero")
    return value


PRICE_KEYS = ("input", "output", "long_input", "long_output")  # $ per million tokens; long: prompts over 100K


def _prices(value: Any) -> dict:
    if not isinstance(value, dict) or not value:
        raise SettingsError("expected {model: {input, output, long_input, long_output}}")
    prices = {}
    for model, price in value.items():
        if not isinstance(price, dict) or set(price) != set(PRICE_KEYS):
            raise SettingsError(f"{model}: expected exactly {', '.join(PRICE_KEYS)}")
        prices[model] = {key: _money(price[key]) for key in PRICE_KEYS}
    return prices


BATCH_HOURS = (1, 2, 3, 4, 6, 8, 12, 24)  # batches are tied to the hours of the day (D19), so N divides 24


def _general_batch_hours(value: Any) -> int:
    if isinstance(value, bool) or value not in BATCH_HOURS:
        raise SettingsError(f"expected one of {BATCH_HOURS}, got {value!r}")
    return value


def _hours(value: Any) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not 1 <= value <= 168:
        raise SettingsError(f"expected a whole number of hours from 1 to 168, got {value!r}")
    return value


def _whole(low: int, high: int, what: str) -> Callable[[Any], int]:
    def check(value: Any) -> int:
        if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
            raise SettingsError(f"expected {what} from {low} to {high}, got {value!r}")
        return value
    return check


def _clock_time(value: Any) -> str:
    """'21:00'; '9:30' becomes '09:30'."""
    match = re.fullmatch(r"([01]?\d|2[0-3]):([0-5]\d)", value.strip()) if isinstance(value, str) else None
    if match is None:
        raise SettingsError(f"expected a time of day HH:MM, got {value!r}")
    return f"{int(match[1]):02d}:{match[2]}"


GOOGLE_SHEET = re.compile(r"/spreadsheets/d/([A-Za-z0-9_-]+)")  # the id of a Google Sheet in its link
# A Dropbox share link to a file: /scl/fi/<id>/<name> (since 2023) or the older /s/<id>/<name>; a folder's is /scl/fo/
# or /sh/. dl.dropboxusercontent.com serves the same paths.
DROPBOX_HOSTS = frozenset({"www.dropbox.com", "dropbox.com", "dl.dropbox.com", "dl.dropboxusercontent.com"})
DROPBOX_FILE = re.compile(r"/(?:scl/fi|s)/([A-Za-z0-9_-]+)/([^/]+)/?")
DROPBOX_FOLDER = re.compile(r"/(?:scl/fo|sh)/")
DROPBOX_PREFIX = "dropbox-"  # a Dropbox sheet's id: the prefix and the id of its link, apart from Google's ids
NOT_A_SHEET = Phrase(
    "expected a link to a Google Sheet (https://docs.google.com/spreadsheets/d/…) or a Dropbox link to an .xlsx file "
    "(https://www.dropbox.com/scl/fi/…)",
    "нужна ссылка на Google Таблицу (https://docs.google.com/spreadsheets/d/…) или ссылка Dropbox на файл .xlsx "
    "(https://www.dropbox.com/scl/fi/…)")
SHEET_NAME_CHARS = 60
SHEET_LABEL_CHARS = 10  # the course in front of a grade's label: "DM HW 1A" (Tablo's budget of 23 is for both)


def _student(value: Any) -> str:
    """The name as the grade sheets have it, "Иванов Иван"; "" turns the grades off."""
    if not isinstance(value, str) or len(value) > 100:
        raise SettingsError("expected the student's name as the sheets have it, at most 100 characters")
    return " ".join(value.split())


def dropbox_file(url: str) -> tuple[str, str] | None:
    """(the link's id, the file name) of a Dropbox share link to a file; None for a link that is not Dropbox's.
    SettingsError for a folder, for a Dropbox link that is not a share link and for a file that is not .xlsx."""
    parts = urlsplit(url.strip())
    if parts.scheme != "https" or (parts.hostname or "").lower() not in DROPBOX_HOSTS:
        return None
    if DROPBOX_FOLDER.match(parts.path):
        raise SettingsError(Phrase("this is a link to a Dropbox folder: share the .xlsx file itself",
                                   "это ссылка на папку Dropbox: нужна ссылка на сам файл .xlsx"))
    found = DROPBOX_FILE.fullmatch(parts.path)
    if found is None:
        raise SettingsError(NOT_A_SHEET)
    name = unquote(found[2])
    if not name.lower().endswith(".xlsx"):
        raise SettingsError(not_xlsx(name))
    return found[1], name


def not_xlsx(name: str) -> Phrase:
    return Phrase(f"the Dropbox file «{name}» is not an .xlsx workbook", f"файл Dropbox «{name}» — не книга .xlsx")


def grade_sheet_id(url: str) -> str:
    """The id a grade sheet is known by: a Google Sheet's id, or DROPBOX_PREFIX and the id of a Dropbox link.
    SettingsError for any other link."""
    url = url.strip()
    if (file := dropbox_file(url)) is not None:
        return DROPBOX_PREFIX + file[0]
    found = GOOGLE_SHEET.search(url)
    if not url.startswith("https://docs.google.com/") or found is None:
        raise SettingsError(NOT_A_SHEET)
    return found[1]


def _grade_sheets(value: Any) -> list[dict]:
    """[{url, label, name?}]: Google Sheets open to anyone with the link, or Dropbox share links to .xlsx files;
    label is the course's short name, "" for the start of the sheet's title; name is the owner's for the Mini App
    (D44), kept only when given."""
    if not isinstance(value, list):
        raise SettingsError("expected a list of {url, label}")
    sheets, ids = [], set()
    for sheet in value:
        if not isinstance(sheet, dict) or set(sheet) - {"url", "label", "name"} or \
                not isinstance(sheet.get("url"), str) or not isinstance(sheet.get("label", ""), str) or \
                not isinstance(sheet.get("name", ""), str):
            raise SettingsError("expected a list of {url, label}")
        url = sheet["url"].strip()
        sid = grade_sheet_id(url)
        if sid in ids:
            raise SettingsError("the same sheet is listed twice")
        ids.add(sid)
        label = " ".join(sheet.get("label", "").split())
        if len(label) > SHEET_LABEL_CHARS:
            raise SettingsError(f"a course label has at most {SHEET_LABEL_CHARS} characters, got {label!r}")
        name = " ".join(sheet.get("name", "").split())
        if len(name) > SHEET_NAME_CHARS:
            raise SettingsError(f"a sheet's name has at most {SHEET_NAME_CHARS} characters")
        sheets.append({"url": url, "label": label, **({"name": name} if name else {})})
    return sheets


WIKI_PAGES = 10


def _wiki_pages(value: Any) -> list[str]:
    """Addresses of course pages of a MediaWiki (D60): http(s), each once."""
    if not isinstance(value, list) or len(value) > WIKI_PAGES or not all(isinstance(v, str) for v in value):
        raise SettingsError(f"expected a list of at most {WIKI_PAGES} page addresses")
    pages = []
    for url in (v.strip() for v in value):
        if not re.match(r"^https?://[^/\s]+/\S+$", url):
            raise SettingsError(f"not a wiki page's address: {url!r}")
        if url not in pages:
            pages.append(url)
    return pages


ABOUT_ME_CHARS = 500  # what the student tells the models about themselves
CHAT_NOTE_CHARS = 300


def _text(limit: int) -> Callable[[Any], str]:
    """Free text for the models' prompts: lines kept, spaces within them squeezed, empty lines dropped."""
    def check(value: Any) -> str:
        if not isinstance(value, str):
            raise SettingsError("expected text")
        text = "\n".join(line for line in (" ".join(line.split()) for line in value.splitlines()) if line)
        if len(text) > limit:
            raise SettingsError(f"at most {limit} characters, got {len(text)}")
        return text
    return check


def _flag(value: Any) -> bool:
    if not isinstance(value, bool):
        raise SettingsError(f"expected true or false, got {value!r}")
    return value


def _language(value: Any) -> str:
    if value not in LANGUAGES:
        raise SettingsError(f"expected one of {', '.join(LANGUAGES)}, got {value!r}")
    return value


PUSH_KINDS = ("deadlines", "reminders", "failures", "llm")  # what the web app may push (§8.6)


def _push_kinds(value: Any) -> list[str]:
    """Some of PUSH_KINDS, in their order; an empty list pushes nothing."""
    if not isinstance(value, list) or not all(isinstance(v, str) for v in value) or set(value) - set(PUSH_KINDS):
        raise SettingsError(f"expected some of {', '.join(PUSH_KINDS)}")
    return [kind for kind in PUSH_KINDS if kind in value]


def _quiet_hours(value: Any) -> str:
    """'23:00-08:00': the notifications of these hours come without a sound; '' for none."""
    if value == "":
        return ""
    if not isinstance(value, str) or value.count("-") != 1:
        raise SettingsError(f"expected HH:MM-HH:MM or nothing, got {value!r}")
    start, end = (_clock_time(part) for part in value.split("-"))
    if start == end:
        raise SettingsError("the quiet hours start and end at the same time")
    return f"{start}-{end}"


# name -> (default, check); the check returns the value to keep or raises SettingsError.
SPECS: dict[str, tuple[Any, Callable[[Any], Any]]] = {
    "llm_daily_limit_usd": (0.50, _positive_money),  # D27
    "llm_prices": ({"claude-haiku-5-5": {"input": 0.10, "output": 0.50, "long_input": 0.50, "long_output": 2.50}},
                   _prices),
    "batch_hours": (3, _general_batch_hours),  # D19
    "news_ttl_hours": (24, _hours),            # a news item lives this long; older non-study messages are skipped
    # Poll HSE App X (§5.2). One client per token chain: the refresh token rotates, so Tablo's own must be off.
    "hseapp_enabled": (True, _flag),
    # Reading a PDF whole (§6.3, D14): at most so many pages and tokens of a file, what comes first.
    "pdf_pages": (10, _whole(1, 50, "a number of pages")),
    "pdf_tokens": (30_000, _whole(1_000, 200_000, "a number of tokens")),
    # Images read a Moscow day in chats with «Картинки» (D12); inside the LLM limit, off above its 80 % (§7).
    "images_daily": (30, _whole(0, 500, "a number of images")),
    # Deadline reminders in «Дедлайны» (D47): on the eve, what is due tomorrow; in the morning, what is due today.
    "deadline_reminders": (True, _flag),
    "reminder_eve": ("20:00", _clock_time),
    "reminder_day": ("09:00", _clock_time),
    # Grades from teachers' Google Sheets (§6.7, Tablo's §3.2): the own row is found by the name.
    "grades_student": ("", _student),
    "grades_sheets": ([], _grade_sheets),
    "grades_window_days": (7, _whole(1, 60, "a number of days")),  # a grade is new this long: the API for Tablo
    # What the owner reads (D41): the bot's posts and what the models write for them, and the Mini App.
    "forum_language": ("en", _language),
    "miniapp_language": ("en", _language),
    # What the student tells the batches about themselves (D52): group, subgroup, English group,
    # electives, so that what is meant for others does not count.
    "about_me": ("", _text(ABOUT_ME_CHARS)),
    # Course pages of the faculty's wiki (D60): their new sheets and homework come as notices in the mailbox.
    "wiki_pages": ([], _wiki_pages),
    # The web app's notifications (§8.6, D65): which kinds are pushed, and the hours they come without a sound.
    "push_kinds": (list(PUSH_KINDS), _push_kinds),
    "push_quiet": ("", _quiet_hours),
}


def _filter(value: Any) -> str:
    if value not in ("all", "important"):
        raise SettingsError(f"expected all or important, got {value!r}")
    return value


def _people(value: Any) -> list[int]:
    if not isinstance(value, list) or any(isinstance(v, bool) or not isinstance(v, int) for v in value):
        raise SettingsError("expected a list of Telegram user ids")
    return sorted(set(value))


def _batch_hours(value: Any) -> int | None:
    if value is not None and (isinstance(value, bool) or value not in BATCH_HOURS):
        raise SettingsError(f"expected null (the general N) or one of {BATCH_HOURS}, got {value!r}")
    return value


CHAT_SPECS: dict[str, tuple[Any, Callable[[Any], Any]]] = {
    "enabled": (False, _flag),      # a new chat is off (D11)
    "study": (False, _flag),
    "filter": ("all", _filter),
    "people": ([], _people),
    "images": (False, _flag),
    "pdf_content": (False, _flag),
    "urgent": (False, _flag),
    "batch_hours": (None, _batch_hours),
    "note": ("", _text(CHAT_NOTE_CHARS)),  # for the batches: its course, who is who (D52)
}


class Settings:
    def __init__(self, store: Store):
        self.db = store.db

    def get(self, name: str) -> Any:
        default, _ = self._spec(name)
        row = self.db.execute("select value from settings where name = ?", (name,)).fetchone()
        return json.loads(row["value"]) if row else copy.deepcopy(default)

    def set(self, name: str, value: Any) -> Any:
        _, check = self._spec(name)
        value = check(value)
        with self.db:
            self.db.execute("insert into settings(name, value) values(?, ?) "
                            "on conflict(name) do update set value = excluded.value",
                            (name, json.dumps(value, ensure_ascii=False)))
        return value

    def reset(self, name: str) -> None:
        self._spec(name)
        with self.db:
            self.db.execute("delete from settings where name = ?", (name,))

    def all(self) -> dict[str, Any]:
        return {name: self.get(name) for name in SPECS}

    @staticmethod
    def _spec(name: str):
        if name not in SPECS:
            raise SettingsError(f"unknown setting {name!r}")
        return SPECS[name]


class ChatSettings:
    """Settings of one chat (§5.1), stored per chat and name like the general ones."""

    def __init__(self, store: Store):
        self.db = store.db

    def get(self, chat_id: int, name: str) -> Any:
        return self.all(chat_id)[self._check_name(name)]

    def all(self, chat_id: int) -> dict[str, Any]:
        saved = {row["name"]: json.loads(row["value"]) for row in self.db.execute(
            "select name, value from chat_settings where chat_id = ?", (chat_id,)) if row["name"] in CHAT_SPECS}
        return {name: saved[name] if name in saved else copy.deepcopy(default)
                for name, (default, _) in CHAT_SPECS.items()}

    def update(self, chat_id: int, values: dict[str, Any]) -> dict[str, Any]:
        """Check every value first, then save them together; returns all settings of the chat."""
        checked = {name: CHAT_SPECS[self._check_name(name)][1](value) for name, value in values.items()}
        with self.db:
            self.db.executemany("insert into chat_settings(chat_id, name, value) values(?, ?, ?) "
                                "on conflict(chat_id, name) do update set value = excluded.value",
                                [(chat_id, name, json.dumps(value, ensure_ascii=False))
                                 for name, value in checked.items()])
        return self.all(chat_id)

    def enabled(self, chat_id: int) -> bool:
        return self.get(chat_id, "enabled")

    def enabled_ids(self) -> set[int]:
        return self.flagged_ids("enabled")

    def flagged_ids(self, name: str) -> set[int]:
        """Chats where a flag is on: a flag is off by default, so only saved rows can turn it on."""
        return {row["chat_id"] for row in self.db.execute(
            "select chat_id from chat_settings where name = ? and value = 'true'", (self._check_name(name),))}

    @staticmethod
    def _check_name(name: str) -> str:
        if name not in CHAT_SPECS:
            raise SettingsError(f"unknown chat setting {name!r}")
        return name
