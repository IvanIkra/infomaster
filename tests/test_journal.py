import datetime as dt

from conftest import spend


def test_totals_by_task_and_chat(journal, now):
    spend(journal, 0.02, now(), task="batch", chat_id=-1001)
    spend(journal, 0.03, now(), task="batch", chat_id=-1002)
    spend(journal, 0.01, now(), task="subjects")
    journal.add({**journal.runs(1)[0], "ok": False, "error": "boom", "cost_usd": 0.0})
    totals = journal.totals(now() - dt.timedelta(hours=1))
    assert totals["runs"] == 4 and totals["failed"] == 1
    assert totals["cost_usd"] == 0.06
    assert totals["by_task"]["batch"]["runs"] == 2
    assert set(totals["by_chat"]) == {-1001, -1002}


def test_empty(journal, now):
    totals = journal.totals(now())
    assert totals == {"runs": 0, "failed": 0, "cost_usd": 0.0, "tokens": 0, "by_task": {}, "by_chat": {}}


def test_usage_periods(journal, now):
    # now: Tuesday 2026-10-20, noon in Moscow
    spend(journal, 1.0, now())
    spend(journal, 2.0, now() - dt.timedelta(days=1))   # Monday: this week
    spend(journal, 4.0, now() - dt.timedelta(days=3))   # Saturday: this month
    spend(journal, 8.0, now() - dt.timedelta(days=30))  # September
    usage = journal.usage(now())
    assert [usage[p]["cost_usd"] for p in ("today", "week", "month")] == [1.0, 3.0, 7.0]


def test_daily(journal, now):
    """Moscow days: 00:30 in Moscow is still the evening before in UTC, and belongs to its Moscow day."""
    spend(journal, 0.10, now())
    journal.add({**journal.runs(1)[0], "ok": False, "error": "boom", "cost_usd": 0.0})
    spend(journal, 0.20, now().replace(hour=21, minute=30))         # 00:30 Moscow, Oct 21: tomorrow, not shown
    spend(journal, 0.30, now() - dt.timedelta(hours=11, minutes=30))  # 00:30 Moscow, Oct 20: today
    spend(journal, 0.40, now() - dt.timedelta(days=2))
    spend(journal, 9.00, now() - dt.timedelta(days=7))               # before the week asked for
    days = journal.daily(now(), days=7)
    assert [d["day"] for d in days] == [f"2026-10-{n}" for n in range(14, 21)]
    assert days[-1] == {"day": "2026-10-20", "runs": 3, "failed": 1, "cost_usd": 0.4}
    assert days[-3]["cost_usd"] == 0.4 and days[0] == {"day": "2026-10-14", "runs": 0, "failed": 0, "cost_usd": 0.0}
