"""Daily LLM limit in dollars for all calls together (§7, D21, D27). Days are Moscow days.

Before a call the worst case is reserved: input estimated from the text length plus max_tokens of output.
A reserve that does not fit what is left is refused. After the answer the journal holds the real cost and the
reserve is dropped.

- 80 % of the limit: optional tasks stop (images, whole PDFs, the daily summary).
- 100 %, or a required call that does not fit: every call stops until midnight or until the limit is raised.

Days that stopped are remembered with their limit (kv "llm.days", the last DAYS_KEPT days) for the chart of the
«Расход LLM» screen in the Mini App.
"""

import datetime as dt
import enum
import itertools
import math
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from .. import clock
from ..settings import Settings
from ..store import Store
from .journal import Journal

LONG_PROMPT = 100_000           # tokens; longer prompts are billed at the long rates
CACHE_WRITE, CACHE_READ = 1.25, 0.1  # times the input price
CHARS_PER_TOKEN = 2             # pessimistic for Russian text: the reserve must not come out below the real cost
PAGE_TOKENS = 3000              # a PDF page given as a document: its image and its text, the high end of the docs
IMAGE_TOKENS = 4800             # an image: the most a high-resolution model takes for one (2576 px on the long edge)
SOFT_SHARE = 0.8
DAYS_KEPT = 40
OPTIONAL_TASKS = frozenset({"image", "pdf", "summary"})


class Level(enum.StrEnum):
    OK = "ok"
    SOFT = "soft"               # optional tasks are off
    HARD = "hard"               # every call waits for midnight


@dataclass(frozen=True)
class Notice:
    level: Level
    spent: float
    limit: float


class BudgetExceeded(RuntimeError):
    """The call was not made; the caller keeps its input for later."""


class DoesNotFit(BudgetExceeded):
    """The call alone is too big for what is left today; other calls still run."""


def cost_usd(price: dict, input_tokens: int, output_tokens: int, cache_write: int = 0, cache_read: int = 0) -> float:
    """`price` is one model's entry of the llm_prices setting, $ per million tokens."""
    long = input_tokens + cache_write + cache_read > LONG_PROMPT
    price_in, price_out = (price["long_input"], price["long_output"]) if long else (price["input"], price["output"])
    total = (input_tokens + cache_write * CACHE_WRITE + cache_read * CACHE_READ) * price_in + output_tokens * price_out
    return total / 1_000_000


class Budget:
    def __init__(self, settings: Settings, journal: Journal, store: Store,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.settings, self.journal, self.store, self.now = settings, journal, store, now
        self.notify: Callable[[Notice], None] | None = None  # crossing 80 % or stopping, once a day each
        self._reserved: dict[int, float] = {}
        self._tickets = itertools.count(1)

    def price(self, model: str) -> dict | None:
        return self.settings.get("llm_prices").get(model)

    @staticmethod
    def estimate(price: dict, input_chars: int, max_tokens: int) -> float:
        return cost_usd(price, math.ceil(input_chars / CHARS_PER_TOKEN), max_tokens)

    def limit(self) -> float:
        return self.settings.get("llm_daily_limit_usd") + self._today("llm.raised", 0.0)

    def spent(self) -> float:
        """Today's real cost plus what calls in flight have reserved."""
        return self.journal.cost_since(clock.day_start_utc(self._day())) + sum(self._reserved.values())

    def level(self) -> Level:
        if Level.HARD in self._today("llm.notified", []):
            return Level.HARD
        spent, limit = self.spent(), self.limit()
        if spent >= limit:
            return Level.HARD
        return Level.SOFT if spent >= SOFT_SHARE * limit else Level.OK

    def reserve(self, task: str, cost: float, asked: bool = False) -> int:
        """A ticket for release(), or BudgetExceeded. `asked`: a call the owner asked for (/summary). It runs above
        80 % like a required task, but one that does not fit what is left is refused alone (DoesNotFit) instead of
        stopping every call until midnight."""
        level = self.level()
        if level is Level.HARD:
            raise BudgetExceeded("the daily LLM limit is spent, calls wait for midnight")
        optional = task in OPTIONAL_TASKS and not asked
        if optional and level is Level.SOFT:
            raise BudgetExceeded(f"optional LLM tasks are off above {SOFT_SHARE:.0%} of the daily limit")
        if self.spent() + cost > self.limit():
            if optional or asked:
                raise DoesNotFit("the call does not fit what is left of the daily LLM limit")
            self._announce(Level.HARD)
            raise BudgetExceeded("the daily LLM limit is spent, calls wait for midnight")
        ticket = next(self._tickets)
        self._reserved[ticket] = cost
        return ticket

    def release(self, ticket: int) -> None:
        """After the call is in the journal: the real cost replaces the reserve."""
        self._reserved.pop(ticket, None)
        level = self.level()
        if level is not Level.OK:
            self._announce(level)

    def raise_today(self, usd: float) -> float:
        """The button in the System topic: more money until midnight. Returns today's new limit."""
        self._put_today("llm.raised", self._today("llm.raised", 0.0) + usd)
        self.limit_changed()
        return self.limit()

    def limit_changed(self) -> None:
        """After the limit of today changed: calls run again if it now leaves room, and reaching it is announced
        again."""
        self._put_today("llm.notified", [Level.SOFT] if self.spent() >= SOFT_SHARE * self.limit() else [])

    def stopped_days(self) -> dict[str, float]:
        """Moscow days (ISO) when every call stopped, with the limit of that day."""
        return self.store.get_json("llm.days", {})

    def status(self) -> dict:
        return {"spent_usd": round(self.spent(), 4), "limit_usd": round(self.limit(), 4),
                "raised_usd": round(self._today("llm.raised", 0.0), 4), "level": self.level().value}

    def _announce(self, level: Level) -> None:
        notified = self._today("llm.notified", [])
        if level in notified:
            return
        reached = [Level.SOFT, Level.HARD] if level is Level.HARD else [Level.SOFT]
        self._put_today("llm.notified", sorted(set(notified) | set(reached)))
        if level is Level.HARD:
            days = {**self.stopped_days(), self._day().isoformat(): round(self.limit(), 4)}
            self.store.put_json("llm.days", dict(sorted(days.items())[-DAYS_KEPT:]))
        if self.notify is not None:
            self.notify(Notice(level, self.spent(), self.limit()))

    def _day(self) -> dt.date:
        return clock.local_day(self.now())

    def _today(self, key: str, default: Any) -> Any:
        saved = self.store.get_json(key)
        return saved["value"] if saved and saved["day"] == self._day().isoformat() else default

    def _put_today(self, key: str, value: Any) -> None:
        self.store.put_json(key, {"day": self._day().isoformat(), "value": value})
