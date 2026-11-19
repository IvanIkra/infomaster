"""Course wikis (§5.2, D60): a course page of a MediaWiki (the faculty's wiki.cs.hse.ru) as a source.

Some courses post their sheets and homework only on the course's wiki page: a list of links to PDFs on Google Drive,
and a table of due dates by group. The pages are listed in the `wiki_pages` setting. Every POLL_EVERY the page's
wikitext is read through the wiki's API (only when its revision changed); the links in it, outside HTML comments,
are its items, each with its section and, in a table, the row's cells under the table's column heads.

An item new on the page, or one whose row changed (a due date was set), becomes a notice in the mailbox: a letter
from "Course wiki" whose subject names the page and the item and whose text gives the link and the row. The batches
read it as they read a letter (D30): the PDF behind the link is fetched (collectors/links.py, D59), the model decides
whether it is a sheet, and takes the due date of the student's group (it knows the group from `about_me`, D52). So
the group is never configured here, and nothing new is needed downstream. A notice's msg_id is negative, counting
down, so it never meets a letter's UID.

Only links to files are items (collectors/links.downloadable: Drive, Dropbox, Yandex Disk, PDFs): not the teachers'
Telegram, not other pages, not the course's grade spreadsheet. The first read of a page is a baseline: of what is there
already, only the last item of each section of tasks (sheets, homework) becomes a notice, not the whole semester and
not the literature.
"""

import asyncio
import datetime as dt
import json
import logging
import re
from collections.abc import Callable
from dataclasses import dataclass
from urllib.parse import parse_qs, unquote, urlsplit

import aiohttp

from .. import clock
from ..i18n import Alert, Phrase
from ..settings import Settings
from ..store import Store
from .inbox import Inbox
from .links import downloadable

log = logging.getLogger(__name__)

STATE_KEY = "wiki.state"    # {page url: {"revid", "title", "items": {link: row}}}
NEXT_KEY = "wiki.next"      # the msg_id of the next notice: negative, counting down
POLL_EVERY = 3 * 3600       # seconds
TIMEOUT = aiohttp.ClientTimeout(total=60)
SENDER = "Course wiki"
STALE_AFTER = dt.timedelta(days=1)  # a page failing longer than this is told to the System topic
LINK = re.compile(r"\[(https?://[^\s\]]+)\s*([^\]]*)\]")
HEADING = re.compile(r"^(={2,6})\s*(.*?)\s*\1\s*$")
COMMENT = re.compile(r"<!--.*?(-->|$)", re.S)
MARKUP = re.compile(r"'{2,}|<[^>]+>")
# A section or item of tasks: sheets, homework, tests (the baseline tells only these).
TASKS = re.compile(r"листо?к|задач|задани|\bдз\b|домашн|контрольн|sheet|problem|homework|assignment|\bhw\b", re.I)


@dataclass(frozen=True)
class Item:
    url: str
    name: str
    section: str
    row: str = ""               # a table's row: "101: 15.11 · 102: — · …" under the table's column heads


def page_api(url: str) -> tuple[str, str]:
    """(api.php address, page title) of a wiki page's URL: /Title, /index.php?title=Title or /wiki/Title."""
    parts = urlsplit(url.strip())
    if parts.scheme not in ("http", "https") or not parts.netloc:
        raise ValueError(f"not a wiki page's address: {url!r}")
    title = parse_qs(parts.query).get("title", [""])[0] or unquote(parts.path).removeprefix("/wiki/").lstrip("/")
    if not title or title in ("index.php", "api.php"):
        raise ValueError(f"no page title in {url!r}")
    base = "/w" if parts.path.startswith("/w/") else ""
    return f"https://{parts.netloc}{base}/api.php", title.replace("_", " ")


def document(url: str) -> bool:
    """A link to a file that may be a sheet: Drive, Dropbox, Yandex Disk, a PDF. Not a chat, a person, a page, a
    spreadsheet (the course's grades)."""
    return downloadable(url) and "/spreadsheets/" not in url


