# infomaster: the external contract of the Python service

What the Go port must match so it can take over the same `data/` directory, the same SQLite file
(`data/infomaster.db`), the static web client in `web/static` (unchanged), Tablo / Home Assistant on the
internal port and claude.ai through the MCP connector.

Paths are relative to the repository root. `file:N-M` points at Python source to copy from. Every example value
below is made up. "Code points" means Python `len()`, which counts Unicode code points, not bytes (see §12).

Contents: 1 schema · 2 kv · 3 settings · 4 HTTP · 5 MCP · 6 ids and hashes · 7 time and JSON · 8 secrets ·
9 environment · 10 LLM · 11 outside sources · 12 porting traps · 13 test vectors · 14 cutover.

---

## 1. SQLite schema

The file is `data/infomaster.db`, opened with the default `sqlite3` settings: no WAL pragma, no foreign keys, rows by
name. Each module creates its own tables with `create table if not exists` when it starts. A Go port must run the
same DDL, because an existing database already has these tables and only gains columns.

### 1.1 `infomaster/store.py` SCHEMA (verbatim, run with `executescript` at every start)

```sql
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
    task text not null,         -- batch, pdf, image, subjects, grades, hseapp_labels; summary in old rows (D55)
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
create table if not exists messages (   -- raw messages and mail, kept 14 days (D29)
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
    sheet_id integer,           -- read in the pages of a sheet: an edit of its message does not withdraw it
    assumed integer not null default 0  -- a sheet's due date the service assumed: its course's next seminar (§6.3)
);
create index if not exists deadlines_due on deadlines(due);
create table if not exists own_deadlines (     -- the owner's own: HA, the web app, Claude (§5.4, D8)
    id integer primary key,     -- the uid is ha-<id>
    title text not null,
    due text not null,          -- YYYY-MM-DD
    category text not null,     -- study, personal
    course text                 -- the course key (its abbr) the web app gave it (§8.6); null: none
);
create table if not exists hidden_deadlines (  -- deadlines of the sources hidden for good (§5.4)
    uid text primary key,       -- <source>-<sha1(source|title|due)[:10]>
    source text not null,
    course text,                -- the abbr the merge compares by, null when the dictionary had none
    title text not null,
    due text not null,
    ref text,                   -- the row it came from (dl:<id>, hseapp:<id>): hidden through renames too
    reason text,                -- done, not_mine, duplicate (the web app, §8.6); null: hidden elsewhere, no reason
    kept text                   -- of a duplicate: the uid of the deadline it was merged into
);
drop table if exists bot_posts;      -- the forum's posts (§8.3 until D69): the forum is gone

create table if not exists subjects (   -- the course dictionary, one for everything (§6.2, §2.2)
    name text primary key,      -- as a source gives it, cleaned
    title text not null,        -- short English: the timetable, the iCal feed
    abbr text not null,         -- at most 6 characters: the prefix of deadline labels
    manual integer not null default 0  -- set by the owner (D3): Haiku never overwrites it
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
    required integer not null,  -- the main calendar (1) or the optional one (0), D28
    chosen integer not null default 0,  -- `required` set by the owner: moves and edits keep it
    hidden integer not null default 0,  -- out of the feeds; a hidden correction is not applied
    cancelled integer not null default 0,
    copy_of text,               -- an own copy of a chat's event: the uid of the hidden one it stands in for (D49)
    replaced_by integer,        -- a newer version: moved, cancelled or edited; the newest one is in force
    gone_at text,               -- its messages were deleted or edited into something else, or RUZ overruled it
    created_at text not null
);
create index if not exists events_uid on events(uid);
create index if not exists events_class on events(class_uid);

create table if not exists files (      -- PDFs and images of chat messages, fetched when a batch needs them;
                                        -- kept 14 days (D29)
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
create table if not exists sheets (     -- листочки (§6.3, D14): kept for good
    id integer primary key,
    chat_id integer not null,
    msg_id integer not null,    -- the message with the PDF
    name text not null,
    sha256 text not null,       -- its file while it is kept (collectors/files.py); the same file again is no new sheet
    course text,                -- as the model names it, a name of the dictionary
    number text not null default '',
    due text,                   -- YYYY-MM-DD
    due_assumed integer not null default 0,  -- none was given: `due` is the next seminar's, if any (§6.3)
    due_at text,                -- of an assumed date: the start of that seminar
    sent_at text,               -- the message's time (an own sheet's upload): the next seminar comes after it
    caption text,               -- of a PDF sent to the bot: the owner's words with it (D73)
    status text not null,       -- new: waits to be published; posted (published); failed (the file was lost)
    error text,
    text text,                  -- its pages read locally, for the diff of a newer version; null until prepared
    summary text,               -- what the model read in it (pdf_content)
    changes text,               -- of a newer version: what changed, from the diff
    previous integer,           -- the version it updates: the same message, or the rules of D14, or the owner
    version integer not null default 1,
    post_id integer,            -- the forum's message, until D69: kept for old rows
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
create table if not exists grade_events (       -- new and changed grades: the API's window
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
    post_id integer             -- the bot's message in the «Grades» topic, removed (D55): kept for old rows
);
create index if not exists grade_events_seen on grade_events(seen_at);
create table if not exists own_grades (         -- the owner's grades (D44): added by hand, or a sheet's changed
    id integer primary key,
    key text unique,            -- the sheet's work it changes (grade_items.key); null for one added by hand
    sheet_id text,              -- of that work
    course text,                -- of one added by hand: in front of its label for Tablo
    label text not null,        -- a sheet's: whole, "DM HW 1"; one added by hand: the work, "Essay"
    value real not null,        -- 0-10
    sheet_value real,           -- the sheet's grade when it was changed: once the sheet changes it, the sheet wins
    created_at text not null    -- or changed: new for Tablo from then
);
```

### 1.2 Column migrations (`store.py:257-274`, `Store._upgrade`)

At every start, for each `(table, column, definition)`, if `pragma table_info(table)` lacks the column it runs
`alter table <table> add column <column> <definition>`. The whole loop is one transaction.

