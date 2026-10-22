"""Time rules (§9): the database keeps Moscow local time without a zone, except llm_runs.at in UTC."""

import datetime as dt
from zoneinfo import ZoneInfo

TZ = ZoneInfo("Europe/Moscow")


def now_utc() -> dt.datetime:
    return dt.datetime.now(dt.UTC)


def local_day(moment: dt.datetime) -> dt.date:
    """The Moscow date of an aware moment: days, weeks and the LLM limit are counted by it."""
    return moment.astimezone(TZ).date()


def day_start_utc(day: dt.date) -> dt.datetime:
    """Moscow midnight that starts `day`, as UTC."""
    return dt.datetime.combine(day, dt.time(), TZ).astimezone(dt.UTC)


def utc_iso(moment: dt.datetime) -> str:
    """One format for every UTC column, so plain string comparison orders them."""
    return moment.astimezone(dt.UTC).isoformat(timespec="seconds")


def local_iso(moment: dt.datetime) -> str:
    """Moscow local time without a zone: the format of every column except llm_runs.at (§9)."""
    return moment.astimezone(TZ).replace(tzinfo=None).isoformat(timespec="seconds")
