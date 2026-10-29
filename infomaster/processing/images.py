"""Images of chat messages (§5.1, D12): what Haiku reads on a photo or a screenshot, for the batches.

In a chat with «Картинки» on, the image of a message waiting for its batch is downloaded by the twin
(collectors/files.py) and read by Haiku in a call of its own (task `image`): what the picture is and the text on it,
word for word. The batch sees the reading in place of the picture, so a screenshot of the timetable or a photo of a
notice gives news, deadlines and events like a text does. The summaries take readings already made, never a new call.

Limits (§7): reading images is optional, so it stops above 80 % of the daily LLM limit, and at most `images_daily`
images are read a Moscow day (failed calls count too). An image left unread goes to the batch as a "photo" only, and a
message with nothing else to read is parsed without a call. When the whole limit is spent the batch waits for midnight
with its images. The reading is kept with the file: an edited message is not read again, and the same image in another
message (a forward, a repost) takes the reading it has.
"""

import asyncio
import datetime as dt
import json
import logging
from collections.abc import Callable

from .. import clock
from ..collectors.files import Files, media_type, reading
from ..llm.budget import Budget, BudgetExceeded, Level
from ..llm.client import BadAnswer, Haiku, Image, Rejected
from ..settings import Settings

log = logging.getLogger(__name__)

MAX_TOKENS = 3_000
SHOWS_CHARS = 300          # of what the image is
TEXT_CHARS = 2_500         # of the text on it
MESSAGE_CHARS = 500        # of the message's own text in the prompt

SYSTEM = """\
You read images from the Telegram chats of a university student (HSE University, Moscow, first year, Software \
Engineering). Another model then reads your answer together with the chat's messages, without the image, and picks \
out news, deadlines and events, so it has only what you write.

- shows: what the image is and what it shows, in 1-2 short English sentences: "Screenshot of the Calculus timetable \
for Oct 26-31", "Photo of a notice on the door of R405", "Lecture slide on series convergence", "Meme".
- text: the text on the image as written, in its language. Keep dates, times, rooms, names, numbers and links exactly. \
Keep the lines; a table goes row by row, its cells separated by " | ". Leave out what tells nothing: buttons and menus \
of an app, status bars, watermarks. "" when the image has no text worth reading. At most about 2000 characters: for a \
longer text keep what matters for a student (dates, tasks, changes) and drop the rest.

Write only what is on the image; a word you cannot make out is "[?]". The message the image came with is given for \
context only: do not repeat it.
"""

SCHEMA = {
    "type": "object",
    "properties": {"shows": {"type": "string"}, "text": {"type": "string"}},
    "required": ["shows", "text"],
    "additionalProperties": False,
}


class Images:
    def __init__(self, settings: Settings, budget: Budget, files: Files, llm: Haiku,
                 now: Callable[[], dt.datetime] = clock.now_utc):
        self.settings, self.budget, self.files, self.llm, self.now = settings, budget, files, llm, now
        self.ok_at: str | None = None  # the last image read
        self.error: str | None = None  # the last call failed on the way to the model

    def today(self) -> int:
        """Images read today, Moscow day: the calls of the task, failed ones too."""
        return self.budget.journal.count_since(clock.day_start_utc(clock.local_day(self.now())), "image")

    def status(self) -> dict:
        return {"ok_at": self.ok_at, "error": self.error, "daily": self.settings.get("images_daily"),
                "today": self.today()}

    async def read(self, chat: dict, settings: dict, message: dict, row: dict) -> dict | None:
        """What the image of a row (downloaded, see Files.fetch_image) shows: {shows, text}, read now if need be.
        None when it is not read: the daily number is reached, optional tasks are off, the model could not.

        Raises BudgetExceeded when the whole limit is spent (the batch waits with its images) and LLMError when the
        call failed on the way: the message stays in the queue."""
        if row["reading"] is not None:
            return reading(row)
        if (same := self.files.read_as(row)) is not None:
            return reading(self.files.set_reading(row, same))
        if self.today() >= self.settings.get("images_daily"):
            log.info("image of message %s in %s not read: the daily number of images is reached", message["msg_id"],
                     chat["id"])
            return None
        path = self.files.path(row)
        if path is None:
            return None
        data = await asyncio.to_thread(path.read_bytes)
        try:
            answer = await self.llm.json("image", SYSTEM, self._prompt(chat, settings, message), SCHEMA,
                                         max_tokens=MAX_TOKENS, chat_id=chat["id"],
                                         images=[Image(data, media_type(data))])
        except BudgetExceeded:
            if self.budget.level() is Level.HARD:
                raise
            log.info("image of message %s in %s not read: optional LLM tasks are off", message["msg_id"], chat["id"])
            return None
        except (BadAnswer, Rejected) as e:  # the same image would fail again: it is not tried twice
            log.warning("image of message %s in %s not read: %s", message["msg_id"], chat["id"], e)
            self.files.set_reading(row, "", str(e))
            return None
        except Exception as e:
            self.error = str(e) or type(e).__name__
            raise
        self.ok_at, self.error = clock.local_iso(self.now()), None
        shows = " ".join(str(answer.get("shows") or "").split())[:SHOWS_CHARS]
        text = "\n".join(line.strip() for line in str(answer.get("text") or "").splitlines() if line.strip())
        if len(text) > TEXT_CHARS:
            text = text[:TEXT_CHARS].rsplit("\n", 1)[0] + "\n…"
        value = json.dumps({"shows": shows, "text": text}, ensure_ascii=False) if shows or text else ""
        return reading(self.files.set_reading(row, value))

    def _prompt(self, chat: dict, settings: dict, message: dict) -> str:
        posted = dt.datetime.fromisoformat(message["at"])
        text = " ".join(message["text"].split())[:MESSAGE_CHARS]
        return "\n".join([
            f"Chat: \"{chat['title']}\" ({'study' if settings['study'] else 'not a study chat'})",
            f"Posted: {posted:%a %Y-%m-%d %H:%M}",
            f"The message with the image: {text or '(no text)'}",
        ])