| table | column | definition |
|---|---|---|
| news | hidden | `integer not null default 0` |
| messages | subject | `text` |
| deadlines | sheet_id | `integer` |
| files | kind | `text not null default 'pdf'` |
| events | copy_of | `text` |
| files | reading | `text` |
| deadlines | assumed | `integer not null default 0` |
| sheets | due_assumed | `integer not null default 0` |
| sheets | due_at | `text` |
| sheets | sent_at | `text` |
| files | url | `text` (the link a PDF came from, D59; null for a message's own file) |
| files | tries | `integer not null default 0` (failed downloads) |
| hidden_deadlines | reason | `text` |
| own_deadlines | course | `text` |
| hidden_deadlines | kept | `text` |
| sheets | caption | `text` |

`files.url` and `files.tries` exist only as migrations: they are not in the CREATE statement. A fresh Go database
must have them too.

Other migrations:
- `connector/changes.py:41-43` adds `connector_changes.via text not null default 'claude'` if it is missing.
- `settings/__init__.py:298-311` (`Settings.__init__`): renames the row `miniapp_language` to `language` unless
  `language` exists (then deletes the old row), and deletes the row `forum_language`.
- `drop table if exists bot_posts` (in the SCHEMA above).

### 1.3 Tables created by other modules (verbatim)

`infomaster/web/auth.py:31-49`:

```sql
create table if not exists web_sessions (   -- devices logged in to the web app (D61)
    id integer primary key,
    token_hash text not null unique,        -- SHA-256 of the cookie's token
    device text not null,                   -- from the User-Agent, for the list of devices
    created_at integer not null,            -- unix seconds
    used_at integer not null
);
create table if not exists web_passkeys (   -- the owner's WebAuthn credentials (D61)
    id integer primary key,
    credential_id text not null unique,     -- base64url
    public_key blob not null,               -- COSE
    sign_count integer not null,
    transports text,                        -- JSON list, a hint for the browser
    device text not null,                   -- where it was added
    created_at integer not null,
    used_at integer
);
```

`infomaster/web/push.py:35-46`:

```sql
create table if not exists web_push (     -- devices that get the web app's notifications (§8.6)
    id integer primary key,
    session_id integer not null,          -- web_sessions.id: the device; its subscription goes with it
    endpoint text not null unique,        -- the push service's URL for this browser
    p256dh text not null,                 -- the browser's public key, base64url
    auth text not null,                   -- its auth secret, base64url
    created_at integer not null,          -- unix seconds
    sent_at integer,                      -- the last message the service took
    failures integer not null default 0   -- failed sends in a row
);
```

`infomaster/web/check.py:26-31`:

```sql
create table if not exists web_checked (  -- questions of the «Check» queue the owner put aside (§8.6)
    id text primary key,        -- dup:<uid>|<uid>, course:<uid>, assumed:<uid>
    at text not null            -- local time
);
```

`infomaster/library/__init__.py:35-86`:

```sql
create table if not exists materials (  -- raw material of the courses (D58): kept for good
    id integer primary key,
    course text,                -- a name of the timetable (classes.subject) or as a source names it; null: the inbox
    class_uid text,             -- its RUZ class (classes.uid); null: the course as a whole, or not known
    day text,                   -- YYYY-MM-DD: of its class, else of its arrival
    kind text not null,         -- photo, audio, video, notes, slides, sheet, link, text, transcript, file
    title text not null default '',
    author text not null default '',  -- who wrote or sent it: "owner", a classmate's name, a chat's
    text text,                  -- a text's or a link's words; a transcript
    url text,                   -- a link: remembered, never fetched
    path text,                  -- its file under the data directory; null for a link or a text
    name text,                  -- the file's name, as it came
    media_type text,
    size integer,
    sha256 text,
    parent integer,             -- the item it belongs to: a transcript's recording
    source text not null,       -- chat, bot, miniapp, connector, sheet, web
    chat_id integer,            -- where it came from
    msg_id integer,
    status text not null,       -- uploading (a draft: no bytes yet), new, processed
    note text not null default '',  -- why it is in the inbox; what Claude made of it
    created_at text not null,
    processed_at text,
    removed_at text             -- removed by the owner or Claude: listed nowhere, the file goes after REMOVED_KEEP
);
create index if not exists materials_course on materials(course, day);
create index if not exists materials_origin on materials(source, chat_id, msg_id);
create index if not exists materials_sha256 on materials(sha256);
create table if not exists knowledge (  -- the knowledge base (D58): one document per course and class
    id integer primary key,
    course text not null,
    kind text not null,         -- book, lecture, seminar, cheatsheet
    class_uid text,             -- a lecture's or a seminar's RUZ class
    day text,                   -- YYYY-MM-DD of that class
    title text not null,
    created_at text not null
);
create index if not exists knowledge_course on knowledge(course, kind);
create table if not exists knowledge_versions (
    id integer primary key,
    doc_id integer not null,
    version integer,            -- 1, 2…; null for a draft whose files are still coming
    source_path text,           -- the .tex (Markdown of a cheat sheet)
    pdf_path text,
    note text not null default '',      -- what changed
    materials text not null default '[]',  -- JSON [material id] it was made from
    created_at text not null,
    published_at text
);
create index if not exists knowledge_doc on knowledge_versions(doc_id, version);
```

`infomaster/library/feed.py:31-42`:

```sql
create table if not exists library_queue (  -- materials the batches found, waiting to go into the library (D58)
    chat_id integer not null,
    msg_id integer not null,
    course text,                -- as the model named it; null: not sure, the item goes to the inbox
    kind text not null,         -- what the model took it for: photo, notes, slides, audio, video, link, text, file
    title text not null default '',
    tries integer not null default 0,   -- failures other than NotNow
    created_at text not null,
    primary key (chat_id, msg_id)
);
```

`infomaster/connector/changes.py:18-32`:

```sql
create table if not exists connector_changes (  -- every write through the MCP connector (§8.5)
    id integer primary key,
    at text not null,           -- local time
    grant_id integer,           -- the connection it came through (connector_grants)
    tool text not null,
    arguments text not null,    -- JSON, as the client sent them
    summary text not null,      -- one line for the web app
    undo text,                  -- JSON [{tool, arguments}]; null when it cannot be undone
    note text not null default '',  -- why not, or what an undo does not bring back
    undone_at text,
    undo_error text,            -- the last failed undo
    via text not null default 'claude'  -- claude: through the connector; web: the owner in the web app (§8.6)
);
```

`infomaster/connector/oauth.py:38-61`:

```sql
create table if not exists connector_clients (  -- clients registered for the MCP connector (RFC 7591)
    id text primary key,        -- client_id
    name text not null,
    redirect_uris text not null,    -- JSON
    created_at integer not null     -- unix time
);
create table if not exists connector_grants (   -- connections the owner allowed: one refresh token each
    id integer primary key,
    client_id text not null,
    created_at integer not null,
    used_at integer not null,
    refresh_hash text,          -- SHA-256 of the refresh token
    refresh_until integer,
    previous_hash text,         -- the one it replaced, still good for REFRESH_GRACE
    previous_until integer,
    revoked_at integer
);
create table if not exists connector_tokens (   -- access tokens
    hash text primary key,      -- SHA-256
    grant_id integer not null,
    expires_at integer not null
);
```

### 1.4 Value conventions inside the tables

- Booleans are `0/1` integers. `chat_settings` and `settings` hold JSON text, so a flag is the text `true`/`false`.
  `ChatSettings.flagged_ids` runs `value = 'true'` (`settings/__init__.py:373-376`): Go must write exactly `true`.
- JSON columns: `messages.forward`, `messages.files`, `files.reading` and `connector_changes.arguments`/`undo` are
  written with `json.dumps(..., ensure_ascii=False)`. `news.msg_ids`, `deadlines.msg_ids`, `events.msg_ids` and
  `knowledge_versions.materials` use plain `json.dumps` (ASCII, `", "` separators): `[101, 102]`.
- `messages.files` is NULL when there are no files; it is never `[]`. The decoder maps NULL to `[]`.
- `connector_changes.arguments` is cut to 4000 code points before it is stored, so the cut text may not be valid
  JSON. The reader then returns `{"cut": "<text>"}` (`changes.py:79-86`). Only the newest 200 rows are kept:
  `delete ... where id <= new_id - 200`.
- Times: see §7.
- Paths in `files.path`, `materials.path`, `knowledge_versions.source_path`/`pdf_path` are relative to `data/`:
  - `files/<sha256>.pdf` or `files/<sha256>.<jpg|png|gif|webp>` (`collectors/files.py:93-111, 286-310`);
  - `library/raw/<sha[:2]>/<sha256><suffix>`, where `suffix` is the lowercased extension of the name, at most 10
    characters (`library/__init__.py:324-336`);
  - `library/knowledge/<doc_id>/<version_id>.tex|.pdf|.md` (`library/__init__.py:545-546`).
  - Partial files end in `.part` (`<hex>.part`, `<chat>_<msg>.part`, `mail_<msg>_<hex>.part`,
    `link_<chat>_<msg>.part`, `own_<hex>.part`). The prune jobs delete `.part` files older than a day.
- The pseudo chats: `chat_id 0` is the mailbox (letters and wiki notices), and `chat_id -1` (`OWN_CHAT`,
  `collectors/files.py:40`) holds the owner's own sheets and PDFs sent to the bot. Wiki notices are letters with
  negative `msg_id`s that count down from -1 (kv `wiki.next`).

---

## 2. `kv` keys

Values are written by `Store.put_json` as `json.dumps(value, ensure_ascii=False)`, with default separators
`", "`/`": "`, unless the table says otherwise. A reader must accept any valid JSON.

| key | owner | JSON |
|---|---|---|
| `llm.days` | `llm/budget.py:132-148` | `{"YYYY-MM-DD": limit_usd_float}`: the Moscow days when every call stopped, the newest 40 (sorted by day) |
| `llm.raised` | `llm/budget.py:121-123, 155-160` | `{"day": "YYYY-MM-DD", "value": float}`: the extra dollars for that Moscow day; a different day reads as 0.0 |
| `llm.notified` | `llm/budget.py:127-145` | `{"day": "YYYY-MM-DD", "value": ["hard", "soft"]}` (sorted strings); levels already announced today. `"hard"` in it means every call stops today |
| `ruz.etag` | `collectors/ruz.py:176-198` | `{"url": sha256(RUZ_ICAL_URL)[:16] hex, "etag": str or null}` |
| `ruz.size` | `collectors/ruz.py:159, 272` | int: the number of classes in the last feed that came |
| `mail.state` | `collectors/mail.py:46, 355-364` | `{"uidvalidity": int, "last": int (the highest UID), "base": int}` where `msg_id = base + UID`. Written with plain `json.dumps` (ASCII) |
| `wiki.state` | `collectors/wiki.py:42, 220-234` | `{"<page url>": {"revid": int, "title": str, "items": {"<link>": "<row text>"}}}` |
| `wiki.next` | `collectors/wiki.py:43` | int: the next notice `msg_id`, negative and counting down (default -1) |
| `hseapp.auth` | `collectors/hseapp.py:55, 97-114` | secret (§8): `{"access_token", "expires_at": float, "refresh_token", "refresh_expires_at": float, "refreshed_at": float}`, in float unix seconds. `{}` after a forced logout. Absent: never logged in. Present but empty: the session ended |
| `hseapp.login_told` | `collectors/hseapp.py:56, 365-392` | bool |
| `hseapp.deadlines` | `processing/hseapp.py:33, 113-147` | `{"fetched_at": local_iso, "items": [row], "done": [row]}` with `row = {"id": str, "label": str, "due": "YYYY-MM-DD", "course": str or null (the discipline), "title": str}`, sorted by `(due, title)` |
| `hseapp.label:<16 hex>` | `processing/hseapp.py:79-81, 155-180` | str: the cached label (formula in §6) |
| `deadlines.shown` | `outputs/deadlines.py:29-38, 78-90`; also appended by `tablo.py:83-85` | `[{"ref", "title", "due", "source", "course"}]`: the merged list as last seen, used to tell news |
| `deadlines.reminded` | `outputs/reminders.py:25-75` | `{"day": "YYYY-MM-DD", "eve": "YYYY-MM-DD"}`: the Moscow day each reminder slot last ran |
| `batches.done` | `processing/batches.py:766-770` | `{"<chat_id>": "<local_iso of the slot hour>"}` |
| `batches.failed` | `processing/batches.py:97, 963-979` | `{"<source>:<chat_id>:<msg_id>": int failures}`. Written by `put_json`, except at `:978-979`, which uses plain `json.dumps` (ASCII) |
| `grades.view:<sid>` | `processing/grades.py:54, 270-277, 482-525` | `{"course": str, "tabs": [tab], "constants": ..., "how": {cell: {"en", "ru"}}}`, where `tab` is built by `describe()` at `processing/grades.py:203-213`. The web app's gradebook screen reads it |
| `grades.own_layout:<sid>\|<tab>` | `processing/grades.py:55, 490-505` | `{"skip": bool, "reason": "laid out by hand", "header_rows": [int], "items": [{"column", "label", "max", "bonus_column"}], "by": "you"}` |
| `grades.layout:<24 hex>:<budget>` | `processing/grades.py:153-155, 318-342` | `{"watch": [int rows], "header": <view.header(watch)>, "spec": {"skip", "reason", "header_rows", "items", "failed"?}}`: Haiku's cached layout of a tab |
| `grades.explain:<sid>` | `processing/grades.py:64, 285-311` | `{"hash": "<24 hex>", "how": {"<Tab!A1>": {"en": str, "ru": str}}}` |
| `web.vapid` | `web/push.py:47, 93-106` | secret: `{"private": "<PKCS#8 PEM of a P-256 key>"}` |
| `library_link_secret` | `library/links.py:26, 47-51` | secret: a JSON string of 64 hex characters (`secrets.token_hex(32)`); the HMAC key is `bytes.fromhex(value)` |

`forget(sids)` (`processing/grades.py:552-562`) deletes `grades.view:<sid>`, `grades.explain:<sid>` and every key that
starts with `grades.own_layout:<sid>|`. The prefix test is `substr(key, 1, ?) = ?`, where `?` is the prefix length in
code points.

---

## 3. Settings (`infomaster/settings/__init__.py`)

A setting is stored as a `settings(name, value)` row holding `json.dumps(value, ensure_ascii=False)`. No row means the
default. `Settings.set` validates the value, then stores the normalized value it returns. Validation errors are 400
texts; their wording is in the source.

### 3.1 General settings (`SPECS`, `:233-264`)

| name | type | default | rule |
|---|---|---|---|
| `llm_daily_limit_usd` | float | `0.5` | number > 0; stored as float (`float(value)`, so `1` is stored as `1.0`) |
| `llm_prices` | object | `{"claude-haiku-5-5": {"input": 0.1, "output": 0.5, "long_input": 0.5, "long_output": 2.5}}` | non-empty `{model: {input, output, long_input, long_output}}` with exactly these 4 keys, numbers >= 0 stored as floats; $ per million tokens |
| `batch_hours` | int | `3` | one of 1, 2, 3, 4, 6, 8, 12, 24 (not bool) |
| `news_ttl_hours` | int | `24` | 1..168 |
| `hseapp_enabled` | bool | `true` | |
| `pdf_pages` | int | `10` | 1..50 |
| `pdf_tokens` | int | `30000` | 1000..200000 |
| `images_daily` | int | `30` | 0..500 |
| `deadline_reminders` | bool | `true` | |
| `reminder_eve` | str | `"20:00"` | `([01]?\d\|2[0-3]):([0-5]\d)` after strip; normalized to `HH:MM` |
| `reminder_day` | str | `"09:00"` | same |
| `grades_student` | str | `""` | at most 100 code points; whitespace collapsed (`" ".join(s.split())`) |
| `grades_sheets` | list | `[]` | `[{url, label, name?}]`, see `:139-163`: the url must give a sheet id (§6), no duplicate ids; `label` is collapsed and at most 10; `name` at most 60 and kept only when non-empty |
| `grades_window_days` | int | `7` | 1..60 |
| `language` | str | `"en"` | `en` or `ru` |
| `about_me` | str | `""` | text rule: lines kept, spaces inside a line collapsed, empty lines dropped, at most 500 code points |
| `wiki_pages` | list[str] | `[]` | at most 10; each `^https?://[^/\s]+/\S+$` after strip; duplicates dropped, order kept |
| `push_kinds` | list[str] | `["deadlines", "reminders", "failures", "llm"]` | a subset, re-ordered to this order |
| `push_quiet` | str | `""` | `""` or `HH:MM-HH:MM` (both parts normalized, start != end) |

"int" means a JSON integer that is not a boolean. The check `value not in BATCH_HOURS` also accepts `3.0`, which is
then stored as `3.0`.

### 3.2 Chat settings (`CHAT_SPECS`, `:285-295`), stored in `chat_settings(chat_id, name, value)`

| name | type | default | rule |
|---|---|---|---|
| `enabled` | bool | `false` | |
| `study` | bool | `false` | |
| `filter` | str | `"all"` | `all` or `important` |
| `people` | list[int] | `[]` | Telegram user ids; stored as `sorted(set(...))` |
| `images` | bool | `false` | |
| `pdf_content` | bool | `false` | |
| `urgent` | bool | `false` | |
| `batch_hours` | int or null | `null` | null (use the general value) or one of the batch hours |
| `note` | str | `""` | text rule, at most 300 |

Rows with names that are not listed are ignored when read. `ChatSettings.update` checks every value before it saves
any of them.

---

## 4. HTTP

There are two aiohttp apps, each `web.AppRunner(app, access_log=None)` on `0.0.0.0` (`app.py:241-256`):
- public: `HTTP_PUBLIC_PORT`, default 8080, behind Tailscale Funnel at `PUBLIC_URL`;
- internal: `HTTP_INTERNAL_PORT`, default 8081, on the docker network only.

Generic aiohttp behaviour a client may see:
- An unknown path gives 404 `text/plain; charset=utf-8` with the body `404: Not Found`.
- A wrong method gives 405 with an `Allow` header.
- The public app has `client_max_size = 2 MiB` (`actions.py:81`). A larger body read whole (`request.json()`,
  `request.post()`) gives 413. Streamed bodies (`/mcp`, `/library/{token}`, multipart uploads) are not capped by it.
- `web.json_response(data)` writes `Content-Type: application/json; charset=utf-8` and a body from
  `json.dumps(data)`: ASCII-only (`\uXXXX` escapes), `", "` and `": "` separators, floats in Python repr
  (`8.0`, `1e-07`). Where a `dumps=partial(json.dumps, default=json_default(lang))` is given, a `Phrase` (a two-language
  error text) becomes the string in that language (English for `/api/status` and MCP results).

### 4.1 Internal port (`outputs/api.py`, `outputs/ical_feed.py`)

A middleware guards every path that starts with `/api/`: `Authorization` must split on the first space into the exact
scheme `Bearer` (case-sensitive) and a token equal to `API_TOKEN` (constant-time compare). Otherwise the answer is
401 `text/plain`, body `Bearer token required`.

| method, path | answer 200 (`json_response`, ASCII) |
|---|---|
| GET `/api/status` | `{<source>: status, ..., "llm": {"spent_usd", "limit_usd", "raised_usd", "level"}}`. The sources are `telegram`, `batches`, `ruz`, `hseapp`, `mail`, `sheets`, `grades`, `wiki`, `images` (`app.py:114-117`). Each status is built by `status()` in its module (`collectors/telegram.py:72-75`, `processing/batches.py:653-656`, `collectors/ruz.py:137-138` plus `app._ruz_status`, `collectors/hseapp.py:270-274` plus `processing/hseapp.py:124-126` merged in `app._hseapp_status`, `collectors/mail.py:290-294`, `processing/sheets.py:170`, `collectors/grades.py:390-392` plus `processing/grades.py:529-530` in `app._grades_status`, `collectors/wiki.py:176-181`, `processing/images.py:69-71`). Phrases are written in English. The money values are rounded to 4 decimal places |
| GET `/api/deadlines` | `{"items": [{"uid", "title", "due": "YYYY-MM-DD", "category": "study"\|"personal", "source": "hseapp"\|"ical"\|"mail"\|"tg"\|"ha", "overdue": bool, "assumed": bool}]}` in screen order (`processing/merge.py:176-181`) |
| GET `/api/news` (only when news and settings are wired) | `{"items": [{"id", "source", "study", "posted_at", "full", "short", "importance"}], "ttl_hours": int}`, in rank order (`News.live`, `processing/news.py`) |
| GET `/api/grades` (only when grades are wired) | `{"items": [{"key", "label", "value", "bonus", "seen_at"}], "window_days": int}`, newest first (`processing/grades.py:397-414`). An own grade's key is `own-<id>` or the sheet key it changes |
| GET `/calendar/{secret}/{feed}.ics` | see 4.4. Only when `CALENDAR_SECRET` is set; feeds `main`, `optional`, `tablo` |

### 4.2 Public port: routes

| path | module | auth |
|---|---|---|
| GET `/hseapp/callback` | `actions.py:577-591` | the PKCE `state` in the query |
| GET `/calendar/{secret}/{feed}.ics` | `ical_feed.py` | `CALENDAR_PUBLIC_SECRET`; feeds `main`, `optional` only |
| GET `/library/{token}/{name}`, GET `/library/{token}`, PUT and POST `/library/{token}` | `library/links.py:126-181` | signed token (§6) |
| `*` `/mcp` | `connector/__init__.py:56-67`, `connector/mcp.py` | OAuth bearer |
| `/.well-known/oauth-*`, `/oauth/*` | `connector/oauth.py:602-611` | public |
| `/web`, `/web/`, `/web/sw.js`, `/web/manifest.webmanifest`, `/web/static/*`, POST `/web/share` | `web/__init__.py:106-115` | public |
| `/web/api/*` | `web/__init__.py`, `web/machine.py`, `web/shelves.py`, `web/fixes.py` | session cookie |

The owner's actions in `actions.py` (chats, deadlines, events, grades, settings...) have no routes of their own.
The connector tools call them through `call_action` (`connector/tools.py:160-180`), and so do the web app's
`/machine` handlers. They answer with `actions.respond`, which uses `json_response` with the app's language for
Phrases, and refuse with `HTTPBadRequest`/`HTTPNotFound(text=...)`. That text is what a tool's `ToolError` and the
web app's `{"error": ...}` carry.

#### `/hseapp/callback`

The query is the OAuth answer from the HSE Keycloak (`state`, `code`, or `error`). It completes the login (§11). The
answer is a small HTML page (`actions.py:594-614`, `PAGE`) in the app's language, `text/html; charset=utf-8`, with
`Cache-Control: no-store` and `Referrer-Policy: no-referrer`. The status is 200 on success and 400 on failure.

#### `/library/...` (`library/links.py`)

- GET `/library/{token}[/{name}]`: the token must verify (§6.4) with `a == "get"`, else 404 `text/plain`
  `the link is wrong or expired`. A missing file gives 404 `the file is gone`. The answer is a `FileResponse`
  (Range and conditional requests supported) with:
  - `Content-Type`: for a material, `served_type(media_type)`: the type itself if it starts with one of
    `image/png image/jpeg image/gif image/webp image/heic image/avif audio/ video/ application/pdf`; else
    `text/plain; charset=utf-8` for any `text/*`; else `application/octet-stream`. A kept sheet PDF is
    `application/pdf`; a version's `pdf` part is `application/pdf`, and its source is `text/plain; charset=utf-8`.
  - `Content-Disposition: <inline|attachment>; filename="<ascii name>"; filename*=UTF-8''<quote(name)>`. It is
    `inline` for the INLINE types and `text/plain`; the ASCII name has non-ASCII characters, `"` and `?` replaced
    by `_` (`:116-119`).
  - `Cache-Control: private, max-age=3600` and `X-Content-Type-Options: nosniff`.
  - For anything that is not a PDF: `Content-Security-Policy: sandbox; default-src 'none'; img-src 'self';
    media-src 'self'; style-src 'unsafe-inline'`.
  - The served name: a material's `name` (or `file`); for `k` tokens, the `{name}` segment of the path (or
    `sheet.pdf`); for a version, `file_name(version, part)` (§6.4).
