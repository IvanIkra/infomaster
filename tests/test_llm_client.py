from types import SimpleNamespace

import pytest

from uni_master.llm.budget import BudgetExceeded
from uni_master.llm.client import Haiku, LLMError

SCHEMA = {"type": "object", "properties": {"x": {"type": "integer"}}, "required": ["x"]}


def answer(text: str, stop_reason: str = "end_turn", output_tokens: int = 100):
    usage = SimpleNamespace(input_tokens=1000, output_tokens=output_tokens, cache_creation_input_tokens=0,
                            cache_read_input_tokens=0)
    return SimpleNamespace(usage=usage, stop_reason=stop_reason, stop_details=None,
                           content=[SimpleNamespace(type="text", text=text)])


class FakeMessages:
    def __init__(self, response):
        self.response, self.calls = response, []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        return self.response


@pytest.fixture
def haiku(budget, journal, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test")

    def make(response):
        llm = Haiku(budget, journal)
        llm.messages = FakeMessages(response)
        llm._client = SimpleNamespace(messages=llm.messages)
        return llm

    return make


async def test_ok_call_is_logged_and_released(haiku, budget, journal):
    llm = haiku(answer('{"x": 1}'))
    assert await llm.json("batch", "sys", "data", SCHEMA, chat_id=-100) == {"x": 1}
    call = llm.messages.calls[0]
    assert call["model"] == "claude-haiku-5-5"
    assert call["output_config"]["format"] == {"type": "json_schema", "schema": SCHEMA}
    run = journal.runs()[0]
    assert run["ok"] and run["task"] == "batch" and run["chat_id"] == -100
    assert run["cost_usd"] == pytest.approx((1000 * 0.10 + 100 * 0.50) / 1e6)
    assert budget.spent() == pytest.approx(run["cost_usd"])  # the reserve is gone


async def test_cut_answer_is_logged_as_failure(haiku, budget, journal):
    llm = haiku(answer('{"x": ', stop_reason="max_tokens", output_tokens=8000))
    with pytest.raises(LLMError, match="max_tokens"):
        await llm.json("batch", "sys", "data", SCHEMA)
    run = journal.runs()[0]
    assert not run["ok"] and run["output_tokens"] == 8000
    assert budget.spent() == pytest.approx(run["cost_usd"])


async def test_budget_refusal_makes_no_call(haiku, budget, journal):
    budget.settings.set("llm_daily_limit_usd", 0.001)
    llm = haiku(answer('{"x": 1}'))
    with pytest.raises(BudgetExceeded):
        await llm.json("batch", "sys", "data", SCHEMA)
    assert llm.messages.calls == [] and journal.runs() == []


async def test_no_key(haiku, monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY")
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    with pytest.raises(LLMError, match="not set"):
        await haiku(answer("{}")).json("batch", "sys", "data", SCHEMA)


async def test_unknown_task(haiku):
    with pytest.raises(ValueError):
        await haiku(answer("{}")).json("news", "sys", "data", SCHEMA)
