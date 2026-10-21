"""HSE App X deadlines in English, and the lists the merge reads (§5.2, §6.4; Tablo's labels of §5.1, §10.3).

After every poll of the collector the disciplines go through the course dictionary (§6.2), whose abbr starts the
labels. Every deadline, active or done, without a label in the cache goes to Haiku in one call (task
"hseapp_labels", Tablo's prompt word for word). A label over the budget gets one retry with the reason; the budget is
counted in characters like the batches' labels until the check by the font comes. Labels are cached in kv for good,
keyed like Tablo's (LABEL_VERSION, budget, title, discipline, abbr), so a new abbr in the dictionary asks again. A
call cut or refused is split in halves to find the deadline that breaks it; that one keeps its Russian title.
Haiku down or the limit spent: deadlines without a cached label show their Russian title and are asked again at the
next poll, while the cached ones keep their labels.

The lists live in kv "hseapp.deadlines" ({fetched_at, items, done}, rows {id, label, due, course, title} by date and
title), so a restart has them before the first poll and a failed poll keeps them. `course` is the discipline as HSE
App X names it, like `deadlines.course`: the merge takes its abbr from the dictionary (§5.5). `title` is the Russian
original. Done ones are never shown: they hide copies of the same work from the other sources (§5.5).
"""

import datetime as dt
import hashlib
import logging
from collections.abc import Awaitable, Callable

from .. import clock
from ..collectors.hseapp import Raw
from ..llm.budget import BudgetExceeded
from ..llm.client import BadAnswer, Haiku, LLMError
from ..store import Store
from .batches import LABEL_CHARS
from .subjects import Subjects

log = logging.getLogger(__name__)

LISTS_KEY = "hseapp.deadlines"
LABEL_VERSION = 2       # Tablo's: the prompt is the same. Bump when it changes, to drop the cached labels
MAX_TOKENS = 2000
TOKENS_PER_LABEL = 60   # {"id": …, "label": …} of a long id; a semester of done ones must not cut the answer

SYSTEM = """\
You write deadline titles for a student's e-ink dashboard (HSE University, Moscow, first year, Software Engineering).

Input: deadlines from the university app, in Russian: the title and, when known, the course.
Output: for each one a short English label that fits the given number of characters.

Style:
- Telegraphic English, no articles, digits instead of words.
- Start with the course abbreviation when the course is known or obvious from the title, then the work: "DM HW 3".
- Course abbreviations: Alg (Алгебра, линал, линейная алгебра), Calc (Математический анализ, матан), DM (Дискретная \
математика), Prog (программирование, C++, Python), Eng (английский), Hist (история), Safety (БЖД, безопасность \
жизнедеятельности), PE (физкультура). Other courses: a clear short English name.
- When the course comes with an abbreviation, start with exactly that abbreviation.
- Work abbreviations: HW (ДЗ, домашнее задание), Quiz, Test (контрольная, КР), Midterm, Exam, Lab, Essay, Project, \
Sem (семинар).
- Drop words that add nothing on a deadline list: "Дедлайн", "сдать", "по".
- Keep numbers and letters of the work: "ДЗ-2б" -> "HW 2B".
- Known acronyms: СОП (student survey on teaching quality) -> "Course survey".

Examples:
- "Дедлайн ДЗ 5 по Дискретной Математике" (Дискретная математика) -> "DM HW 5"
- "ДЗ 2 по Линалу" (Алгебра) -> "Alg HW 2"
- "Доп листок матан" -> "Calc extra sheet"
- "Эссе по истории" -> "Hist essay"
- "Записаться на майнор" -> "Minor sign-up"

Never exceed the character budget: shorten words rather than cut them."""

SCHEMA = {
    "type": "object",
    "properties": {"items": {"type": "array", "items": {
        "type": "object",
        "properties": {"id": {"type": "string"}, "label": {"type": "string"}},
        "required": ["id", "label"],
        "additionalProperties": False,
    }}},
    "required": ["items"],
    "additionalProperties": False,
}


def label_key(raw: Raw, budget: int, abbr: str | None) -> str:
    digest = hashlib.sha256(f"{LABEL_VERSION}\0{budget}\0{raw.title}\0{raw.discipline or ''}\0{abbr or ''}".encode())
    return f"hseapp.label:{digest.hexdigest()[:16]}"


def prompt(raws: list[Raw], budget: int, too_long: dict[str, str] | None = None,
           courses: dict[str, str] | None = None) -> str:
    """courses: discipline -> abbr from the dictionary."""
    courses = courses or {}
    lines = [f"Budget: at most {budget} characters per label.", ""]
    for raw in raws:
        abbr = courses.get(raw.discipline or "")
        course = ""
        if raw.discipline:
            course = f" (course: {raw.discipline}" + (f", abbreviation: {abbr})" if abbr else ")")
        lines.append(f'- id {raw.id}: "{raw.title}"{course}')
        if too_long and raw.id in too_long:
            lines.append(f'  Your previous label "{too_long[raw.id]}" was too wide for the screen; make it shorter.')
    return "\n".join(lines)


def fits(label: str) -> bool:
    return len(label) <= LABEL_CHARS


