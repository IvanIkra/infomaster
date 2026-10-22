"""SQLite storage (§9). Tables arrive with the stages that fill them; `create table if not exists` adds them to a
database of an earlier stage, and UPGRADES add the columns a later stage gave to an existing table."""

import json
import sqlite3
from pathlib import Path

SCHEMA = """
create table if not exists kv (
    key text primary key,
    value text not null         -- JSON
);
create table if not exists settings (
    name text primary key,
    value text not null         -- JSON; no row means the default from infomaster.settings
);
create table if not exists llm_runs (   -- every LLM call, also a failed one, kept for good (§7)
    id integer primary key,
    at text not null,           -- UTC, ISO 8601
    task text not null,         -- batch, pdf, image, summary, subjects, grades, hseapp_labels
    chat_id integer,            -- chat the input came from, if any
    model text not null,
    ok integer not null,
    error text,
    input_tokens integer not null,
    output_tokens integer not null,
    cache_write integer not null,
    cache_read integer not null,
    cost_usd real not null,
    duration_s real not null
);
create index if not exists llm_runs_at on llm_runs(at);

create table if not exists chats (      -- every chat of the twin's account, enabled or not (§5.1)
    id integer primary key,     -- Telethon's marked id: users > 0, groups < 0, channels -100...
    title text not null,
    kind text not null,         -- user, bot, group, supergroup, forum, channel
    username text,              -- for t.me links
    last_id integer not null default 0,  -- the newest message seen; the catch-up after a restart starts here
    added_at text not null
);
create table if not exists chat_settings (
    chat_id integer not null,
    name text not null,
    value text not null,        -- JSON; no row means the default from infomaster.settings
    primary key (chat_id, name)
);
create table if not exists messages (   -- raw messages and mail, kept 14 days (р. 29)
    id integer primary key,
    source text not null,       -- tg, mail
    chat_id integer not null,   -- a letter: 0, the mailbox (collectors/mail.py)
    msg_id integer not null,    -- a letter: its UID plus an offset that grows when UIDVALIDITY changes
    sender_id integer,
    sender_name text,
    at text not null,
    text text not null,         -- hidden links are spelled out after their text
    topic_id integer,           -- forum topic
    reply_to integer,           -- msg_id in the same chat
    reply_quote text,           -- the replied message when it is not stored itself
    forward text,               -- JSON {from, chat_id, msg_id}
    grouped_id integer,         -- album: its messages share it
    files text,                 -- JSON [{kind, name, mime, size}]; downloaded only when needed
    pinned integer not null default 0,
    edited_at text,
    deleted_at text,
    parsed_at text,
    summarized_at text,
    subject text,               -- a letter's: shown instead of a link (§8.3)
    unique (source, chat_id, msg_id)
);
create index if not exists messages_at on messages(at);

create table if not exists news (       -- parsed news, kept for good (§6.1, §8.3)
    id integer primary key,
    source text not null,       -- tg, mail
    chat_id integer not null,
    msg_ids text not null,      -- JSON [msg_id]: the messages it was made of, the first one tells it
    study integer not null,
    posted_at text not null,    -- the newest of its messages: its 24 hours count from here
    full text not null,
    short text not null,
    importance integer not null,
    replaced_by integer,        -- a newer item that tells the same story
    gone_at text,               -- its messages were deleted or edited into something else
    hidden integer not null default 0,  -- by the owner's button under its alert: off the list and the API
    created_at text not null
);
create table if not exists deadlines (  -- deadlines from the chats, mail and RUZ (§6.4); HSE App X keeps its own in kv
    id integer primary key,
    source text not null,       -- tg, mail; ical: the Midterm of an interim assessment (§4.4)
    chat_id integer,
    msg_ids text,               -- JSON [msg_id]
    course text,                -- as the source names it; its abbr is in the dictionary (subjects)
    label text not null,
    due text not null,          -- YYYY-MM-DD
    moved_by integer,           -- the deadline with the new date
    gone_at text,
    created_at text not null,
    sheet_id integer            -- read in the pages of a sheet: an edit of its message does not withdraw it
);
create index if not exists deadlines_due on deadlines(due);
create table if not exists own_deadlines (     -- the owner's own, from HA's to-do lists or the Mini App (§5.4, р. 8)
    id integer primary key,     -- the uid is ha-<id>
    title text not null,
    due text not null,          -- YYYY-MM-DD
    category text not null      -- study, personal
);
create table if not exists hidden_deadlines (  -- deadlines of the sources hidden for good (§5.4)
    uid text primary key,       -- <source>-<sha1(source|title|due)[:10]>
    source text not null,
    course text,                -- the abbr the merge compares by, null when the dictionary had none
    title text not null,
    due text not null,
    ref text                    -- the row it came from (dl:<id>, hseapp:<id>): hidden through renames too
);
create table if not exists bot_posts (  -- what the bot published, to edit or delete it later (§8.3)
    id integer primary key,
    topic text not null,
    msg_id integer not null,
    kind text not null,         -- news_board, news_alert, deadline_board, deadline, event, ruz
    ref integer,                -- news, deadline or event id
    key text,                   -- of a deadline post: the merged deadline its button acts on (dl:<id>, hseapp:<id>)
    digest text,                -- of the text: an edit that changes nothing is skipped
    at text not null
);

create table if not exists subjects (   -- the course dictionary, one for everything (§6.2, §2.2)
    name text primary key,      -- as a source gives it, cleaned
    title text not null,        -- short English: the timetable, the iCal feed
    abbr text not null,         -- at most 6 characters: the prefix of deadline labels
    manual integer not null default 0  -- set in the Mini App (р. 3): Haiku never overwrites it
);
create table if not exists classes (    -- the RUZ timetable (§5.2, §4.2): the current feed and the past classes
    id integer primary key,     -- c<id> in the batch prompt
    uid text not null unique,   -- UID of the feed, ours too
    subject text not null,      -- cleaned SUMMARY: a name of the dictionary
    start text not null,
    end text not null,
    kind text not null,         -- the first line of DESCRIPTION: Лекция, Seminar, Ongoing interim assessment
    room text not null,         -- R609, online or ""
    link text,                  -- of an online class: from LOCATION, DESCRIPTION or URL
    cancelled integer not null default 0,  -- STATUS:CANCELLED in the feed
    deadline_id integer,        -- the Midterm deadline of an interim assessment (§4.4)
    seen_at text not null,      -- the last poll that had it; a past class leaves the feed and stays here
    gone_at text                -- left the feed before it started: RUZ removed it
);
create table if not exists events (     -- events from chats and chat corrections of RUZ classes (§6.5)
    id integer primary key,     -- e<id> in the batch prompt
    uid text not null,          -- UID of the feed: a move keeps it; a correction has the uid of its class
    source text not null,       -- tg (mail and pdf come with their stages)
    chat_id integer,
    msg_ids text not null,      -- JSON [msg_id]: the messages that told it, the first one first
    class_uid text,             -- a correction of this class: moved or cancelled; null for an event of its own
    course text,                -- as the source names it
    title text not null,        -- short English
    kind text not null,         -- consultation, event...; a correction keeps the kind of its class
    start text not null,
    end text,                   -- null when the source does not say
    room text not null default '',  -- a room or a link
    required integer not null,  -- the main calendar (1) or the optional one (0), р. 28
    chosen integer not null default 0,  -- `required` set by the owner: moves and edits keep it
    hidden integer not null default 0,  -- out of the feeds; a hidden correction is not applied
    cancelled integer not null default 0,
    replaced_by integer,        -- a newer version: moved, cancelled or edited; the newest one is in force
    gone_at text,               -- its messages were deleted or edited into something else, or RUZ overruled it
    created_at text not null
);
create index if not exists events_uid on events(uid);
create index if not exists events_class on events(class_uid);

create table if not exists files (      -- PDFs and images of chat messages, fetched when a batch needs them;
                                        -- kept 14 days (р. 29)
    id integer primary key,
    source text not null,       -- tg
    chat_id integer not null,
    msg_id integer not null,
    at text not null,           -- the message's: the file goes with it
    name text,                  -- as Telegram gives it
    size integer,
    sha256 text,                -- null when the download or the reading failed
    path text,                  -- under the data directory
    pages integer,
    first_page text,            -- read locally; "" for a scan
    error text,
    kind text not null default 'pdf',  -- pdf, image
    reading text,               -- of an image: JSON {shows, text} as Haiku read it; "" when it could not; null unread
    unique (source, chat_id, msg_id)
);
create index if not exists files_sha256 on files(sha256);
create table if not exists sheets (     -- листочки (§6.3, р. 14): kept for good, the files live in the forum
    id integer primary key,
    chat_id integer not null,
    msg_id integer not null,    -- the message with the PDF
    name text not null,
    sha256 text not null,       -- its file while it is kept (collectors/files.py); the same file again is no new sheet
    course text,                -- as the model names it, a name of the dictionary
    number text not null default '',
    due text,                   -- YYYY-MM-DD
    status text not null,       -- new: waits for the forum; posted; failed (the file was lost)
    error text,
    text text,                  -- its pages read locally, for the diff of a newer version; null until prepared
    summary text,               -- what the model read in it (pdf_content)
    changes text,               -- of a newer version: what changed, from the diff
    previous integer,           -- the version it updates: the same message, or the rules of р. 14, or the owner
    version integer not null default 1,
    post_id integer,            -- the bot's message in «Листочки»; every version of a sheet edits the first one's
    created_at text not null
);
create index if not exists sheets_chat on sheets(chat_id, created_at);

create table if not exists grade_sheets (       -- grade sheets read at least once (§6.7): the first read is a baseline
    sheet_id text primary key,  -- from its link
    title text,                 -- as Google names it, when known
    baseline_at text not null
);
create table if not exists grade_items (        -- the current grade of every work in the sheets (Tablo's §3.6)
    key text primary key,       -- <sheet>|<tab>|<column>|<its header>: a new label is no new grade
    sheet_id text not null,
    tab text not null,
    header text not null,       -- the column's header: a work whose column moved is found by it with the same grade
    label text not null,        -- "<course> <work>": DM HW 1A
    value real not null,        -- 0-10
    raw real not null,          -- the points as the sheet has them
    max real not null,          -- out of
    bonus real,
    position integer not null,  -- in sheet order
    gone integer not null default 0  -- not in the sheet now (a hidden tab, a cleared cell): back with the same grade,
                                     -- it is no new grade
);
create table if not exists grade_events (       -- new and changed grades: the «Оценки» topic and the API's window
    id integer primary key,
    key text not null,
    sheet_id text not null,
    label text not null,
    value real not null,
    raw real not null,
    max real not null,
    bonus real,
    previous real,              -- the grade before, null for a first one
    seen_at text not null,
    post_id integer             -- the bot's message in «Оценки»; null while it waits
);
create index if not exists grade_events_seen on grade_events(seen_at);
"""