- PUT/POST `/library/{token}`: the token must have `a == "put"`. The body is streamed. For `m`, the answer is
  `{"material": id, "size": int, "sha256": hex}`; for `v`, `{"version_draft": id, "part": "source"|"pdf",
  "uploaded": true}`. A `LibraryError` gives 400 text. The size limits are `MAX_FILE = 2 GiB` for a material and
  `MAX_DOC_FILE = 64 MiB` for a version file (`library/__init__.py:95-97`).

### 4.3 The web app (`infomaster/web/`)

Pages:
- GET `/web` gives 302 to `/web/`.
- GET `/web/` serves `static/index.html` with `Cache-Control: no-cache`.
- GET `/web/sw.js` is served with `Content-Type: text/javascript` and `no-cache`.
- GET `/web/manifest.webmanifest` is served as `application/manifest+json` with `no-cache`.
- `/web/static/*` uses aiohttp `add_static`: the type comes from Python `mimetypes` (`.js text/javascript`,
  `.css text/css`, `.woff2 font/woff2`, `.png image/png`, `.svg image/svg+xml`, `.webmanifest
  application/manifest+json`, `.txt text/plain`, `.html text/html`). ETag/Last-Modified and Range are supported,
  and a response without `Cache-Control` gets `Cache-Control: no-cache` (`revalidate_static`).
- POST `/web/share` gives 303 to `/web/#/add?shared=lost`.

Headers on every response whose path starts with `/web` (`secure_headers`, set only if absent):
`X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `X-Frame-Options: DENY`, and this CSP:

```
default-src 'self'; script-src 'self' <hashes>; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:;
media-src 'self' blob:; connect-src 'self'; worker-src 'self'; object-src 'none'; base-uri 'none';
form-action 'self'; frame-ancestors 'none'
```

It is one line; the break above is only for reading. `<hashes>` is, for each non-empty inline `<script>` body of
`index.html` (regex `<script[^>]*>(.*?)</script>`, DOTALL), `'sha256-<std base64 of sha256(body utf-8)>'`, joined by
spaces. Today only the import map is inline. Paths under `/web/api/` also get `Cache-Control: no-store`.

The `/web/api` guard (`guarded`, `web/__init__.py:171-196`), in order:
1. For POST/PUT/PATCH/DELETE, the `Origin` header must equal `scheme://netloc` of `PUBLIC_URL`, else 403
   `{"error": "wrong origin"}`.
2. Except for `/login/code`, `/login/code/check`, `/login/passkey/options` and `/login/passkey`, the cookie
   `im_session` must map to a live session, else 401 `{"error": "log in"}`.
3. Errors come back as JSON `{"error": text}` with the status:
   - an `AuthError` gives its own status and message;
   - an `HTTPException` >= 400 gives its `text`, or its reason phrase when the text is aiohttp's default `"<status>: <reason>"`.
     A bare `HTTPNotFound()` therefore gives `{"error": "Not Found"}`.

The session cookie: `im_session=<token>; Max-Age=7776000; Path=/web; Secure; HttpOnly; SameSite=Strict`
(`SESSION_IDLE` = 90 days). Logout deletes it on `Path=/web`. A session is gone when `used_at` is 90 days old or
`created_at` is 365 days old. `used_at` is updated at most once an hour.

Request and response shapes. The body is JSON unless the table says so, and every handler answers with
`json_response` (ASCII).

