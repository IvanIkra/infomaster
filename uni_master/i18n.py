"""The languages of what the owner reads (р. 41): English or Russian, English by default.

Two general settings: `forum_language` for the bot (its posts, buttons and answers in the forum and in its chat, and
what the models write for them: the summaries, a sheet's summary and what changed in it) and `miniapp_language` for
the Mini App and the page the browser comes back to after the HSE login. A summary asked for with /summary may name
its own language. What Tablo and the iCal feeds get is English either way, so the news, the deadline labels and the
course titles the models write stay English in the forum too.

Text made away from its output (a collector's line for the System topic, the reason a login failed, an error kept for
a status) is a Phrase with both languages; the output picks one when it shows it: the forum when it posts, the Mini
App when it answers. str() of a Phrase is the English: the log and the API for Tablo.
"""

from dataclasses import dataclass
from typing import Any

LANGUAGES = ("en", "ru")


@dataclass(frozen=True)
class Phrase:
    en: str
    ru: str

    def __str__(self) -> str:
        return self.en

    def get(self, lang: str) -> str:
        return self.ru if lang == "ru" else self.en


def tr(lang: str, en: str, ru: str) -> str:
    return ru if lang == "ru" else en


def say(text: str | Phrase, lang: str) -> str:
    """A Phrase in the language; a plain text as it is."""
    return text.get(lang) if isinstance(text, Phrase) else text


def reason(error: Exception, lang: str) -> str:
    """The text of an exception, in the language when it was raised with a Phrase."""
    first = error.args[0] if error.args else None
    return first.get(lang) if isinstance(first, Phrase) else str(error)


def json_default(lang: str):
    """json.dumps(default=…) that writes a Phrase in the language."""
    def default(value: Any) -> str:
        if isinstance(value, Phrase):
            return value.get(lang)
        raise TypeError(f"{type(value).__name__} is not JSON serializable")
    return default
