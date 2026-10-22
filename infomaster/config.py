"""Secrets and addresses from the environment (§10). User settings live in the database, not here."""

import os
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path


class ConfigError(ValueError):
    pass


def load_env(path: Path) -> None:
    """KEY=VALUE lines from .env; real environment variables win."""
    if not path.exists():
        return
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            os.environ.setdefault(key.strip(), value.strip().strip("'\""))


@dataclass(frozen=True)
class Env:
    api_id: int
    api_hash: str
    bot_token: str
    owner_id: int
    forum_id: int
    api_token: str
    public_url: str         # Funnel address of the public port, no trailing slash
    public_port: int = 8080
    internal_port: int = 8081
    ruz_ical_url: str = ""      # optional: without it the timetable has no RUZ classes
    calendar_secret: str = ""   # optional: without it the iCal feeds are off
    mail_user: str = ""         # optional, with the password: without both the mail source is off
    mail_password: str = field(default="", repr=False)  # an app password of the box (Yandex 360)
    mail_host: str = "imap.yandex.ru"
    mail_port: int = 993

    @classmethod
    def from_environ(cls, environ: Mapping[str, str] = os.environ) -> "Env":
        required = {
            "api_id": "TG_API_ID", "api_hash": "TG_API_HASH", "bot_token": "TG_BOT_TOKEN",
            "owner_id": "TG_OWNER_ID", "forum_id": "TG_FORUM_ID", "api_token": "API_TOKEN",
            "public_url": "PUBLIC_URL",
        }
        missing = [var for var in required.values() if not environ.get(var, "").strip()]
        if missing:
            raise ConfigError(f"not set in the environment or .env: {', '.join(missing)}")
        values = {name: environ[var].strip() for name, var in required.items()}
        for name, var in (("public_port", "HTTP_PUBLIC_PORT"), ("internal_port", "HTTP_INTERNAL_PORT"),
                          ("mail_user", "MAIL_USER"), ("mail_password", "MAIL_PASSWORD"),
                          ("mail_host", "MAIL_HOST"), ("mail_port", "MAIL_PORT")):
            if environ.get(var, "").strip():
                values[name] = environ[var].strip()
        for name in ("api_id", "owner_id", "forum_id", "public_port", "internal_port", "mail_port"):
            if name in values:
                try:
                    values[name] = int(values[name])
                except ValueError:
                    raise ConfigError(f"{name} must be an integer, got {values[name]!r}") from None
        if not values["public_url"].startswith("https://"):
            raise ConfigError("PUBLIC_URL must be the https:// Funnel address")
        values["public_url"] = values["public_url"].rstrip("/")
        if ruz := environ.get("RUZ_ICAL_URL", "").strip():
            if not re.match(r"https?://", ruz):
                raise ConfigError("RUZ_ICAL_URL must be an http(s):// link")
            values["ruz_ical_url"] = ruz
        if secret := environ.get("CALENDAR_SECRET", "").strip():
            # A path segment that cannot be guessed: calendars send no headers, the secret is all the protection.
            if not re.fullmatch(r"[A-Za-z0-9_-]{16,}", secret):
                raise ConfigError("CALENDAR_SECRET must be at least 16 characters of A-Z, a-z, 0-9, _ and -")
            values["calendar_secret"] = secret
        return cls(**values)
