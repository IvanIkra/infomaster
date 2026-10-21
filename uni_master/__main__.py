"""Run the service: python -m uni_master [--data data]; log the twin in once: python -m uni_master login; copy the own
and hidden deadlines and the manual dictionary entries from Tablo once: python -m uni_master import-tablo --tablo-db
<tablo.db> (uni_master/tablo.py)."""

import argparse
import asyncio
import logging
import sqlite3
import sys
from pathlib import Path

from . import clock, tablo
from .app import Service, twin_session
from .collectors import telegram
from .config import ConfigError, Env, load_env
from .store import Store


def main() -> None:
    parser = argparse.ArgumentParser(prog="uni_master")
    parser.add_argument("command", nargs="?", choices=("run", "login", "import-tablo"), default="run")
    parser.add_argument("--data", type=Path, default=Path("data"), help="directory for the database and sessions")
    parser.add_argument("--log-level", default="INFO")
    parser.add_argument("--tablo-db", type=Path, help="import-tablo: a copy of Tablo's tablo.db")
    args = parser.parse_args()
    if args.command == "import-tablo":
        sys.exit(import_tablo(args))
    load_env(Path(".env"))

    logging.basicConfig(level=args.log_level, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    logging.getLogger("telethon").setLevel(logging.WARNING)
    try:
        env = Env.from_environ()
    except ConfigError as e:
        sys.exit(f"uni-master: {e}")
    if args.command == "login":
        asyncio.run(telegram.login(env, twin_session(args.data)))
    else:
        asyncio.run(Service(env, args.data).run())


def import_tablo(args: argparse.Namespace) -> str | None:
    """Needs no secrets: only the two databases. Run it with the service stopped."""
    if args.tablo_db is None:
        return "uni-master: import-tablo needs --tablo-db <tablo.db>"
    try:
        source = tablo.open_tablo(args.tablo_db)
    except (tablo.TabloError, sqlite3.Error) as e:
        return f"uni-master: {e}"
    args.data.mkdir(parents=True, exist_ok=True)
    store = Store(args.data / "uni-master.db")
    try:
        counts = tablo.import_tablo(store, source, clock.local_day(clock.now_utc()))
    finally:
        store.close()
        source.close()
    print(f"own deadlines: {counts['own']} added, {counts['own_skipped']} skipped\n"
          f"hidden deadlines: {counts['hidden']} added, {counts['hidden_skipped']} skipped\n"
          f"dictionary entries set by hand: {counts['subjects']} added, {counts['subjects_skipped']} skipped")
    return None


if __name__ == "__main__":
    main()
