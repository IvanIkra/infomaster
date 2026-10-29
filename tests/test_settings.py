import pytest

from infomaster.settings import ChatSettings, Settings, SettingsError
from infomaster.store import Store


@pytest.fixture
def settings():
    return Settings(Store())


def test_defaults(settings):
    assert settings.get("llm_daily_limit_usd") == 0.50
    assert settings.get("llm_prices")["claude-haiku-5-5"]["output"] == 0.50


def test_default_is_not_shared(settings):
    settings.get("llm_prices")["claude-haiku-5-5"]["input"] = 99
    assert settings.get("llm_prices")["claude-haiku-5-5"]["input"] == 0.10


def test_set_reset(settings):
    assert settings.set("llm_daily_limit_usd", 1) == 1.0
    assert settings.get("llm_daily_limit_usd") == 1.0
    settings.reset("llm_daily_limit_usd")
    assert settings.get("llm_daily_limit_usd") == 0.50


def test_survives_reopen(tmp_path):
    Settings(Store(tmp_path / "db.sqlite")).set("llm_daily_limit_usd", 2)
    assert Settings(Store(tmp_path / "db.sqlite")).get("llm_daily_limit_usd") == 2.0


@pytest.mark.parametrize("value", [0, -1, "1", True, None])
def test_bad_limit(settings, value):
    with pytest.raises(SettingsError):
        settings.set("llm_daily_limit_usd", value)


@pytest.mark.parametrize("value", [{}, {"m": {"input": 1}}, {"m": {"input": 1, "output": 1, "long_input": 1,
                                                                   "long_output": -1}}])
def test_bad_prices(settings, value):
    with pytest.raises(SettingsError):
        settings.set("llm_prices", value)


def test_unknown(settings):
    with pytest.raises(SettingsError):
        settings.get("nope")
    with pytest.raises(SettingsError):
        settings.set("nope", 1)


def test_chat_defaults():
    chats = ChatSettings(Store())
    assert chats.all(-1001) == {"enabled": False, "study": False, "filter": "all", "people": [], "images": False,
                                "pdf_content": False, "summary": False, "urgent": False, "batch_hours": None}
    assert chats.enabled_ids() == set()


def test_chat_update():
    chats = ChatSettings(Store())
    assert chats.update(-1001, {"enabled": True, "batch_hours": 6})["batch_hours"] == 6
    chats.update(-1002, {"enabled": True})
    chats.update(-1002, {"enabled": False})
    assert chats.enabled_ids() == {-1001}
    assert chats.update(-1001, {"batch_hours": None})["batch_hours"] is None


@pytest.mark.parametrize("values", [{"enabled": 1}, {"filter": "none"}, {"people": [1, "2"]}, {"people": [True]},
                                    {"batch_hours": 5}, {"batch_hours": True}, {"nope": 1}])
def test_bad_chat_settings(values):
    chats = ChatSettings(Store())
    with pytest.raises(SettingsError):
        chats.update(-1001, {"study": True, **values})
    assert chats.get(-1001, "study") is False  # checked before anything is saved


def test_languages(settings):
    """D41: English by default, each its own, only English or Russian."""
    assert settings.get("forum_language") == settings.get("miniapp_language") == "en"
    assert settings.set("forum_language", "ru") == "ru" and settings.get("miniapp_language") == "en"
    for wrong in ("de", "RU", "", None):
        with pytest.raises(SettingsError):
            settings.set("miniapp_language", wrong)
