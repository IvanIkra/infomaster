"""The course dictionary (§6.2, §2.2, р. 3): one for the timetable, the deadlines and the iCal feed.

A source names a course its own way: «Математический анализ» in HSE App X, *Calculus* in RUZ, «матан» in a chat.
Each cleaned name gets a short English `title` (the timetable, Tablo's Classes column) and an `abbr` of up to six
characters (the prefix of deadline labels, the key of the merge, processing/merge.py). Haiku names new courses in one
call that also shows the whole dictionary, so the same course in Russian and in English gets the same answer (§10.1).

Entries set by hand in the Mini App are `manual`: Haiku never overwrites them, and deleting one lets Haiku name the
course again. A change by hand tells `on_manual`, so the Midterm labels follow it at once, not at the next RUZ poll
(app.py). When Haiku is down or the limit is spent, names show as the source gives them and are asked again at the
next RUZ poll, batch run or HSE App X poll. The error of a failed naming is kept for each caller with the names it
left unnamed: it stays while any of them waits, and goes once they are named (by Haiku or by hand) or the caller no
longer needs them.
"""

import asyncio
import logging
import re
from collections.abc import Callable, Iterable
from dataclasses import dataclass

from ..i18n import Phrase
from ..llm.budget import BudgetExceeded
from ..llm.client import Haiku, LLMError
from ..store import Store

log = logging.getLogger(__name__)

# The subject column of Tablo's Classes block (§2.1): its width / 8.1 px is 12-13, and "Intro to Prog" fits.
# Counted in characters until the check by the font comes.
TITLE_CHARS = 13
ABBR_CHARS = 6
MANUAL_TITLE_CHARS = 40  # a title set by hand may be longer: the owner decides, Tablo cuts it with "…"
MAX_TOKENS = 2000


class SubjectError(ValueError):
    pass


@dataclass(frozen=True)
class Subject:
    title: str
    abbr: str
    manual: bool = False


def clean(name: str | None) -> str:
    """'Алгебра  (рус)' -> 'Алгебра'; collapses whitespace."""
    name = " ".join(str(name or "").split())
    return re.sub(r"\s*\(рус\)$", "", name).strip()


SYSTEM = """\
You name university courses for a student's e-ink dashboard and calendar (HSE University, Moscow, first year, \
Software Engineering).

Input: course names as the university gives them, in Russian or in English, some long.
Output: for each one
- title: a short English course name for the timetable that fits the given number of characters;
- abbr: a course abbreviation of at most 6 characters, used as a prefix in deadline labels ("Calc HW 5").

Style:
- Plain English, no articles. Shorten words rather than cut them: "Intro to Prog", not "Introduction t".
- Keep the usual name when it fits: "Calculus", "Algebra".
- Abbreviations: Alg (Алгебра, линейная алгебра), Calc (Математический анализ), DM (Дискретная математика), Prog \
(программирование), SE (программная инженерия), Eng (английский), Hist (история), Safety (БЖД), PE (физкультура). \
Other courses: a clear short abbreviation.
- The same course in Russian and in English gets the same title and abbr. Courses already in the dictionary are \
listed: reuse their title and abbr for the same course.

Examples:
- "Математический анализ" -> title "Calculus", abbr "Calc"
- "Введение в программирование" -> title "Intro to Prog", abbr "Prog"
- "Discrete Mathematics" -> title "Discrete Math", abbr "DM"
- "История России" -> title "Russian Hist", abbr "Hist"

Never exceed the character budget for the title."""

SCHEMA = {
    "type": "object",
    "properties": {"items": {"type": "array", "items": {
        "type": "object",
        "properties": {"id": {"type": "string"}, "title": {"type": "string"}, "abbr": {"type": "string"}},
        "required": ["id", "title", "abbr"],
        "additionalProperties": False,
    }}},
    "required": ["items"],
    "additionalProperties": False,
}


def prompt(names: list[str], known: dict[str, Subject], too_long: dict[str, str] | None = None) -> str:
    lines = [f"Title budget: at most {TITLE_CHARS} characters.", ""]
    if known:
        lines.append("Already in the dictionary:")
        lines += [f'- "{name}": title "{s.title}", abbr "{s.abbr}"' for name, s in known.items()]
        lines.append("")
    lines.append("New courses:")
    for i, name in enumerate(names):
        lines.append(f'- id {i}: "{name}"')
        if too_long and name in too_long:
            lines.append(f'  Your previous title "{too_long[name]}" was too wide for the screen; make it shorter.')
    return "\n".join(lines)


def fits(title: str) -> bool:
    return len(title) <= TITLE_CHARS