def clean(text: str) -> str:
    """Wiki markup out of a cell: links as their text, bold and tags gone, spaces squeezed."""
    text = LINK.sub(lambda m: m[2] or m[1], text)
    text = re.sub(r"\[\[(?:[^\]|]*\|)?([^\]]*)\]\]", r"\1", text)
    return " ".join(MARKUP.sub("", text).split())


def _cells(line: str) -> list[str]:
    """The cells of a table line ("| a || b" or "! a !! b"), each without its attributes ("colspan=2 | x")."""
    body = line[1:]
    cells = re.split(r"\|\||!!", body)
    found = []
    for cell in cells:
        if "|" in cell and "[" not in cell.split("|", 1)[0] and "=" in cell.split("|", 1)[0]:
            cell = cell.split("|", 1)[1]
        found.append(cell.strip())
    return found


def items(wikitext: str) -> list[Item]:
    """The links of a page outside comments, in order, each once, with their section and table row."""
    text = COMMENT.sub("", wikitext)
    found: dict[str, Item] = {}
    section, heads, row = "", [], []

    def close_row():
        nonlocal row
        cells, row = row, []
        if not cells:
            return
        links = [(m[1], clean(m[2]) or m[1]) for cell in cells for m in LINK.finditer(cell) if document(m[1])]
        if not links:
            if any(clean(cell) for cell in cells):
                heads[:] = [clean(cell) for cell in cells]  # a row of column heads (the groups)
            return
        url, name = links[0]
        values = [clean(cell) for cell in cells]
        first = next(i for i, cell in enumerate(cells) if url in cell)
        pairs = []
        for i, value in enumerate(values):
            if i == first:
                continue
            head = heads[i] if i < len(heads) and heads[i] else f"column {i + 1}"
            pairs.append(f"{head}: {value or '—'}")
        if url not in found:
            found[url] = Item(url, name, section, " · ".join(pairs))

    in_table = False
    for raw in text.splitlines():
        line = raw.strip()
        if heading := HEADING.match(line):
            section = clean(heading[2])
            continue
        if line.startswith("{|"):
            in_table, heads, row = True, [], []
            continue
        if in_table:
            if line.startswith("|}"):
                close_row()
                in_table = False
            elif line.startswith("|-"):
                close_row()
            elif line.startswith(("|", "!")):
                row += _cells(line)
            continue
        for match in LINK.finditer(line):
            url = match[1]
            if document(url) and url not in found:
                found[url] = Item(url, clean(match[2]) or url, section)
    return list(found.values())


def notice(title: str, item: Item, changed: bool) -> tuple[str, str]:
    """(subject, text) of the letter that tells an item."""
    subject = f"Wiki · {title}: {item.name}"
    what = "Changed" if changed else "New"
    lines = [f"{what} on the course's wiki page «{title}», section «{item.section or 'no section'}»: {item.name}",
             item.url]
    if item.row:
        lines.append(f"Row of the table: {item.row}")
    return subject, "\n".join(lines)