class HseAppDeadlines:
    def __init__(self, store: Store, subjects: Subjects, llm: Haiku, now: Callable[[], dt.datetime] = clock.now_utc):
        self.store, self.subjects, self.llm, self.now = store, subjects, llm, now
        self.labels_error: str | None = None
        # After a poll that changed the lists; True when there were none before (the first poll: a baseline).
        self.on_change: Callable[[bool], Awaitable[None]] | None = None

    # --- for the merge, the status and the Mini App -------------------------------------------

    def lists(self) -> dict:
        return self.store.get_json(LISTS_KEY) or {"fetched_at": None, "items": [], "done": []}

    def current(self) -> list[dict]:
        """Active deadlines, overdue ones too (the screen picks, §5.6): {id, label, due, course, title}."""
        return self.lists()["items"]

    def done(self) -> list[dict]:
        """Completed in the app: never shown, they hide their copies from the other sources (§5.5)."""
        return self.lists()["done"]

    def status(self) -> dict:
        lists = self.lists()
        return {"items": len(lists["items"]), "done": len(lists["done"]), "labels_error": self.labels_error}

    # --- after a poll ---------------------------------------------------------------------------

    async def after_poll(self, active: list[Raw], done: list[Raw]) -> bool:
        """HseApp.on_poll: name the courses, label the deadlines, keep both lists. True when they changed."""
        every = active + done
        disciplines = {raw.discipline for raw in every if raw.discipline}
        await self.subjects.try_resolve(disciplines, who="hseapp")
        courses = {name: abbr for name in disciplines if (abbr := self.subjects.abbr(name))}
        labels = await self.labels(every, courses)

        def rows(raws: list[Raw]) -> list[dict]:
            return [{"id": raw.id, "label": labels.get(raw.id, raw.title), "due": raw.due.isoformat(),
                     "course": raw.discipline, "title": raw.title}
                    for raw in sorted(raws, key=lambda raw: (raw.due, raw.title))]

        saved = self.store.get_json(LISTS_KEY)
        items, done_rows = rows(active), rows(done)
        changed = saved is None or (saved["items"], saved["done"]) != (items, done_rows)
        self.store.put_json(LISTS_KEY, {"fetched_at": clock.local_iso(self.now()), "items": items, "done": done_rows})
        if changed:
            log.info("HSE App X lists changed: %d deadlines, %d completed", len(items), len(done_rows))
            if self.on_change is not None:
                try:
                    await self.on_change(saved is None)
                except Exception:
                    log.exception("publishing the HSE App X changes failed")
        return changed

    async def labels(self, raws: list[Raw], courses: dict[str, str]) -> dict[str, str]:
        """Labels by id: cached ones, and Haiku's for the rest. One without a label is left out: its title shows."""
        keys = {raw.id: label_key(raw, LABEL_CHARS, courses.get(raw.discipline or "")) for raw in raws}
        result, todo = {}, {}
        for raw in raws:
            if cached := self.store.get_json(keys[raw.id]):
                result[raw.id] = cached
            else:
                todo[raw.id] = raw
        if not todo:
            self.labels_error = None
            return result
        try:
            got = await self._ask_all(list(todo.values()), courses)
        except BudgetExceeded as e:  # the budget tells the System topic itself; asked again at the next poll
            log.info("HSE App X labels wait for the LLM limit: %s", e)
            return result
        except LLMError as e:
            self.labels_error = str(e)
            log.warning("HSE App X labels: %s; the Russian titles show until the next poll", e)
            return result
        self.labels_error = None
        for raw_id, label in got.items():
            self.store.put_json(keys[raw_id], label)
            result[raw_id] = label
        log.info("HSE App X labels: %d asked, %d given", len(todo), len(got))
        return result

    async def _ask_all(self, raws: list[Raw], courses: dict[str, str]) -> dict[str, str]:
        """One call for all; a bad answer (cut or refused) is split in halves. A deadline the model cannot answer
        for on its own keeps its title, cached like a label: the same input would fail again."""
        try:
            got = await self._ask(raws, courses)
        except BadAnswer as e:
            if len(raws) == 1:
                log.warning("HSE App X label of %s: %s; the title stays", raws[0].id, e)
                return {raws[0].id: raws[0].title}
            half = len(raws) // 2
            return {**await self._ask_all(raws[:half], courses), **await self._ask_all(raws[half:], courses)}
        wide = {raw_id: label for raw_id, label in got.items() if not fits(label)}
        if wide:
            try:
                again = await self._ask([raw for raw in raws if raw.id in wide], courses, wide)
            except (BudgetExceeded, LLMError) as e:  # the wide labels stand; Tablo cuts them with "…"
                log.info("the retry for width failed, the first labels stay: %s", e)
            else:
                got.update({raw_id: label for raw_id, label in again.items() if raw_id in wide})
        return got

    async def _ask(self, raws: list[Raw], courses: dict[str, str],
                   too_long: dict[str, str] | None = None) -> dict[str, str]:
        answer = await self.llm.json("hseapp_labels", SYSTEM, prompt(raws, LABEL_CHARS, too_long, courses), SCHEMA,
                                     max_tokens=max(MAX_TOKENS, TOKENS_PER_LABEL * len(raws)))
        ids, result = {raw.id for raw in raws}, {}
        for item in answer.get("items") or []:
            if not isinstance(item, dict):
                continue
            raw_id, label = str(item.get("id")), " ".join(str(item.get("label") or "").split())
            if raw_id in ids and label:
                result[raw_id] = label
        return result