class Subjects:
    def __init__(self, store: Store, llm: Haiku):
        self.db, self.llm = store.db, llm
        # Caller ("ruz", "batches", "hseapp") -> the error of its last naming and the names it left unnamed. Per
        # caller, so one caller's success does not hide the names another one still waits for.
        self._failed: dict[str, tuple[str, set[str]]] = {}
        # One resolve at a time: RUZ and the batches resolve together at start-up, and the second one must see
        # what the first one added, or one course gets two answers.
        self._lock = asyncio.Lock()
        self.on_manual: Callable[[], None] | None = None  # after an entry is set or deleted by hand

    @property
    def error(self) -> str | None:
        """Why some names wait for Haiku; None when none waits."""
        return next((error for error, _ in self._failed.values()), None)

    # --- lookups ----------------------------------------------------------------

    def get(self, name: str | None) -> Subject | None:
        row = self.db.execute("select * from subjects where name = ?", (clean(name),)).fetchone()
        return Subject(row["title"], row["abbr"], bool(row["manual"])) if row else None

    def title(self, name: str) -> str:
        """English title of a course; the name as the source gives it until the dictionary has it."""
        subject = self.get(name)
        return subject.title if subject else clean(name)

    def abbr(self, name: str | None) -> str | None:
        subject = self.get(name)
        return subject.abbr if subject else None

    def all(self) -> dict[str, Subject]:
        rows = self.db.execute("select * from subjects order by name")
        return {row["name"]: Subject(row["title"], row["abbr"], bool(row["manual"])) for row in rows}

    def unknown(self, names: Iterable[str | None]) -> list[str]:
        known = self.all()
        return sorted({name for name in map(clean, names) if name and name not in known})

    # --- manual entries (the Mini App) -----------------------------------------------

    def set_manual(self, name: str, title: str, abbr: str) -> dict:
        """Add or change an entry by hand; it becomes manual. Returns the entry."""
        name, title, abbr = clean(name), " ".join(str(title or "").split()), " ".join(str(abbr or "").split())
        if not name:
            raise SubjectError(Phrase("the course's name is needed", "нужно название курса"))
        if not title or len(title) > MANUAL_TITLE_CHARS:
            raise SubjectError(Phrase(f"the English title: 1 to {MANUAL_TITLE_CHARS} characters",
                                      f"название на английском: от 1 до {MANUAL_TITLE_CHARS} символов"))
        if not abbr or len(abbr) > ABBR_CHARS:
            raise SubjectError(Phrase(f"the abbreviation: 1 to {ABBR_CHARS} characters",
                                      f"сокращение: от 1 до {ABBR_CHARS} символов"))
        self._put(name, title, abbr, manual=True)
        self._settle()
        self._changed()
        return {"name": name, "title": title, "abbr": abbr, "manual": True}

    def delete(self, name: str) -> bool:
        """Drop an entry; the next resolve asks Haiku about the name again."""
        with self.db:
            deleted = bool(self.db.execute("delete from subjects where name = ?", (clean(name),)).rowcount)
        if deleted:
            self._changed()
        return deleted

    def _changed(self) -> None:
        if self.on_manual is not None:
            try:
                self.on_manual()
            except Exception:  # the entry is saved; the labels follow at the next poll anyway
                log.exception("following a manual dictionary change failed")

    def _put(self, name: str, title: str, abbr: str, manual: bool) -> None:
        with self.db:
            self.db.execute("insert into subjects(name, title, abbr, manual) values(?, ?, ?, ?) "
                            "on conflict(name) do update set title = excluded.title, abbr = excluded.abbr, "
                            "manual = excluded.manual where excluded.manual or not subjects.manual",
                            (name, title, abbr, int(manual)))

    # --- Haiku ------------------------------------------------------------------------

    async def resolve(self, names: Iterable[str | None]) -> dict[str, Subject]:
        """Entries for the names, asking Haiku about the new ones. Raises BudgetExceeded or LLMError."""
        async with self._lock:
            wanted = sorted({name for name in map(clean, names) if name})
            known = self.all()
            todo = [name for name in wanted if name not in known]
            if todo:
                got = await self._ask(todo, known)
                wide = {name: got[name].title for name in todo if name in got and not fits(got[name].title)}
                if wide:
                    try:
                        got.update(await self._ask(list(wide), known, wide))
                    except (BudgetExceeded, LLMError) as e:  # the wide titles stand; Tablo cuts them with "…"
                        log.info("the retry for width failed, the first titles stay: %s", e)
                for name in todo:
                    if name in got:
                        self._put(name, got[name].title, got[name].abbr, manual=False)
                log.info("subjects: %d new names, %d named", len(todo), sum(name in got for name in todo))
                known = self.all()
            return {name: known[name] for name in wanted if name in known}

    async def try_resolve(self, names: Iterable[str | None], who: str) -> None:
        """resolve() for the RUZ poll (`who` "ruz"), the batches ("batches") and HSE App X ("hseapp"): a failure
        leaves the names as they are until the next try of the same caller."""
        unknown = self.unknown(names)
        if not unknown:  # named meanwhile, or no longer needed: nothing of this caller waits
            self._failed.pop(who, None)
            return
        try:
            await self.resolve(unknown)
            self._failed.pop(who, None)
        except BudgetExceeded as e:  # the budget tells the System topic itself
            log.info("subject names wait for the LLM limit: %s", e)
        except LLMError as e:
            self._failed[who] = (str(e), set(unknown))
            log.warning("subject names: %s; the names stay as the sources give them", e)

    def _settle(self) -> None:
        """A caller whose waiting names all got named by hand waits no longer."""
        known = set(self.all())
        self._failed = {who: failed for who, failed in self._failed.items() if not failed[1] <= known}

    async def _ask(self, names: list[str], known: dict[str, Subject],
                   too_long: dict[str, str] | None = None) -> dict[str, Subject]:
        answer = await self.llm.json("subjects", SYSTEM, prompt(names, known, too_long), SCHEMA,
                                     max_tokens=max(MAX_TOKENS, 80 * len(names)))
        result = {}
        for item in answer.get("items") or []:
            try:
                index = int(item["id"])
            except (KeyError, TypeError, ValueError):
                continue
            if not 0 <= index < len(names):
                continue
            name = names[index]
            title = " ".join(str(item.get("title") or "").split())
            abbr = " ".join(str(item.get("abbr") or "").split())[:ABBR_CHARS].strip()
            if title and abbr:
                result[name] = Subject(title, abbr)
        return result
