import datetime as dt

import pytest
from conftest import spend

from infomaster.llm.budget import BudgetExceeded, Level, cost_usd

HAIKU = {"input": 0.10, "output": 0.50, "long_input": 0.50, "long_output": 2.50}


def test_cost():
    assert cost_usd(HAIKU, 100_000, 0) == pytest.approx(0.01)
    assert cost_usd(HAIKU, 0, 1_000_000) == pytest.approx(0.50)
    assert cost_usd(HAIKU, 0, 0, cache_write=100_000) == pytest.approx(0.0125)
    # over 100K tokens of prompt, cache included: the long rates
    assert cost_usd(HAIKU, 50_000, 0, cache_read=60_000) == pytest.approx((50_000 + 6_000) * 0.50 / 1e6)
    assert cost_usd(HAIKU, 1_000_000, 0) == pytest.approx(0.50)


def test_estimate_is_pessimistic():
    # 2 chars per token: 10 000 chars -> 5 000 tokens in, plus the whole max_tokens out
    assert cost_usd(HAIKU, 5_000, 8_000) == pytest.approx(0.0045)


def test_reserve_counts_until_release(budget):
    ticket = budget.reserve("batch", 0.30)
    assert budget.spent() == pytest.approx(0.30)
    budget.release(ticket)
    assert budget.spent() == 0
    assert budget.notices == []


def test_soft_level_stops_optional_tasks(budget, journal, now):
    spend(journal, 0.41, now())
    ticket = budget.reserve("batch", 0.01)
    budget.release(ticket)
    assert budget.level() is Level.SOFT
    assert [n.level for n in budget.notices] == [Level.SOFT]
    with pytest.raises(BudgetExceeded):
        budget.reserve("image", 0.001)
    budget.release(budget.reserve("batch", 0.01))  # required tasks go on
    assert len(budget.notices) == 1  # announced once a day


def test_required_call_that_does_not_fit_stops_everything(budget, journal, now):
    spend(journal, 0.30, now())
    with pytest.raises(BudgetExceeded):
        budget.reserve("batch", 0.25)
    assert budget.level() is Level.HARD
    assert [n.level for n in budget.notices] == [Level.HARD]
    with pytest.raises(BudgetExceeded):
        budget.reserve("subjects", 0.001)


def test_optional_call_that_does_not_fit_stops_nothing(budget, journal, now):
    spend(journal, 0.10, now())
    with pytest.raises(BudgetExceeded):
        budget.reserve("pdf", 0.45)
    assert budget.level() is Level.OK
    assert budget.notices == []


def test_spent_by_journal_reaches_hard(budget, journal, now):
    ticket = budget.reserve("batch", 0.01)
    spend(journal, 0.60, now())  # the real cost came out above the estimate
    budget.release(ticket)
    assert budget.level() is Level.HARD
    assert budget.notices[-1].level is Level.HARD


def test_raise_today(budget, journal, now):
    spend(journal, 0.45, now())
    with pytest.raises(BudgetExceeded):
        budget.reserve("batch", 0.10)
    assert budget.raise_today(0.50) == pytest.approx(1.00)
    assert budget.level() is Level.OK
    budget.release(budget.reserve("batch", 0.10))
    assert budget.status()["raised_usd"] == 0.5


def test_new_day_starts_clean(budget, journal, now):
    spend(journal, 0.60, now())
    budget.raise_today(0.50)
    with pytest.raises(BudgetExceeded):
        budget.reserve("batch", 0.50)
    now.moment += dt.timedelta(hours=12)  # past Moscow midnight
    assert budget.limit() == 0.50
    assert budget.level() is Level.OK
    budget.release(budget.reserve("batch", 0.10))


def test_day_is_moscow_day(budget, journal, now):
    now.moment = dt.datetime(2026, 10, 20, 21, 30, tzinfo=dt.UTC)  # 00:30 on the 21st in Moscow
    spend(journal, 0.20, dt.datetime(2026, 10, 20, 20, 59, tzinfo=dt.UTC))  # 23:59 on the 20th
    spend(journal, 0.05, dt.datetime(2026, 10, 20, 21, 1, tzinfo=dt.UTC))
    assert budget.spent() == pytest.approx(0.05)


def test_stopped_days_and_a_changed_limit(budget, journal, now):
    spend(journal, 0.30, now())
    with pytest.raises(BudgetExceeded):
        budget.reserve("batch", 0.25)
    assert budget.stopped_days() == {"2026-10-20": 0.5}
    budget.settings.set("llm_daily_limit_usd", 1.0)  # the Mini App's limit: calls run again at once
    assert budget.level() is Level.HARD
    budget.limit_changed()
    assert budget.level() is Level.OK and budget.stopped_days() == {"2026-10-20": 0.5}  # the day stays stopped
    budget.release(budget.reserve("batch", 0.25))
    now.moment += dt.timedelta(days=1)
    spend(journal, 0.95, now())
    ticket = budget.reserve("batch", 0.04)
    spend(journal, 0.05, now())  # the call cost more than it reserved: the limit is reached after it
    budget.release(ticket)
    assert budget.level() is Level.HARD and budget.stopped_days() == {"2026-10-20": 0.5, "2026-10-21": 1.0}
