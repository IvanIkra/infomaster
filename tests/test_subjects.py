import asyncio

import pytest

from uni_master.llm.budget import BudgetExceeded
from uni_master.llm.client import LLMError
from uni_master.processing.subjects import Subject, SubjectError, Subjects, clean


@pytest.fixture
def subjects(store, namer):
    return Subjects(store, namer)


def test_clean():
    assert clean("  Алгебра  (рус) ") == "Алгебра"
    assert clean("Algebra") == "Algebra" and clean(None) == ""


async def test_one_call_with_the_dictionary(subjects, namer):
    got = await subjects.resolve(["Algebra", "", None])
    assert got == {"Algebra": subjects.get("Algebra")} and got["Algebra"].abbr == "Alg"
    await subjects.resolve(["Algebra", "Алгебра (рус)", "Введение в программирование"])
    assert len(namer.prompts) == 2
    assert '- "Algebra": title "Algebra", abbr "Alg"' in namer.prompts[1]  # known names keep languages consistent
    assert '- id 0: "Алгебра"\n- id 1: "Введение в программирование"' in namer.prompts[1]
    assert namer.prompts[1].startswith("Title budget: at most 13 characters.")
    await subjects.resolve(["Алгебра", "Algebra"])
    assert len(namer.prompts) == 2  # all known: no call
    assert (subjects.title("Алгебра (рус)"), subjects.abbr("Введение в программирование")) == ("Algebra", "Prog")
    assert (subjects.title("Матан"), subjects.abbr("Матан")) == ("Матан", None)  # not named yet: as the source


async def test_wide_title_gets_one_retry(subjects, namer):
    async def answer(task, system, prompt, schema, *, max_tokens):
        namer.prompts.append(prompt)
        title = "Discrete Math" if "too wide" in prompt else "Discrete Mathematics"
        return {"items": [{"id": "0", "title": title, "abbr": "DMath-long"}]}

    namer.json = answer
    got = await subjects.resolve(["Discrete Mathematics"])
    assert (got["Discrete Mathematics"].title, got["Discrete Mathematics"].abbr) == ("Discrete Math", "DMath-")
    assert 'Your previous title "Discrete Mathematics" was too wide for the screen' in namer.prompts[1]


async def test_failed_retry_keeps_the_wide_title(subjects, namer):
    calls = []

    async def answer(task, system, prompt, schema, *, max_tokens):
        calls.append(prompt)
        if len(calls) == 2:
            raise LLMError("down")
        return {"items": [{"id": "0", "title": "Discrete Mathematics", "abbr": "DM"}]}

    namer.json = answer
    assert (await subjects.resolve(["Discrete Mathematics"]))["Discrete Mathematics"].title == "Discrete Mathematics"


async def test_bad_items_are_skipped(subjects, namer):
    async def answer(task, system, prompt, schema, *, max_tokens):
        return {"items": [{"id": "-1", "title": "X", "abbr": "X"}, {"id": "7", "title": "X", "abbr": "X"},
                          {"id": "one", "title": "X", "abbr": "X"}, {"id": "0", "title": " ", "abbr": "A"},
                          {"id": "1", "title": "History", "abbr": "Hist"}]}

    namer.json = answer
    assert list(await subjects.resolve(["История", "Алгебра"])) == ["История"]  # ids follow the sorted names
    assert subjects.get("Алгебра") is None  # its title came blank: asked again next time


async def test_concurrent_resolves_ask_each_name_once(subjects, namer):
    ask = namer.json

    async def slow(*args, **kwargs):
        await asyncio.sleep(0.01)  # RUZ and a batch wait on Haiku at once, as at start-up
        return await ask(*args, **kwargs)

    namer.json = slow
    await asyncio.gather(subjects.resolve(["Algebra"]), subjects.resolve(["Algebra", "Discrete Mathematics"]))
    assert len(namer.prompts) == 2 and '- id 0: "Discrete Mathematics"' in namer.prompts[1]


async def test_manual_entries(subjects, namer):
    assert subjects.set_manual(" Алгебра (рус) ", " Linear  Alg ", "LA") == {
        "name": "Алгебра", "title": "Linear Alg", "abbr": "LA", "manual": True}
    subjects._put("Алгебра", "Algebra", "Alg", manual=False)  # Haiku never overwrites a manual entry
    assert subjects.get("Алгебра").title == "Linear Alg"
    assert await subjects.resolve(["Алгебра"]) == {"Алгебра": subjects.get("Алгебра")} and namer.prompts == []
    for bad in (("", "X", "X"), ("Матан", "", "C"), ("Матан", "C" * 41, "C"), ("Матан", "Calculus", "Calcul8")):
        with pytest.raises(SubjectError):
            subjects.set_manual(*bad)
    assert subjects.delete("Алгебра") and not subjects.delete("Алгебра")
    await subjects.resolve(["Алгебра"])
    assert subjects.get("Алгебра") == Subject("Algebra", "Alg")  # Haiku named it again, and it is not manual

    await subjects.resolve(["Calculus"])
    subjects.set_manual("Calculus", "Calc", "Calc")  # a Haiku entry overridden by hand becomes manual
    assert subjects.get("Calculus").manual


async def test_try_resolve(subjects, namer):
    namer.error = LLMError("ANTHROPIC_API_KEY is not set")
    await subjects.try_resolve(["Алгебра"], who="ruz")
    assert subjects.error == "ANTHROPIC_API_KEY is not set" and subjects.get("Алгебра") is None
    namer.error = BudgetExceeded("spent")
    await subjects.try_resolve(["Алгебра"], who="ruz")  # the budget tells the System topic itself
    assert subjects.error == "ANTHROPIC_API_KEY is not set"
    namer.error = None
    await subjects.try_resolve(["Алгебра"], who="ruz")
    assert subjects.error is None and subjects.abbr("Алгебра") == "Alg"
    calls = len(namer.prompts)
    await subjects.try_resolve(["Алгебра", None, ""], who="ruz")
    assert len(namer.prompts) == calls  # nothing unknown: no call


async def test_error_goes_once_nothing_waits(subjects, namer):
    namer.error = LLMError("ANTHROPIC_API_KEY is not set")
    await subjects.try_resolve(["Матан"], who="batches")
    assert subjects.error == "ANTHROPIC_API_KEY is not set"
    subjects.set_manual("Матан", "Calculus", "Calc")  # the owner names it by hand: nothing waits for Haiku
    assert subjects.error is None
    calls = len(namer.prompts)
    await subjects.try_resolve(["Матан"], who="batches")
    assert subjects.error is None and len(namer.prompts) == calls

    await subjects.try_resolve(["История"], who="batches")
    await subjects.try_resolve(["Algebra"], who="ruz")
    namer.error = None
    await subjects.try_resolve(["Algebra"], who="ruz")
    assert subjects.error == "ANTHROPIC_API_KEY is not set"  # RUZ is fine; the batches' course still waits
    subjects.set_manual("Algebra", "Algebra", "Alg")
    assert subjects.error == "ANTHROPIC_API_KEY is not set"  # another name: it still waits
    await subjects.try_resolve([], who="batches")  # its deadline aged out: the batches need no name any more
    assert subjects.error is None
