"""Journal of LLM calls (§7, D22): every call, also a failed one, with tokens, cost and duration."""

import datetime as dt

from .. import clock
from ..store import Store

FIELDS = ("at", "task", "chat_id", "model", "ok", "error", "input_tokens", "output_tokens", "cache_write",
          "cache_read", "cost_usd", "duration_s")


class Journal:
    def __init__(self, store: Store):
        self.db = store.db

    def add(self, run: dict) -> None:
        with self.db:
            self.db.execute(f"insert into llm_runs({', '.join(FIELDS)}) values({', '.join('?' * len(FIELDS))})",
                            [run.get(f) for f in FIELDS])

    def runs(self, limit: int = 100, task: str | None = None) -> list[dict]:
        """Newest first."""
        where, args = ("where task = ?", [task]) if task else ("", [])
        rows = self.db.execute(f"select * from llm_runs {where} order by id desc limit ?", [*args, limit])
        return [{**{f: row[f] for f in FIELDS}, "ok": bool(row["ok"])} for row in rows]

    def cost_since(self, since: dt.datetime) -> float:
        row = self.db.execute("select coalesce(sum(cost_usd), 0) cost from llm_runs where at >= ?",
                              (clock.utc_iso(since),)).fetchone()
        return row["cost"]

    def count_since(self, since: dt.datetime, task: str) -> int:
        """Calls of a task since a moment, failed ones too: the daily number of images counts them (D12)."""
        row = self.db.execute("select count(*) n from llm_runs where at >= ? and task = ?",
                              (clock.utc_iso(since), task)).fetchone()
        return row["n"]

    def totals(self, since: dt.datetime) -> dict:
        """Runs, cost and tokens since a moment, overall and by task and chat (for the Mini App, §8.4)."""
        sums = ("count(*) runs, coalesce(sum(ok = 0), 0) failed, coalesce(sum(cost_usd), 0) cost, "
                "coalesce(sum(input_tokens + output_tokens + cache_write + cache_read), 0) tokens")
        since_utc = clock.utc_iso(since)

        def entry(row) -> dict:
            return {"runs": row["runs"], "failed": row["failed"], "cost_usd": round(row["cost"], 6),
                    "tokens": row["tokens"]}

        def by(column: str) -> dict:
            rows = self.db.execute(f"select {column} k, {sums} from llm_runs where at >= ? and {column} is not null "
                                   f"group by {column} order by cost desc", (since_utc,))
            return {row["k"]: entry(row) for row in rows}

        overall = self.db.execute(f"select {sums} from llm_runs where at >= ?", (since_utc,)).fetchone()
        return {**entry(overall), "by_task": by("task"), "by_chat": by("chat_id")}

    def usage(self, now: dt.datetime) -> dict:
        """Today, this week (from Monday) and this month, Moscow days."""
        today = clock.local_day(now)
        starts = {"today": today, "week": today - dt.timedelta(days=today.weekday()), "month": today.replace(day=1)}
        return {name: self.totals(clock.day_start_utc(day)) for name, day in starts.items()}

    def daily(self, now: dt.datetime, days: int = 30) -> list[dict]:
        """Cost, runs and failed runs of each of the last `days` Moscow days, today last; days without runs too."""
        today = clock.local_day(now)
        first = today - dt.timedelta(days=days - 1)
        result = {first + dt.timedelta(days=n): {"runs": 0, "failed": 0, "cost_usd": 0.0} for n in range(days)}
        rows = self.db.execute("select at, ok, cost_usd from llm_runs where at >= ?",
                               (clock.utc_iso(clock.day_start_utc(first)),))
        for row in rows:
            day = result.get(clock.local_day(dt.datetime.fromisoformat(row["at"])))
            if day is not None:
                day["runs"] += 1
                day["failed"] += not row["ok"]
                day["cost_usd"] += row["cost_usd"]
        return [{"day": day.isoformat(), **{**values, "cost_usd": round(values["cost_usd"], 6)}}
                for day, values in result.items()]
