"""Course wikis (D60): a course page's new sheets and homework as notices in the mailbox."""

import pytest
from test_sheet_sources import remote_links  # noqa: F401
from test_sheets import SHEET3, batches, files, llm, sheet_item, sheets  # noqa: F401

from infomaster.collectors.inbox import MAILBOX
from infomaster.collectors.wiki import NEXT_KEY, SENDER, STALE_AFTER, Wiki, items, page_api
from infomaster.processing.batches import Changes
from infomaster.settings import Settings

PAGE = "https://wiki.example.org/Discrete_maths_26/27"
DRIVE = "https://drive.google.com/file/d/{}/view?usp=sharing"

WIKITEXT = """== About ==
Lecturer: [https://t.me/someone A. Lecturer]. [https://docs.google.com/spreadsheets/d/1AbCdEfGhIjK/edit Grades]

== Seminar sheets ==
* [{s1} Sheet 1]
* [{s2} Sheet 2]
<!--
* [{s3} Sheet 3]
-->

== Homework ==
{{| class="wikitable"
|-
! Task !! colspan="3" | Due in group
|-
| || 101 || 102 || 103
|-
| [{h1} HW 1a ] ||  18.09 ||  || 24.09
|-
| [{h2} HW 1b] || 04.10 || {due} || 08.10
|}}

== Literature ==
* [https://example.org/books/logic.pdf A. Author. Logic.]
"""


def page(due="", s3=False) -> str:
    text = WIKITEXT.format(s1=DRIVE.format("1Sheet1AAAAA"), s2=DRIVE.format("1Sheet2AAAAA"),
                           s3=DRIVE.format("1Sheet3AAAAA"), h1=DRIVE.format("1Hw1aAAAAAAA"),
                           h2=DRIVE.format("1Hw1bAAAAAAA"), due=due)
    return text.replace("<!--\n", "").replace("-->\n", "") if s3 else text


def test_page_api():
    assert page_api(PAGE) == ("https://wiki.example.org/api.php", "Discrete maths 26/27")
    assert page_api("https://wiki.example.org/index.php?title=Calculus_101") == \
        ("https://wiki.example.org/api.php", "Calculus 101")
    assert page_api("https://wiki.example.org/w/index.php?title=X") == ("https://wiki.example.org/w/api.php", "X")
    with pytest.raises(ValueError):
        page_api("https://wiki.example.org/")


def test_items():
    found = items(page())
    assert [(i.section, i.name) for i in found] == [
        ("Seminar sheets", "Sheet 1"), ("Seminar sheets", "Sheet 2"), ("Homework", "HW 1a"), ("Homework", "HW 1b"),
        ("Literature", "A. Author. Logic.")]  # no Telegram, no spreadsheet, nothing commented out
    assert found[2].row == "101: 18.09 · 102: — · 103: 24.09"


class Reader:
    def __init__(self):
        self.revid, self.text, self.fail = 1, page(), None

    async def __call__(self, api, title):
        if self.fail:
            raise self.fail
        return self.revid, title, self.text


@pytest.fixture
def wiki(store, now, batches):  # noqa: F811
    settings = Settings(store)
    settings.set("wiki_pages", [PAGE])
    wiki = Wiki(store, settings, batches.inbox, now=now)
    wiki.reader, wiki.told = Reader(), []
    wiki.notify = wiki.told.append
    return wiki


def letters(wiki):
    return {m["msg_id"]: m for m in wiki.inbox.pending(MAILBOX, "mail")}


async def test_notices(wiki):
    assert await wiki.poll() == 2  # the baseline: the current sheet and homework, not the literature
    first = letters(wiki)
    assert sorted(first) == [-2, -1] and wiki.store.get_json(NEXT_KEY) == -3
    sheet, homework = first[-1], first[-2]
    assert sheet["sender_name"] == SENDER and sheet["subject"] == "Wiki · Discrete maths 26/27: Sheet 2"
    assert DRIVE.format("1Sheet2AAAAA") in sheet["text"]
    assert "Row of the table: 101: 04.10 · 102: — · 103: 08.10" in homework["text"]

    assert await wiki.poll() == 0  # the same revision: not read again
    wiki.reader.revid, wiki.reader.text = 2, page(due="29.11", s3=True)
    assert await wiki.poll() == 2
    new = {m["subject"]: m for msg_id, m in letters(wiki).items() if msg_id < -2}
    assert new["Wiki · Discrete maths 26/27: Sheet 3"]["text"].startswith("New on the course's wiki page")
    assert new["Wiki · Discrete maths 26/27: HW 1b"]["text"].startswith("Changed on")
    assert "102: 29.11" in new["Wiki · Discrete maths 26/27: HW 1b"]["text"]
    assert wiki.status()["pages"][PAGE] == {"title": "Discrete maths 26/27", "items": 6, "error": None}


async def test_a_failing_page_is_told_after_a_day(wiki, now):
    wiki.reader.fail = TimeoutError("timed out")
    await wiki.poll()
    assert wiki.told == [] and "TimeoutError" in wiki.status()["pages"][PAGE]["error"]
    start = now()
    wiki.now = lambda: start + STALE_AFTER
    await wiki.poll()
    assert len(wiki.told) == 1 and "not read since" in str(wiki.told[0])
    wiki.reader.fail = None
    await wiki.poll()
    assert len(wiki.told) == 2 and wiki.status()["pages"][PAGE]["error"] is None


async def test_a_notice_becomes_a_sheet(wiki, batches, llm, sheets, files, remote_links):  # noqa: F811
    remote_links[DRIVE.format("1Hw1bAAAAAAA")] = SHEET3
    await wiki.poll()
    llm.answers = [{"items": [sheet_item(-2, number="1b")]}]
    changes = Changes()
    await batches.parse_chat(MAILBOX, changes)
    prompt = llm.calls[0].prompt
    assert 'Letters from "Course wiki"' in prompt and "downloaded from the link" in prompt
    [sheet] = sheets.waiting()
    assert sheet["chat_id"] == MAILBOX and sheet["msg_id"] == -2 and sheet["number"] == "1b"
    assert batches.inbox.pending(MAILBOX, "mail") == []  # both notices parsed
