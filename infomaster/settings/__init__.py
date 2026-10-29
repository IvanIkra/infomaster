"""General and chat settings (§5.1, §10): kept in SQLite, edited in the Mini App. A setting never set has its default.

General settings arrive with the stages that use them. Chat settings are all here from the start, so the Mini App can
set a chat up once; the twin uses `enabled`, the batches `study`, `filter`, `people`, `urgent`, `batch_hours` and
`images`, the sheets `pdf_content`, the summaries `summary` (and `filter` and `people`).
"""

import copy
import json
import re
from collections.abc import Callable
from typing import Any

from ..i18n import LANGUAGES
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
SHEET_NAME_CHARS = 60
SHEET_LABEL_CHARS = 10  # the course in front of a grade's label: "DM HW 1A" (Tablo's budget of 23 is for both)


def _student(value: Any) -> str:
    """The name as the grade sheets have it, "Иванов Иван"; "" turns the grades off."""
    if not isinstance(value, str) or len(value) > 100:
        raise SettingsError("expected the student's name as the sheets have it, at most 100 characters")
    return " ".join(value.split())


def _grade_sheets(value: Any) -> list[dict]:
    """[{url, label, name?}]: Google Sheets open to anyone with the link; label is the course's short name, "" for
    the start of the sheet's title; name is the owner's for the Mini App (D44), kept only when given."""
    if not isinstance(value, list):
        raise SettingsError("expected a list of {url, label}")
    sheets, ids = [], set()
    for sheet in value:
        if not isinstance(sheet, dict) or set(sheet) - {"url", "label", "name"} or \
                not isinstance(sheet.get("url"), str) or not isinstance(sheet.get("label", ""), str) or \
                not isinstance(sheet.get("name", ""), str):
            raise SettingsError("expected a list of {url, label}")
        url = sheet["url"].strip()
        found = GOOGLE_SHEET.search(url)
        if not url.startswith("https://docs.google.com/") or found is None:
            raise SettingsError("expected a link to a Google Sheet: https://docs.google.com/spreadsheets/d/…")
        if found[1] in ids:
            raise SettingsError("the same sheet is listed twice")
        ids.add(found[1])
        label = " ".join(sheet.get("label", "").split())
        if len(label) > SHEET_LABEL_CHARS:
            raise SettingsError(f"a course label has at most {SHEET_LABEL_CHARS} characters, got {label!r}")
        name = " ".join(sheet.get("name", "").split())
        if len(name) > SHEET_NAME_CHARS:
            raise SettingsError(f"a sheet's name has at most {SHEET_NAME_CHARS} characters")
        sheets.append({"url": url, "label": label, **({"name": name} if name else {})})
    return sheets


def _flag(value: Any) -> bool:
    if not isinstance(value, bool):
        raise SettingsError(f"expected true or false, got {value!r}")
    return value


def _language(value: Any) -> str:
    if value not in LANGUAGES:
        raise SettingsError(f"expected one of {', '.join(LANGUAGES)}, got {value!r}")
    return value


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
    "summary_time": ("21:00", _clock_time),   # the daily summary, Moscow time (D20)
    # Grades from teachers' Google Sheets (§6.7, Tablo's §3.2): the own row is found by the name.
    "grades_student": ("", _student),
    "grades_sheets": ([], _grade_sheets),
    "grades_window_days": (7, _whole(1, 60, "a number of days")),  # a grade is new this long: the API for Tablo
    # What the owner reads (D41): the bot's posts and the models' summaries for them, and the Mini App.
    "forum_language": ("en", _language),
    "miniapp_language": ("en", _language),
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
    "summary": (False, _flag),
    "urgent": (False, _flag),
    "batch_hours": (None, _batch_hours),
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
