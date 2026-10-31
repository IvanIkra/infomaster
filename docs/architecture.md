# infomaster architecture

How the service is built: modules, data, outputs, the contract with Tablo, the order of work. Why it was decided this
way is in [decisions.md](decisions.md) (references like D12). How Tablo worked before infomaster is in
[sources-overview.md](sources-overview.md) (references like §5.5 that this document does not have).

This started as a plan; what is already done is in §11 and in the [README](../README.md#status).

## 1. What it is

infomaster runs on a server next to Tablo. It:

- **reads** study and other chats and channels through a separate Telegram account (the twin), plus the
  `@edu.hse.ru` mailbox, HSE App X, RUZ and grade sheets;
- **computes everything itself**: LLM parsing, the course dictionary, merging deadlines, new grades, summaries;
- **serves the results** to three places: a Telegram forum (through the bot), a Mini App for the settings and an API
  for Tablo.

After the move Tablo only displays (D1).

## 2. Overview

```
                 ┌────────────── collection ────────────┐
Telegram (twin) ──► messages ──┐
IMAP mail ────────► messages ──┤
                               ├─► batch parsing (Haiku) ──┬─► news
PDFs from chats ──► sheets_pdf ┘   every N h / "urgent"    ├─► deadlines (source=tg|mail)
                                                           ├─► events (consultations, events)
                                                           └─► problem sheets
RUZ iCal ─────────► classes ────────────────────────────────► events (classes)
HSE App X ────────► deadlines (source=hseapp)
Google Sheets ────► grade_items ─► grade_events
Mini App, buttons ► own_deadlines / hidden_deadlines

One subjects dictionary for everything.

                 ┌──────────────── output ──────────────┐
events ───────────► iCal feed (English) ─► Tablo, any calendar
deadlines ─ merge §5.5 ─ hiding ─────► API ─► Tablo
news, grade_events ─────────────────► API ─► Tablo
everything new ─────────────────────► bot ─► forum (topics)
settings ◄──────────────────────────── Mini App
```

## 3. Stack

| What | With | Why |
|---|---|---|
| Language | Python 3.12+, one asyncio process | Telethon exists only for Python; Tablo is built the same way (§1) |
| Twin | Telethon (MTProto, a user account) | the only maintained userbot library for Python |
| Bot | Telethon with a bot token | one library for both clients; over MTProto the bot uploads files up to 2 GB, not 50 MB |
| HTTP | aiohttp | Mini App, API, HSE App X callback |
| Storage | SQLite | as in Tablo, one file is enough |
| LLM | `anthropic`, model `claude-haiku-5-5` | as in Tablo (§2.1) |
| PDF | pypdf (text), page rendered to an image for scans | MIT licence; PyMuPDF is AGPL |
| Spreadsheets | openpyxl | as in Tablo (§3.3) |
| Calendar | icalendar | reading RUZ and publishing our own feed |
| Text width | Pillow + the same Inter file as Tablo | line budgets (§2.1) |
| Mini App | static HTML/JS + `telegram-web-app.js`, no build | few screens |
| External access | Tailscale Funnel | a permanent HTTPS address without a domain (D16) |
| Deployment | Docker Compose on Tablo's server | like Tablo (D26) |

**Docker:**

- `data/` (SQLite, the twin's session, HSE App X tokens) is a volume and never gets into the image.
- Tablo and infomaster share a Docker network: Tablo reaches the internal port by service name, and that port is not
  published outside.
- **Tailscale runs in a separate sidecar container** (D31): `tailscale/tailscale`, userspace mode, its own state in a
  volume. It joins the personal tailnet (the same one as the host's Tailscale) as a separate node `infomaster` with
  the tag `tag:infomaster`; Funnel is allowed only for that tag and proxies only infomaster's public port (§8.1). The
  host's Tailscale is left alone: no routes, no DNS, no Funnel settings on it. At first the host's Tailscale was
  believed to be corporate; it is not.
- The internal port is not published on the host at all.
- Traffic to the Anthropic API and Telegram goes through the host's routing, that is through the AmneziaWG VPN.

## 4. Modules

```
collectors/   telegram, mail, hseapp, ruz, grades       — collection only, store raw data
processing/   batches, pdf, subjects, deadlines, grades,
              schedule, summaries                        — everything that computes
llm/          client, budget, journal                    — the single entry to Haiku: limit + journal
outputs/      forum (bot), miniapp, api, ical_feed
settings/     storing and validating settings
```

The rule: collectors never call the LLM, and processing never goes to the network except through `llm/`.

## 5. Collection

### 5.1 Telegram (the twin)

- Listens to new messages, edits and deletions in real time. After a restart it catches up from the last id in
  every chat.
- Every new chat of the account appears in the settings **turned off** (D11).
- Only messages from enabled chats reach the database. Albums (`grouped_id`) are glued together; replies and forwards
  keep their context.
- Files are downloaded on demand: PDFs for problem sheets (§6.3), images only if the chat has "read images" on.
- The twin **writes nothing** to chats. Everything outgoing is sent by the bot (D15).

**Chat settings:**

| Setting | Default | Meaning |
|---|---|---|
| `enabled` | no | read the chat or not |
| `study` | no | a study chat: news rank, deadlines |
| `filter` | `all` | `all` or `important`: only people from the list, pins, files, messages with dates and replies to them |
| `people` | empty | important senders (the class representative, teachers) |
| `images` | no | read images (D12) |
| `pdf_content` | no | read whole PDFs within the limit (D14) |
| `summary` | no | include in the daily summary (D17) |
| `urgent` | no | a pin, a PDF or a message from the people list starts processing at once (D19) |
| `batch_hours` | the common N | an individual processing interval |

**Stage 9 details (images, D12):**

- An image is a photo or a picture sent as a file (JPEG, PNG, GIF, WebP; not stickers or GIF animations). Like a
  PDF, it is downloaded only when processing reaches the message and only if the chat has `images` on:
  `data/files/<sha256>.<ext>`, a `files` row with `kind = image`. The type is checked by the first bytes; files over
  3.75 MB (5 MB in base64, the API limit) are not downloaded. A link preview's picture and file are not files of the
  message (Telethon gives them in `photo` and `document`): the preview is not read, and its late arrival by an edit
  does not queue the message. Messages saved before stage 9 have photos recorded without a size that may have been
  previews; because of that the first edit of such a message does not count as new.
- Haiku reads an image in a separate call with the task `image` (`processing/images.py`): `shows`, what it shows
  (1–2 sentences in English), and `text`, the text on it as is, up to 2500 characters. The reading is stored in
  `files.reading`; in the batch prompt it goes under the message ("Image, as another model read it"). Editing a
  message does not read its image again, and the same image in another message (by `sha256`, a forward) takes the
  existing reading.
- Limits: the common setting `images_daily` (30 per Moscow day by default, counting `image` calls in the journal,
  failed ones too; edited on the Chats screen), and above 80 % of the LLM limit images are not read (§7). An unread
  image goes into the batch only as "photo"; a message with nothing else to read is marked processed without a call.
  If the limit is exhausted entirely or Haiku is unavailable, processing of the chat stops, and messages with images
  wait with the rest. A model refusal or an API 400 for an image is remembered: it is not tried again.
- Summaries (§6.6) use what processing has already read and do not read images themselves.

### 5.2 Other sources

The logic moves over from Tablo unchanged:

| Source | Where in Tablo | What changes |
|---|---|---|
| IMAP mail | §6.2 | letters go to the common `messages` table (`source=mail`) and are processed together with the chats |
| HSE App X | §5.1 | the login page moves to the Mini App, the callback goes through Funnel |
| RUZ iCal | §4.1–4.2 | the data becomes part of our feed (§6.5) instead of being served as is |
| Google Sheets | §3 | unchanged |

**Stage 5 details (HSE App X):**

- Active and completed HSE App X deadlines are a snapshot of the source, not a history, so, as in Tablo, they live in
  `kv["hseapp.deadlines"]` (`id`, `label`, `due`, `course`, `title`), not in the `deadlines` table. `course` is the
  discipline as HSE App X names it; the merge takes `abbr` from the dictionary, as for `deadlines.course`.
- Polling is turned on by the common setting `hseapp_enabled` (yes by default, a switch on the HSE App X screen).
  Login links (PKCE states, the last 5) live in memory: after a restart a new one is needed.
- A needed login is one message to System per change of state (remembered in `kv`, so a restart does not repeat it)
  with a button. Web App buttons do not work in groups, so the button opens the chat with the bot
  (`/start hseapp`), and the bot answers with a Mini App button for the HSE App X screen. A restored login is one
  line. The API unavailable for 3 h (Tablo's threshold, §5.1) is one line, and one more when it is back.
- Labels: a budget of 15 characters, checked in characters, as in batch parsing. If Haiku is unavailable, cached
  labels stay, and only deadlines without a label keep the Russian title (in Tablo a failure made them all Russian).
  A truncated or rejected answer is split in half; the deadline it breaks on keeps its Russian title, which is cached
  as its label so as not to pay for it every 15 minutes.

## 6. Processing

### 6.1 Batch parsing

- **When.** On a schedule at fixed hours of the day, every `batch_hours` (3 by default, a chat may have its own).
  Plus at once on the "urgent" trigger. The last scheduled run happens before the daily summary (D19–D20).
- **How.** One call per chat: the new messages since the last run, up to a size limit (a long batch is split into
  several calls). The prompt also carries the chat name, the course dictionary, live news, upcoming deadlines and
  events from chats, so that the model can point out a replacement or a move, as in §6.4.
- **What the model returns** for every meaningful message (nothing for chatter):

```json
{"items": [{
  "msg_ids": [1234, 1236],
  "news": {"full": "…", "short": "…", "importance": 4, "replaces": ""},
  "deadlines": [{"course": "Математический анализ", "label": "Calc HW 5", "due": "2026-10-22", "moves": ""}],
  "events": [{"course": "…", "title": "Calc consultation", "start": "2026-10-23T18:10", "end": "…", "room": "R405", "kind": "consultation", "moves": "", "cancels": ""}],
  "sheet": {"msg_id": 1236, "course": "…", "number": "3", "due": "2026-10-30"}
}]}
```

- The answer is validated and retried by length as in §6.4. Letters and channel posts go through the same parsing.
- The model sees every message **once**: `parsed_at` is set after the call. If the call fails, the messages stay in
  the queue.

### 6.2 Course dictionary

As in §2.2: one dictionary, manual entries win, `resolve` runs under a lock. Manual corrections are made in the Mini
App (D3).

Stage 4 details: the title budget is 13 characters (what Tablo's column really holds: "Intro to Prog"); Haiku's
answer can also be deleted in the Mini App, and then the course is named again. RUZ courses pass through the
dictionary after every poll, and the courses of deadlines and events after every batch run. The parsing model and
the summaries see the whole dictionary, one line per course: `abbr`, `title` and every name it goes by. So a course is
known under any name (a short form like «ВвП», a nickname, a translation): the parsing model names it as in the
dictionary, and the deadline label and event title start with its `abbr`; it invents its own abbreviation only for a
course not in the dictionary; a summary names a course by its `title`. Naming a new name, Haiku also sees the whole
dictionary and gives a short form of a known course that course's `title` and `abbr`. A short form Haiku misses can
be added in the Mini App as a manual entry with the course's `title` and `abbr`. `deadlines.course` keeps the source's name; `abbr` comes from the dictionary at merge time (stage 5).
A Haiku error stays visible (`ruz.subjects_error`, the Mini App) while at least one of the names it failed on waits
for a translation; a manual entry or the course disappearing from the source clears it.

### 6.3 Problem sheets

1. A PDF from an enabled chat is downloaded and the text of its first page extracted locally. If there is no text (a
   scan), the page is rendered and goes to the model as an image.
2. Whether it is a sheet is decided by the file name and the first page. This step is part of batch parsing, or a
   separate call if the "urgent" trigger fired.
3. If `pdf_content` is on, the model reads up to 10 pages and 30k tokens (whichever comes first) and writes a summary.
4. Deadlines, news and events from the PDF go to the common tables (D14).
5. The bot posts the file to the Sheets topic with a caption: course, number, due date, a link to the original
   message, a course hashtag.
6. **A new version** is detected by the rules of D14. The bot replaces the file in the old message (a media `edit`)
   and replies to it with "sheet updated" and a short description of the changes. For that the difference of the
   texts is computed locally, and only it goes to the model.

**Stage 6 details:**

- A PDF is downloaded not when the message arrives but when parsing reaches it (`collectors/files.py`): the file goes
  to `data/files/<sha256>.pdf`, and the `files` row remembers both the result and a failure of the file itself
  (deleted, broken, not an image), so parsing the same message again does not download the file again. A failure on
  the way (the twin not connected, the network, FloodWait, a Telegram server error) is not remembered: the message
  stays in the queue until the next run, and the chat's other messages are parsed without it. A chat closed to the
  twin is a failure of the file itself. Files over 50 MB are not downloaded. Mail attachments are not read at this
  stage.
- There is no page rendering (pypdf does not draw): a scan (fewer than 5 words on the first page) goes to the model
  as a PDF of its first pages, as a document; the model sees the pages as images. The budget reserve is 3000 tokens
  per page.
- The batch schema got a `sheets` array (`msg_id`, `course`, `number`, `due`) in every item, by analogy with
  `deadlines` and `events`, instead of a single `sheet` field. A sheet enters the `sheets` table in the batch's
  transaction (status `new`) and the forum after the run; a Telegram failure leaves it queued until the next run.
- The same file (by `sha256`) a second time is not a sheet. A new version is the same message with the file replaced,
  or the same chat and course (by the dictionary's `abbr`) and the same number; if at least one has no number,
  almost the same file name (without the extension and version marks like `v2`, `final`, `(1)`, `исправленный`;
  the numbers in the name must match).
- Full reading (`pdf_content`) is the task `pdf`: a summary in the forum's language (D41) in the caption and
  deadlines from all pages. They go to `deadlines` with the source `tg` and a `sheet_id`: editing the message text
  does not withdraw them, deleting the message does. Events and news are not taken from the full reading: batch
  parsing provides them from the message and the first page. For a new version only the difference of the texts goes
  to the model, and the answer is "what changed" in a reply to the old message.
- The reading limits are the common settings `pdf_pages` and `pdf_tokens`, edited on the Sheets screen (there was no
  common settings screen yet).

### 6.4 Deadlines

- **Sources in order of reliability:** `hseapp` → `ical` (midterms) → `mail` → `tg` (chats, channels, PDFs) →
  `ha` (own).
- **Merging and dropping completed and hidden ones** follows the algorithm of §5.5, plus D45: a letter or a chat
  moves HSE App X's date. **Picking for the
  screen** follows §5.6.
- **Own and hidden** (`own_deadlines`, `hidden_deadlines`) live here and are managed in the Mini App and with the
  forum buttons; HA has no to-do lists any more (D34).
- **The Deadlines topic.** The bot edits the pinned message with the list of upcoming deadlines in place. Every new
  or moved deadline arrives as its own message.

**Stage 5 details (the merge and the Deadlines topic):**

- There is one merge for everyone (`processing/merge.py`): `GET /api/deadlines`, the Deadlines pin and the Mini App
  take one list in one order. `deadlines` rows (RUZ, mail, chats) are taken while in force and at most 3 days old,
  like `news_deadlines` in Tablo; a row's `course` is the source's name, and the `abbr` for comparison comes from the
  dictionary or, without an entry, from the first word of the label, as in Tablo.
- A hidden deadline remembers the row it came from (`hidden_deadlines.ref`: `dl:<id>`, `hseapp:<id>`) and stays
  hidden after that row is renamed (the English HSE App X label after Haiku answers, `<abbr> Midterm` after the
  dictionary). In Tablo hiding compared only the title and date, and a renamed deadline came back.
- Topic messages come from the difference of lists: the new list is compared with the last one shown
  (`kv["deadlines.shown"]`) by source row. New or moved means a message; withdrawn by the source (message deleted or
  edited, the class is gone, gone from HSE App X) means "withdrawn"; gone otherwise (hidden, completed in HSE App X,
  long past) means silence; a copy of the same task on the same date from another source is not news; a rename only
  edits the pin.
- Silently, into the pin only, come: everything on the first showing of the list, on the first HSE App X poll and on
  the first RUZ poll (in stage 4 midterms of the first poll arrived as messages), and changes made by the owner (HA,
  the Mini App, buttons).
- Under a deadline message is **Hide** (**Delete** for an own one: it is deleted, as in HA), and under a hidden one
  **Restore**. Under a news notification is **Hide**: the item leaves the pin for good (and `/api/news`); an item
  that replaced a hidden one (an edit of the same message) is hidden too.
- A change of `RUZ_ICAL_URL` (the link's hash differs from the stored one) makes the next poll with classes the first
  one: System reports the changed link, Timetable gets nothing, and the old classes' midterms are withdrawn silently.
- A manual dictionary correction in the Mini App renames `<abbr> Midterm` at once, without waiting for a RUZ poll.
- The pin is rebuilt after every batch run, RUZ and HSE App X poll, owner action, and at midnight ("today" and
  "tomorrow" shift).

### 6.5 Timetable and the iCal feed

- The feed holds classes from RUZ plus consultations and events from chats (D2).
- Titles are English, from the dictionary (`title`). The first line of `DESCRIPTION` is the type: *Lecture*,
  *Seminar*, *Consultation*, *Event*, *Interim assessment*. `LOCATION` is the room or a link. This way Tablo's
  current `ical.py` reads the feed unchanged (§4.2, §4.4).
- The `UID` is stable: classes take it from RUZ; events from chats use a hash of the chat and the id of the first
  message. Moving an event changes the time and keeps the `UID`.
- A cancellation or move of a class announced in a chat **is applied** to the RUZ class; Tablo did not do this
  (§4.5). A cancellation becomes `STATUS:CANCELLED`, a move a new time with a note in `DESCRIPTION`. If RUZ later
  changes the class itself, RUZ wins.
- **Two calendars** (D28):
  - `main`: classes and everything one has to attend (a consultation before an exam, a test, a submission);
  - `optional`: where one may go: optional consultations, events, meetings.

  The model decides where an event goes during batch parsing (the `required` field in `events`). A mistake can be
  fixed with a button under the message in the Timetable topic or in the Mini App.
- **The feed for Tablo** is a third one, `tablo`: both calendars together, with marks on every event:
  - optional ones: `STATUS:TENTATIVE` and `X-INFOMASTER-OPTIONAL:TRUE`;
  - the source: `X-INFOMASTER-SOURCE`: `ruz`, `tg`, `mail`, `pdf` (for an icon, as with deadlines, §5.6);
  - a link to the original message: `X-INFOMASTER-LINK`. Not `URL`: the current `ical.py` treats an event with a
    `URL` as an online class (§4.2).

  The current `ical.py` skips only `CANCELLED`, so it reads `TENTATIVE` as an ordinary class and ignores the `X-`
  fields: the feed works in Tablo without changes. Showing the marks needs a small change (the checklist in
  [decisions.md](decisions.md)).
- The feeds are served by a secret link inside the internal network only (§8). Timetable changes go to the Timetable
  topic.

**Stage 4 details:**

- Classes with `STATUS:CANCELLED` in RUZ itself are not skipped, as in Tablo, but stored with a mark: in the feed
  they are `CANCELLED` (Tablo still skips them), and Timetable gets a cancellation.
- A class gone from RUZ before it starts was removed by RUZ: it leaves the feeds, and Timetable gets "removed". Gone
  after it started means it left the five-week window, and it is kept as it was. New classes beyond the old end of
  the feed are just the window moving and are not reported. An empty feed while future classes exist counts as a RUZ
  failure. An empty feed while there are no classes yet (between terms) is not a "first poll": "RUZ connected" comes
  once, with the first feed that has classes. The status (`ruz.classes`) holds the number of classes of the last feed
  received, without stored past ones.
- The RUZ link is a secret: a poll error reaches the status, the log and System without it (an HTTP error as the code
  and status text, others with the link cut out).
- Times in the feeds are in UTC: every calendar reads it, and Moscow has no daylight saving time. An event without an
  end lasts 80 minutes (one class).
- The model sees classes 21 days ahead and only in study chats. A class counts by the later of its RUZ time and its
  changed time: one moved later stays in the list while the move is ahead, so the message edit can be repeated and
  the class moved or cancelled again. A change of a class not shown to the model (the chat stopped being a study
  one) is not withdrawn when the message is edited. A wrong change of a class is fixed with **Don't apply** (a hidden
  change does not apply); a class has no main/optional choice.
- The midterm deadline (`<abbr> Midterm`, source `ical`) follows its class as the feed shows it: a move from a chat
  moves it, a cancellation from a chat withdraws it, **Don't apply** brings back the RUZ date. They are reconciled
  after every RUZ poll, every batch run and every toggle of a change by the owner.
- Event versions: if the message of a version already replaced by a newer one is deleted, it is withdrawn too;
  deleting the message of the current version brings back the nearest earlier version that was not withdrawn, and
  Timetable gets one message.
- A class changed from a chat has the change's source (`tg`) in `X-INFOMASTER-SOURCE` and a link to the message in
  `X-INFOMASTER-LINK`.

### 6.6 Summaries

- **Daily:** at 21:00 (configurable) for chats with `summary`, to the Summaries topic (D20).
- **On demand:** `/summary [chat] [period]` (D17). Without a chat the bot shows buttons with the list of chats. The
  command can also be a reply to a message forwarded from a chat.
- They are built from the raw messages in `messages` and do not wait for a batch. A long period is first compressed
  day by day, and the result is assembled from the daily digests.

**Stage 7 details:**

- Code: `processing/summaries.py` (which messages, the period, Haiku calls) and `outputs/summaries.py` (the Summaries
  topic, the daily run, `/summary`).
- "Since the last summary" means messages that have not yet been in any summary (`messages.summarized_at`) within the
  last 7 days. Only a summary without a period sets the mark (the daily one or `/summary` without a period), and only
  after publishing. A summary for a period (`3h`, `2d`, `20.10`, `20.10-22.10`; `3ч` and `2д` also work) marks
  nothing: it is a look back. A date later than today means last year.
- The model gets what batch parsing gets (§10): only readable messages, and for a chat with the `important` filter
  only important ones and replies to them. A summary is a list of points in the forum's language or the one named in
  `/summary` (D41), each with the `msg_id` of the message its "→" link leads to (private chats and basic groups have
  no links). Up to 12 points. A period over 30k characters is split by day (a day that does not fit on its own goes
  into several calls of up to 15 points each), and the result is assembled from the daily digests.
- Daily: over enabled chats with `summary`, study chats first, one message per chat (a long one as several in a row);
  chats with nothing important share one "Nothing important" line. A summary time missed while the service was down
  is caught up at start; the very first start only remembers the day. The time is changed on the Summaries screen of
  the Mini App (`summary_time`); there was no common settings screen yet.
- Limit: the daily summary is an optional task (above 80 % it is skipped, Summaries gets a line, and the messages
  wait for the next one). `/summary` works above 80 % too, but a summary that does not fit in what is left is refused
  alone and does not stop other calls until midnight the way a mandatory task would (`asked` on the LLM call,
  `DoesNotFit`); both are `summary` in the journal. A network failure keeps the messages for the next summary; an
  answer the model cannot give (a refusal, a cut-off) skips them: the same input would fail again.
- `/summary` is accepted only from the owner, in a private chat with the bot or in the forum; `/summary@other_bot`
  is left alone. The bot answers in HTML (Telethon defaults to Markdown, and a chat name would break the markup). A
  failure to post to the topic is reported in the reply to the command; in the daily summary it does not hold up the
  other chats, and the chat's messages wait for the next one. A point longer than 500 characters is cut so that a
  line always fits in a message. The chat is found by part of its name among the enabled ones (an exact match wins);
  several matches give buttons, and none or no chat give buttons for all enabled chats (daily-summary and study ones
  first, up to 30). A reply to a forwarded message: a channel (and an anonymous group admin) is visible in the
  forward itself, while a person's message is looked up by its send time and text among the stored ones, so only
  from enabled chats and at most 14 days old. The summary goes to the Summaries topic and the bot answers with a link
  to it; if nothing is important, it answers with just a line.
- "The last scheduled run before the summary" (D20) is not tied separately: summaries do not depend on batches.

### 6.7 Grades

Unchanged from §3: tab layout by Haiku with a cache, a 0–10 scale, a snapshot on the first poll, placeholder zeros,
a journal of new grades. A new grade goes to the Grades topic and the API.

**Stage 8 details:**

- Code: `collectors/grades.py` (downloading the xlsx, the sheet title, the top of the tab and the own row,
  `TabView`), `processing/grades.py` (the Haiku layout with a cache, the scale, new grades, settings),
  `outputs/grades.py` (the Grades topic). The label budget (23 characters for "course task") and the layout rules are
  as in Tablo.
- **Haiku picks the header.** Tablo guessed where the header ends itself (up to 6 rows above the first row that looks
  like a capitalised full name), and a sheet with students written in lower case or with an "average" row above them
  broke the guess. Now the model sees the top of the tab (up to 15 rows above the own row, other students' rows
  included) and the own row without the owner's name, and names the header row numbers (`header_rows`) in the same
  layout call (`LAYOUT_VERSION = 3`). The cache is keyed by sheet, tab and budget together with the text of the named
  header rows: while that stays the same, the layout holds whatever changes in other rows (other students' grades, a
  new student, the average). If no header was named or there was no answer, the cache holds until the whole top of
  the tab changes.
- The settings are common: `grades_student` (the full name), `grades_sheets` (`[{url, label}]`, where `label` is the
  course before the task label, up to 10 characters; empty means the first 8 characters of the sheet title),
  `grades_window_days` (7). They are edited on the Grades screen of the Mini App; sheets are read right after an
  edit and every 30 minutes after that. `include_baseline` was not carried over: the Grades screen shows all current
  grades with the new ones marked.
- Differences from Tablo. No own row in any tab is an error of the sheet, not an empty list: grades are not erased.
  A grade no longer in the sheet (the tab hidden, the row gone from one tab, the cell cleared) is remembered as gone
  (`grade_items.gone`): coming back with the same grade is not news, with a different one it is news with the
  previous grade. A column shifted by an insertion before it is the same task (the tab, header and grade match one of
  those gone in the same read), not a new grade. A task the model named twice counts once. The layout answer gets at
  least 60 tokens per column (a tab of dates is not cut off). An answer the model cannot give (a refusal, a cut-off)
  is cached as a skipped tab so as not to pay for it every 30 minutes, and is visible in the status (`unread`) and
  the Mini App; a network failure or the limit is a sheet error, the grades stay, and the tab is laid out on the next
  poll. Removing a sheet or changing the name erases its grades: the next read is a snapshot again. A closed sheet (a
  login page instead of an xlsx) is a "not a workbook" error.
- The Grades topic: after a poll, one message per sheet (the heading is the sheet title linking to it and a course
  hashtag), a line per task: the label, the grade 0–10 (as on Tablo's screen), the bonus, the sheet's points in
  brackets if its scale is not 0–10, and the previous grade for a changed one. Unsent ones (a Telegram failure) wait
  for the next poll while within `grades_window_days`; a long message is split into several, and each marks its own
  grades as sent, so a failure in the middle repeats nothing. The first read of a sheet is a line in System
  ("connected: tabs…, grades…"); a sheet that cannot be read for 12 h is one line in System, and one more when it is
  read again.
- `GET /api/grades` (internal port, Bearer) is level C of §11.5: `{"items": [{key, label, value, bonus, seen_at}],
  "window_days"}`, the last event within the window for every task, newest first. In `GET /api/status` → `grades`:
  `configured`, `sheets` by sheet id (`title`, `ok_at`, `error`, `tabs`, `items`, `unread`; neither links nor names),
  `llm`, `new`.

## 7. LLM: the journal and the daily limit

**The journal (D22).** Every call, failed ones included, is written to `llm_runs`: the time (UTC), the task (`batch`,
`pdf`, `image`, `summary`, `subjects`, `grades`, `hseapp_labels`), the chat, the model, tokens (input, output,
cache), the cost, the duration, the error. The Mini App shows spending for today, the week and the month, broken down
by task and chat.

**The limit (D21).** A daily dollar limit for all calls together. Days are counted in Moscow time; prices are set in
the settings.

- **Before a call** the budget reserves the worst case: an input estimate from the text length plus `max_tokens` for
  the output. If the reserve does not fit in what is left, the call is not made. After the answer the reserve is
  replaced by the real cost.
- **80 % of the limit.** Optional tasks are switched off: images, whole-PDF reading, the daily summary. A warning
  goes to the System topic.
- **100 %.** All calls stop until midnight. Messages pile up in the queue and are processed the next day. The bot
  reports this in System. A button raises the limit for today.
- **Working through the queue after a pause** (D30):
  1. Order: study chats → mail → non-study chats and channels.
  2. Non-study messages older than `ttl_hours` (24 h) are marked processed without calling the model: their news
     would not be live anyway (as in Tablo, §6.3).
  3. Study messages are never thrown away: they hold deadlines and events.
  4. If the limit hits the ceiling three days in a row, the bot suggests raising it in System and shows which chats
     eat the most.
- The limits on the number of images and on PDF pages and tokens work inside the common limit.

## 8. Outputs

### 8.1 Two HTTP ports

Only the **public port** is exposed through Funnel. The API for Tablo and HA sits on a separate port reachable only
on the local network, so neither the API nor Tablo's feed faces the internet. The `main` and `optional` feeds for
one's own calendar (Apple, Google) are open through Funnel as well, but under a different secret (D43): Google and
iCloud subscriptions fetch the feed from their own servers and cannot reach the internal port or the tailnet.

| Port | Paths | Protection |
|---|---|---|
| public (Funnel) | `/app/*` (the Mini App), `/hseapp/callback`, `/calendar/<CALENDAR_PUBLIC_SECRET>/main.ics` and `optional.ics` | Mini App: an `initData` check, only the owner's Telegram id is let in. Callback: the PKCE `state`. Feeds: the secret in the path; no `tablo.ics` here |
| internal | `/api/*`, `/calendar/<CALENDAR_SECRET>/main.ics`, `optional.ics`, `tablo.ics` | a Bearer token; the feeds use a secret in the path because calendars cannot send headers |

The secrets differ: a leaked link of one's own calendar is replaced without touching Tablo. Without
`CALENDAR_PUBLIC_SECRET` the public port has no feeds, and it may not equal `CALENDAR_SECRET`.

### 8.2 API for Tablo and HA (internal)

Where possible the answers repeat the format of Tablo's current API (§8), so that Tablo and HA need fewer changes.

| Method and path | What it serves |
|---|---|
| `GET /calendar/<secret>/main.ics`, `optional.ics`, `tablo.ics` | the feeds (§6.5) |
| `GET /api/deadlines` | the final list after merging and hiding, in screen order (§5.6), in Tablo's current format |
| `GET /api/news` | live news: `id`, `source`, `study`, `posted_at`, `full`, `short`, `importance` (replaced items are not served) |
| `GET /api/grades` | new grades within the window: `key`, `label`, `value`, `bonus`, `seen_at`, newest first |
| `GET /api/status` | per source: `ok_at`, `error`, plus `hseapp.login_needed`, today's LLM spending and the limit |

The API serves the full current state, with no cursors (D10). There is no "refresh now" endpoint (D5).

**Stage 10 details:**

- `GET /api/news` returns `{"items": [{id, source, study, posted_at, full, short, importance}], "ttl_hours"}`: live
  news in rank order, exactly what the News pin shows (`News.live`), without items replaced, withdrawn by the source
  or hidden with the button. `id` is a stable number; `source` is `tg` or `mail`; `posted_at` is local time without a
  zone (§9). This is level B of §11.4 without `deadlines[]` and `replaced`: deadlines from news come already merged in
  `/api/deadlines`, and replaced items are not served. The lengths of `full` and `short` are checked in characters
  for now, not by the font (§2.1).
- `GET /api/status` needs Bearer, like all of `/api/*` (in Tablo the status was open). Keys are per source:
  `telegram`, `batches` (batch parsing: freshness of the news), `ruz`, `hseapp` (with `login_needed`), `mail`,
  `sheets`, `summaries`, `grades`, `images`, plus `llm` (`spent_usd`, `limit_usd`, `raised_usd`, `level`). There is
  no separate `news` key as in Tablo: `batches` plays its role.
- There are no write routes (`POST/PATCH/DELETE /api/deadlines` as in Tablo): own and hidden deadlines are managed in
  the Mini App and with the forum buttons. infomaster is not connected to HA (D34): no to-do lists, no
  `tablo_changed` events, and the internal port is not published on the host. Only Tablo reads it, by service name
  on the `infomaster` network.
- **The move from Tablo** is the command `python -m infomaster import-tablo --tablo-db <tablo.db>`
  (`infomaster/tablo.py`), run once at the switch. From a read-only copy of Tablo's database it takes own deadlines
  (except those already present with the same title, date and list), hidden ones (by `uid`, which is computed the same
  way; they have no `ref`, so hiding is by title and date, as in Tablo) and the manual dictionary entries (a manual
  entry from the Mini App wins; a Haiku answer gives way). Rows more than 30 days past their due date are skipped. If
  the Deadlines pin has already shown something, the copied own deadlines are added to `kv["deadlines.shown"]` so
  that they do not arrive as messages.

### 8.3 Forum (the bot)

Topics of the forum group:

| Topic | What arrives |
|---|---|
| Timetable | new events, moves and cancellations |
| Deadlines | a pin with the list (edited) + new and moved deadlines |
| News | one message that the bot edits (see below) |
| Sheets | PDFs with a caption, updates as replies to the old message |
| Grades | new grades |
| Summaries | daily and on demand |
| System | source errors, "HSE App X login needed", the LLM budget |

**The News message** (D23). A pinned message that the bot rebuilds after every run. The data is the same that
`GET /api/news` serves Tablo: one `news` table and the same rules (§6.6):

- live news: not replaced and not older than `ttl_hours` (24 h);
- ranked by `importance + study`, the newer one higher on a tie;
- **no space limit**: all live news is shown, `importance = 1` included, always as `full` text. Screen limits
  (column height, `short`) stay only in Tablo;
- if the text does not fit in one Telegram message (4096 characters), the bot keeps several messages in a row and
  edits them all;
- every item has a source icon and a link to the original message: `t.me/<channel>/<id>` or `t.me/c/<id>/<msg>`.
  Letters have no link; the subject stands in its place.

**Notifications about what matters.** Editing a message does not notify, so for every new item with `importance` 4 or
5 the bot sends a short separate message to the topic. At the next rebuild it deletes it: by then the item is in the
common list.

Every bot message keeps a link to its original. News, deadlines and events have a hide button, and hiding works the
same way as hiding from HA did.

### 8.4 Mini App

Opened with a button in the bot. Reworked on 22.10.2026 (D44): four tabs at the bottom, each with one job, and every
thing lives in one place. Lists only show; to change something, tap a row: a card slides up from the bottom (what
it is, when, where from, with a link to the message) with the actions. Switches live only in the settings. **+** in
the header of the Deadlines, Timetable and Study tabs adds a deadline, an event, a grade or a sheet. Links from the
forum (`?screen=…`, `start_param`) open the right tab or screen.

- **Deadlines:** the common list after the merge, in groups (overdue, today, tomorrow, this week, later), each with
  its origin (a chat or a letter with a link to the message, HSE App X, RUZ, own). An own one can be edited or
  deleted; someone else's can be edited (it becomes own, the original is hidden) or hidden. Hidden ones are on a
  separate screen with **Restore**.
- **Timetable:** what the feeds show (`Schedule.agenda`), four weeks ahead by day: RUZ classes with changes, events
  from chats and mail, own ones. A class card: move or cancel (an own change), "the change is wrong, as in RUZ" for
  a chat's change, "apply the change" for a disabled one. An event card: edit, required or optional, remove (delete
  for an own one). Removed events are on a separate screen.
- **Study:** "Grades", by sheet and own ones by course; tapping a grade edits it (a correction of a sheet's grade
  holds until the sheet itself changes the grade) or deletes an own one. "Sheets": a card with the summary, editing
  the course, number, due date and description (the message caption in the Sheets topic is edited), "this is a new
  version of another one" (D14), and an own sheet can be uploaded as a PDF and deleted.
- **Settings:** at the top, **Needs attention** lists everything broken (the twin, mail, RUZ, HSE App X, grade
  sheets, images, the LLM limit, the iCal feeds), and the count shows on the tab's badge. Sources: the twin and chats
  (the twin's login and logout, the chat list, chat settings and important people), mail, RUZ, HSE App X (login),
  grade sheets. A grade sheet has its own screen: the name, the course before the grades, a check (when it was read,
  by which name, how many grades, "read now"), and the tabs with their layout: which column is which task, what the
  header says, the number in the own row and the 0–10 grade, all columns of the own row; a tab's layout can be
  corrected (the own one overrides Haiku's answer) and handed back to Haiku. Processing: batches and news, the daily
  summary (time, chats), whole PDFs, images per day, the course dictionary. **LLM spending** (§7, D40) is the only
  place spending is visible: three period cards, a 30-day chart with the limit line, the period's tasks and chats,
  the limit and a raise for today, model prices. The languages of the Mini App and the forum. `.env` holds only
  secrets.

## 9. Storage (SQLite)

Times are local Moscow time without a zone, except `llm_runs.at` (UTC), as in Tablo (§9).

| Table | Purpose |
|---|---|
| `kv` | collector state, HSE App X tokens, layout and label caches |
| `settings`, `chat_settings` | common and per-chat settings |
| `chats` | all chats of the account: id, title, type, last read id |
| `messages` | raw messages and letters: source, chat, author, time, text, files, `parsed_at`, `summarized_at` |
| `files` | downloaded PDFs and images: message, kind, hash, path, the page count and first-page text of a PDF, the reading of an image, the error |
| `subjects` | the dictionary (§2.2) |
| `news` | parsed news, `replaced_by` |
| `deadlines` | deadlines from external sources, `moved_by` |
| `own_deadlines`, `hidden_deadlines` | own and hidden ones (§5.4) |
| `events` | events from chats (consultations, events) and changes to RUZ classes; own events and changes have the source `own` (D44) |
| `classes` | classes from RUZ |
| `sheets` | problem sheets: message, file hash, course, number, due date, text, summary, previous version, the bot's message id |
| `grade_sheets`, `grade_items`, `grade_events` | grades (§3.6); `kv` holds the view of every tab after a read and the own tab layouts (D44) |
| `own_grades` | own grades and corrections of sheet grades (D44) |
| `bot_posts` | what the bot published: topic, message id, what it refers to (for edits and hiding) |
| `llm_runs` | the LLM journal |

## 10. Settings and secrets

- **Settings** are stored in the database and edited in the Mini App. There is no config file for the user.
- **Secrets** live in `.env` (listed in `.gitignore`), except those that appear at run time: the HSE App X tokens and
  the twin's session are kept in `data/`, which is never committed.

| Variable | Purpose |
|---|---|
| `TG_API_ID`, `TG_API_HASH` | the Telegram application for the twin (my.telegram.org) |
| `TG_BOT_TOKEN` | the bot |
| `TG_OWNER_ID` | the owner's Telegram id, the only one the Mini App and the bot let in |
| `TG_FORUM_ID` | the forum group |
| `ANTHROPIC_API_KEY` | Haiku |
| `MAIL_USER`, `MAIL_PASSWORD` | IMAP |
| `RUZ_ICAL_URL` | the RUZ feed |
| `API_TOKEN` | Bearer for the internal API |
| `CALENDAR_SECRET` | the path secret of the feeds on the internal port |
| `CALENDAR_PUBLIC_SECRET` | another secret: the `main` and `optional` feeds for one's own calendar through Funnel (D43) |
| `PUBLIC_URL` | the Funnel address of the public port (`https://infomaster.<tailnet>.ts.net`): the Mini App button, the HSE App X callback |
| `TS_AUTHKEY` | the personal tailnet's key for the sidecar, needed only on the first start |
| `CONTAINER_DNS_1`, `CONTAINER_DNS_2` | DNS servers of the containers (`compose.yaml`) |
| `HTTP_PUBLIC_PORT`, `HTTP_INTERNAL_PORT` | ports inside the container, 8080 and 8081 by default |

**The twin's session** (`*.session`) gives full access to the account. It lives only on the server, and only the
owner can read the file.

**Other people's data.** The model gets messages only from enabled chats, and for chats with the `important` filter
only the selected ones. From grade sheets it gets the top of the tab (up to 15 rows above the own row, other
students' rows included, so that the model finds the header itself, §6.7) and the own row without the owner's name.
Tablo sent only the header and the own row (§1); it was decided that classmates' grades seen by the model do no
harm. Raw messages and downloaded files are deleted after 14 days (D29); parsed news, deadlines, events and sheets
stay.

## 11. Order of work

Every stage delivers something that works.

1. **Skeleton.** Project structure, settings in SQLite, `llm/` with the journal and the limit, the bot with the forum
   and the System topic, a Mini App stub behind Funnel with the `initData` check.
2. **Twin and chat settings.** The Telethon login, the chat list in the Mini App, collecting messages from enabled
   chats, edits and deletions.
3. **Batch parsing.** The schedule of runs, the "urgent" trigger, the prompt and the schema, news and deadlines from
   chats in the database and in the forum.
4. **Dictionary and timetable.** The dictionary with manual corrections, RUZ collection, events from chats, our own
   iCal feed, the Timetable topic.
5. **Deadlines in full.** HSE App X (login through the Mini App), mail, the §5.5 merge, own and hidden ones, the pin
   in the Deadlines topic.
6. **Problem sheets.** PDFs, the first page, reading within the limit, versions.
7. **Summaries.** Daily and `/summary`, the Summaries screen in the Mini App.
8. **Grades.** The port of §3.
9. **Images.** Reading images with limits.
10. **API for Tablo** and the one-step switch of Tablo (D35) following "Handed over to Tablo" in
    [decisions.md](decisions.md).

Stage 10 could be moved earlier piece by piece. For example, the iCal feed could be plugged into Tablo right after
stage 4.

## 12. Open questions

1. **The Tailscale sidecar next to the host's Tailscale.** They share one tailnet (a personal one, D31), and the
   sidecar in userspace mode changes neither routes nor DNS, so no conflict is expected. To check at deployment: the
   sidecar has joined, Funnel answers, and nothing changed for the host and AmneziaWG.
