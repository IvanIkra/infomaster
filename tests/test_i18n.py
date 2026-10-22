import json

from infomaster.i18n import Phrase, json_default, reason, say, tr


def test_phrase():
    phrase = Phrase("the file is gone", "файла нет")
    assert str(phrase) == "the file is gone" and f"{phrase}" == "the file is gone"  # the log speaks English
    assert phrase.get("ru") == "файла нет" and phrase.get("en") == "the file is gone"
    assert say(phrase, "ru") == "файла нет" and say("as it is", "ru") == "as it is"
    assert tr("ru", "yes", "да") == "да" and tr("en", "yes", "да") == "yes"


def test_reasons_and_json():
    assert reason(ValueError(Phrase("no code", "нет кода")), "ru") == "нет кода"
    assert reason(ValueError("plain"), "ru") == "plain" and reason(ValueError(), "ru") == ""
    data = {"error": Phrase("the LLM limit", "лимит LLM"), "ok_at": None}
    assert json.loads(json.dumps(data, default=json_default("ru"))) == {"error": "лимит LLM", "ok_at": None}
    try:
        json.dumps({"x": object()}, default=json_default("en"))
    except TypeError:
        pass
    else:
        raise AssertionError("anything but a Phrase is still not JSON")
