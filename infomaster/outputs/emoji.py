"""The bot's own emoji: Tablo's MDI glyphs in one colour, repainted by Telegram to the colour of the text.

The pack `infomaster_by_<bot>` belongs to the owner: the bot creates it at start from `emoji-pack/` and adds what it
lacks. In the bot's posts every emoji the pack has is shown as the pack's one (Telegram allows custom emoji to a bot
whose owner has Premium); the texts keep the ordinary emoji, they are what Telegram shows without the pack. The topics'
icons are the owner's: a bot may set only Telegram's own ones.
"""

import re
from pathlib import Path

from telethon.tl import types

ASSETS = Path(__file__).parent / "emoji-pack"
TITLE = "infomaster"
# key -> the emoji it stands for (its file is emoji-pack/<key>.webp, the MDI glyph in the comment)
ICONS = {
    "new": "🆕",  # plus-circle-outline
    "removed": "✖️",  # close-circle-outline
    "moved": "↪️",  # calendar-arrow-right
    "restored": "↩️",  # arrow-u-left-top
    "room": "🚪",  # door
    "alert": "🔔",  # bell-outline
    "chat": "💬",  # send, as Tablo shows Telegram
    "channel": "📢",  # bullhorn-outline
    "person": "👤",  # account-outline
    "mail": "✉️",  # email-outline
    "hseapp": "🎓",  # school
    "calendar": "📅",  # calendar
    "own": "🏠",  # home-outline
    "grades": "📊",  # chart-box-outline
    "sheet": "📄",  # file-document-outline
    "updated": "🔄",  # sync
    "system": "⚙️",  # cog-outline: the icons of the topics from here on
    "news": "📰",  # newspaper-variant-outline
    "deadlines": "⏰",  # alert-circle-outline, as Tablo's deadlines
    "summaries": "📝",  # text-box-outline
}
VARIATION = "️"  # the emoji form of a symbol: ✖ and ✖️ are one emoji


def pack_name(bot_username: str) -> str:
    return f"infomaster_by_{bot_username}"


def plain(emoji: str) -> str:
    return emoji.replace(VARIATION, "")


def pack_ids(documents) -> dict[str, int]:
    """emoji -> the id of the pack's custom emoji for it, from the pack's documents."""
    ids = {}
    for document in documents:
        for attribute in document.attributes:
            if isinstance(attribute, types.DocumentAttributeCustomEmoji):
                ids.setdefault(plain(attribute.alt), document.id)
    return ids


def missing(ids: dict[str, int]) -> list[str]:
    """The keys of ICONS the pack has no emoji for."""
    return [key for key, emoji in ICONS.items() if plain(emoji) not in ids]


def entities(text: str, found: list, ids: dict[str, int]) -> list:
    """The text's entities and a custom emoji over every emoji the pack has. Offsets are in UTF-16, as Telegram's."""
    if not ids:
        return found
    pattern = re.compile("|".join(re.escape(e) + VARIATION + "?" for e in sorted(ids, key=len, reverse=True)))
    custom = [types.MessageEntityCustomEmoji(utf16(text[:m.start()]), utf16(m.group()), ids[plain(m.group())])
              for m in pattern.finditer(text)]
    return sorted(found + custom, key=lambda entity: entity.offset)


def utf16(text: str) -> int:
    return len(text.encode("utf-16-le")) // 2
