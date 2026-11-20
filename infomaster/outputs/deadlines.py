"""What is news in the deadlines (§6.4, D24, D69): a new or moved deadline goes to the web app's notifier (Web Push).

The merged list (processing/merge.py: the one Tablo and HA get) is compared with the one seen last (kv
"deadlines.shown") by the row each deadline comes from (`Deadline.ref`), so a rename (a Midterm once the dictionary
knows its course, an HSE App X label once Haiku answers) is no news, and a move of the chats or RUZ (a new row that
moves the old one) is a move. A copy of the same work and date taking the place of another is no news either; a
deadline that leaves the list is told nothing.

Quiet: the first comparison (nothing seen yet) takes the list as it is, and so do the first poll of HSE App X and the
first RUZ poll for what they bring, so a source joining floods nothing. The owner's own changes (the web app, Claude,
HA) are no news either.

Several tasks change deadlines (the batches, the RUZ and HSE App X polls, the owner): one refresh runs at a time under
a lock and compares the list as it is then, so nothing is told twice. At midnight the past rows are pruned.
"""

import asyncio
import datetime as dt
import logging
from collections.abc import Callable, Iterable

from .. import clock
from ..processing.merge import DeadlineList, same
from ..processing.todo import Deadline
from ..store import Store

log = logging.getLogger(__name__)

SHOWN_KEY = "deadlines.shown"   # the list as seen last: [{ref, title, due, source, course}]
MIDNIGHT_DELAY = 5              # seconds past midnight


def entry(d: Deadline) -> dict:
    return {"ref": d.ref, "title": d.title, "due": d.due.isoformat(), "source": d.source, "course": d.course}


def from_entry(e: dict) -> Deadline:
    return Deadline(e["title"], dt.date.fromisoformat(e["due"]), "study", e["source"], e["course"], ref=e["ref"])


class DeadlineWatch:
    def __init__(self, store: Store, merged: DeadlineList, now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.merged, self.now = store, merged, now
        self._lock = asyncio.Lock()
        self._quiet: set[str] = set()
        self._tasks: set[asyncio.Task] = set()
        # A new deadline (was None) or a moved one (was: its old date): the web app's notifier (§8.6).
        self.notify: Callable[[Deadline, str | None], None] | None = None

    def quiet(self, refs: Iterable[str]) -> None:
        """Deadlines a source brought that are no news: the next refresh takes them into the list silently. Call it
        right after they are saved, before any await, so no refresh sees them first."""
        self._quiet |= set(refs)

    def changed(self, quiet: set[str]) -> None:
        """DeadlineList.on_change: the owner's change is taken in, quietly."""
        self.quiet(quiet)
        task = asyncio.create_task(self.refresh())
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def settle(self) -> None:
        """Wait for the refreshes `changed` started (the tests)."""
        while self._tasks:
            await asyncio.gather(*list(self._tasks))

    async def run(self) -> None:
        """At the start (the very first one takes the list as it is), then every midnight: own and hidden rows long
        past are removed."""
        while True:
            try:
                self.merged.prune()
                await self.refresh()
            except Exception:
                log.exception("refreshing the deadlines failed")
            await asyncio.sleep(self._seconds_to_midnight())

    async def refresh(self) -> None:
        """Compare the list with the one seen last and tell the notifier what is news."""
        async with self._lock:
            shown = self.merged.visible()
            before = self.store.get_json(SHOWN_KEY)
            quiet = set(self._quiet)  # what came before this comparison; later ones wait for the next refresh
            self._quiet -= quiet
            self.store.put_json(SHOWN_KEY, [entry(d) for d in shown])  # first: a failure is not told twice
            if before is None or self.notify is None:
                return
            for d, was in self.news(before, shown, quiet):
                try:
                    self.notify(d, was)
                except Exception:
                    log.exception("notifying of a deadline failed")

    def news(self, before: list[dict], shown: list[Deadline], quiet: set[str]) -> list[tuple[Deadline, str | None]]:
        """(the deadline, the date it moved from or None for a new one) of every change worth telling, in list
        order."""
        refs = {d.ref for d in shown}
        was = {e["ref"]: e for e in before if e["ref"] in refs}  # a shown deadline -> as it was seen last
        lost = [e for e in before if e["ref"] not in refs]
        for e in list(lost):  # a move of the chats or RUZ: the old row points at the new one
            newer = self._newer(e["ref"])
            if newer in refs and newer not in was:
                was[newer] = e
                lost.remove(e)
        joined = [d for d in shown if d.ref not in was]
        for d in list(joined):  # the same work on the same date in another row takes the place: no news
            e = next((e for e in lost if same(d, from_entry(e))), None)
            if e is not None:
                joined.remove(d)
                lost.remove(e)
        new = {d.ref for d in joined}
        result = []
        for d in shown:
            if d.ref in quiet:
                continue
            if d.ref in new:
                result.append((d, None))
            elif d.ref in was and was[d.ref]["due"] != d.due.isoformat():  # the same date: a rename, no news
                result.append((d, was[d.ref]["due"]))
        return result

    def _newer(self, ref: str) -> str | None:
        """The ref of the row that moved a row of the chats, mail or RUZ, or None."""
        if not ref.startswith("dl:"):
            return None
        row = self.merged.deadlines.latest(int(ref[3:]))
        return f"dl:{row['id']}" if row is not None and f"dl:{row['id']}" != ref else None

    def _seconds_to_midnight(self) -> float:
        local = self.now().astimezone(clock.TZ)
        midnight = (local + dt.timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0)
        return (midnight - local).total_seconds() + MIDNIGHT_DELAY