| method, path under `/web/api` | request | answer |
|---|---|---|
| GET `/me` | | `{"lang", "session": id, "device", "passkeys": n, "push": bool}` |
| POST `/login/code` | | `{"ok": true}`. Errors: 429 (resend within 30 s, more than 6 an hour or 12 a day), 503 (no bot), 502 (the bot failed) |
| POST `/login/code/check` | `{"code"}` | `{"ok": true}` and the cookie set. 400 on a wrong or expired code (3 attempts, 600 s) |
| POST `/login/passkey/options` | | raw py_webauthn `options_to_json` of `generate_authentication_options(rp_id=<host of PUBLIC_URL>, user_verification=required)`, `application/json` |
| POST `/login/passkey` | `{"credential": <PublicKeyCredential JSON>}` | `{"ok": true}` and the cookie |
| POST `/logout` | | `{"ok": true}` and the cookie deleted |
| GET `/devices` | | `{"current": id, "sessions": [{"id", "device", "created_at", "used_at"}], "passkeys": [{"id", "device", "created_at", "used_at"}]}`, times in unix seconds |
| DELETE `/sessions/{session_id}` | | same as `/devices`; 404 `no such device` |
| POST `/passkeys/options` | | `options_to_json(generate_registration_options(rp_id, rp_name="infomaster", user_id=b"infomaster-owner", user_name="infomaster", user_display_name="infomaster", resident_key=required, user_verification=required, exclude_credentials=<all stored>))` |
| POST `/passkeys` | `{"credential"}` | same as `/devices`. The bot also tells the owner about the new passkey |
| DELETE `/passkeys/{passkey_id}` | | same as `/devices`; 404 `no such passkey` |
| GET `/feed?course=<key>` | | `Data.feed(course)` (`web/views.py:173-190`) plus `"check": Check.items(course)`, `"problems": n`, `"inbox": n` |
| GET `/week?start=YYYY-MM-DD&course=` | | `Data.week` (`views.py:192-203`); 400 on a bad date |
| GET `/deadline?uid=` | | `Data.deadline_card` (`views.py:218-253`); 404 `no such deadline` |
| GET `/class?uid=` | | `Data.class_card` (`views.py:264-284`); 404 |
| GET `/course?key=` | | `Data.course` (`views.py:373-389`); 404 |
| POST `/course/calc` | `{"key", "book", "target"?, "values": {cell: number\|null}, "solve"?: {"cell", "goal"}}` | `Data.calc` (`views.py:485-510`); 404 |
| GET `/news` | | `Data.news_list` (`views.py:353-358`) plus `"problems"` |
| GET `/news/{news_id}` | | `Data.news_card` (`views.py:360-371`) |
| POST `/news/{news_id}/hide` | | change answer (below), via the tool `hide_news` |
| GET `/check?course=` | | `{"items": Check.items(course), "courses": Data.courses(), "today"}` |
| POST `/check/put-aside` | `{"id"}` | change answer; journaled as tool `put_aside` with undo `[{"tool": "_ask_again", "arguments": {"id"}}]` |
| POST `/deadlines/{action}` | `{"uid", ...}`; action is `done`, `not-mine`, `merge` `{keep}`, `edit` `{title?, due?, category?}`, `confirm`, or `course` `{course: key}` | change answer; mapping in `web/__init__.py:460-496` |
| POST `/changes/{change_id}/undo` | | `{"change": {"id", "summary", "undone_at"}}`; 400 with the reason |
| GET `/push` | | `{"key": VAPID public key (base64url of the uncompressed point), "on": bool, "devices": n}` |
| POST `/push` | `{"subscription": {"endpoint", "keys": {"p256dh", "auth"}}}` | same as GET `/push`; 400 PushError |
| DELETE `/push` | | same as GET `/push` |
| POST `/push/test` | | `{"sent": n}`; 502 if no push service took it |
| GET `/add` | | `{**Quick.place(), "courses", "line": bool, "library": bool, "grades": bool}`; `place` is at `web/quick.py:82-92` |
| POST `/add/line` | `{"text"}` | `Quick.checked` (`web/quick.py:139-163`): `{"kind", "title", "course", "course_title", "due", "start", "end", "room", "category", "why"}`. Errors: 503 (no key), 429 (budget), 400 (empty), 502 (other) |
| POST `/add/files` | multipart: `class_uid` or `course` first (at most 400 bytes each), then the files | `{"added": [ids], "change": {"id", "summary", "undo"}}`, or 207 with `"error"` when a later file was refused |
| POST `/add/note` | `{"text", "class_uid"?, "course"?}` | `{"added": [id], "change"}` |
| POST `/add/{deadline\|event\|grade}` | `{title, due, category?, course?}` / `{title, start, end?, room?, kind?}` / `{course, label, value}` | change answer |

The change answer is `{"change": {"id", "summary", "undo": bool, "note"}}` (`web/__init__.py:455-457`). A tool's
refusal is 400 `{"error": text}`.

`/machine` (`web/machine.py:41-68`). It answers with `json_response(..., default=json_default(lang))`.

| method, path | request | answer |
|---|---|---|
| GET `` | | `{"attention": [{"text", "screen"}], "sources": [{"key", "state", "ok_at", "error", "count"}], "llm": {...status, "limit_usd"}, "connections": n, "push": n, "library": usage, "has": {twin, settings, grades, reminders, pdf, connector, hseapp}, "lang"}`. The rules are at `machine.py:75-174`; `LATE_MINUTES = {"mail": 30, "ruz": 180, "hseapp": 60, "grades": 120, "wiki": 540}` |
| GET `/journal` | | `{"changes": [{"id", "at", "tool", "summary", "via", "undo": bool, "note", "undone_at", "undo_error"}]}`, the newest 100 |
| GET `/chats` | | `actions.chats`: `{"twin", "mail", "images", "chats": [{...chat row, "settings": {...}}]}` with enabled chats first |
| PATCH `/chats/{chat_id}` | `{setting: value}` | `{"settings": {...}, "change": {...}}`, via `update_chat_settings` |
| GET `/chats/{chat_id}/senders` | | `[{"id", "name", "messages"}]` |
| POST `/twin/{login\|cancel\|logout}` | login: `{phone}`, `{code}` or `{password}` | the twin's status (`collectors/telegram.py:72-75`) |
| GET `/hseapp`, PATCH `/hseapp` `{enabled}` | | `{"status", "callback": PUBLIC_URL + "/hseapp/callback"}` |
| POST `/hseapp/link` | `{"app"?: bool}` | `{"url"}` |
| POST `/hseapp/login` | `{"url"}` | same as GET `/hseapp` |
| GET `/settings` | | `{"general": {batch_hours, news_ttl_hours, language, about_me, wiki_pages}, "batch_choices", "images": {"daily", "today"?}, "notifications": {"kinds", "quiet"}, "reminders"?: {"enabled", "eve", "day"}, "pdf"?: {...}, "wiki"?: pages}` |
| PATCH `/settings/{group}` | the tool's arguments | `{"settings", "change"}`. Groups: `general`, `reminders`, `images`, `pdf`, `grades`, `notifications`, each mapped to its `update_*` tool |
| GET `/courses`, PUT `/courses` `{name, title, abbr}`, POST `/courses/remove` `{name}` | | `{"subjects": [{"name", "title", "abbr", "manual"}], "error", "title_chars", "abbr_chars"}` (plus `"change"` on a write) |
| GET `/llm`, PATCH `/llm` `{daily_limit_usd}`, POST `/llm/raise` | | `actions._llm_state` (`actions.py:708-731`) |
| GET `/grades`, POST `/grades` `{url, label?, name?}` | | `actions._grades_state` (`actions.py:761-768`) (plus `"change"`) |
| GET, PATCH `{name?, label?}`, DELETE `/grades/{sid}`; POST `/grades/{sid}/read` | | `actions._gradebook_state` (`actions.py:799-811`), or the grades state after DELETE |
| PUT `/grades/{sid}/tabs/{tab}` `{skip, works}`; DELETE the same path | | the gradebook state plus `"change"` |
| GET `/connector`; DELETE `/connector/{grant_id}` | | `{"url": PUBLIC_URL + "/mcp", "connections": [{"id", "name", "created_at", "used_at" (local iso), "redirect": host}], "tools": {"read": n, "write": n}}` |

`/shelf` (`web/shelves.py:47-62`; sheets need the sheet publisher, else 404 `the sheets are off`):

| method, path | answer |
|---|---|
| GET `` | `{"library": bool, "inbox": n, "sheets": n or null, "courses": [{"key", "title", "items", "new", "documents"}]}` |
| GET `/sheets?course=` | `{"course", "title", "sheets": [entry], "courses"}`; `entry` is built at `shelves.py:149-157` |
| POST `/sheets` | multipart `course`, `number`, `due`, `file` (one PDF) → `{"id", "change"}` |
| GET `/sheets/{id}` | `{...entry, "versions_list": [{"id", "version", "name", "created_at", "changes", "file_url"}], "candidates": [...], "courses"}` |
| PATCH `/sheets/{id}` `{course?, number?, due?, summary?}` | `{"change", "id"}` |
| DELETE `/sheets/{id}` | `{"change"}` |
| POST `/sheets/{id}/join` `{previous}` | `{"change", "id": previous}` |
| GET `/sheets/{id}/file` | the PDF (`application/pdf`, `inline; filename="<ascii>"`, `private, max-age=3600`) |
| GET `/raw?course=` | `{"course", "title", "classes": [{uid, start, end, kind, items, new, "items_list"}], "items"}` |
| GET `/inbox` | `{"items": [view], "courses"}`; `view` is built at `shelves.py:339-353` |
| GET `/classes?course=` | `{"course", "classes"}` |
| GET, PATCH `{title?, note?, status?}`, DELETE `/items/{id}` | the item `{...view, "class", "children", "courses"}`, or `{"change"}` |
| POST `/sort` `{ids, class_uid}` or `{ids, course: key\|null}`; POST `/remove` `{ids}` | `{"change"}` |

`/fix` (`web/fixes.py:32-39`): POST `/class` `{uid, start, end, room, cancelled, kind, required, title?}`,
POST `/event` `{uid, some of title, kind, start, end, room, cancelled}`, POST `/options` `{id, required?, hidden?}`,
POST `/remove` `{id}`, GET `/hidden` (gives `Data.hidden()`: `{"deadlines", "events"}`, `views.py:324-337`),
POST `/hidden/deadline` `{uid}`, POST `/grade` `{id, value, label?}`, POST `/grade/reset` `{id}`. The writes answer
with the change answer.

### 4.4 iCal feeds (`outputs/ical_feed.py`)

`GET /calendar/{secret}/{feed}.ics`. The secret is compared in constant time. A wrong secret or an unserved feed gives
404. The answer has `Content-Type: text/calendar; charset=utf-8` and `Cache-Control: no-cache`, and its body is
`icalendar` 7.3 `to_ical()`.

What Tablo needs are the values, not the byte order. For reference, the `icalendar` output:
- CRLF line endings. Lines are folded at 75 octets with CRLF and a space. Text escapes `\n`, `\,`, `\;` and `\\`.
- Calendar properties in this order: `VERSION:2.0`, `PRODID:-//infomaster//timetable//EN`, `CALSCALE:GREGORIAN`,
  `METHOD:PUBLISH`, `X-WR-CALNAME:<Study | Study: optional | Study for Tablo>`, `REFRESH-INTERVAL;VALUE=DURATION:PT1H`,
  `X-PUBLISHED-TTL:PT1H`.
- In each VEVENT: `SUMMARY`, `DTSTART`, `DTEND`, `DTSTAMP`, `UID`, `DESCRIPTION`, `LOCATION`, `STATUS`, then the `X-`
  properties alphabetically (`X-INFOMASTER-LINK`, `X-INFOMASTER-OPTIONAL`, `X-INFOMASTER-SOURCE`).
- Times are UTC `YYYYMMDDTHHMMSSZ`. An all-day event is `DTSTART;VALUE=DATE:YYYYMMDD` with `DTEND;VALUE=DATE` on the
  next day. `DTEND` is left out when end <= start. `DTSTAMP` is now, in UTC, without microseconds.

The rules are at `ical_feed.py:53-112`:
- `main` has the required entries, `optional` the others, `tablo` all of them.
- `SUMMARY` is `"<title> · <type>"` in `main` and `optional` when the entry is a class whose type is not `Class`;
  `Interim assessment` is written `Midterm`. `tablo` keeps the bare title.
- `DESCRIPTION` lines are: the type, the entry's notes, then in `main` and `optional` only `From: RUZ`, `From: you`
  or `From: <origin or source>[ <link>]`, and `Edit: <PUBLIC_URL>/web/#/c?uid=<quote(uid, safe='')>`.
- `LOCATION` is the room or link, left out when empty.
- `STATUS` is `CANCELLED`, `TENTATIVE` (an optional entry in `tablo`) or `CONFIRMED`. An optional entry in `tablo` also
  gets `X-INFOMASTER-OPTIONAL:TRUE`.
- `X-INFOMASTER-SOURCE` is always present; `X-INFOMASTER-LINK` only when there is a message link. No feed has `URL`.

---

## 5. MCP connector

