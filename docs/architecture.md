# infomaster architecture

How the service is built: modules, data, outputs, the contract with Tablo, the order of work. Why it was decided this
way is in [decisions.md](decisions.md) (references like D12). How Tablo worked before infomaster is in
[sources-overview.md](sources-overview.md) (references like §5.5 that this document does not have).

This started as a plan; what is already done is in §11 and in the [README](../README.md#status).

## 1. What it is

infomaster runs on a server next to Tablo. It:

- **reads** study and other chats and channels through a separate Telegram account (the twin), plus the
  `@edu.hse.ru` mailbox, HSE App X, RUZ and grade sheets;
- **computes everything itself**: LLM parsing, the course dictionary, merging deadlines, new grades;
- **serves the results**: a home-screen web app with Web Push (§8.6), a Claude connector (§8.5), iCal feeds and an
  API for Tablo. Until stage 16 a Telegram forum and a Mini App did the web app's job; both are gone (D69).

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
web app, Claude ──► own_deadlines / hidden_deadlines

One subjects dictionary for everything.

                 ┌──────────────── output ──────────────┐
events ───────────► iCal feed (English) ─► Tablo, any calendar
deadlines ─ merge §5.5 ─ hiding ─────► API ─► Tablo
news, grade_events ─────────────────► API ─► Tablo
everything ────────────────────────► web app (+ Web Push), Claude
settings ◄──────────────────────────── web app «Machine», Claude
```

## 3. Stack

| What | With | Why |
|---|---|---|
| Language | Python 3.12+, one asyncio process | Telethon exists only for Python; Tablo is built the same way (§1) |
| Twin | Telethon (MTProto, a user account) | the only maintained userbot library for Python |
| Bot | Telethon with a bot token | one library for both clients; the bot sends login codes and takes files for the library |
| HTTP | aiohttp | the web app, the connector, the API, the HSE App X callback |
| Storage | SQLite | as in Tablo, one file is enough |
| LLM | `anthropic`, model `claude-haiku-5-5` | as in Tablo (§2.1) |
| PDF | pypdf (text), page rendered to an image for scans | MIT licence; PyMuPDF is AGPL |
| Spreadsheets | openpyxl | as in Tablo (§3.3) |
| Calendar | icalendar | reading RUZ and publishing our own feed |
| Text width | Pillow + the same Inter file as Tablo | line budgets (§2.1) |
| Web app | Preact + htm as vendored ES modules, no build; `webauthn` for passkeys; Web Push on `cryptography` | a home-screen app, the only interface besides Claude (§8.6, D61, D63, D69); no Node in the image |
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
              schedule, images                           — everything that computes
llm/          client, budget, journal                    — the single entry to Haiku: limit + journal
outputs/      bot, deadlines (news to push), reminders, sheets (publishing), api, ical_feed
actions.py    the owner's actions as handlers           — what the connector and the web app call
connector/    oauth, mcp, tools, catalog, changes       — the MCP connector for claude.ai (§8.5)
web/          the web app: server and static client     — §8.6
library/      raw material and the knowledge base       — §6.8
settings/     storing and validating settings
net.py        HTTP to public addresses only             — every fetch of a link others chose (D79)
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
| `urgent` | no | a pin, a PDF or a message from the people list starts processing at once (D19) |
| `batch_hours` | the common N | an individual processing interval |
| `note` | empty | for the batches: the chat's course, who is who (D52) |

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

### 5.2 Other sources

The logic moves over from Tablo unchanged:

| Source | Where in Tablo | What changes |
|---|---|---|
| IMAP mail | §6.2 | letters go to the common `messages` table (`source=mail`) and are processed together with the chats |
| HSE App X | §5.1 | the login starts in the web app («Machine»), the callback goes through Funnel |
| RUZ iCal | §4.1–4.2 | the data becomes part of our feed (§6.5) instead of being served as is |
| Google Sheets | §3 | unchanged; Dropbox links to `.xlsx` files are read the same way (§6.7) |
| Course wiki | — | new (D60): course pages of a MediaWiki, listed in `wiki_pages` |

**Course wikis (D60).** Some courses post sheets and homework only on their wiki page (the faculty's MediaWiki): a
list of links to PDFs on Google Drive and a table of due dates by group. Every 3 hours the page's wikitext is read
through the wiki's API (`action=parse`), only when its revision changed (`collectors/wiki.py`). Its items are the links
to files outside HTML comments, each with its section and, in a table, its row under the column heads. A new item, or
one whose row changed, becomes a notice in the mailbox: a letter from "Course wiki", subject "Wiki · page: item", with
the link and the row; its msg_id is negative, counting down, so it never meets a letter's UID. The batches read it as
a letter: the PDF behind the link is fetched (D59), the model decides whether it is a sheet and takes the due date of
the student's group, which it knows from `about_me` (D52). The first read is a baseline: only the last item of each
section of tasks becomes a notice. A page failing for a day is pushed once. The wiki refuses
curl's user agent; the service sends its own.

**Stage 5 details (HSE App X):**

- Active and completed HSE App X deadlines are a snapshot of the source, not a history, so, as in Tablo, they live in
  `kv["hseapp.deadlines"]` (`id`, `label`, `due`, `course`, `title`), not in the `deadlines` table. `course` is the
  discipline as HSE App X names it; the merge takes `abbr` from the dictionary, as for `deadlines.course`.
- Polling is turned on by the common setting `hseapp_enabled` (yes by default, a switch on the HSE App X screen).
  Login links (PKCE states, the last 5) live in memory: after a restart a new one is needed.
- A needed login is one push per change of state (remembered in `kv`, so a restart does not repeat it), leading to
  «Machine → HSE App X». A restored login is one line. The API unavailable for 3 h (Tablo's threshold, §5.1) is one line, and one more when it is back.
- Labels: a budget of 15 characters, checked in characters, as in batch parsing. If Haiku is unavailable, cached
  labels stay, and only deadlines without a label keep the Russian title (in Tablo a failure made them all Russian).
  A truncated or rejected answer is split in half; the deadline it breaks on keeps its Russian title, which is cached
  as its label so as not to pay for it every 15 minutes.

## 6. Processing

### 6.1 Batch parsing

- **When.** On a schedule at fixed hours of the day, every `batch_hours` (3 by default, a chat may have its own).
  Plus at once on the "urgent" trigger.
- **How.** One call per chat: the new messages since the last run, up to a size limit (a long batch is split into
  several calls). The prompt also carries the chat name, the course dictionary, live news, upcoming deadlines and
  events from chats, so that the model can point out a replacement or a move, as in §6.4.
  Before the new messages come a few earlier ones of the same chat, for context only (D51), and when the owner
  wrote them, what they told about themselves and the chat's note; passed deadlines stay listed for a week (D52).
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
answer can also be deleted in the web app, and then the course is named again. RUZ courses pass through the
dictionary after every poll, and the courses of deadlines and events after every batch run. The parsing model
sees the whole dictionary, one line per course: `abbr`, `title` and every name it goes by. So a course is
known under any name (a short form like «ВвП», a nickname, a translation): the parsing model names it as in the
dictionary, and the deadline label and event title start with its `abbr`; it invents its own abbreviation only for a
course not in the dictionary. Naming a new name, Haiku also sees the whole
dictionary and gives a short form of a known course that course's `title` and `abbr`. A short form Haiku misses can
be added in the web app as a manual entry with the course's `title` and `abbr`. `deadlines.course` keeps the source's name; `abbr` comes from the dictionary at merge time (stage 5).
A Haiku error stays visible (`ruz.subjects_error`, the web app) while at least one of the names it failed on waits
for a translation; a manual entry or the course disappearing from the source clears it.

### 6.3 Problem sheets

1. A PDF from an enabled chat is downloaded and the text of its first page extracted locally. If there is no text (a
   scan), the page is rendered and goes to the model as an image.
2. Whether it is a sheet is decided by the file name and the first page. This step is part of batch parsing, or a
   separate call if the "urgent" trigger fired.
3. If `pdf_content` is on, the model reads up to 10 pages and 30k tokens (whichever comes first) and writes a summary.
4. Deadlines, news and events from the PDF go to the common tables (D14).
5. The sheet is published (status `posted`, D69): the web app's shelves show it with its course, number, due date,
   source and PDF (§8.6).
6. **A new version** is detected by the rules of D14 and chained to the older one, with a short description of the
   changes. For that the difference of the texts is computed locally, and only it goes to the model.

**Sheets that are no PDF of a chat (D59).** A study chat's message or a letter without a PDF of its own may link to
one: the first such link (at most three are tried) is downloaded before the batch (`collectors/links.py`) and its
first page goes to the model like a sent PDF's, with the link it came from. Followed: Dropbox (by
`dl.dropboxusercontent.com`: some resolvers block `www.dropbox.com`), Google Drive files, Google documents and
presentations (their PDF export), public Yandex Disk files (by the public API; a folder is skipped), plain URLs of a
PDF. Not tried: pages behind a login (Google Classroom, Smart LMS). What comes must start as a PDF does. A link that
cannot be read now is tried at the next runs, three times in all. A letter's first PDF attachment is kept when the
letter arrives (`files` row of source `mail`), so its batch finds it; a sheet of a letter belongs to the mailbox
(`chat_id` 0) and names the letter as its source. A message the model cannot answer for is tried at the next two runs
of its chat before it is skipped, and the pushed line about it names its PDF, so the owner can add the sheet
by hand.

**Stage 6 details:**

- A PDF is downloaded not when the message arrives but when parsing reaches it (`collectors/files.py`): the file goes
  to `data/files/<sha256>.pdf`, and the `files` row remembers both the result and a failure of the file itself
  (deleted, broken, not an image), so parsing the same message again does not download the file again. A failure on
  the way (the twin not connected, the network, FloodWait, a Telegram server error) is not remembered: the message
  stays in the queue until the next run, and the chat's other messages are parsed without it. Any other failure (a
  chat closed to the twin) is tried again at the next runs, three times in all (`files.tries`); a deleted or broken
  file is given up at once. Files over 20 MB are not downloaded (D79). A document whose type is not `application/pdf` but
  whose name ends in `.pdf` counts as a PDF.
- There is no page rendering (pypdf does not draw): a scan (fewer than 5 words on the first page) goes to the model
  as a PDF of its first pages, as a document; the model sees the pages as images. The budget reserve is 3000 tokens
  per page.
- The batch schema got a `sheets` array (`msg_id`, `course`, `number`, `due`) in every item, by analogy with
  `deadlines` and `events`, instead of a single `sheet` field. A sheet enters the `sheets` table in the batch's
  transaction (status `new`) and is published after the run; a failure leaves it queued until the next run.
- The same file (by `sha256`) a second time is not a sheet. A new version is the same message with the file replaced,
  or the same chat and course (by the dictionary's `abbr`) and the same number; if at least one has no number,
  almost the same file name (without the extension and version marks like `v2`, `final`, `(1)`, `исправленный`;
  the numbers in the name must match).
- Full reading (`pdf_content`) is the task `pdf`: a summary in the app's language (D41) and
  deadlines from all pages. They go to `deadlines` with the source `tg` and a `sheet_id`: editing the message text
  does not withdraw them, deleting the message does. Events and news are not taken from the full reading: batch
  parsing provides them from the message and the first page. For a new version only the difference of the texts goes
  to the model, and the answer is "what changed" of the new version.
- The reading limits are the common settings `pdf_pages` and `pdf_tokens` («Machine»).

**The assumed due date (stage 12, §11.1):**

- A sheet often gives no due date. Then it is due before the next seminar of its course: the first class of the
  course in the timetable as the feeds show it (RUZ with the chats' and the owner's corrections, cancelled classes
  skipped) that starts after the sheet's message (an own sheet: its upload) and within 21 days, and whose type is
  *Seminar* (seminars, practical classes, tutorials: `type_of`). A course with no seminar in the timetable at all takes
  its next class of any kind except an interim assessment or an exam. The course is matched as elsewhere: the same
  name, or the same `title` or `abbr` in the dictionary («Математический анализ» of a chat is *Calculus* of RUZ).
  Nothing found: no date, as before.
- The date is the class's day (deadlines are dates); the sheet also keeps the class's start (`sheets.due_at`) and
  shows it. The sheet is marked `due_assumed`, and its date becomes a deadline of its own: source `tg`, the sheet's
  message, `sheet_id`, `assumed`, the label `<abbr> sheet <number>` (renamed once the dictionary knows the course).
  Both are marked as assumed in the web app (the deadline's row and card, the sheet's card) and in its pushes;
  `GET /api/deadlines` has `assumed`.
- The assumed date follows the timetable while the class has not started: it is synced with the Midterms after every
  RUZ poll, batch run, correction by the owner and manual dictionary change (`Schedule.followers`). A moved class
  moves the deadline (a pushed move), a cancelled one gives the next seminar, none left withdraws it. A deadline the
  owner hid stays as it is.
- A real date wins, and the mark goes: the first page or the message (the batch gives the sheet's `due`), the sheet's
  pages (`pdf_content`), a later message (the batch model sees the deadline marked "assumed" and moves it, or gives
  the same work with a date), or the owner (the sheet's due date in the web app, or the date of the deadline itself:
  with the same title and list it stays the chat's deadline and moves; otherwise it becomes an own copy as usual).
  Clearing the sheet's date leaves it without one.
- A new version of a sheet takes the older one's assumed deadline over (its message, so deleting the older message
  does not withdraw it), and its own date is found the same way, from the new message's time. Deleting the message
  withdraws the deadline, and the sheet stops assuming. Sheets saved before stage 12 are not assumed.

### 6.4 Deadlines

- **Sources in order of reliability:** `hseapp` → `ical` (midterms) → `mail` → `tg` (chats, channels, PDFs) →
  `ha` (own).
- **Merging and dropping completed and hidden ones** follows the algorithm of §5.5, plus D45: a letter or a chat
  moves HSE App X's date. **Picking for the
  screen** follows §5.6.
- **Own and hidden** (`own_deadlines`, `hidden_deadlines`) live here and are managed in the web app and by Claude;
  HA has no to-do lists any more (D34).
- **News of the deadlines** (`outputs/deadlines.py`, D69): every new or moved deadline is pushed to the web app's
  devices (§8.6).

**Stage 5 details (the merge and its news):**

- There is one merge for everyone (`processing/merge.py`): `GET /api/deadlines`, the web app and the connector take
  one list in one order. `deadlines` rows (RUZ, mail, chats) are taken while in force and at most 3 days old, like
  `news_deadlines` in Tablo; a row's `course` is the source's name, and the `abbr` for comparison comes from the
  dictionary or, without an entry, from the first word of the label, as in Tablo.
- A hidden deadline remembers the row it came from (`hidden_deadlines.ref`: `dl:<id>`, `hseapp:<id>`) and stays
  hidden after that row is renamed (the English HSE App X label after Haiku answers, `<abbr> Midterm` after the
  dictionary). In Tablo hiding compared only the title and date, and a renamed deadline came back.
- The news come from the difference of lists: the new list is compared with the last one seen
  (`kv["deadlines.shown"]`) by source row. New or moved means a push; gone means silence; a copy of the same task on
  the same date from another source is not news; a rename is not news either.
- Silent are: everything on the first comparison of the list, on the first HSE App X poll and on the first RUZ poll,
  and changes made by the owner (HA, the web app, Claude).
- A change of `RUZ_ICAL_URL` (the link's hash differs from the stored one) makes the next poll with classes the first
  one, and the old classes' midterms are withdrawn silently.
- A manual dictionary correction renames `<abbr> Midterm` at once, without waiting for a RUZ poll.
- The comparison runs after every batch run, RUZ and HSE App X poll, owner action, and at midnight.
- A sheet without a due date has an assumed one, its course's next seminar (§6.3, stage 12): a `tg` row with
  `assumed` set, marked as assumed in the web app and its pushes, and `assumed: true` in `GET /api/deadlines`. It
  follows its class like a Midterm and gives way to a real date.

### 6.5 Timetable and the iCal feed

- The feed holds classes from RUZ plus consultations and events from chats (D2).
- Titles are English, from the dictionary (`title`). The first line of `DESCRIPTION` is the type: *Lecture*,
  *Seminar*, *Consultation*, *Event*, *Interim assessment*. `LOCATION` is the room or a link. This way Tablo's
  current `ical.py` reads the feed unchanged (§4.2, §4.4).
  In `main` and `optional` a class's title also ends with its type ("Calculus · Seminar"); `tablo` keeps the bare
  title (D46).
- An event from a chat that only repeats a class (a regular kind during a class of the same course) is not an
  event; an event whose day is told without its time is an all-day one (D46).
- The `UID` is stable: classes take it from RUZ; events from chats use a hash of the chat and the id of the first
  message. Moving an event changes the time and keeps the `UID`.
- A cancellation or move of a class announced in a chat **is applied** to the RUZ class; Tablo did not do this
  (§4.5). A cancellation becomes `STATUS:CANCELLED`, a move a new time with a note in `DESCRIPTION`. If RUZ later
  changes the class itself, RUZ wins.
- **Two calendars** (D28):
  - `main`: classes and everything one has to attend (a consultation before an exam, a test, a submission);
  - `optional`: where one may go: optional consultations, events, meetings.

  The model decides where an event goes during batch parsing (the `required` field in `events`). A mistake can be
  fixed on the event's card in the web app (§8.6).
- **The feed for Tablo** is a third one, `tablo`: both calendars together, with marks on every event:
  - optional ones: `STATUS:TENTATIVE` and `X-INFOMASTER-OPTIONAL:TRUE`;
  - the source: `X-INFOMASTER-SOURCE`: `ruz`, `tg`, `mail`, `pdf` (for an icon, as with deadlines, §5.6);
  - a link to the original message: `X-INFOMASTER-LINK`. Not `URL`: the current `ical.py` treats an event with a
    `URL` as an online class (§4.2).

  The current `ical.py` skips only `CANCELLED`, so it reads `TENTATIVE` as an ordinary class and ignores the `X-`
  fields: the feed works in Tablo without changes. Showing the marks needs a small change (the checklist in
  [decisions.md](decisions.md)).
- The feeds are served by a secret link inside the internal network only (§8).

**Stage 4 details:**

- Classes with `STATUS:CANCELLED` in RUZ itself are not skipped, as in Tablo, but stored with a mark: in the feed
  they are `CANCELLED` (Tablo still skips them), and Timetable gets a cancellation.
- A class gone from RUZ before it starts was removed by RUZ: it leaves the feeds, and Timetable gets "removed". Gone
  after it started means it left the five-week window, and it is kept as it was. New classes beyond the old end of
  the feed are just the window moving and are not reported. An empty feed while future classes exist counts as a RUZ
  failure. An empty feed while there are no classes yet (between terms) is not a "first poll": "RUZ connected" comes
  once, with the first feed that has classes. The status (`ruz.classes`) holds the number of classes of the last feed
  received, without stored past ones.
- The RUZ link is a secret: a poll error reaches the status, the log and the pushes without it (an HTTP error as the code
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

### 6.6 Summaries (removed)

Stage 7 made a daily summary of chats for a Summaries topic and the `/summary` command. They were removed in the
second round (stage 11, D55): what matters reaches the news and the deadlines anyway, and the summaries were one more
thing to read. The code, the topic, the command, the web app screen, the `summary_time` setting and the chats'
`summary` switch are gone. Old data stays where it is and is never read: the saved settings rows, the
`messages.summarized_at` column and the `summary` rows of the LLM journal, which the web app still shows by task.

### 6.7 Grades

Unchanged from §3: tab layout by Haiku with a cache, a 0–10 scale, a snapshot on the first poll, placeholder zeros,
a journal of new grades. New grades go to the API and the course lenses of the web app; they are not pushed (D55).
Zeros differ from Tablo's (D72): a read keeps, for each column of the own row, the share of the other students with
points above zero there (only the share, no one's grade); a zero counts as the student's own once half of them have
points, is a placeholder while none do, and is «being graded» in between. In a stream's sheet with a «Группа» column
the others are the student's own group (D80).
A read also keeps the own row's formulas and the teachers' comments on its cells (17.6, D75), never another row's
comments or their authors; from the header it keeps only the cells those formulas read. The formulas are counted by
`processing/formulas.py` (the part of Excel the gradebooks use) for the grade card and the calculator, and Haiku
(task `grades`) writes each in a line, once for a sheet while its formulas stay.

**Stage 8 details:**

- Code: `collectors/grades.py` (downloading the xlsx, the sheet title, the top of the tab and the own row,
  `TabView`), `processing/grades.py` (the Haiku layout with a cache, the scale, new grades, settings). The label
  budget (23 characters for "course task") and the layout rules are
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
  `grades_window_days` (7). They are edited in «Machine → Gradebooks»; sheets are read right after an edit and every
  30 minutes after that. `include_baseline` was not carried over: the lens shows all current grades with the new ones
  marked.
- Differences from Tablo. No own row in any tab is an error of the sheet, not an empty list: grades are not erased.
  A grade no longer in the sheet (the tab hidden, the row gone from one tab, the cell cleared) is remembered as gone
  (`grade_items.gone`): coming back with the same grade is not news, with a different one it is news with the
  previous grade. A column shifted by an insertion before it is the same task (the tab, header and grade match one of
  those gone in the same read), not a new grade. A task the model named twice counts once. The layout answer gets at
  least 60 tokens per column (a tab of dates is not cut off). An answer the model cannot give (a refusal, a cut-off)
  is cached as a skipped tab so as not to pay for it every 30 minutes, and is visible in the status (`unread`) and
  «Machine»; a network failure or the limit is a sheet error, the grades stay, and the tab is laid out on the next
  poll. Removing a sheet or changing the name erases its grades: the next read is a snapshot again. A closed sheet (a
  login page instead of an xlsx) is a "not a workbook" error.
- **Dropbox sheets (stage 15).** Some courses share the grade table as a Dropbox link to an `.xlsx` file. A link in
  `grades_sheets` is either a Google Sheet or a Dropbox share link to a file:
  `https://www.dropbox.com/scl/fi/<id>/<name>.xlsx?rlkey=…&dl=0`, the older
  `https://www.dropbox.com/s/<id>/<name>.xlsx?dl=0`, or the same paths on `dl.dropboxusercontent.com`. The sheet's id
  is `dropbox-<id>` (the same file on both hosts is one sheet). The file is downloaded by the link's path on
  `dl.dropboxusercontent.com` with `dl=1` (`rlkey` and the other parameters kept, redirects followed): some resolvers
  block `www.dropbox.com`, and `dropbox.com` redirects there. The title is the file name without the
  extension, URL-decoded, or the name in `Content-Disposition` when the answer has one (a renamed file). Everything
  after the download is the same as for a Google Sheet; a deleted or private link answers with an HTML page, which is
  a "not a workbook" error. A folder link (`/scl/fo/`, `/sh/`), a file that is not `.xlsx` and any other Dropbox link
  are refused when the sheet is added (in the app's language); a file that turns out not to be `.xlsx` by its
  `Content-Disposition` is an error in the sheet's status. Code: `settings.grade_sheet_id` and `dropbox_file` (the
  link), `collectors/grades.download_url` (the download).
- Stage 8 posted new grades to a Grades topic of the forum; the second round removed it (D55), and
  `grade_events.post_id` is left from it. A sheet that cannot be read for 12 h is pushed once (D69).
- `GET /api/grades` (internal port, Bearer) is level C of §11.5: `{"items": [{key, label, value, bonus, seen_at}],
  "window_days"}`, the last event within the window for every task, newest first. In `GET /api/status` → `grades`:
  `configured`, `sheets` by sheet id (`title`, `ok_at`, `error`, `tabs`, `items`, `unread`; neither links nor names),
  `llm`, `new`.

### 6.8 The library: raw material and the knowledge base

Stage 14 of §11.1 (D58), `infomaster/library/`. Kept for good under `data/library/`, apart from the 14 days of §9.

**Raw material** (`materials`). An item is a file (a photo of the board, a recording, slides, a sheet, any file), a
text (notes typed or pasted, a transcript) or a link (remembered, never fetched). It has a course, named as the
timetable names it (Schedule.same_course maps a chat's name to it), and a RUZ class, or neither: then it is in the
*inbox*, and Claude sorts it. An item that comes with a time finds its class: the course's last class that started
by 20 minutes after it, within a week. It is `new` until a published version of the knowledge base names it, then
`processed`. A transcript hangs on its recording (`parent`). A removed item is out of every list and comes back
with an undo for 30 days. The same bytes are stored once (`raw/<sha256[:2]>/<sha256>.<ext>`). A file arrives at a
`.part` path and is moved into place when complete; an item made for an upload link is a draft (`uploading`) until
its bytes come, and a draft older than a day is dropped.

**The knowledge base** (`knowledge`, `knowledge_versions`). Per course: a `book` (the whole course in one LaTeX
document), a `lecture` or a `seminar` per class (a seminar is its sheet's topics and new notions; its problems are
not solved), and a `cheatsheet` for Claude (notation, formatting, the topics covered so far; Markdown, no PDF).
A new version is a draft first: its source and its PDF are uploaded, then it is published and gets the next number.
Every version is kept; only the newest can be taken back (an undo). The server never runs TeX: the PDF comes built.

**Links** (`library/links.py`). A file goes in or out by a signed link that names one file and one direction:
`GET /library/<token>/<name>` (24 hours), `PUT /library/<token>` (6 hours), on the public port (the signature is the check). The key is kept in `kv`, so links survive a restart. Claude Code
uses them with `curl`; the web app opens a PDF by one (§8.6), a sheet's kept PDF too (`k`, D69).

**Sources** (`library/feed.py`). Besides uploads from the connector, three sources feed the library
on their own:
- *Study chats.* In a study chat the batch model (§6.1) also returns `materials`: messages that carry course
  material (a photo of the board or of notes, notes, slides, a recording, a link to one), each with its course as the
  dictionary names it (or "" when unsure), a kind and a short title; other chats are not asked, and their schema has
  no such field. There a photo or a recording without text goes to the model too, so it can be told. The materials
  are queued in the batch's transaction (`library_queue`) and taken after the run, outside it: a file is downloaded by
  the twin straight to disk, a link or a text is kept as it is. The class is the one the message was sent at; with no
  course the item goes to the inbox. A file the twin cannot download now (offline, a flood wait) stays queued and is
  tried again after the next batch run; any other failure is tried three times. A row goes when its item is made or
  when its message is deleted or leaves the inbox after its 14 days. A file over the size limit is skipped.
- *The web app* (§8.6, «+»). A photo of the board, a recording, a file, a note or a link of the owner (source
  `web`), at the class in progress or a recent one the owner picks, else in the inbox; Android's «Share» lands here
  too.
- *Sheets* (§6.3). When a sheet or a new version of it is prepared, its PDF is copied in as kind
  `sheet` (source `sheet`, its message as the origin), at the class of its course that was on when it was sent, else
  at its course alone; once per file.
- *The bot.* A private message of the owner to the bot that is no command goes to the inbox (source `bot`): a photo,
  a document, audio, a voice message or a video is downloaded by the bot itself; a text becomes a link (its first URL)
  or a text. A forward's origin is the author, the caption the title. Each message of an album is its own item; the
  bot answers once, quietly, in the app's language. Anyone else's messages are ignored. A PDF is first read as a
  sheet (D73, `processing/bot_sheets.py`): Haiku (task `pdf`) sees its name, its first page and the caption and says
  whether it is a sheet, with its course, number and due date. A sheet becomes an own sheet with the caption kept
  (`sheets.caption`), published at once and journaled with an undo, and reaches the library as kind `sheet`; the bot
  says so for each one. Anything else, or no answer from the model, goes to the inbox as above.

## 7. LLM: the journal and the daily limit

**The journal (D22).** Every call, failed ones included, is written to `llm_runs`: the time (UTC), the task (`batch`,
`pdf`, `image`, `subjects`, `grades`, `hseapp_labels`, `quick` (one line of the web app's «+», §8.6); old rows may
have `summary`, D55), the chat, the model,
tokens (input, output, cache), the cost, the duration, the error. «Machine → LLM» in the web app shows spending for today, the week and the month,
broken down by task and chat.

**The limit (D21).** A daily dollar limit for all calls together. Days are counted in Moscow time; prices are set in
the settings.

- **Before a call** the budget reserves the worst case: an input estimate from the text length plus `max_tokens` for
  the output. If the reserve does not fit in what is left, the call is not made. After the answer the reserve is
  replaced by the real cost.
- **80 % of the limit.** Optional tasks are switched off: images and whole-PDF reading. Nothing is pushed (D55).
- **100 %.** All calls stop until midnight. Messages pile up in the queue and are processed the next day. It is pushed: new
  deadlines stop too. «Machine → LLM» raises the limit for today.
- **Working through the queue after a pause** (D30):
  1. Order: study chats → mail → non-study chats and channels.
  2. Non-study messages older than `ttl_hours` (24 h) are marked processed without calling the model: their news
     would not be live anyway (as in Tablo, §6.3).
  3. Study messages are never thrown away: they hold deadlines and events.
- The limits on the number of images and on PDF pages and tokens work inside the common limit.

## 8. Outputs

### 8.1 Two HTTP ports

Only the **public port** is exposed through Funnel. The API for Tablo and HA sits on a separate port reachable only
on the local network, so neither the API nor Tablo's feed faces the internet. The `main` and `optional` feeds for
one's own calendar (Apple, Google) are open through Funnel as well, but under a different secret (D43): Google and
iCloud subscriptions fetch the feed from their own servers and cannot reach the internal port or the tailnet.

| Port | Paths | Protection |
|---|---|---|
| public (Funnel) | `/hseapp/callback`, `/calendar/<CALENDAR_PUBLIC_SECRET>/main.ics` and `optional.ics`, `/library/<token>` | Callback: the PKCE `state`. Feeds: the secret in the path; no `tablo.ics` here. Library links: their signature (§6.8) |
| public (Funnel) | `/mcp`, `/.well-known/oauth-protected-resource[/mcp]`, `/.well-known/oauth-authorization-server`, `/oauth/*` (the MCP connector, §8.5) | `/mcp`: an OAuth access token; the metadata is public; `/oauth/authorize` lets in only with a one-time code the bot sends to the owner |
| public (Funnel) | `/web/*` (the web app, §8.6) | `/web/api/*`: a session cookie, got with a code the bot sends to the owner or with a passkey; a change must come from the app's own origin. The page, its files and the service worker are public |
| internal | `/api/*`, `/calendar/<CALENDAR_SECRET>/main.ics`, `optional.ics`, `tablo.ics` | a Bearer token; the feeds use a secret in the path because calendars cannot send headers |

The secrets differ: a leaked link of one's own calendar is replaced without touching Tablo. Without
`CALENDAR_PUBLIC_SECRET` the public port has no feeds, and it may not equal `CALENDAR_SECRET`.

### 8.2 API for Tablo and HA (internal)

Where possible the answers repeat the format of Tablo's current API (§8), so that Tablo and HA need fewer changes.

| Method and path | What it serves |
|---|---|
| `GET /calendar/<secret>/main.ics`, `optional.ics`, `tablo.ics` | the feeds (§6.5) |
| `GET /api/deadlines` | the final list after merging and hiding, in screen order (§5.6), in Tablo's current format, plus `assumed` (§6.3) |
| `GET /api/news` | live news: `id`, `source`, `study`, `posted_at`, `full`, `short`, `importance` (replaced items are not served) |
| `GET /api/grades` | new grades within the window: `key`, `label`, `value`, `bonus`, `seen_at`, newest first |
| `GET /api/status` | per source: `ok_at`, `error`, plus `hseapp.login_needed`, today's LLM spending and the limit |

The API serves the full current state, with no cursors (D10). There is no "refresh now" endpoint (D5).

**Stage 10 details:**

- `GET /api/news` returns `{"items": [{id, source, study, posted_at, full, short, importance}], "ttl_hours"}`: live
  news in rank order, exactly what the web app's news show (`News.live`), without items replaced, withdrawn by the source
  or hidden. `id` is a stable number; `source` is `tg` or `mail`; `posted_at` is local time without a
  zone (§9). This is level B of §11.4 without `deadlines[]` and `replaced`: deadlines from news come already merged in
  `/api/deadlines`, and replaced items are not served. The lengths of `full` and `short` are checked in characters
  for now, not by the font (§2.1).
- `GET /api/status` needs Bearer, like all of `/api/*` (in Tablo the status was open). Keys are per source:
  `telegram`, `batches` (batch parsing: freshness of the news), `ruz`, `hseapp` (with `login_needed`), `mail`,
  `sheets`, `grades`, `images`, plus `llm` (`spent_usd`, `limit_usd`, `raised_usd`, `level`). There is
  no separate `news` key as in Tablo: `batches` plays its role.
- There are no write routes (`POST/PATCH/DELETE /api/deadlines` as in Tablo): own and hidden deadlines are managed in
  the web app and by Claude. infomaster is not connected to HA (D34): no to-do lists, no
  `tablo_changed` events, and the internal port is not published on the host. Only Tablo reads it, by service name
  on the `infomaster` network.
- **The move from Tablo** is the command `python -m infomaster import-tablo --tablo-db <tablo.db>`
  (`infomaster/tablo.py`), run once at the switch. From a read-only copy of Tablo's database it takes own deadlines
  (except those already present with the same title, date and list), hidden ones (by `uid`, which is computed the same
  way; they have no `ref`, so hiding is by title and date, as in Tablo) and the manual dictionary entries (a manual
  entry set by the owner wins; a Haiku answer gives way). Rows more than 30 days past their due date are skipped. If
  the list was seen already, the copied own deadlines are added to `kv["deadlines.shown"]` so that they are not
  pushed as new.

### 8.3 The bot

Until stage 16 the bot kept a forum group with topics (Timetable, Deadlines, News, Sheets, System), and a Mini App
in Telegram held the settings (§8.4). The web app (§8.6) took both places at step 16.10, and both are gone from the
code (D69). The bot (`outputs/bot.py`) keeps two jobs:

- it sends the owner the login codes of the web app and of the connector, in the private chat;
- it takes material for the library: a private message of the owner that is no command goes to the inbox (§6.8),
  and the bot says so, quietly.

`/start` answers with the web app's address (an old link of a forum post or of the calendar lands here too), and the
bot takes the Mini App's menu button away. Anyone else's messages are ignored. Notifications are Web Push (§8.6):
only a new or moved deadline, a reminder, a failure of a source or a login it needs, and the spent LLM limit (D55).

### 8.4 Mini App (removed)

The Mini App (D44) was removed at step 16.10 (D69). Its handlers stay in `infomaster/actions.py` as the owner's
actions: the one place of the rules, which the connector's tools call (`connector/tools.call_action`) and «Machine»
and the shelves of the web app call through the tools, or directly for what has no tool (a login, the LLM limit).
`actions.public_app` builds the public port with the services under their keys.

### 8.5 MCP connector (claude.ai)

Stage 13 of §11.1 (D53, D57). infomaster serves a remote MCP server on the public port, and the owner adds
`https://infomaster.<tailnet>.ts.net/mcp` to claude.ai as a custom connector (Customize → Connectors, any plan). The
same connector then works in the web, desktop and phone apps; Claude Code adds the same address. claude.ai connects
from Anthropic's cloud through Funnel. Claude reads what the web app shows and changes almost everything the web
app changes; infomaster keeps working without it (D53).

**Protocol** (`connector/mcp.py`). Streamable HTTP: `POST /mcp`, one JSON-RPC message per request, a plain JSON answer
(no SSE stream), no sessions; a notification gets 202, `GET` and `DELETE` get 405. The server is dual-era, as the
specification's versioning allows, because a client of either era may connect:

- a request whose `params._meta` carries `io.modelcontextprotocol/protocolVersion` is served by revision
  **2026-07-28**: the `MCP-Protocol-Version`, `Mcp-Method` and, for `tools/call`, `Mcp-Name` headers must match the
  body (else 400 with `HeaderMismatch`, -32020), another version is 400 with `UnsupportedProtocolVersion` (-32022)
  and the list, `server/discover` answers, results carry `resultType`, an unknown method is a 404;
- any other request is legacy (**2025-03-26** to **2025-11-25**): `initialize` agrees on the version, and since the
  server keeps no state and gives no `Mcp-Session-Id`, every request stands alone.

Methods: `server/discover`, `initialize`, `ping`, `tools/list`, `tools/call`. An `Origin` other than claude.ai,
claude.com or `PUBLIC_URL` is refused with 403. The subset is written directly in aiohttp (about 200 lines): the
official `mcp` package brings Starlette, uvicorn and pydantic, and the container has 256 MB. Measured: about 0.4 MB of
Python heap for the connector's modules, the tool registry and its listing; no change in RSS.

**Authorization** (`connector/oauth.py`). OAuth 2.1 with infomaster as its own authorization server, for one person:

| Path | What |
|---|---|
| `/.well-known/oauth-protected-resource/mcp` (also without `/mcp`) | RFC 9728: `resource` is `PUBLIC_URL/mcp`, the authorization server is `PUBLIC_URL` |
| `/.well-known/oauth-authorization-server` | RFC 8414: the endpoints, `S256` only, public clients only (`none`), `iss` in the answer (RFC 9207) |
| `POST /oauth/register` | Dynamic Client Registration (RFC 7591); only claude.ai's and claude.com's callback and loopback addresses on any port (Claude Code) are taken as redirect URIs |
| `/oauth/authorize` | the owner's page, see below; PKCE `S256` is required, the `resource` (RFC 8707) must name this server |
| `POST /oauth/token` | `authorization_code` with the verifier, `refresh_token` |
| `POST /oauth/revoke` | RFC 7009: a refresh token takes its connection with it |

A request to `/mcp` without a valid token gets 401 with `WWW-Authenticate: Bearer resource_metadata="…"`. Client ID
Metadata Documents are not advertised, so claude.ai registers itself. There are no scopes: one owner, one level of
access; the client's own approval of each change is the gate.

There is no password. The authorize page names the client and where the browser goes back to; **Send a code to
Telegram** makes the bot send a six-digit code to the owner's private chat (`TG_OWNER_ID`; the owner must have pressed
Start there once), and the owner types it in. Three wrong codes end the page's code; a page gets a code every 30
seconds at most and the bot sends at most 6 an hour and 12 a day (D79); a page and its code live 10 minutes. Then the browser goes back
with an authorization code (5 minutes, one use; used twice, it revokes the connection it gave). An access token lives
an hour; a refresh token rotates at each use (the one just used still works two minutes, for two refreshes in flight)
and lapses after 30 days unused. Tokens are kept as SHA-256 hashes (`connector_grants`, `connector_tokens`); the open
pages and the codes live in memory, so a restart only asks to sign in again.

**Tools** (`connector/catalog.py`). Each has a JSON Schema of its arguments (checked before the call, unknown fields
refused) and annotations: `readOnlyHint` for reads; `destructiveHint` for changes and removals, so that the client
asks the owner first (adding a deadline, an event, a gradebook or a grade, restoring a hidden deadline and "read the
gradebooks now" are not destructive). A change is the owner's own action: the tool calls its handler in `actions.py`,
with the same checks and the same side effects (the reminders rescheduled), so no rule lives twice.

| Reads | Writes |
|---|---|
| `get_status` (what needs attention, every source, the LLM spending), `list_deadlines`, `list_hidden_deadlines`, `get_timetable` (a range of days), `list_news`, `search_messages` (text, chat, days; 14 days are kept), `get_message` (as D50's page), `list_chats`, `list_chat_senders`, `list_courses`, `get_settings`, `list_sheets`, `get_sheet` (the text; a scan's first page as its JPEG while the file is kept), `list_grades`, `get_gradebook` | deadlines: `add_deadline` (with a course for the web app's lens), `edit_deadline`, `hide_deadline` (with a reason: done, not_mine, duplicate of `kept`), `delete_deadline`, `unhide_deadline`, `confirm_deadline_date` (an assumed date, §6.3), `set_deadline_course`; the timetable: `add_event`, `edit_event`, `set_event_options`, `remove_event`, `correct_class`; `edit_sheet`, `join_sheet` (a sheet becomes a newer version of another), `delete_sheet` (an own sheet; neither can be undone); `update_chat_settings`; the dictionary: `set_course`, `delete_course`; settings: `update_general_settings`, `update_reminders`, `update_images`, `update_pdf_limits`; grades: `update_grade_settings`, `add_gradebook`, `update_gradebook`, `remove_gradebook`, `read_gradebook_now`, `set_tab_layout`, `reset_tab_layout`, `add_grade`, `change_grade`, `delete_grade` |

Not exposed: the secrets of `.env`, the twin's login and logout, the HSE App X login, the LLM limit and the model
prices (a lower price would raise the limit too), anything of the summaries.

**Library tools** (`connector/library.py`, stage 14, D58). Registered when the public app holds the library
(`library.links.add_routes` runs before the connector attaches). They serve §6.8's processing from two kinds of
client: Claude Code on the owner's Mac, which has a shell, and claude.ai on the web or the phone, which has none.

| Reads | Writes |
|---|---|
| `list_library` (courses with counts, the inbox, disk usage), `list_materials` (the inbox, a course, a class; by status or kind; no long text, a 200-character preview), `get_material` (one item: its text, a text file's or a PDF's text, in pages of 30,000 characters by default; a photo inline; `download_url`; the items on it), `list_classes` (a course's started classes with counts and their documents), `list_documents`, `get_document` (versions; a version's source inline in pages, links to its source and PDF), `get_cheatsheet` | `add_material` (a text, a link or a transcript inline; or a file: a draft and its upload link), `sort_material` (course and class of up to 100 items; destructive), `update_material` (title, kind, author, note, status, a text's words; destructive), `remove_material` (one item or up to 100 as one change; destructive), `start_document_version` (not journaled), `publish_document_version` |

Files never travel inside a call (a recording is hundreds of megabytes, and a result is read by the model): a tool
gives a signed link (§6.8) and the answer carries the exact command, `curl --fail-with-body -T <file> '<link>'` for an
upload; `download_url` is fetched with `curl -o`. `add_material` with `file_name` makes a draft item (`uploading`)
and returns its upload link; the item is listed once the bytes are in. `start_document_version` makes a draft
version, takes the source inline (`source_text`: a cheat sheet, or anything from a client without a shell) and
returns an upload link for each part still missing (the source, the PDF built locally); `publish_document_version`
makes it the newest version and marks the items it names processed. A client without a shell reads text inline
(texts, transcripts, a PDF's text layer) and a photo as an image when it is a JPEG, PNG, GIF or WebP of at most
3.75 MB, whose base64 stays within the 5 MB the Messages API takes of one image; a bigger photo (a camera's full
JPEG, a HEIC) is given by its link only. Starting a version is not journaled: a draft is seen by no one and is
dropped after a day, and the change is the publish. The undos: adding removes the item, sorting puts each item back
where it was, a change puts the old values back, removing restores the item and its transcripts (30 days), and
publishing takes the version back, deletes its files and makes the items it marked processed new again. A seminar's
document is its sheet's topics and new notions; the tools' descriptions say its problems are not solved.

**Journal and undo** (`connector/changes.py`). Every write through the connector is a row of `connector_changes`:
the time, the connection, the tool, its arguments, one line about it, and the undo steps, which are calls of tools
(and of a few steps no client sees, like putting Haiku's dictionary entry back as it was) worked out from the state
before the change. «Machine → Claude connector» in the web app shows the address to copy, the connections
with **Revoke**, and the last 50 changes with **Undo**. An undo runs at the time it is asked for: when what it acts on
has changed since (a deadline its source dropped), it fails, keeps the error and changes nothing more. Some changes
say what an undo does not bring back (removing a gradebook forgets its grades, own corrections and tab layouts), and
some cannot be undone at all ("read now"). The last 200 changes are kept. The web app's changes go into the same
journal through the same tools (`via` web, §8.6), and «Machine → Journal» lists both.

**Prompt injection.** Claude reads messages strangers wrote. Tool results are JSON, never prose; a result that
carries chat text or what a model made of it has an `untrusted` field saying that `text`, `full`, `short`, `summary`,
`title` and `note` are data, not instructions, and the server's instructions say to change nothing because a message
asks for it. The other guards are the client's approval of destructive tools, the journal with undo, and that only
the owner can connect.

### 8.6 Web app

Stage 16 of §11.1 (D61). A home-screen app (PWA) at `https://infomaster.<tailnet>.ts.net/web/` that took the place
of the forum and the Mini App (D69). It lives in `infomaster/web/`: the server side next to the connector on the public
port, the client in `web/static/`.

**Client.** Plain ES modules and an import map, no build: Preact and its hooks and htm in `static/vendor/` (with
their licences), Lora and Nunito Sans in `static/fonts/` (Latin and Cyrillic subsets, SIL OFL), the look «Paper» in
`app.css` with light and dark colours after the system, both languages in `i18n.js` (D41). The service worker
(`/web/sw.js`, scope `/web/`) fetches the app's own files from the network first and falls back to its cache, so an
update shows on the next start and the app opens without a connection; `/web/api` is never cached. The manifest
makes it installable; on iOS, notifications need the app on the Home Screen, and the app says so.

**Login** (`web/auth.py`). Only the owner:

- the first login on a device is a six-digit code the bot sends to the owner's private chat on request (a new code
  at most every 30 seconds, 6 an hour and 12 a day; 3 wrong tries drop it; it lives 10 minutes (D79));
- a logged-in device may add a passkey: discoverable, with user verification, bound to the host of `PUBLIC_URL`.
  Later logins use it with no name and no code. If the Funnel name changes, passkeys stop working and the code still
  does;
- a session is a random token in a cookie on `/web` (HttpOnly, Secure, SameSite=Strict, 90 days); the database keeps
  its SHA-256 (`web_sessions`), and a session unused for 90 days, or a year after its login, is gone; the bot tells
  the owner when a passkey is added (D79). **Machine → Login and devices** lists the
  sessions and passkeys, ends a session elsewhere, removes a passkey, adds one and logs out;
- a request under `/web/api` that changes something must carry the app's own `Origin`; codes and WebAuthn
  challenges live in memory, so a restart only asks for a new code;
- every response under `/web` has a CSP that runs only the app's own files and its import map, no framing and no
  type sniffing; the library's files (`/library/<token>`) never run on this origin (D79).

**Reading** (`web/views.py`, step 16.2). A course is known by its key, the dictionary's abbreviation of its name
(§6.2), or the name until the dictionary has one: classes, events and deadlines name their course each in their own
way, and the key is what they share, so a course's lens is one filter over all of them.

- `GET /web/api/feed?course=`: the class going on or next today, the overdue deadlines, then every day with a class
  in the next 14 days or a deadline however far, and the lenses (courses with a class in the last month or the next
  two, the soonest first). Deadlines have a date and no time: a study deadline due on a day its course has a class
  is shown at that class (a seminar first), since it is handed in there; the others close their day;
- `GET /web/api/week?start=&course=`: the seven days of a week, past ones too;
- `GET /web/api/deadline?uid=`: a deadline's card: its course, the class it is handed in at, the message it came
  from while it is kept (D29) with its link, and its history (found, every move, from which chat or letter);
- `GET /web/api/class?uid=`: a class's or an event's card: its course, the deadlines handed in at it, the message
  that changed it, how much material the library has for it;
- `GET /web/api/course?key=`: a course's lens: the next class, the grades of its gradebooks (a plain average of the
  works on the 0–10 scale, not a formal grade), its knowledge base documents with links, the counts of its sheets and
  raw material.

**Changes** (step 16.3, D62). The owner's changes are the connector's write tools (§8.5), called through
`Connector.act`: the same checks as Claude's calls, and every change goes into the connector's journal (`via` web)
with the steps that undo it. After a change a note at the top of the screen offers **Undo** for 10 seconds.

- `POST /web/api/deadlines/{action}` `{uid, ...}`: `done` and `not-mine` hide a source's deadline with that reason
  (`hidden_deadlines.reason`; HA, Tablo and the merge see every reason the same, as hidden), and delete an own one;
  `edit` `{title?, due?, category?}` is the owner's edit (a source's deadline becomes an own copy; a sheet's
  assumed date given another one is its real date, §6.3); `merge` `{keep}` hides `uid` as a duplicate of `keep`
  (`reason` duplicate, `kept`); `confirm` makes an assumed date real; `course` `{course: a lens key}` sets the course
  of a deadline from a chat, a letter or RUZ;
- `GET /web/api/check?course=`: the «Check» queue (`web/check.py`), only what the service is unsure of, the soonest
  first, upcoming deadlines only: an assumed date (answers: «Right», another date); a likely duplicate, two deadlines
  of one course the merge kept apart that are the same work on dates up to a week apart, or carry the same number
  («Sheet 12» and «HW 12»), never two of HSE App X or two Midterms (answers: «Merge», keeping a real date over an
  assumed one, then the more reliable source as the merge orders them; «They are different»); a deadline from a
  chat, a letter or RUZ without a course (answers: a course of the lenses, «Outside the courses»). The feed carries
  the same questions in `check`;
- `POST /web/api/check/put-aside` `{id}`: «They are different», «Outside the courses»: the question is kept in
  `web_checked` and not asked again, until the change is undone. Its id holds the deadlines' uids, which change with
  a title or a date, so a changed deadline is asked about anew;
- `POST /web/api/changes/{id}/undo`: the undo of a journaled change.

**Notifications** (step 16.4, D63). Web Push to every device that turns it on; `web/push.py` sends, `web/notify.py`
decides what:

- the notifier is the service's only notification (D55, D69): a new or moved deadline (`DeadlineWatch.notify`, a
  withdrawn one is not pushed), the reminders of the evening before and the morning (`DeadlineReminders.notify`), a
  failure of a source (an `Alert`: the twin, HSE App X, mail, RUZ, the wiki, a grade sheet) or a login it needs and the daily LLM limit spent (not the 80 % warning). A
  message is {title, body, url, tag}: the url is the app's screen (a deadline's card, the feed, «Machine»), and a
  deadline's messages share a tag, so a move replaces its news on the device;
- a subscription belongs to a web session (`web_push`): ending the session, or 90 days without it, ends the
  device's notifications. Its endpoint must be https on Apple's, Google's, Mozilla's or Microsoft's push service.
  A subscription the service answers 404 or 410 to is deleted, and so is one that fails 10 times in a row;
- the server's VAPID key pair (P-256) is made at the first start and kept in `kv["web.vapid"]`; its subject is
  `PUBLIC_URL`. The messages are encrypted for each device (aes128gcm, RFC 8291, checked against the RFC's example)
  and carry an ES256 token (RFC 8292). There is no push library: pywebpush would bring the synchronous `requests`;
- `GET /web/api/push` (the public key, whether this device gets notifications, how many do), `POST` `{subscription}`,
  `DELETE`, `POST /web/api/push/test` (to this device only). **Machine → Notifications** turns them on and off and
  sends a test; the feed offers them once on a device without them (except an iPhone outside the Home Screen, where
  the app says to add it first). The service worker shows a message and, on a tap, brings the app to its screen.

**«+»** (step 16.5, D64, `web/quick.py`). One button for everything added by hand:

- material for the library (D58, source `web`): a photo of the board (the camera), a recording, any files, a note or a
  link. It goes to the class in progress: one that starts within 20 minutes, is on, or ended within 90 minutes;
  «Another» lists the classes of the last week, and «No class» sends it to the library's inbox. Files are written to
  disk as they come (`POST /web/api/add/files`, multipart: `class_uid` or `course` first, then the files; a refused
  file stops the upload and the ones before it stay). Every addition is journaled with its undo (`remove_material`);
- one line (`POST /web/api/add/line`): Haiku (task `quick`, within the daily limit; no key or the limit spent: «add
  by hand») is given today, the lenses with their other names and the classes of the next three weeks, and answers
  what the line is (a deadline, an event or a note), its course among the lenses, the date, and a sentence on how it
  read the date. The server checks the answer (an unknown course or a bad date is dropped; a deadline without a date
  is a note) and nothing is saved before the owner confirms: «Save», «No, save it as a note», or «Edit» into the
  form. Deadlines keep a date only, so a time the line gives stays in the title («Lab 3, 18:00»);
- by hand (`POST /web/api/add/deadline|event|grade`): the connector's `add_deadline`, `add_event`, `add_grade`. An own
  deadline may now have a course (`own_deadlines.course`, its key), so it shows in the course's lens; an edited copy
  of a source's deadline keeps the source's course;
- Android's «Share» (the manifest's `share_target`, a POST to `/web/share`): the service worker keeps the files and
  the text in a cache of its own and opens «+», where the owner picks the class; a link waits as a note. Without the
  worker (the app never opened) the server only sends the app to «+» and asks to share again.

**«Machine»** (step 16.6, D65, `web/machine.py`), behind the status dot of the feed, which turns red when something
needs the owner (`problems` in the feed):

- `GET /web/api/machine`: what needs attention, each with the screen that fixes it (none for what only `.env` and the
  logs fix), and the sources with their freshness: the twin, the mail, RUZ, HSE App X, the gradebooks, the wiki pages,
  the batches, the images and the LLM limit, each ok, late (its last good read is older than its poll allows: the
  mail 30 minutes, HSE App X an hour, the gradebooks two, RUZ three, the wiki nine), failing, off or waiting for a
  login, with the time of that read and a count;
- `GET /web/api/machine/journal`: the connector's journal, the owner's changes in the app and Claude's through the
  connector in one list, the newest 100, each with its undo (`POST /web/api/changes/{id}/undo`);
- the Mini App's settings, moved over. The screens read through the owner's actions (`actions.py`), as Claude does; a
  change goes through the connector's write tool, journaled with its undo: a chat's settings
  (`PATCH /machine/chats/{id}`), the batches, news, about me and the wiki pages, the reminders, whole PDFs, images and
  the notifications (`PATCH /machine/settings/{group}`), the dictionary (`PUT /machine/courses`,
  `POST /machine/courses/remove`), the gradebooks, their names and the layouts of their tabs
  (`/machine/grades...`). What has no tool is the owner's alone and is not journaled: the twin's login
  (`POST /machine/twin/login|cancel|logout`), HSE App X (`/machine/hseapp...`; the callback page now sends the owner
  back to infomaster), the LLM limit and its raise for today (`/machine/llm`), reading a gradebook now and revoking a
  connection of Claude's (`/machine/connector`). The model prices stay in the code's defaults;
- notifications: which kinds come (`push_kinds`: new and moved deadlines, reminders, failures and logins, the LLM
  limit) and the quiet hours (`push_quiet`, Moscow time, over midnight too), when a message comes without a sound or
  vibration rather than not at all. The connector's `update_notifications` changes them.

**News** (step 16.7, D66): a third view next to the list and the week.

- `GET /web/api/news`: the live news in rank order (`importance + study`, the newer first), the same items as Tablo's
  `/api/news`, each with where it came from (a chat by its name, a letter by its subject while
  it is kept) and when; `ttl_hours` says how long news lives. The News view carries no count;
- `GET /web/api/news/{id}`: an item with the message it came from (while it is kept, D29) and its link where the
  chat has one;
- `POST /web/api/news/{id}/hide`: the connector's `hide_news`, journaled; its undo shows the item again, and the items
  that replaced it since.

**Shelves** (step 16.8, D67, `web/shelves.py`): the problem sheets, the library's raw material and its inbox, behind
the books button in the feed's header (a mark counts the inbox) and the course lens's «Sheets» and «Raw» shelves:

- `GET /web/api/shelf`: what the entry shows: the sheets' count, the inbox's count and the courses with material;
- `GET /web/api/shelf/sheets[?course=<key>]`: the sheets of a course (a lens's key) or all of them, each with its
  versions' count, due date (marked when it is a guess), source and state (not read yet, failed);
  `GET /shelf/sheets/{id}`: a sheet with every version, its summary, what each version changed and its PDF, and the
  sheets it may become a newer version of. A version's PDF opens by the library's copy (made when the sheet was read,
  kept for good, a signed link), else by the file the sheets keep for 14 days (a signed link, D69), else it is shown as
  gone;
- changes of a sheet: `PATCH /shelf/sheets/{id}` (course as a key, number, due, summary: `edit_sheet`, with its undo),
  `POST /shelf/sheets/{id}/join {previous}` (`join_sheet`), `DELETE /shelf/sheets/{id}` (`delete_sheet`, an own sheet
  only); the last two have no undo, and the journal says why. `POST /shelf/sheets` (multipart: course, number, due and
  the PDF) adds an own sheet and journals it with the undo that deletes it;
- raw material: `GET /shelf/raw?course=<key>` (a course's classes with their items, and the items of no class),
  `GET /shelf/inbox` (what has no course, and the courses to sort it into), `GET /shelf/classes?course=<key>` (where
  an item can go), `GET /shelf/items/{id}` (the whole text, its class, a signed link to its file, the items on it);
- sorting and changing: `POST /shelf/sort {ids, class_uid | course}` (`sort_material`; `course: null` puts the items
  back in the inbox), `PATCH /shelf/items/{id}` (title, note, status: `update_material`), `DELETE /shelf/items/{id}`
  (`remove_material`); all journaled with their undo. Without the library the feed has no mark and the library's
  screens answer 404, the sheets work.

**Screens**: the feed with the lenses and the «Check, please» box (the first two questions with their quick answer),
the course's lens (grades, next class, shelves), the week as a grid with the deadlines of each day above it, an
item's card over them with «Done», «Edit», «Not mine» (an own one: «Delete»), «It is a duplicate…» (the course's
deadlines within two weeks to pick the one that stays) and, for an assumed date, «The date is right», and the «Check»
queue one question at a time («Check · 1 of 3», «Later» puts one off for the visit). «Machine» with the sources, what
needs attention, the journal and the settings (16.6), «+» (16.5), the news (16.7) and the shelves of sheets and raw
material with the inbox (16.8), and the corrections (16.9). Since the switch (16.10, D69) the app is the only
interface besides Claude and Tablo.

**The switch** (step 16.10, D69). The forum and the Mini App are removed (§8.3, §8.4): the bot only sends login
codes and takes material. What their topics did besides posting stays: `DeadlineWatch` compares the list with the one
seen last and pushes a new or moved deadline, the reminders are pushed, and a sheet is published without a message.
What came with it:
- the lens's «Knowledge» shelf links every published version of a document (`documents[].versions`: version, date,
  note, link), the newest first;
- «Machine» shows the library's disk and the free space (`library` in `GET /machine`, `Library.usage`);
- a sheet's PDF the sheets keep for 14 days opens by a signed link (`/library/<token>/<name>`, payload `k`: its hash),
  so it opens outside the session, as Safari does for an app on the iPhone's Home Screen; the session's route
  (`GET /shelf/sheets/{id}/file`) stays for a port without the links;
- the calendar's «Edit» link is the entry's card (`/web/#/c?uid=…`), which quotes the source message.

**Corrections** (step 16.9, D68, `web/fixes.py`, client `fixes.js`): the card of a class or an event carries `fix`,
what stands behind it with every field as it is now (a class: RUZ's time and room, the owner's correction, the chat's
correction and whether it is ignored; an event: its id, own or an own copy, its calendar). The changes, each the
connector's tool with its undo:
- `POST /fix/class {uid, start, end, room, cancelled, kind, required, title?}` (`correct_class`): moved, cancelled or
  put in the optional calendar; the app sends every field, as the tool resets the ones it is not given;
- `POST /fix/event {uid, some of title, kind, start, end, room, cancelled}` (`edit_event`; a chat's event is hidden
  and an own copy stands for it, so the card closes);
- `POST /fix/options {id, required?, hidden?}` (`set_event_options`): an event hidden, shown again or put in the
  other calendar, a chat's correction of a class ignored or applied again;
- `POST /fix/remove {id}` (`remove_event`): an own event deleted, an own correction or an own copy taken back («As it
  was»);
- `GET /fix/hidden` (the hidden deadlines with their reason, the hidden events from today on), `POST /fix/hidden/deadline
  {uid}` (`unhide_deadline`); a hidden event is restored by `/fix/options`. «Machine → Hidden» shows them;
- `POST /fix/grade {id, value, label?}` (`change_grade`), `POST /fix/grade/reset {id}` (`delete_grade`: a sheet's grade
  as the sheet has it, one added by hand deleted); the lens lists the grades with their ids, the ones added by hand too.

**Deleting and finding** (stage 17, D70). A deadline's and an event's card end with one «Delete»: an own item is
deleted, a source's one is hidden (`not-mine`; `/fix/options {hidden}`), and «Machine → Hidden» brings it back. A
class keeps «Mark cancelled». `POST /shelf/remove {ids}` deletes several items of the inbox as one change
(`remove_material` with `ids`). The lens is «All» and a «Courses» button that lists the courses in a card; its tiles
open `#/docs?course=`, `#/sheets?course=`, `#/grades?course=` and `#/raw?course=` (the average opens the grades), each
a screen with a way back and no views bar, and the shelf of all sheets is grouped by the course key. The course's
`grades` (D71) list every work its gradebooks lay out, from the tabs' views, with `raw`, `max`, `bonus`, `parts` (the
own row's cells between the work before it and its column), `pending` (a zero no grade stands behind: in no average)
and `checking` (pending while part of the group is graded, D72),
then the grades added by hand, and `books` with each gradebook's summary tabs; the tile and `#/grades` read it alike. A card over the
screen closes when pulled down from its top (`app.js`), and the «Machine» dot is grey while infomaster does not answer
(`MachineDot`).

**Icons of kinds** (stage 17.4, D73). A row's mark says what it is: a class by its type (lecture, seminar, lab,
class, interim assessment, exam, consultation, event, talk), a deadline by an hourglass, an item of the library by
its kind (photo, audio, video, notes, slides, sheet, link, text, file). `mark(kind)` in `common.js` draws them as line
icons in the colours the dot and the diamond had. A sheet's card shows the caption it came to the bot with.

**Routes and «Back»** (stage 17.8, D77). «Back» returns to the screen the owner came from since the last top screen
(the feed, a course, the week, the news), else to the screen's parent; cards over a screen do not count. «Machine →
Course wiki» is a screen of its own; a route may name a section to scroll to (`?at=images`).

**A sheet's number or title** (stage 17.7, D76). The sheet's form has one field «Number or title»: a number shows as
«№ 3», a title (a sheet without a number, «Test 1 problems») as it is. The note on top after an action has a
cross and goes when swiped up or aside.

**A grade in full, and the calculator** (stage 17.6, D75). A work's card opens as a view: the points of its problems
(a problem with a comment marked), the teachers' comments on the own row's cells (the work's own first, then each
problem's under its number), and how it is counted (`how`: Haiku's line in the owner's language, else the formula
with the names of its cells, «SUM(1а…2*) − HW 1*»); «Change», a small button, opens the grade's field. A part is
named without a header row of numbers only (a maximum), and a column left of the student's name is no part. The
summary tabs show each cell's comment and how it is counted. A gradebook whose own row has formulas has a
«Calculator»: `POST /course/calc {key, book, target?, values: {cell: number|null}, solve?: {cell, goal}}` gives the
targets (every formula cell of the own row, the course's grade by default), the target counted with the given values
over the sheet's, its inputs (the works and the hand-filled cells it is counted from, a formula that is no work
followed through), and with `solve` the least value of one input in [0, its maximum] for a goal, or none. Nothing is
saved. The lens bar has no cross: «All» goes back from a course.

## 9. Storage (SQLite)

Times are local Moscow time without a zone, except `llm_runs.at` (UTC), as in Tablo (§9).

| Table | Purpose |
|---|---|
| `kv` | collector state, HSE App X tokens, layout and label caches |
| `settings`, `chat_settings` | common and per-chat settings |
| `chats` | all chats of the account: id, title, type, last read id |
| `messages` | raw messages and letters: source, chat, author, time, text, files, `parsed_at` (`summarized_at` is left from the summaries, D55) |
| `files` | downloaded PDFs and images: message, kind, hash, path, the page count and first-page text of a PDF, the reading of an image, the error |
| `subjects` | the dictionary (§2.2) |
| `news` | parsed news, `replaced_by` |
| `deadlines` | deadlines from external sources, `moved_by` |
| `own_deadlines`, `hidden_deadlines` | own and hidden ones (§5.4); an own one may have a course (its key, from the web app); a hidden one keeps why, when the web app says (`reason`: done, not_mine, duplicate of `kept`) |
| `events` | events from chats (consultations, events) and changes to RUZ classes; own events and changes have the source `own` (D44) |
| `classes` | classes from RUZ |
| `sheets` | problem sheets: message, file hash, course, number, due date, text, summary, previous version, the bot's message id |
| `grade_sheets`, `grade_items`, `grade_events` | grades (§3.6); `kv` holds the view of every tab after a read and the own tab layouts (D44) |
| `own_grades` | own grades and corrections of sheet grades (D44) |
| `llm_runs` | the LLM journal |
| `connector_clients`, `connector_grants`, `connector_tokens` | the MCP connector's OAuth (§8.5): registered clients, the connections the owner allowed with their refresh token, access tokens; tokens as SHA-256 |
| `connector_changes` | the journal of changes made through the connector or the web app (`via`), with their undo steps (§8.5) |
| `web_sessions`, `web_passkeys` | the web app's login (§8.6): a session per device with the SHA-256 of its token, the owner's passkeys (COSE public key, sign count) |
| `web_checked` | the «Check» questions the owner put aside (§8.6) |
| `web_push` | the devices that get notifications (§8.6): a web session, its push endpoint and keys, failures in a row |

## 10. Settings and secrets

- **Settings** are stored in the database and edited in the web app's «Machine» (or by Claude). There is no config file for the user.
- **Secrets** live in `.env` (listed in `.gitignore`), except those that appear at run time: the HSE App X tokens and
  the twin's session are kept in `data/`, which is never committed.

| Variable | Purpose |
|---|---|
| `TG_API_ID`, `TG_API_HASH` | the Telegram application for the twin (my.telegram.org) |
| `TG_BOT_TOKEN` | the bot |
| `TG_OWNER_ID` | the owner's Telegram id, the only one the bot lets in and sends login codes to |
| `ANTHROPIC_API_KEY` | Haiku |
| `MAIL_USER`, `MAIL_PASSWORD` | IMAP |
| `RUZ_ICAL_URL` | the RUZ feed |
| `API_TOKEN` | Bearer for the internal API |
| `CALENDAR_SECRET` | the path secret of the feeds on the internal port |
| `CALENDAR_PUBLIC_SECRET` | another secret: the `main` and `optional` feeds for one's own calendar through Funnel (D43) |
| `PUBLIC_URL` | the Funnel address of the public port (`https://infomaster.<tailnet>.ts.net`): the web app, the HSE App X callback, the library's links |
| `TS_AUTHKEY` | the personal tailnet's key for the sidecar, needed only on the first start |
| `CONTAINER_DNS_1`, `CONTAINER_DNS_2` | DNS servers of the containers (`compose.yaml`) |
| `HTTP_PUBLIC_PORT`, `HTTP_INTERNAL_PORT` | ports inside the container, 8080 and 8081 by default |

**The twin's session** (`*.session`) gives full access to the account. It lives only on the server, and only the
owner can read the file.

**Other people's data.** The model gets messages only from enabled chats, and for chats with the `important` filter
only the selected ones. From grade sheets it gets the top of the tab (up to 15 rows above the own row, other
students' rows included, so that the model finds the header itself, §6.7) and the own row without the owner's name.
Tablo sent only the header and the own row (§1); it was decided that classmates' grades seen by the model do no
harm. Raw messages and downloaded files are deleted 14 days after the message or its last edit (D29, D74); parsed news, deadlines, events and sheets
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
7. **Summaries.** Daily and `/summary`, the Summaries screen in the Mini App. Removed in stage 11 (D55).
8. **Grades.** The port of §3.
9. **Images.** Reading images with limits.
10. **API for Tablo** and the one-step switch of Tablo (D35, done 22.10.2026) following "Handed over to Tablo" in
    [decisions.md](decisions.md).

Stage 10 could be moved earlier piece by piece. For example, the iCal feed could be plugged into Tablo right after
stage 4.

### 11.1 Second round (from 19.11.2026)

The service was meant to take work off the student, but it sends too much and has to be checked all the time. The
second round makes it quiet by default and lets Claude in when the owner wants it. infomaster keeps working on its
own; Claude is an optional door, not the brain (D53).

11. **Quiet** (done, D55). Only deadlines notify: new and moved deadlines, the reminders of D47, and source failures. Grades,
    news, sheets and the timetable arrive silently or not at all: grades are a section in Tablo and the Mini App,
    news is a small block in Tablo, and the separate "important news" messages go. Summaries (stage 7) are removed
    entirely: what matters reaches the news and the deadlines anyway.
12. **Sheets** (done, D56, D59). A sheet without a due date is due before the next seminar of its course in RUZ, marked as
    assumed. About half of the sheets were missed because they never came as a PDF of a chat: they come behind
    links and as letters' attachments now, and a failed download or answer is tried again before it is given up.
13. **MCP connector** (done, D57). infomaster serves a remote MCP server on the public port, added to claude.ai as a custom
    connector (it works on the Pro plan and in the phone app). Tools read deadlines, the timetable, grades, news,
    messages, sheets and the knowledge base, upload files, and change almost every setting the Mini App changes.
    Not exposed: secrets, the twin's and HSE App X's logins, raising the LLM limit. Tools that change or delete are
    marked destructive so the client asks first, and every change made through the connector goes to a journal
    with an undo in the Mini App: Claude reads chat messages written by strangers. Built as §8.5 (D57); the
    knowledge base and uploads came with stage 14 (`connector/library.py`).
14. **Raw material and the knowledge base** (done, §6.8, D58). Two stores per course. *Raw*: an inbox (Haiku was not sure of the
    course) and the course's classes by RUZ date with photos of the board, audio, notes of the owner and classmates,
    slides, links (remembered, not downloaded); each item is new or processed. Sources: study chats (Haiku decides
    during batch parsing), files forwarded to the bot, uploads. *Knowledge base*: the whole course as one LaTeX book,
    a PDF and `.tex` per lecture and seminar (a seminar is its sheet's topics and new notions, the problems are not
    solved), and a cheat sheet for Claude (notation, formatting, topics covered); every version is kept. Processing
    is manual, from Claude Code on the owner's machine through the connector: it sorts the inbox, transcribes audio,
    writes the LaTeX, builds the PDF and uploads both. The server neither transcribes nor runs TeX.
15. **New sources** (done, D54, D60). Sheets and homework from course wiki pages; grade sheets shared as Dropbox
    links.
16. **An app instead of the forum** (done, §8.6, D61, D69). A home-screen web app, built anew, with its own login and
    Web Push for deadlines, reminders and failures. Then the forum is switched off; the bot stays to receive
    forwarded files and to send login codes. In steps:
    - 16.1 the frame and the login (done): the page, manifest, icons, fonts and service worker; a code from the bot,
      then a passkey; sessions per device;
    - 16.2 reading (done): one feed of classes and deadlines with the class now, the week, an item's card with its
      source and history, a course's lens with its grades, next class and knowledge base;
    - 16.3 actions (done, D62): «Done» and «Not mine» as two reasons of a hidden deadline (Tablo and HA still see
      both as hidden), editing, merging duplicates by hand, confirming an assumed date, a course for a deadline
      without one; the «Check» queue; every change journaled with an undo;
    - 16.4 Web Push (done, D63): subscriptions per device and one notifier for what the forum announces now (new
      and moved deadlines, reminders, failures, the LLM budget, a login the twin or HSE App X needs);
    - 16.5 «+» (done, D64): uploads to the class in progress, one line parsed by Haiku within the budget and
      confirmed, a deadline, an event or a grade by hand, Share Target on Android;
    - 16.6 «Machine» (done, D65): the sources and their freshness, what needs attention, the journal with undo, the
      Mini App's settings moved over, the notifications' kinds and quiet hours;
    - 16.7 news (done, D66): the live news in rank order as a third view next to the list and the week, an item's
      card with its message, «Hide» with undo;
    - 16.8 the shelves (done, D67): problem sheets with their PDFs and versions, raw material by course, sorting the
      library's inbox; changes are the connector's tools, two of them new (`join_sheet`, `delete_sheet`);
    - 16.9 corrections (done, D68): moving, cancelling and changing a class, hiding a chat's event, the main or
      optional calendar, the hidden deadlines and events with «Restore», correcting grades;
    - 16.10 the switch (done, D69): every version of a knowledge document with its own link, the library's disk in
      «Machine», a signed link to a sheet's kept PDF, the calendar's links to the app's cards; then the forum and the
      Mini App are removed from the code, the bot stays for login codes and material, and the deadlines' news, the
      reminders and the sheets go on without posting.
17. **After the first days on the phone** (D70). The owner's notes from the iPhone, in rounds:
    - 17.1 what misled (done): a dark icon, the «Machine» dot with a state for no connection, the «Check» queue's
      fixes, cards that close when pulled down, no views bar under the shelves, the header below the iOS blur;
    - 17.2 deleting and finding (done): one «Delete» on a deadline's and an event's card, «Mark cancelled» for a
      class, deleting several items of the library's inbox, a «Courses» button for the lens, a screen per lens tile,
      the sheets by course;
    - 17.3 grades (done, D71): every work of the course's gradebooks with its points, bonus and the points of its
      parts, a zero real once half the group is graded (D72), «not graded yet» or «being graded» otherwise and
      outside the average, the summary tabs, the gradebooks' state and actions on the course's grades;
    - 17.4 own icons and a sheet from the bot (done, D73): an icon for each kind of class, a deadline and each
      kind of the library's items; a PDF sent to the bot that Haiku reads as a sheet is an own sheet with its
      caption, the rest goes to the inbox.
    - 17.5 an old message's edit (done, D74): a post edited after its 14 days is stored again and read as an edit,
      its 14 days counted from the edit.
    - 17.6 a grade in full (done, D75): the teachers' comments on the own row and the own row's formulas read from
      the gradebooks, a work's card as a view with its problems, comments and how it is counted, the edit behind a
      small button, and a calculator of the course's grade with «How much is needed».
    - 17.7 a sheet's title and notes that go (done, D76): a sheet without a number takes a short title in the same
      field, shown as it is («№ 3» for a number); the note after an action closes with a cross or a swipe.
    - 17.8 routes (done, D77): «Back» returns to the screen the owner came from, the wiki pages have their own
      screen, the dead search button is gone.
    - 17.9 a pass over every screen's flows (done, D78): cards leave no history entry, the course carries through
      News and «+», confirmations where there is no undo, labelled forms, the week never strands the owner.
    - 17.10 a security review (done, D79): library files never run on the app's origin and the app has a CSP, fewer
      guesses of the bot's codes, small bodies before anyone is known, fetches to public addresses only, memory bounds
      for PDFs, letters, grade sheets and formulas, only trusted study sources move what others told.

No agent runs on the server: when the code needs changing, Claude Code connects over SSH.

## 12. Open questions

None now. The last one, the Tailscale sidecar next to the host's Tailscale, was settled at deployment: the sidecar
joined the same tailnet (D31) and Funnel answers, with nothing changed for the host. One thing showed up: when the
host sends its traffic through a VPN tunnel, the sidecar needs a direct path past it (a fixed address in the
`infomaster` network, `compose.yaml`), or its relay and certificate connections break.
