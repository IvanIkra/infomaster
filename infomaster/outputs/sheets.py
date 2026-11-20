"""Publishing the problem sheets (§6.3, D14, D69): a read sheet joins the list the web app's shelves show.

After every batch run and RUZ poll the sheets waiting are prepared (read, compared with their older version, put in
the library) and published, oldest first; one that fails stays waiting for the next run, one whose file is gone fails.
Published is the row's status `posted`, a name kept from the forum's time (D69), so a newer version chains to the
older one. The owner's joining of two sheets, changing a sheet and deleting an own one go through here too, under the
same lock.
"""

import asyncio
import logging

from ..processing.sheets import Sheets

log = logging.getLogger(__name__)


class SheetPublisher:
    def __init__(self, sheets: Sheets):
        self.sheets, self.files = sheets, sheets.files
        self._lock = asyncio.Lock()

    async def publish(self) -> None:
        """The sheets waiting, published."""
        async with self._lock:
            for waiting in self.sheets.waiting():
                try:
                    sheet = await self.sheets.prepare(waiting["id"])
                    if sheet is not None:
                        self._publish(sheet)
                except Exception:
                    log.exception("publishing sheet %d failed, it waits for the next run", waiting["id"])

    async def join(self, sheet_id: int, previous_id: int) -> None:
        """A sheet published as new is a newer version of another. ValueError when it cannot be."""
        async with self._lock:
            await self.sheets.join(sheet_id, previous_id)

    async def edit(self, sheet_id: int, fields: dict) -> dict | None:
        """The owner's change of a sheet; None for no such sheet."""
        async with self._lock:
            return self.sheets.edit(sheet_id, fields)

    async def remove(self, sheet_id: int) -> bool:
        """An own sheet deleted with every version. False when it is no own sheet."""
        async with self._lock:
            return self.sheets.remove_own(sheet_id) is not None

    def _publish(self, sheet: dict) -> None:
        if self.files.by_hash(sheet["sha256"]) is None:
            self.sheets.failed(sheet["id"], self.sheets.file_gone())
            return
        self.sheets.posted(sheet["id"])