### 5.1 OAuth (`connector/oauth.py`); the issuer is `PUBLIC_URL`

| endpoint | behaviour |
|---|---|
| GET `/.well-known/oauth-protected-resource` and `/.well-known/oauth-protected-resource/mcp` | `{"resource": PUBLIC_URL + "/mcp", "authorization_servers": [PUBLIC_URL], "bearer_methods_supported": ["header"], "resource_name": "infomaster"}` |
| GET `/.well-known/oauth-authorization-server` | `oauth.py:179-190`: issuer and the authorize, token, register and revoke endpoints; `response_types ["code"]`, `grant_types ["authorization_code", "refresh_token"]`, `code_challenge_methods ["S256"]`, `token_endpoint_auth_methods ["none"]`, `revocation_endpoint_auth_methods ["none"]`, `authorization_response_iss_parameter_supported: true` |
| POST `/oauth/register` | RFC 7591. `redirect_uris` must all be `https://claude.ai/api/mcp/auth_callback` or `https://claude.com/api/mcp/auth_callback` (no port, no fragment, no userinfo), or `http://` on `localhost`, `127.0.0.1` or `::1` with any port and path. `grant_types` must include `authorization_code`; `response_types` must be `["code"]`. The name is collapsed and cut at 80, falling back to the host of the first uri. `client_id = token_urlsafe(16)`. Answer 201 `{"client_id", "client_id_issued_at", "client_name", "redirect_uris", "grant_types": ["authorization_code", "refresh_token"], "response_types": ["code"], "token_endpoint_auth_method": "none"}` with `Cache-Control: no-store` and `Pragma: no-cache`. Registering also deletes clients older than a day without a live grant, and keeps at most 50 such clients |
| GET `/oauth/authorize` | It checks `client_id`, then `redirect_uri`, which must equal a registered one; for a loopback uri the port may differ. Those errors give a 400 HTML page. Then `response_type=code`, `code_challenge_method=S256` with a challenge of 43-128 characters, and `resource` (optional; it must equal `PUBLIC_URL/mcp` or `PUBLIC_URL`, case-insensitive, trailing `/` ignored). Those errors give 302 back to the client with `error`, `error_description`, `state` and `iss`. On success, an HTML consent page (`_page`, `oauth.py:561-599`) holding a hidden `page` id (`token_urlsafe(24)`, kept in memory for 10 min, at most 20 pages) |
| POST `/oauth/authorize` (form) | `page` and `action`. `action=send`: the bot sends a 6-digit code (resend after 30 s; at most 6 an hour and 12 a day over all pages). `action=allow` with `code`: wrong 3 times kills the code; on success, 303 to `redirect_uri?code=<token_urlsafe(32)>&state=..&iss=..` (appended with `&` when the uri already has a query). `action=deny`: 303 with `error=access_denied` and `error_description=the owner said no`. The page responses carry `X-Frame-Options: DENY`, `Content-Security-Policy: frame-ancestors 'none'`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` and `Pragma: no-cache` |
| POST `/oauth/token` (form; `client_id` also taken from HTTP Basic) | `authorization_code`: the code is single use and lives 300 s. Reusing a spent code revokes the grant it created. `client_id` and `redirect_uri` must match; `code_verifier` must be 43-128 characters with `s256(verifier) == challenge`; `resource` must be ok and equal the authorize one. `refresh_token`: matches the current or the previous refresh hash (the previous one stays valid for 120 s after rotation); `client_id`, if given, must match. The answer is `{"access_token", "token_type": "Bearer", "expires_in": 3600, "refresh_token"}`, both `token_urlsafe(32)`, refresh lifetime 30 days. Errors give 400 `{"error", "error_description"}` with no-store headers |
| POST `/oauth/revoke` | form `token`. A refresh hash revokes its grant; an access token is deleted. Always 200 with an empty body and no-store |

Storage: only SHA-256 hex digests of tokens (`connector_tokens.hash`, `connector_grants.refresh_hash` and
`previous_hash`). `check(token)` updates `grants.used_at` when it is more than 60 s old. Authorization codes,
authorize pages and the bot codes live in memory only.

### 5.2 `/mcp` (`connector/__init__.py:56-67`, `connector/mcp.py`)

1. `Authorization: <scheme> <token>` with the scheme compared case-insensitively. A missing or invalid token gives 401
   with `WWW-Authenticate: Bearer resource_metadata="<PUBLIC_URL>/.well-known/oauth-protected-resource/mcp"`; when a
   token was sent, it also has `, error="invalid_token", error_description="the token is unknown, revoked or expired"`.
   The body is `{"error": "invalid_token"}`, or `{"error": "unauthorized"}` without a token.
2. A method other than POST gives 405 with `Allow: POST`. GET and DELETE are 405, but only after the token check.
3. If an `Origin` header is present, it must be `https://claude.ai`, `https://claude.com` or the public origin, else
   403 with JSON-RPC error -32600.
4. The body is streamed and capped at 32 MiB (else 413, -32600). It is one JSON-RPC 2.0 object; there are no batches.
   - Not JSON: 400, -32700.
   - Not an object, or no `jsonrpc: "2.0"`: 400, -32600.
   - No `method` (a response): 202 with an empty body.
   - No `id` (a notification): 202, after the era check.
5. Era (`mcp.py:109-135`):
   - If `params._meta["io.modelcontextprotocol/protocolVersion"]` is present, the request is modern. The
     `MCP-Protocol-Version` header must equal it and the version must be `2026-07-28`. `Mcp-Method` must equal the
     method, and for `tools/call`, `Mcp-Name` must equal `params.name`. `Mcp-Name` may be `=?base64?<b64>?=`.
     Errors give 400 with -32020 (HeaderMismatch) or -32022 (unsupported, with data `{"supported", "requested"}`).
   - Otherwise the request is legacy, and a present header must be one of `2025-11-25`, `2025-06-18`, `2025-03-26`.
     A header of `2026-07-28` without `_meta` gives -32020.
6. Methods:
   - `initialize` (legacy): `{"protocolVersion": the asked version if it is legacy, else "2025-11-25", "capabilities":
     {"tools": {"listChanged": false}}, "serverInfo": {"name": "infomaster", "title": "infomaster", "version":
     "0.1.0"}, "instructions"}`.
   - `server/discover`: `{"supportedVersions": [all 4], "capabilities", "_meta": {"io.modelcontextprotocol/serverInfo":
     ...}, "instructions"}`.
   - `ping`: `{}`.
   - `tools/list`: `{"tools": [tool.listing()]}`.
   - `tools/call`.
   - Any other method gives -32601, with HTTP 404 when modern and 200 when legacy.
   - A modern result gets `"resultType": "complete"` merged in first.
7. The answer is `{"jsonrpc": "2.0", "id": <echoed as received>, "result"|"error"}`, written with
   `json.dumps(ensure_ascii=False)` as `application/json; charset=utf-8`.
8. `tools/call` (`mcp.py:154-186`):
   - An unknown tool gives JSON-RPC -32602 with HTTP 200.
   - Arguments that fail the schema check (`tools.py:117-149`) give `{"content": [{"type": "text", "text":
     "<problems joined by '; '>"}], "isError": true}`. So does a `ToolError` (with its text), and so does any other
     exception, with the text `internal error: see the service's log`.
   - On success: `{"content": [{"type": "text", "text": dumps(data)}, {"type": "image", "data": b64, "mimeType"}...],
     "structuredContent": data (or {"result": data} when data is not an object), "isError": false}`. Here `dumps` is
     `json.dumps(ensure_ascii=False, default=<Phrase in English>)`.
   - A write tool with `journal=True` first records the change and puts `"change": {"id", "summary", "undo": "the
     owner can undo it in the web app" | "cannot be undone[: note]"}` in front of the data's keys.

`INSTRUCTIONS` is at `mcp.py:31-40`; it embeds `UNTRUSTED` from `tools.py:27-28`.

Tool listing (`tools.py:60-65`): `{"name", "title", "description", "inputSchema", "annotations": {"title",
"readOnlyHint": !write, "openWorldHint": false, + for writes "destructiveHint", "idempotentHint"}}`. Every
`inputSchema` is `{"type": "object", "properties": {...}, "required": [...], "additionalProperties": false}`.

The schema check supports only `type` (one or a list), `enum`, `properties`, `required`,
`additionalProperties: false`, `items`, `minimum`, `maximum`, `minLength` and `maxLength` (in code points).
`minItems` and `maxItems` appear in the library schemas but are not enforced. Each problem text is
`"<path>: expected <types joined by ' or '>"`, `"<path>: one of <json values joined by ', '>"`, `"<path>: required"`,
`"<path>: unknown"`, `"<path>: at least <n>"`, `"<path>: at most <n> characters"`, and so on. The path starts at
`arguments`: `arguments.uid`, `arguments.ids[0]`.

### 5.3 The tool catalog

Tools are registered in this order. Some depend on what the app has wired; descriptions and schemas are at the
file:line given, to copy verbatim. W marks a write tool; the flags are destructive (d), idempotent (i), and
not journaled (nj).

`connector/catalog.py:860-1113` (the shared schema fragments `DATE`, `MOMENT`, `UID`, `EVENT_ID`, `GRADEBOOK`, `TAB`,
`GRADE_VALUE`, `EVENT_PROPS` and `CHAT_PROPS` are at `:31-32` and `:826-857`):

| tool | kind | condition |
|---|---|---|
| get_status | read | |
| list_deadlines | read | |
| list_hidden_deadlines | read | |
| get_timetable | read | |
| list_news | read | |
| search_messages | read | |
| get_message | read | |
| list_chats | read | |
| list_chat_senders | read | |
| list_courses | read | |
| get_settings | read | settings |
| list_sheets, get_sheet | read | settings and sheets |
| list_grades, get_gradebook | read | grades |
| hide_news | W d | |
| add_deadline | W | |
| edit_deadline | W d | |
| hide_deadline | W d | |
| confirm_deadline_date | W | |
| set_deadline_course | W | |
| delete_deadline | W d | |
| unhide_deadline | W | |
| add_event | W | |
| edit_event | W d | |
| set_event_options | W d i | |
| remove_event | W d | |
| correct_class | W d | |
| update_chat_settings | W d i | |
| set_course | W d i | |
| delete_course | W d | |
| update_general_settings, update_images, update_notifications | W d i | settings |
| update_reminders | W d i | settings and reminders |
| edit_sheet, delete_sheet, join_sheet (W d), update_pdf_limits (W d i) | | settings and sheets |
| update_grade_settings (W d), add_gradebook (W), remove_gradebook (W d), update_gradebook (W d i), read_gradebook_now (W i), set_tab_layout (W d i), reset_tab_layout (W d), add_grade (W), change_grade (W d i), delete_grade (W d) | | grades |

`connector/library.py:512-620` (only when the library is wired; fragments at `:496-509`):

