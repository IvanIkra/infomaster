"""PDFs read locally (§6.3, D14): the text of their pages, the first pages as a PDF of their own for the model, the
comparison of two versions. pypdf only (MIT): it cannot render a page, so a scan goes to the model as a PDF of its
first pages, which the model reads as page images.

Everything here is synchronous and may take a while on a big file: call it through `asyncio.to_thread`.
"""

import difflib
import io
import re
from dataclasses import dataclass
from pathlib import Path

from pypdf import PdfReader, PdfWriter

SCAN_WORDS = 5  # a first page with fewer words has no text layer worth the name: a scan


class PdfError(ValueError):
    """The file is no PDF pypdf can read: broken, or encrypted with a password."""


@dataclass(frozen=True)
class Extract:
    pages: int                  # in the whole file
    texts: list[str]            # of the first pages read

    @property
    def first(self) -> str:
        return self.texts[0] if self.texts else ""

    @property
    def text(self) -> str:
        return "\n".join(self.texts)


def scanned(first_page: str) -> bool:
    return len(first_page.split()) < SCAN_WORDS


def _reader(path: Path) -> PdfReader:
    try:
        reader = PdfReader(str(path))
        if reader.is_encrypted and not reader.decrypt(""):
            raise PdfError("the PDF is encrypted with a password")
        reader.pages[0]  # noqa: B018 - an empty or broken page tree fails here, not later
        return reader
    except PdfError:
        raise
    except Exception as e:
        raise PdfError(f"cannot read the PDF: {e}") from e


def extract(path: Path, pages: int) -> Extract:
    """The text of the first `pages` pages; a page that fails to give its text gives ""."""
    reader = _reader(path)
    texts = []
    for page in reader.pages[:pages]:
        try:
            texts.append(_clean(page.extract_text() or ""))
        except Exception:
            texts.append("")
    return Extract(len(reader.pages), texts)


def head(path: Path, pages: int) -> bytes:
    """A PDF of the first `pages` pages: what the model sees of a scan."""
    reader = _reader(path)
    writer = PdfWriter()
    for page in reader.pages[:pages]:
        writer.add_page(page)
    out = io.BytesIO()
    writer.write(out)
    return out.getvalue()


def _clean(text: str) -> str:
    """Lines without trailing blanks, no empty runs: the extract of pypdf is loose with spaces."""
    lines = [re.sub(r"[ \t ]+", " ", line).strip() for line in text.splitlines()]
    return "\n".join(line for line in lines if line)


def diff(old: str, new: str, limit: int) -> str:
    """The changed lines of two texts, '-' old and '+' new, at most `limit` characters; "" when the text is the same.
    Only this goes to the model when it describes a new version (D14)."""
    lines = [line for line in difflib.unified_diff(old.splitlines(), new.splitlines(), lineterm="", n=0)
             if line[:1] in "+-" and not line.startswith(("+++", "---"))]
    text = "\n".join(lines)
    return text if len(text) <= limit else text[:limit].rsplit("\n", 1)[0] + "\n…"


# --- names of files ------------------------------------------------------------------------

# Words that mark a version, not another sheet: "hw5_v2_final (1).pdf" is still "hw5".
VERSION_WORDS = re.compile(
    r"\((?:\d+)\)|\bv(?:er)?\.?\s*\d+\b|\b(?:new|final|fixed|upd(?:ated)?|copy|rev\d*)\b"
    r"|(?<![а-яё])(?:нов\w*|испр\w*|обновл\w*|финал\w*|копия|верси\w*)", re.IGNORECASE)
NAME_SIMILARITY = 0.8


def name_key(name: str) -> str:
    """A file name without its extension, case, separators and version marks."""
    stem = re.sub(r"\.[A-Za-z0-9]{1,5}$", "", name.strip()).lower().replace("ё", "е")
    stem = VERSION_WORDS.sub(" ", re.sub(r"[_\-.,\[\]{}]+", " ", stem))  # "(1)" is a version mark: kept for it
    return " ".join(re.sub(r"[()]+", " ", stem).split())


def similar_names(a: str, b: str) -> bool:
    """Almost the same name (D14): the same numbers in it and most of the letters. "ДЗ3.pdf" and
    "ДЗ3_исправленный.pdf" are, "ДЗ3.pdf" and "ДЗ4.pdf" are not."""
    ka, kb = name_key(a), name_key(b)
    if not ka or not kb or re.findall(r"\d+", ka) != re.findall(r"\d+", kb):
        return False
    return ka == kb or difflib.SequenceMatcher(None, ka, kb).ratio() >= NAME_SIMILARITY
