"""Own emoji: the pack's ids, the custom emoji over the text's emoji, the posts' formatting."""

from types import SimpleNamespace

from telethon.extensions import html, markdown
from telethon.tl import types

from infomaster.outputs import emoji
from infomaster.outputs.forum import Forum

IDS = {"🆕": 1, "✖": 2, "💬": 3}


def custom(entities) -> list[tuple[int, int, int]]:
    return [(e.offset, e.length, e.document_id) for e in entities if isinstance(e, types.MessageEntityCustomEmoji)]


def test_every_icon_has_its_file():
    for key in emoji.ICONS:
        data = (emoji.ASSETS / f"{key}.webp").read_bytes()
        assert data[:4] == b"RIFF" and data[8:12] == b"WEBP", key
    assert len({emoji.plain(e) for e in emoji.ICONS.values()}) == len(emoji.ICONS)  # one emoji, one icon


def test_pack_ids_and_missing():
    def document(doc_id, alt):
        sticker = types.DocumentAttributeCustomEmoji(alt, types.InputStickerSetEmpty())
        return SimpleNamespace(id=doc_id, attributes=[types.DocumentAttributeFilename("x.webp"), sticker])
    ids = emoji.pack_ids([document(10, "🆕"), document(11, "✖️"), document(12, "🆕")])
    assert ids == {"🆕": 10, "✖": 11}  # the variation selector does not count, the first one wins
    missing = emoji.missing(ids)
    assert "new" not in missing and "removed" not in missing and "chat" in missing


def test_entities_in_utf16_beside_the_text_ones():
    text, found = html.parse("<b>🆕 Calc</b> — 𝔸 ✖️ ✖ 💬")
    entities = emoji.entities(text, found, IDS)
    # 🆕 and 𝔸 are two UTF-16 units each; ✖️ is two characters, ✖ one
    assert custom(entities) == [(0, 2, 1), (13, 2, 2), (16, 1, 2), (18, 2, 3)]
    assert [(e.offset, e.length) for e in entities if isinstance(e, types.MessageEntityBold)] == [(0, 7)]
    assert emoji.entities(text, found, {}) is found


def test_forum_formats_with_the_pack_only():
    forum = SimpleNamespace(emoji={}, client=SimpleNamespace(parse_mode=markdown))
    assert Forum._formatted(forum, "<b>x</b> 🆕", True) == ("<b>x</b> 🆕", {"parse_mode": "html"})
    assert Forum._formatted(forum, "**x** 🆕", False) == ("**x** 🆕", {})

    forum.emoji = IDS
    text, formatting = Forum._formatted(forum, "**x** 🆕", False)
    assert text == "x 🆕" and custom(formatting["formatting_entities"]) == [(2, 2, 1)]
    text, formatting = Forum._formatted(forum, "<a href='https://t.me/x'>chat</a> 💬", True)
    assert text == "chat 💬" and custom(formatting["formatting_entities"]) == [(5, 2, 3)]
    assert Forum._preview(True) == {"link_preview": False} and Forum._preview(False) == {}