| tool | kind |
|---|---|
| list_library, list_materials, get_material, list_classes, list_documents, get_cheatsheet, get_guide, get_document | read (order in the source: list_library, list_materials, get_material, list_classes, list_documents, get_document, get_cheatsheet, get_guide; get_guide is Go's own, D83) |
| build_document_version | W, not journaled (Go's own, D84: the PDF by the TeX builder of TEX_URL) |
| add_material | W |
| sort_material | W d i |
| update_material | W d i |
| remove_material | W d |
| start_document_version | W nj |
| publish_document_version | W |

The tool implementations (their data shapes) are the functions named after the tools in the same files. Signed links
in their results are made as in §6.4: download links live 24 h, upload links 6 h. Upload results include a ready
`curl -T <file> '<url>'` command.

### 5.4 Undo steps stored in `connector_changes.undo`

`undo` is a JSON list of `{"tool": name, "arguments": {...}}`. An undo runs the steps in order through the tool's
`run`; tools are schema-checked, ops are not. A step's `ToolError` is stored in `undo_error`, and the change stays.
Rows written by Python must stay executable after the switch, so Go must accept every name below.

Public tools used as steps, with the arguments their writers give:
- `delete_deadline {uid}`
- `edit_deadline {uid, title, due, category}`
- `unhide_deadline {uid}`
- `hide_deadline {uid}`
- `add_deadline {title, due, category}`
- `remove_event {event_id}`
- `edit_event {event_id, ...fields}`
- `set_event_options {event_id, required?, hidden?}`
- `correct_class {class_uid, start, end, room, cancelled, ...}`
- `add_event {...fields, required}`
- `edit_sheet {sheet_id, ...old}`
- `update_chat_settings {chat_id, ...old}`
- `delete_course {name}`
- `update_general_settings`, `update_reminders`, `update_images`, `update_pdf_limits`, `update_notifications` and
  `update_grade_settings`, each with the previous values
- `remove_gradebook {sheet_id}`
- `add_gradebook {url, label, name?}`
- `update_gradebook {sheet_id, name, label}`
- `set_tab_layout {sheet_id, tab, skip, works}`
- `reset_tab_layout {sheet_id, tab}`
- `delete_grade {grade_id}`
- `change_grade {grade_id, value, label, course?}`
- `add_grade {course, label, value}`
- `remove_material {id}`
- `sort_material {ids, course, class_uid}`
- `update_material {id, ...old}`

Private ops (not listed, not schema-checked):

| op | arguments | registered at |
|---|---|---|
| `_unhide_news` | `{news_id}` | `catalog.py:151-154, 945` |
| `_assume_again` | `{deadline_id, sheet_id: int\|null, due_at: str\|null}` | `catalog.py:349-359, 973` |
| `_set_course_back` | `{deadline_id, course}` | `catalog.py:378-385, 974` |
| `_restore_course` | `{name, title, abbr, manual}` | `catalog.py:618-620, 1019` |
| `_remove_own_sheet` | `{sheet_id}` | `catalog.py:561-565, 1064`; written by `web/shelves.py:326` and `processing/bot_sheets.py:149` |
| `_restore_material` | `{id}` | `library.py:413-425, 600` |
| `_unpublish_version` | `{version_id, processed: [ids]}` | `library.py:478-492, 620` |
| `_ask_again` | `{id}` (a Check question id) | `web/__init__.py:157-163, 508` |

Changes journaled without a tool have these `tool` names: `put_aside`, `add_material` (`{"ids": [...]}`) and
`add_sheet` (`{"sheet_id"}`). `connector_changes.via` is `claude` for changes made through `/mcp` and `web` for
everything else.

---

## 6. Identifiers and hashes

### 6.1 Deadlines (`processing/todo.py:47-51`)

```
own deadline:  uid = "ha-<own_deadlines.id>"
any other:     raw = f"{source}|{' '.join(title.casefold().split())}|{due:YYYY-MM-DD}"
               uid = f"{source}-{sha1(raw.encode('utf-8')).hexdigest()[:10]}"
```

`source` is one of `hseapp`, `ical`, `mail`, `tg`. `casefold` is full Unicode case folding (`ß` becomes `ss`,
`İ` becomes `i̇`). `split()` splits on Python whitespace (§12). `hidden_deadlines.uid` is this uid, so a mismatch
brings hidden deadlines back. The merge also tracks rows by `ref`: `dl:<deadlines.id>`, `hseapp:<id>` or `ha-<id>`.

### 6.2 Events and classes

- An event from a message (`processing/events.py:38-41`):
  `f"{source}-{sha1(key).hexdigest()[:16]}@infomaster"` with `key = f"{chat_id}:{msg_id}"`, plus `f":{n}"` when
  `n != 0`. `chat_id` is the marked id; the mailbox is 0.
- An own event or correction: `f"own-{secrets.token_hex(8)}@infomaster"` (random, kept through versions).
- A RUZ class: the feed's `UID` stripped; if it is empty,
  `f"ruz-{sha1(f'{subject}|{start.isoformat()}').hexdigest()[:16]}@infomaster"`, where `start` is naive Moscow
  local time (`YYYY-MM-DDTHH:MM:SS`, microseconds only if non-zero) and `subject` is cleaned: whitespace collapsed and
  a trailing `(рус)` removed (`processing/subjects.py:50-53`).
- iCal UIDs are these uids. The edit link in the feeds is
  `PUBLIC_URL + "/web/#/c?uid=" + urllib.parse.quote(uid, safe='')` (`app.py:99`).

### 6.3 Other keys

- HSE App X label cache: `"hseapp.label:" + sha256(f"{LABEL_VERSION}\0{budget}\0{title}\0{discipline or ''}\0{abbr or ''}").hexdigest()[:16]`,
  with `LABEL_VERSION = 2` and `budget = 15` (`processing/batches.py:119` `LABEL_CHARS`); `\0` is a NUL character.
- Grade layout cache: `payload = json.dumps([sid, tab, 3], ensure_ascii=False)`, exactly `["<sid>", "<tab>", 3]`
  with `", "` separators and Python string escaping, then
  `"grades.layout:" + sha256(payload).hexdigest()[:24] + ":" + str(budget)`, where
  `budget = max(8, 23 - len(course) - 1)` (`processing/grades.py:148-155`).
- The grade formula explanation hash: `sha256(str(1) + prompt).hexdigest()[:24]`, where `prompt` is the lines
  `"<cell> | <header or '-'> | <formula>"` joined by `\n` (`processing/grades.py:288-297`).
- `grade_items.key`: `f"{sid}|{tab}|{column}|{header}"`.
- A grade id: `grade_items.key` for a sheet's grade, `own-<own_grades.id>` for one added by hand.
- A grade sheet id (`settings/__init__.py:82-136`):
  - a Google Sheet: the `[A-Za-z0-9_-]+` after `/spreadsheets/d/`, and the url must start with
    `https://docs.google.com/`;
  - a Dropbox file link (`https` on `www.dropbox.com`, `dropbox.com`, `dl.dropbox.com` or
    `dl.dropboxusercontent.com`, path `/scl/fi/<id>/<name>` or `/s/<id>/<name>`, with a name ending in `.xlsx`):
    `"dropbox-" + <id>`.
- Check question ids: `dup:<uid>|<uid>`, `course:<uid>`, `assumed:<uid>`.
- Batch failure keys: `"<source>:<chat_id>:<msg_id>"`.
- RUZ etag owner: `sha256(RUZ_ICAL_URL)[:16]` hex.

### 6.4 Library signed links (`library/links.py`)

```
key     = bytes.fromhex(kv["library_link_secret"])          # 32 bytes
body    = b64url_nopad(json.dumps(payload, separators=(",", ":")).encode())   # ASCII JSON, key order as written
mac     = HMAC-SHA256(key, body.encode('ascii'))[:18]
token   = body + "." + b64url_nopad(mac)
```

Payloads, in key order:
- `{"a":"get","m":<material id>,"e":<exp>}`
- `{"a":"put","m":<id>,"e":<exp>}`
- `{"a":"get","v":<version id>,"p":"source"|"pdf","e":<exp>}`
- `{"a":"put","v":<id>,"p":<part>,"e":<exp>}`
- `{"a":"get","k":"<sha256 of a kept sheet PDF>","e":<exp>}`

`exp = int(now + ttl)` in unix seconds, with ttl 86400 for downloads and 21600 for uploads.

Verifying a token: split on the first `.`; compute the HMAC over the received body text, not a re-encoding of it;
compare in constant time with the padded-base64 decode of the mac; base64-decode and JSON-parse the body; require an
object with `e > now`.

URLs:
- download: `PUBLIC_URL/library/<token>/<quote(name)>`, where the name is the material's `name` or `file`, or the
  sheet's name or `sheet.pdf`, or `file_name(version, part)`;
- upload: `PUBLIC_URL/library/<token>`.

`file_name(version, part)`:
1. Start from the title (or `document`). Keep characters for which Python `isalnum()` is true (Unicode letters and
   digits) and the characters ` -_.,()`; replace every other character with `_`; strip.
2. Cut to 100 code points.
3. Add ` v<version>` if there is a version.
4. Add `.pdf`, or `.md` for a cheat sheet's source, or `.tex`.

### 6.5 Auth formulas

- Web sessions, OAuth tokens and the login codes: `sha256(token.encode()).hexdigest()`. Tokens are
  `secrets.token_urlsafe(32)` (43 characters); codes are `f"{randbelow(10**6):06d}"`, compared after removing all
  whitespace.
- PKCE: `s256(v) = b64url_nopad(sha256(v.encode()))`. This applies to the connector's own check and to the HSE login,
  where the verifier is `token_urlsafe(48)` and the state `token_urlsafe(16)`; the last 5 HSE login links stay valid,
  in memory.
- VAPID: an ES256 JWT `b64url(header).b64url(claims).b64url(r||s)`, with header `{"typ":"JWT","alg":"ES256"}` and
  claims `{"aud":"<scheme>://<netloc of the endpoint>","exp":now+43200,"sub":PUBLIC_URL}`, both compact JSON in that
  key order. The header is `Authorization: vapid t=<jwt>, k=<public b64url>`. Push bodies are RFC 8291 aes128gcm:
  salt(16) || rs=4096 (4 bytes, big-endian) || idlen=65 || server public key (65) || ciphertext of
  `payload + b"\x02"`. Other headers: `Content-Encoding: aes128gcm`, `Content-Type: application/octet-stream`,
  `TTL: 86400`, `Urgency`. The payload is `json.dumps({title, body, url, tag}, ensure_ascii=False)`; if it is over
  3000 bytes, `body` is cut to 750 code points plus `…`. A 404 or 410 from the push service deletes the subscription,
  and so do 10 failures in a row. Accepted push hosts are `push.apple.com`, `fcm.googleapis.com`,
  `push.services.mozilla.com` and `notify.windows.com`, the host itself or a subdomain, over https.

### 6.6 Telegram ids

Chat ids are Telethon marked ids (`utils.get_peer_id`):
- a user or bot: `+id`;
- a basic group: `-id`;
- a channel, supergroup or forum: `-(10**12 + id)`, which prints as `-100<id>`.

The same rule applies to `messages.forward.chat_id`. A message link (`collectors/inbox.py:261-267`) is
`https://t.me/<username>/<msg_id>` when the chat has a username; otherwise, for supergroup, forum and channel,
`https://t.me/c/<-chat_id - 10**12>/<msg_id>`; otherwise there is none. `chats.kind` is one of `user`, `bot`,
`group`, `supergroup`, `forum` and `channel` (`collectors/telegram.py:359-366`).

---

## 7. Time and JSON conventions

### 7.1 Times (`infomaster/clock.py`)

- Every text time column is **Moscow local time without a zone**: `local_iso(m) =
  m.astimezone(Europe/Moscow).replace(tzinfo=None).isoformat(timespec="seconds")`, giving `2026-11-22T10:05:09`.
  This covers `messages.at`, `created_at`, `seen_at`, `connector_changes.at`, `hseapp.deadlines.fetched_at`, the
  status `ok_at` values, and so on.
- **The exception is `llm_runs.at`, which is UTC**: `utc_iso(m) = m.astimezone(UTC).isoformat(timespec="seconds")`,
  giving `2026-11-22T07:05:09+00:00`. The suffix is `+00:00`, not `Z`. The column is compared as a string
  (`at >= ?` with a `utc_iso` bound), so Go must write the same format.
- Dates are `YYYY-MM-DD`. "Today", weeks and the LLM day are Moscow days.
- Some values are formatted with `isoformat()` without `timespec`: event and class `start`/`end` in API answers
  (`processing/schedule.py:356-367`), the batch fields at `processing/batches.py:454`, and the RUZ fallback uid. Python
  prints microseconds there only when they are non-zero. Stored timetable values come from second-precision
  sources, so they look like `2026-11-23T09:30:00`.
- The web views use `isoformat(timespec="minutes")` for `now`: `2026-11-22T10:05` (`web/views.py:188, 201, 290`).
- Unix seconds as integers: `web_sessions`, `web_passkeys`, `web_push`, `connector_*`, and the `e` field of library
  tokens. The `hseapp.auth` times are floats.
- Event times that the owner gives are naive local `YYYY-MM-DDTHH:MM[:SS]` (`actions.py:200-249`) and are stored as
  `isoformat(timespec="seconds")`. An all-day event has `start = "YYYY-MM-DD"` and `end = null`.

### 7.2 JSON written by Python

| writer | ensure_ascii | separators | where |
|---|---|---|---|
| `Store.put_json`, settings, chat settings, wiki kv, inbox JSON columns, connector journal | False | `", "`, `": "` | stored |
| plain `json.dumps` | True | `", "`, `": "` | `mail.state`, the second `batches.failed` writer, `*.msg_ids`, `knowledge_versions.materials`, `layout_key`'s payload is ensure_ascii=False |
| `web.json_response` (all HTTP JSON except MCP) | True | `", "`, `": "` | responses |
| MCP answers and tool `content[].text` | False | `", "`, `": "` | `/mcp` |
| Library token body, VAPID JWT | True | `","`, `":"` | signed, so the bytes matter |

Floats print in Python repr: `8.0`, `0.1`, `1e-07`, `0.30000000000000004`. Booleans print as `true`/`false`. No
`sort_keys` is used anywhere: the key order is insertion order, which matters for the signed tokens and the JWT.

Only these texts are hashed or compared as text: `layout_key`'s JSON payload, the library token body (which is
verified as received), the VAPID JWT, the grades explain prompt, and the `chat_settings.value = 'true'` lookup.
Everything else is parsed before use. A Go writer may therefore emit compact JSON in other places, as long as the
readers (Python or the static client) only parse it.

---

## 8. Stored secrets and credentials

| what | where | format |
|---|---|---|
| Web app sessions | `web_sessions.token_hash` | sha256 hex of the cookie token (`token_urlsafe(32)`) |
| Passkeys | `web_passkeys` | `credential_id`: base64url without padding of the credential id bytes; `public_key`: BLOB of the COSE public key bytes exactly as py_webauthn's `verified.credential_public_key`; `sign_count`: int, updated to `new_sign_count` at each login; `transports`: JSON list or NULL. RP id = host of `PUBLIC_URL`, origin = `scheme://netloc` of it, user id = bytes `infomaster-owner`, user verification is required |
| VAPID key | kv `web.vapid` | `{"private": PKCS#8 PEM, unencrypted, P-256}`. The public key (`applicationServerKey`) is base64url without padding of the X9.62 uncompressed point (65 bytes). A new key would invalidate every push subscription |
| Push subscriptions | `web_push` | endpoint URL, `p256dh` (base64url, 65-byte point), `auth` (base64url, 16 bytes), one per session |
| Connector OAuth | `connector_clients`, `connector_grants`, `connector_tokens` | sha256 hex of the tokens only; see §5.1 |
| HSE App X tokens | kv `hseapp.auth` | §2. Plain token strings, float unix times. The refresh token rotates on every refresh and is saved at once. A 400/401 on refresh writes `{}` |
| Library link key | kv `library_link_secret` | 64 hex characters |
| Mail credentials | environment only (`MAIL_USER`, `MAIL_PASSWORD`) | never stored; replaced by `***` in error texts |
| Telethon sessions | `data/twin.session` (the account) and `data/bot.session` (the bot), chmod 600; `data/` is 700 | Telethon 1.45 `SQLiteSession` files, schema version 8 |
| gotd sessions (Go) | `data/twin.gotd.json` (the account, removed on logout) and `data/bot.gotd.json` (the bot, made from its token), chmod 600 | gotd `session.FileStorage` JSON; secret. The Telethon files are never read: the twin logs in again at the switch (D81) |

The Telethon session schema (from the installed package, `telethon/sessions/sqlite.py`):

```sql
create table version (version integer primary key);   -- 8
create table sessions (dc_id integer primary key, server_address text, port integer, auth_key blob,
                       takeout_id integer, tmp_auth_key blob);
create table entities (id integer primary key, hash integer not null, username text, phone integer,
                       name text, date integer);       -- id: marked peer id; hash: access_hash
create table sent_files (md5_digest blob, file_size integer, type integer, id integer, hash integer,
                         primary key(md5_digest, file_size, type));
create table update_state (id integer primary key, pts integer, qts integer, date integer, seq integer);
```

Still to verify: a Go MTProto library (for example gotd) most likely cannot read this file as it is. It needs the
`dc_id`, the server address and port, and the 256-byte `auth_key`, converted into its own session format; the
`entities` access hashes must be refetched or imported. The fallback is a fresh login of both the twin and the bot.
The bot can also log in from `TG_BOT_TOKEN` alone.

---

## 9. Environment (`infomaster/config.py`, `llm/client.py`)

`.env` is loaded by `load_env`. For each line `KEY=VALUE` that is not empty and not a `#` comment, it splits on the
first `=`, strips the key and the value, strips the characters `'` and `"` from both ends of the value, and calls
`os.environ.setdefault`, so real environment variables win.

| variable | required | default | meaning |
|---|---|---|---|
| `TG_API_ID` | yes | | int, the Telegram app id (twin and bot) |
| `TG_API_HASH` | yes | | the Telegram app hash |
| `TG_BOT_TOKEN` | yes | | the bot's token |
| `TG_OWNER_ID` | yes | | int: the only user the bot serves; the login codes go to this chat |
| `API_TOKEN` | yes | | Bearer token of the internal `/api/` |
| `PUBLIC_URL` | yes | | must start with `https://`; a trailing `/` is stripped. The issuer, the WebAuthn RP, the web origin, the VAPID `sub` and the base of every link |
| `HTTP_PUBLIC_PORT` | no | 8080 | int |
| `HTTP_INTERNAL_PORT` | no | 8081 | int |
| `RUZ_ICAL_URL` | no | "" | `http(s)://` link; without it there are no RUZ classes |
| `CALENDAR_SECRET` | no | "" | `[A-Za-z0-9_-]{16,}`; the internal feeds are off without it |
| `CALENDAR_PUBLIC_SECRET` | no | "" | same rule, and it must differ from `CALENDAR_SECRET`; the public feeds are off without it |
| `MAIL_USER`, `MAIL_PASSWORD` | no | "" | the mail source is off unless both are set |
| `MAIL_HOST` | no | `imap.yandex.ru` | |
| `MAIL_PORT` | no | 993 | int |
| `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` | no | | read by the SDK; without either the LLM is "unavailable" |

Empty or whitespace-only values count as unset. A bad value stops the start with a `ConfigError`. Used only by
`compose.yaml` and not read by Python: `TS_AUTHKEY`, `CONTAINER_DNS_1`, `CONTAINER_DNS_2`, `APP_UID`.

---

## 10. LLM calls

- The client is `llm/client.py`; the model is `claude-haiku-5-5`. It uses the SDK's `AsyncAnthropic(timeout=120,
  max_retries=3)` and one `messages.create` call per request: `system` is a plain string; there is one user message
  whose content is the documents (base64 `application/pdf`), then the images (base64), then the text prompt;
  `output_config={"effort": "low", "format": {"type": "json_schema", "schema": <schema>}}`. There is no streaming, no
  tool use, and **no explicit prompt caching** (no `cache_control`). The cache tokens in the usage are recorded
  anyway.
- An answer counts as failed on `stop_reason == "refusal"` or `"max_tokens"`, or when it has no text block
  (BadAnswer). A 400 is "Rejected". Otherwise the text is `json.loads`-ed.
- Tasks (`llm_runs.task`): `batch`, `pdf`, `image`, `subjects`, `grades`, `hseapp_labels`, `quick`. Old rows may
  have `summary`. `image` and `pdf` are optional tasks.
- The journal: one `llm_runs` row per call, failed calls included, with `at = utc_iso(now)`, `ok` as 0/1, the
  `error` text, the token counts from `usage` (`input_tokens`, `output_tokens`, `cache_creation_input_tokens` →
  `cache_write`, `cache_read_input_tokens` → `cache_read`), `cost_usd`, and `duration_s` rounded to 2 decimals.
- Cost (`llm/budget.py:58-63`), with prices in $ per million tokens from the `llm_prices` setting:

  ```
  long  = input + cache_write + cache_read > 100_000
  p_in, p_out = (long_input, long_output) if long else (input, output)
  cost  = ((input + 1.25*cache_write + 0.1*cache_read) * p_in + output * p_out) / 1e6
  ```

- The reserve before a call: `input_chars = len(system) + len(prompt) + len(json.dumps(schema)) + (pages*3000 +
  images*4800) * 2`, where `len` counts code points and `json.dumps` is the ASCII form;
  `estimate = cost(ceil(input_chars / 2), max_tokens)`.
- The budget (`llm/budget.py:66-160`):
  - `limit = llm_daily_limit_usd + kv llm.raised (today)`;
  - `spent = sum(cost_usd where at >= utc_iso(Moscow midnight)) + the reserves in flight`;
  - the level is `hard` if `"hard"` is in today's `llm.notified` or `spent >= limit`; else `soft` if
    `spent >= 0.8*limit`; else `ok`;
  - `reserve()` refuses everything at `hard`, and optional tasks at `soft`. If `spent + cost > limit`, an optional
    task gets `DoesNotFit`, while a required one triggers `hard`: it is announced and added to `llm.days`;
  - `status()` gives `{"spent_usd", "limit_usd", "raised_usd"}`, each rounded to 4 places, and `"level"`.
- Prompts and schemas: copy them verbatim from the source. They are not reproduced here because some describe the
  student.

| task | system | schema | user prompt builder | max_tokens |
|---|---|---|---|---|
| batch | `processing/batches.py:180-336` | `:338-392` (DEADLINE, EVENT, NEWS, SHEET, MATERIAL, STUDY variants), chosen by `Batches._schema` | `Batches._prompt` `:1001-1044` | 16000 |
| image | `processing/images.py:34-48` | `:50-55` | `Images._prompt` `:115-122` | 3000 |
| pdf (read a sheet) | `processing/sheets.py:74-84` through `Sheets._prompt` `:626-628` (language words `:110-112`) | `:86-99` | `sheets.py:~580-590` | 2000 |
| pdf (changes of a version) | `processing/sheets.py:101-107` | `:114-119` | `sheets.py:~600-613` | 2000 |
| pdf (a PDF sent to the bot) | `processing/bot_sheets.py:40-52` | `:54-60` | `bot_sheets.py:~110-124`; `chat_id = -1` | 400 |
| subjects | `processing/subjects.py:56-83` | `:85-95` | `prompt` `:107-118` | `max(2000, 80*n)` |
| hseapp_labels | `processing/hseapp.py:38-64` | `:66-76` | `prompt` `:84-97` | `max(2000, 60*n)` |
| grades (layout) | `processing/grades.py:88-120`, with `{budget}` replaced | `:122-145` | `layout_prompt` `:158-165` | `max(4000, 60*columns)` |
| grades (explain) | `processing/grades.py:68-76` | `:78-86` | `:288-296` | `max(1000, 120*cells)` |
| quick | `web/quick.py:27-45` | `:47-62` | `Quick.prompt` `:99-119` | 600 |

---

## 11. Outside sources (from the code)

| source | protocol, endpoints |
|---|---|
| Telegram (twin and bot) | MTProto via Telethon with `TG_API_ID`/`TG_API_HASH`. The twin reads every chat; the bot sends codes and takes files from the owner |
| RUZ | GET `RUZ_ICAL_URL` (iCalendar), hourly, 60 s timeout, `If-None-Match` with the saved ETag; a 304 keeps everything. Parsed with `icalendar` (`collectors/ruz.py:87-108`): SUMMARY → subject (cleaned), DTSTART/DTEND converted to naive Moscow time, the first line of DESCRIPTION → kind, LOCATION/DESCRIPTION/URL → room and link (`:66-84`), STATUS:CANCELLED |
| HSE App X | Keycloak `https://saml.hse.ru/realms/hse/protocol/openid-connect` (`/auth`, `/token`), `client_id=app-x-android`, `scope="profile email"`, PKCE S256. The redirect is `PUBLIC_URL/hseapp/callback`, or `ru.hse.hseappx://saml.hse.ru/authorize_callback` (pasted by hand). The token form is `client_id` plus the grant fields. Deadlines: POST `https://api.hseapp.ru/v2/deadlines[?cursor=..]` with JSON `{"is_completed": bool}` and headers `Authorization: Bearer`, `Accept-Language: ru`; it follows `next_cursor` for up to 10 pages and retries once with a forced refresh on 401. Item fields: `id`, `title`, `discipline` (cut at the first `" ("`), `expires_at` (ISO UTC → Moscow date) and `completion_status` (`ACTIVE`, `COMPLETED*`). Polled every 15 min |
| Mail | IMAP4 over SSL to `MAIL_HOST:MAIL_PORT` with LOGIN. `SELECT "INBOX"` read-only; `UIDVALIDITY`; `STATUS (UIDNEXT)`; `UID SEARCH UID <last+1>:*` or `SINCE dd-Mon-yyyy`; `UID FETCH (RFC822.SIZE)`, `(BODY.PEEK[])` or, for an oversized letter, `(BODY.PEEK[HEADER])`. Every 5 min, 60 s per socket operation (`collectors/mail.py:221-258`) |
| Grade sheets | Google: GET `https://docs.google.com/spreadsheets/d/{id}/export?format=xlsx`, and the title from `https://docs.google.com/spreadsheets/d/{id}/htmlview`. Dropbox: the link's path on `https://dl.dropboxusercontent.com` with `dl=1` and the other query parameters kept. The xlsx is read by hand (zip + XML: values, formulas, notes), at most 20 MiB downloaded and 120 MiB unzipped. Every 30 min |
| Wiki (MediaWiki) | GET `https://<host>[/w]/api.php?action=parse&page=<title>&prop=wikitext|revid&format=json&formatversion=2` (`collectors/wiki.py:63-73, 266`). Every 3 h |
| PDFs behind links | `collectors/links.py`: Dropbox through `dl.dropboxusercontent.com` with `dl=1`; Drive through `https://drive.google.com/uc?export=download&id=`; Docs, Slides and Sheets through their PDF exports; Yandex Disk through `https://cloud-api.yandex.net/v1/disk/public/resources?public_key=<url>&fields=type,name,mime_type,file`. The body must start with `%PDF-`. Sent with `User-Agent: infomaster` |
| Push services | POST to the subscription endpoints (§6.5) |
| Anthropic | §10 |