class Wiki:
    def __init__(self, store: Store, settings: Settings, inbox: Inbox,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.db, self.settings, self.inbox, self.now = store, store.db, settings, inbox, now
        self.reader: Callable = read_page  # (api, title) -> (revid, title, wikitext); replaced in tests
        self.notify: Callable[[Phrase], None] | None = None
        self.errors: dict[str, str] = {}
        self.ok_at: str | None = None
        self._failing: dict[str, dt.datetime] = {}
        self._told: set[str] = set()

    def status(self) -> dict:
        state = self.store.get_json(STATE_KEY, {})
        return {"pages": {url: {"title": (state.get(url) or {}).get("title"), "items":
                                len((state.get(url) or {}).get("items") or {}), "error": self.errors.get(url)}
                          for url in self.settings.get("wiki_pages")},
                "ok_at": self.ok_at}

    async def run(self) -> None:
        while True:
            try:
                await self.poll()
            except Exception:
                log.exception("wiki poll failed")
            await asyncio.sleep(POLL_EVERY)

    async def poll(self) -> int:
        """Read every listed page once; how many notices were stored."""
        state = self.store.get_json(STATE_KEY, {})
        stored = 0
        for url in self.settings.get("wiki_pages"):
            try:
                api, title = page_api(url)
                revid, title, wikitext = await self.reader(api, title)
            except (aiohttp.ClientError, TimeoutError, ValueError, KeyError) as e:
                self._failed(url, f"{type(e).__name__}: {e}")
                continue
            self._recovered(url)
            page = state.get(url)
            if page is not None and page.get("revid") == revid:
                continue
            stored += self._take(url, state, revid, title, items(wikitext))
        self.ok_at = clock.local_iso(self.now())
        return stored

    def _take(self, url: str, state: dict, revid: int, title: str, found: list[Item]) -> int:
        """The notices of a page's new and changed items and its new state, in one transaction."""
        page = state.get(url)
        known = (page or {}).get("items", {})
        if page is None:  # a baseline: the current sheet and homework, the last item of their sections
            last = {item.section: item for item in found if TASKS.search(f"{item.section} {item.name}")}
            told = [(item, False) for item in found if last.get(item.section) is item]
        else:
            told = [(item, item.url in known) for item in found if known.get(item.url) != item.row]
        state[url] = {"revid": revid, "title": title, "items": {item.url: item.row for item in found}}
        next_id = self.store.get_json(NEXT_KEY, -1)
        stored = 0
        with self.db:
            for item, changed in told:
                subject, text = notice(title, item, changed)
                if self.inbox.add_letter(next_id, {"sender_name": SENDER, "at": self.now(), "text": text,
                                                   "files": [], "subject": subject}):
                    stored += 1
                next_id -= 1
            for key, value in ((STATE_KEY, state), (NEXT_KEY, next_id)):
                self.db.execute("insert into kv(key, value) values(?, ?) on conflict(key) do update set "
                                "value = excluded.value", (key, json.dumps(value, ensure_ascii=False)))
        if stored:
            log.info("wiki %s: %d notices", title, stored)
        return stored

    def _failed(self, url: str, error: str) -> None:
        self.errors[url] = error
        log.warning("wiki page %s not read: %s", url, error)
        since = self._failing.setdefault(url, self.now())
        if url not in self._told and self.now() - since >= STALE_AFTER and self.notify is not None:
            self._told.add(url)
            self.notify(Alert(f"The course wiki page {url} is not read since {since.astimezone(clock.TZ):%d.%m %H:%M}:"
                              f" {error}.",
                              f"Страница вики курса {url} не читается с {since.astimezone(clock.TZ):%d.%m %H:%M}: "
                              f"{error}."))

    def _recovered(self, url: str) -> None:
        if url in self._told and self.notify is not None:
            self.notify(Phrase(f"The course wiki page {url} is read again.",
                               f"Страница вики курса {url} снова читается."))
        self.errors.pop(url, None)
        self._failing.pop(url, None)
        self._told.discard(url)


async def read_page(api: str, title: str) -> tuple[int, str, str]:
    """(revision id, title, wikitext) of a page by the MediaWiki API. The wiki refuses requests without a user agent of
    their own (curl's is refused)."""
    params = {"action": "parse", "page": title, "prop": "wikitext|revid", "format": "json", "formatversion": "2",
              "redirects": "1"}
    async with aiohttp.ClientSession(timeout=TIMEOUT, headers={"User-Agent": "infomaster"}) as session:
        async with session.get(api, params=params) as response:
            response.raise_for_status()
            data = await response.json(content_type=None)
    if "error" in data:
        raise ValueError(data["error"].get("info") or data["error"].get("code"))
    parsed = data["parse"]
    return int(parsed["revid"]), parsed["title"], parsed["wikitext"]