# (table, column, definition): columns added to a table that an earlier stage created already.
UPGRADES = (
    ("news", "hidden", "integer not null default 0"),
    ("bot_posts", "key", "text"),
    ("messages", "subject", "text"),
    ("deadlines", "sheet_id", "integer"),
    ("files", "kind", "text not null default 'pdf'"),
    ("files", "reading", "text"),
)


class Store:
    def __init__(self, path: Path | str = ":memory:"):
        if path != ":memory:":
            Path(path).parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(str(path))
        self.db.row_factory = sqlite3.Row
        self.db.executescript(SCHEMA)
        self._upgrade()

    def _upgrade(self) -> None:
        with self.db:
            for table, column, definition in UPGRADES:
                columns = {row["name"] for row in self.db.execute(f"pragma table_info({table})")}
                if column not in columns:
                    self.db.execute(f"alter table {table} add column {column} {definition}")

    def get_json(self, key: str, default=None):
        row = self.db.execute("select value from kv where key = ?", (key,)).fetchone()
        return json.loads(row["value"]) if row else default

    def put_json(self, key: str, value) -> None:
        with self.db:
            self.db.execute(
                "insert into kv(key, value) values(?, ?) on conflict(key) do update set value = excluded.value",
                (key, json.dumps(value, ensure_ascii=False)),
            )

    def close(self) -> None:
        self.db.close()