Every request to a host chosen by others (links, wiki, grade sheets) goes through `net.public_session`. It resolves
the host itself and connects only to global, non-multicast addresses (an IPv4-mapped or 6to4 address is checked as
IPv4), on every redirect too (D79). A Go port needs a dialer with the same check.

---

## 12. Porting traps (Python → Go)

1. **Whitespace.** `str.split()` with no argument splits on Unicode whitespace plus `\x1c`-`\x1f` and `\x85`;
   `" ".join(s.split())` is used everywhere to normalize. Go's `strings.Fields` (`unicode.IsSpace`) does not treat
   `\x1c`-`\x1f` as space. Write a helper that matches Python's `str.isspace`.
2. **`casefold()`** is full case folding (`ß`→`ss`, `ﬁ`→`fi`, final sigma→`σ`). `strings.ToLower` is wrong for the
   deadline uid; use `golang.org/x/text/cases.Fold()` and check it against §13.
3. **Lengths and slices** count code points: every `maxLength`, `len(x) > LIMIT`, `[:N]`, SQLite `substr`, the
   arguments cut at 4000, the 750-character push body, and `file_name`'s 100. Use runes, not bytes.
4. **JSON** (§7.2): Go's `encoding/json` is compact and escapes `<`, `>`, `&`, U+2028 and U+2029. Python does not
   escape these, but with ensure_ascii it writes `\uXXXX` for every non-ASCII character (in lowercase hex, with
   surrogate pairs). `layout_key` needs a hand-written encoder. Floats: Go writes `8`, Python `8.0`. Parsing either
   is fine, but a value re-serialized for hashing is not.
