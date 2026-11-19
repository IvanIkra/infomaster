# infomaster

[![CI](https://github.com/IvanIkra/infomaster/actions/workflows/ci.yml/badge.svg)](https://github.com/IvanIkra/infomaster/actions/workflows/ci.yml)
![Python 3.12+](https://img.shields.io/badge/python-3.12%2B-blue)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A self-hosted study assistant for university students. infomaster reads course chats, the university
mailbox and the university's own services, works out what matters with an LLM, and publishes the result to a
Telegram forum, a Telegram Mini App, iCal feeds and a read-only HTTP API.

It was built for HSE University (RUZ timetable, HSE App X, an `@edu.hse.ru` mailbox) and as the data backend of
Tablo, a home dashboard; both integrations are optional.

## Features

- **Telegram reader.** A separate user account (the "twin") reads the chosen chats and channels over MTProto.
  Messages are processed in batches every N hours; in chats marked urgent, soon after a pin, a PDF or a message
  from an important person.
- **News and deadlines from free text.** Claude Haiku turns batches of messages and letters into news items,
  deadlines and events with JSON-schema structured output; a course dictionary keeps course names consistent.
- **One deadline list.** Deadlines from HSE App X, RUZ midterms, mail, chats and the owner's own entries are merged,
  de-duplicated and can be hidden; the same list goes to the forum, the Mini App and the API.
- **Timetable.** Classes from the RUZ iCal feed plus events and class changes found in chats, published as
  `main.ics` (where one has to be) and `optional.ics` (where one may go), ready for Apple or Google Calendar.
- **Problem sheets.** PDFs from chats are downloaded on demand, read within a page limit and versioned; a new
  version edits the old forum message and says what changed.
- **Grades.** Teachers' Google Sheets and Dropbox .xlsx links are read as xlsx; Haiku maps each tab's header once (cached with the header),
  grades are normalised to a 0–10 scale; the new and changed ones are in the Mini App and the API.
- **Quiet by default.** In the forum only new and moved deadlines, deadline reminders and source failures notify;
  everything else arrives silently or not at all.
- **Images.** Pictures in messages are read by Haiku within a daily count and the budget.
- **LLM budget.** Every call is journaled with its cost; a daily dollar limit with priorities (study chats first,
  then mail, then the rest) and a 30-day spending chart in the Mini App.
- **Two languages.** The forum and the Mini App switch independently between English and Russian.
- **Claude connector.** A remote MCP server with its own OAuth (a one-time code from the bot instead of a password):
  added to claude.ai as a custom connector, Claude reads the deadlines, timetable, grades, news and messages and
  changes what the Mini App changes; every change is journaled with an undo in the Mini App.

## How it works

```mermaid
flowchart LR
    subgraph Sources
        TG[Telegram chats<br/>via the twin]
        MAIL[University mail<br/>IMAP]
        RUZ[RUZ timetable<br/>iCal]
        HSE[HSE App X<br/>deadlines]
        GS[Google Sheets<br/>grades]
    end
    CORE[<b>infomaster</b><br/>collectors → SQLite<br/>→ Claude Haiku parsing<br/>→ dictionary and merge]
    subgraph Outputs
        FORUM[Telegram forum<br/>topics and pins]
        APP[Telegram Mini App]
        ICS[iCal feeds]
        API[Read-only API]
        MCP[Claude connector<br/>MCP + OAuth]
    end
    TG & MAIL & RUZ & HSE & GS --> CORE
    CORE --> FORUM & APP & ICS & API & MCP
```

A single asyncio process runs the collectors, the processing pipeline and two HTTP servers:

| Port | Exposed | Serves |
|---|---|---|
| public, `8080` | through Tailscale Funnel | Mini App (Telegram `initData` check, owner only), HSE App X login callback, personal iCal feeds, the MCP connector `/mcp` with its OAuth endpoints |
| internal, `8081` | Docker network only | `/api/deadlines`, `/api/news`, `/api/grades`, `/api/status` (Bearer token), feeds for Tablo |

The full design is in [docs/architecture.md](docs/architecture.md); the reasoning behind each choice is in
[docs/decisions.md](docs/decisions.md) (decisions are referred to as D1, D2… in the code), and the Tablo logic it
inherited is in [docs/sources-overview.md](docs/sources-overview.md) (referred to as §N).

## Tech stack

Python 3.12 · asyncio · [Telethon](https://github.com/LonamiWebs/Telethon) · aiohttp · SQLite ·
[Anthropic SDK](https://github.com/anthropics/anthropic-sdk-python) · pypdf · openpyxl · icalendar ·
Docker Compose · Tailscale Funnel. The Mini App is plain HTML and JavaScript with no build step.

## Getting started

You need a Telegram bot, a forum group, a spare Telegram account for the twin, an Anthropic API key and a Tailscale
account with Funnel enabled.

```bash
git clone https://github.com/IvanIkra/infomaster.git
cd infomaster
cp .env.example .env    # fill in the values, every variable is commented
docker compose up -d --build
```

Then send `/start` to the bot, open the Mini App and log the twin in under **Settings → Twin and chats**.
The step-by-step guide (bot rights, Tailscale policy, every optional source) is in [docs/setup.md](docs/setup.md).

### Configuration

Secrets live in `.env`; everything else is set in the Mini App.

| Variable | Required | Purpose |
|---|---|---|
| `TG_API_ID`, `TG_API_HASH` | yes | Telegram application from my.telegram.org |
| `TG_BOT_TOKEN` | yes | bot from @BotFather, an admin of the forum |
| `TG_OWNER_ID` | yes | the only Telegram user the bot and the Mini App let in |
| `TG_FORUM_ID` | yes | the forum group, `-100…` |
| `ANTHROPIC_API_KEY` | yes | batch processing, sheets, grades, images |
| `API_TOKEN` | yes | Bearer token of the internal API |
| `PUBLIC_URL` | yes | Funnel address, `https://infomaster.<tailnet>.ts.net` |
| `TS_AUTHKEY` | first start | auth key for the Tailscale sidecar |
| `CONTAINER_DNS_1`, `CONTAINER_DNS_2` | yes | DNS servers of the containers |
| `RUZ_ICAL_URL` | no | personal RUZ feed |
| `CALENDAR_SECRET`, `CALENDAR_PUBLIC_SECRET` | no | secret path segments of the internal and public iCal feeds |
| `MAIL_USER`, `MAIL_PASSWORD` | no | university mailbox with an app password |

## Development

```bash
uv sync               # .venv on Python 3.12 from .python-version, exact versions from uv.lock
uv run pytest -q
uv run ruff check .
```

Dependencies are managed with [uv](https://docs.astral.sh/uv/): `uv add <package>` updates `pyproject.toml` and
`uv.lock` together, and the Docker image installs the same locked versions.

Run locally with `uv run python -m infomaster --data data`. Other commands: `login` logs the twin in from a
terminal, `import-tablo --tablo-db <tablo.db>` moves own and hidden deadlines from Tablo once.

```
infomaster/
├── collectors/   # Telegram twin, mail, RUZ, HSE App X, files, grade sheets
├── processing/   # batches, deadlines and merge, events, schedule, sheets, grades, images
├── outputs/      # forum bot and its topics, Mini App, iCal feeds, internal API
├── connector/    # MCP connector for claude.ai: OAuth, the /mcp endpoint, tools, journal with undo
├── library/      # raw course material and the knowledge base, signed file links, feeding from chats and the bot
├── llm/          # client with structured output, daily budget, journal
├── settings/     # general and per-chat settings
├── store.py      # SQLite
└── app.py        # wiring
tests/            # pytest
docs/             # architecture, decisions, setup guide, notes on Tablo
```

## Status

Stages 1–9 of the plan ([docs/architecture.md](docs/architecture.md), §11) are done and covered by tests: the
twin and chat settings, batch processing, the course dictionary and timetable, deadlines, problem sheets,
summaries, grades and images. The API for Tablo (stage 10) is done on the infomaster side, and the service runs
next to Tablo.

The second round (§11.1) has begun. Stage 11, "Quiet", is done: only deadlines, their reminders and source
failures notify; grades no longer go to the forum, the separate messages about important news are gone, and the
summaries of stage 7 are removed. A sheet without a due date is due at the next seminar (stage 12, first half), grade
sheets may be Dropbox links (stage 15, first half), and the MCP connector (stage 13) is done and covered by tests; it
is connected to claude.ai. Setting it up: [docs/setup.md](docs/setup.md), "Claude connector". The library (stage 14,
D58) keeps raw material per course and class (from study chats, sheets, files sent to the bot and Mini App uploads)
and the knowledge base Claude makes of it (LaTeX and PDF per class, a book and a cheat sheet per course, every version
kept); files move by signed links. It is covered by tests, not yet tried on live data.

Not yet verified on live Telegram, Haiku and Google Sheets: problem sheets, grades, images, the reworked Mini App,
the language switch, the silent posts of stage 11 and the assumed due dates. Text length is checked in characters rather than by the rendered
width of the Inter font.

## License

[MIT](LICENSE)