5. **`isoformat()`**: `+00:00` instead of `Z`; microseconds only when non-zero; `timespec="seconds"` truncates.
6. **`isalnum()`** in `file_name` is Unicode-aware: Cyrillic letters are kept.
7. **`urllib.parse.quote`**: the default `safe='/'` keeps `/` and encodes everything else except
   `A-Za-z0-9_.-~`. With `safe=''` (the iCal edit link) `/` and `@` are encoded too. Neither matches Go's
   `url.PathEscape` or `url.QueryEscape` (for example, `QueryEscape` writes a space as `+`).
8. **aiohttp defaults**: the 404/405 texts; `HTTPException` texts like `400: Bad Request` when no text is given;
   `{"error": "Not Found"}` inside the web API guard. `/api/` checks the `Bearer` scheme case-sensitively; `/mcp`
   does not.
9. **The MCP `id`** is echoed exactly as received (string, number or null). A notification answers 202 only after the
   header checks.
10. **The library token** HMAC is computed over the received body text. Do not decode and re-encode it.
11. **Static files**: keep the MIME types in §4.3, because `nosniff` makes them matter. Range/206 support is needed
    for iOS media (`/library/...`, `/web/static`). The CSP hashes come from `index.html` as read in text mode, where
    CRLF becomes LF.
12. **`client_max_size` is 2 MiB on the public port**: whole-body JSON and form reads give 413 above it. `/mcp`
    streams up to 32 MiB; uploads stream up to 2 GiB.
13. **SQLite**: Python's `sqlite3` runs each `with db:` block as a transaction. Several writes must stay atomic, for
    example mail letters with `mail.state`, and wiki notices with `wiki.state` and `wiki.next`. Integer columns
    sometimes hold floats (`value real`).
14. **Settings normalization** returns floats for money, so `1` is stored as `1.0`. The batch hours accept `3.0`.
15. **Phrase serialization**: an error kept as a two-language Phrase is written in the request's language in web and
    action answers, and in English in `/api/status` and MCP.

---

## 13. Test vectors (made-up inputs, computed with the Python code)

Deadline uid (source `tg`, due `2026-11-30`):

| title | uid |
|---|---|
| `Calc HW 5` | `tg-fd3159f5b0` |
| `"  Calc   hw 5 "` | `tg-fd3159f5b0` |
| `ДЗ по Матану` | `tg-677ab8864c` |
| `Straße essay` | `tg-886adf5114` |
| `A B` | `tg-4c0ecdd57f` |
| `A\x1fB` | `tg-4c0ecdd57f` (`\x1f` is whitespace to Python) |
| `İstanbul` | `tg-45ccda2b47` |

Other vectors:
- `casefold("ß ẞ ﬁ Σ ς")` = `"ss ss fi σ σ"`.
- `"a\x1cb\x1dc\x1ed\x1ff\x85g h".split()` = `[a, b, c, d, f, g, h]`.
- `event_uid("tg", -1001234567890, 42)` = `tg-ed3de03ad6dbd3b3@infomaster`; with `n=2` it is
  `tg-690a4aead1368d91@infomaster`; `event_uid("mail", 0, 17)` = `mail-de991becd88b2608@infomaster`.
- The RUZ fallback for subject `Calculus`, start `2026-11-23T09:30:00`: `ruz-58d7f40dda535008@infomaster`.
- `label_key(title="Эссе по истории", discipline="История", budget=15, abbr="Hist")` =
  `hseapp.label:c847ecca6db52d85`; `label_key("Эссе", None, 15, None)` = `hseapp.label:bc4f219cb40b68c0`.
- `layout_key("1AbC_dEf", "Tab <A> & B ", budget("DM")=20)` = `grades.layout:d52ce0370145ea3b29e77d23:20`.
  The payload bytes are `["1AbC_dEf", "Tab <A> & B\xe2\x80\xa8", 3]`.
- `budget("")` = 22; `budget("VeryLongCourse1")` = 8.
- `s256("a"*43)` = `ZtNPunH49FD35FWYhT5Tv8I7vRKQJ8uxMaL0_9eHjNA`.
- Library tokens with key `00`×32:
  - `{"a":"get","m":12,"e":1800000000}` →
    `eyJhIjoiZ2V0IiwibSI6MTIsImUiOjE4MDAwMDAwMDB9.Iqbg-WwHmA33BQCiaj0wyes4`;
  - `{"a":"get","v":3,"p":"pdf","e":1800000000}` →
    `eyJhIjoiZ2V0IiwidiI6MywicCI6InBkZiIsImUiOjE4MDAwMDAwMDB9.GxUts41Sx-t3OkqFZJzyzhAv`.
- `utc_iso(2026-11-22 07:05:09.123456 UTC)` = `2026-11-22T07:05:09+00:00`; `local_iso` of the same moment =
  `2026-11-22T10:05:09`.
- `datetime(2026,11,22,7,5,9,120).isoformat()` = `2026-11-22T07:05:09.000120`; `datetime(2026,11,22,7,5).isoformat()`
  = `2026-11-22T07:05:00`.
- `quote("tg-ab/c@infomaster", safe='')` = `tg-ab%2Fc%40infomaster`; `quote("Лекция 1/v2 (a).pdf")` =
  `%D0%9B%D0%B5%D0%BA%D1%86%D0%B8%D1%8F%201/v2%20%28a%29.pdf`.
- `file_name({"title": "Calc: lecture/1", "version": 2}, "pdf")` = `Calc_ lecture_1 v2.pdf`; a cheat sheet's
  unpublished source is `Cheat.md`.
- `disposition('Конспект "1".pdf', "application/pdf")` =
  `inline; filename="________ _1_.pdf"; filename*=UTF-8''%D0%9A%D0%BE%D0%BD%D1%81%D0%BF%D0%B5%D0%BA%D1%82%20%221%22.pdf`.

---

## 14. Cutover notes

- These live in memory only and are lost on restart, which is acceptable:
  - the web login code and its attempts, and the WebAuthn challenges;
  - the OAuth authorize pages, the bot codes, the authorization codes and the spent-code map;
  - the pending HSE PKCE logins (the last 5);
  - the LLM reserves in flight;
  - the source statuses (`ok_at`, `error`), the failure streaks and the "told" flags (except `hseapp.login_told`,
    which is in kv);
  - the twin's in-progress login.
- Stop Python before Go opens the database, because both run background writers. Back up `data/` first, including
  the Telethon session files.
- The static client calls only `/web/api/*`, `/library/*` and `/web/static/*`. Its field expectations are the
  builders cited in §4.3: `web/views.py`, `web/shelves.py`, `web/machine.py`, `web/check.py`, `web/quick.py` and the
  `actions.py` state builders. Compare the outputs side by side before the switch.
- Losing `web.vapid` forces every device to subscribe to push again. Losing `library_link_secret` invalidates every
  link already given out (24 h at most). Losing the `connector_*` rows forces claude.ai to reconnect.
- Back up `data/twin.gotd.json` and `data/bot.gotd.json` with the database once Go runs; the Telethon session files can go after the switch.
