# infomaster decisions

A living record of what was decided and what must not be forgotten in the integration with Tablo. Code comments and
the other documents refer to these decisions as D1, D2 and so on. Tablo itself is described in
[sources-overview.md](sources-overview.md).

## Decisions

- **D1. The boundary with Tablo.** infomaster does all the computing (parsing, translation, merging duplicates,
  picking what is new). Tablo receives finished data (the timetable, deadlines, grades; news, see below) and only
  displays it.
- **D2. The timetable is an iCal feed in English.** infomaster builds the calendar itself: classes from the
  university portal, consultations and events from chats that one has to attend. The feed is served by a secret link
  and plugged into Tablo as an ordinary iCal source.
- **D3. The course dictionary lives here**, manual corrections included (`subjects` from the HA config). One
  dictionary serves every block (§2.2 of sources-overview.md).
- **D4. Merging duplicates happens here.** The rules of §5.5 of sources-overview.md move to infomaster as they are.
- **D5. No "refresh now" endpoint (`POST /refresh`) in the first version.** Telegram updates arrive in real time;
  everything else is polled on timers.
- **D6. The `@edu.hse.ru` mailbox (IMAP) is read here.** The collection logic is §6.2 of sources-overview.md.
- **D7. HSE App X is handled here**: the login (PKCE), storing and rotating the tokens, the login page (§5.1).
- **D8. Own and hidden deadlines live here.** ~~The HA integration fetches to-do lists from infomaster directly~~;
  HA no longer has to-do lists (D34).
- **D9. News is served finished**: `full`, `short`, `importance`, `study`, `posted_at`, the source and a replacement
  flag. The length check against the Inter 16 font is repeated here. Laying the column out (§6.6) stays in Tablo.
- **D10. The API serves the current state, not changes**, so no cursors are needed.
- **D11. Chats are chosen in the settings** from all chats of the account (the account is new and clean). Every chat
  has its own settings: enabled or not, study or not, whether to read images, a message filter.
- **D12. Haiku reads images.** A chat has a "read images" switch, plus a common daily limit on the number of images.
- **D13. Grades are handled here.** The sheets stay in Google Drive; we read and parse them (§3).
- **D14. Problem sheets** are PDFs with assignments from chats. They go to a separate shared forum topic, Sheets,
  and are stored only in Telegram.
  - **Recognition:** by the file name and the text of the first page.
  - **Publishing:** the bot posts the file itself with a caption: course, number, due date (if any), a link to the
    original message.
  - **A "read the content" switch.** Off: only the text of the first page is parsed. On: the model reads the file
    and writes a summary, within a limit (pages and tokens) so that a whole textbook is never read by accident.
  - **A new version** replaces the file in the bot's old message, and a "sheet updated" reply follows with, if
    reading is on, a short note of what changed (the texts are compared locally, only the difference goes to the
    model).
  - **What counts as a new version:** the same chat and course, plus the same sheet number or almost the same file
    name; the old sheet is at most 30 days old. When unsure, it is published as new; the Mini App can mark it as an
    update of an older one by hand.
  - **The default reading limit:** 10 pages and 30k tokens per file, whichever comes first. Configurable.
  - **Everything read from a PDF goes through the common processing**: deadlines to deadlines, study news to news,
    events to the calendar.
- **D15. Our own Telegram output is a forum group with topics** (grades, sheets, news, deadlines and so on) so that
  the sections do not get in each other's way. The bot (Bot API) writes there, not the twin.
- **D16. The only web interface is the Telegram Mini App**, with no domain of our own and no browser login. Login is
  by `initData` signed with the bot token; only the owner's Telegram id is let in. The public HTTPS address of the
  Mini App comes from Tailscale Funnel on a personal account (a permanent `*.ts.net` address, no domain needed; see
  D31). The HSE App X login callback works through the same address.
- **D17. Chat summaries**: automatically once a day and on the `/summary [chat] [period]` command (`3h`, `2d`,
  `20.10-22.10`; without a period, since the last summary; at most 7 days). A chat has a "summary" switch. It affects
  only the summary: what matters is extracted from all enabled chats regardless.
- **D18. Tablo calls us directly**: both services run on the same server, so no tunnel is needed.
- **D19. What matters is extracted in batches, not per message.** A common interval of N hours (3 by default) and an
  individual one per chat. Runs are tied to hours of the day (say, 8, 12, 16, 20), not to the start time. A chat has
  an "urgent" switch: a pin, a PDF or a message from people on a list starts processing of that chat at once.
  `/summary` does not wait for a batch.
- **D20. The daily summary is at 21:00**, configurable. The last scheduled batch runs before the summary.
- **D21. A daily LLM cost limit in dollars** for all calls together; the image and PDF limits work inside it. What
  happens when it is reached is in [architecture.md](architecture.md), §7.
- **D22. A journal of every LLM call**: each call, failed ones included, with the task, chat, tokens, cost and
  duration (like `llm_runs` in Tablo, §2.1).
- **D23. The News topic is a pin that the bot edits.** The news is the same that goes to Tablo (one table, the same
  rules of §6.6: live for 24 hours, without replaced items, ranked by `importance + study`), plus a link to the
  original message. There is no space limit: all live news is shown as `full` text; what does not fit in 4096
  characters spills into several messages. Screen limits apply only in Tablo.
- **D24. Notifications about important news**: for every new item with `importance` 4 or 5 the bot sends a short
  separate message and deletes it at the next rebuild of the pin.
- **D25. The stack is Python** (details in [architecture.md](architecture.md), §3).
- **D26. Deployment is Docker**, like Tablo, on the same server.
- **D27. The default daily LLM limit is $0.50**, configurable.
- **D28. Two calendars**: `main` (classes and wherever one has to be) and `optional` (where one may go). Tablo gets
  a combined feed with optional items marked (`STATUS:TENTATIVE`, `X-INFOMASTER-OPTIONAL:TRUE`).
- **D29. Raw messages are kept for 14 days.**
- **D30. The queue after the limit is exhausted**: study chats first, then mail, then the rest; non-study items older
  than 24 hours are skipped without the model; study items are never thrown away.
- **D31. Tailscale for the Mini App runs in a separate sidecar container.** ~~The host's Tailscale is a corporate
  VPN~~: it turned out to be the same personal tailnet (checked 22.10.2026). The sidecar stays: it joins that tailnet
  as a separate `infomaster` node with a tag, Funnel is allowed only for that tag, and the host's Tailscale is left
  alone. The host's internet VPN is AmneziaWG.
- **D32. The feed for Tablo marks the source of every event** (`X-INFOMASTER-SOURCE`) and a link to the original
  message (`X-INFOMASTER-LINK`).
- **D33. All settings leave HA.** Everything is configured in the Mini App. ~~HA keeps only the to-do lists~~; those
  go too (D34).
- **D34. The HA to-do lists ("Tablo · Study", "Tablo · Personal") are removed.** Own and hidden deadlines are kept
  only in the Mini App and with the forum buttons. HA is not connected to infomaster at all, and the internal port is
  not published on the host.
- **D35. Tablo moves to infomaster in one step, with a full stop**, without the two services running in parallel:
  the project is personal and not critical. HSE App X, mail, Telegram, news parsing, grades, the dictionary, own and
  hidden deadlines, merging and all of Haiku leave Tablo. What stays: the weather, the home block, the buttons,
  rendering, `ical.py` (it reads our `tablo.ics`) and a new "infomaster" source (deadlines, news, grades, status over
  the API).
- **D36. Optional events** (`X-INFOMASTER-OPTIONAL`) are shown in the classes block with a mark. A source icon
  (`X-INFOMASTER-SOURCE`) comes later, in an extended version of the screen.
- **D37. Failures are shown per block**, from infomaster's `GET /api/status` (deadlines: `hseapp`, classes: `ruz`,
  news: `batches`, grades: `grades`), with Tablo's thresholds unchanged. Tablo also gets its own "infomaster is
  unavailable" icon; the data shown is the last saved.
- **D38. The HSE App X login** is an HA notification, as before, but it links to the bot (`t.me/<bot>?start=hseapp`)
  instead of Tablo's `/hseapp` page.
- **D39. Text length after the move is checked in characters**; Tablo cuts the excess with "…". The check against
  Tablo's font (`pixelfont.py`) moves to infomaster later, as a separate task.
- **D40. LLM spending is viewed only in our Mini App.** The LLM sensors in HA and the LLM block on Tablo's
  diagnostics screen are removed without a replacement. The Mini App has its own LLM spending screen (§8.4): today,
  the week, the month, by task and by chat, the limit.
- **D41. Languages: English by default, Russian by a switch.** Two separate switches: the language of the Mini App
  (the interface, the errors and states from the service, the HSE App X login page) and the language of the forum
  (everything the bot writes, plus Haiku's texts for the forum: the daily summary, the sheet summary and "what
  changed"). `/summary` can name the language of the summary itself (`en`, `ru`). A switch applies only to what comes
  next: sent messages are not rewritten, pins change at their next edit. News, deadline labels, course names and
  iCal are always in English, because Tablo receives them.
- **D42. The project was renamed to infomaster, 22.10.2026.** The old name is gone from the code and the
  configuration: the package `uni_master/` became `infomaster/` (`python -m infomaster`), the `pyproject.toml`
  package `uni-master` became `infomaster`, and so did the Compose service, image and container (the sidecar is
  `infomaster-tailscale`), the Tailscale node name and the Funnel address (`https://infomaster.<tailnet>.ts.net`),
  the Docker network (the internal address for Tablo is `http://infomaster:8081`) and the database
  (`data/uni-master.db` became `data/infomaster.db`). The iCal fields `X-UNI-SOURCE`, `X-UNI-LINK`, `X-UNI-OPTIONAL`
  became `X-INFOMASTER-SOURCE`, `X-INFOMASTER-LINK`, `X-INFOMASTER-OPTIONAL`, and the event UID domain `@uni-master`
  became `@infomaster`. The Tailscale tag is `tag:infomaster`. In Tablo (`tablo-t75`) too: the source `unimaster`
  became `infomaster` (the module, the config section, the database keys, the name in `tablo.refresh_data`), and
  `TABLO_UNIMASTER_TOKEN` became `TABLO_INFOMASTER_TOKEN`. The service had not been deployed yet, so no data had to
  move.
- **D43. Feeds for one's own calendar go through Funnel, by a separate secret, 22.10.2026.** This changes §8.1 of
  [architecture.md](architecture.md), where only the Mini App was exposed. `main.ics` and `optional.ics` are also
  served on the public port at `/calendar/<CALENDAR_PUBLIC_SECRET>/…`, so that Apple Calendar or Google Calendar can
  subscribe to them; `tablo.ics` stays internal. The secret is separate from `CALENDAR_SECRET`: a leaked link is
  replaced without touching Tablo. Tailscale-only access would not do: Google and iCloud subscriptions fetch the feed
  from their own servers.
- **D44. The Mini App was reworked, and the owner gets their own events, grades and sheets, 22.10.2026.** This
  changes §8.4 of [architecture.md](architecture.md). Four tabs (deadlines, timetable, study, settings); lists only
  show, actions live in a card opened by a tap, switches live only in the settings. The timetable shows the same as
  the feeds (`GET /app/api/agenda`). What the owner gets, and how it lives next to the sources:
  - **Own events and class changes** are stored in `events` with the source `own`. An own change to a class wins over
    chats' changes until it is cancelled or RUZ changes the class itself (then it goes, like chats' changes, §6.5);
    chats' changes stay underneath and apply again after a cancellation. Editing an event from a chat hides it and
    puts an own copy in its place, as with deadlines (§5.4): the chat's next message changes only the hidden one.
    Batch processing does not move own items, and a chat message cannot replace them (since D46 the model sees own
    events, so a chat does not repeat them).
  - **Own grades** (`own_grades`) are grades added by hand and corrections of grades from sheets. A correction holds
    until the sheet itself changes that grade. Both go to Tablo through `GET /api/grades` as new grades and are not
    posted to the Grades topic: the owner set them.
  - **A grade sheet** gets its own name (`grades_sheets[].name`, for the Mini App only) and a screen with its layout:
    after every read `kv` holds the view of the tabs (the header and each column's value in the owner's row; no
    other rows) and the layout. An own tab layout overrides Haiku's answer, whatever happens to the header, until it
    is handed back to Haiku.
  - **Sheets:** editing the course, number, due date and description edits the message caption in Sheets; an own
    sheet is a PDF from the Mini App (up to 20 MB, "chat" −1) that goes to the forum like a sheet from a chat and is
    deleted together with its message. Only own sheets can be deleted.
  - **Deadlines** in the Mini App show the chat or letter they came from, with a link to the message.
- **D45. A letter or a chat moves HSE App X's date, 31.10.2026.** This changes §5.5, where the same work on different
  dates stayed twice. A deadline of HSE App X leaves the list when mail or a chat has the same work (§5.5's rule) on
  another date at most 14 days away: the letter's date stands. It is the reason the 14-day window of completed work
  exists: extensions are announced in letters and chats, while HSE App X keeps the first date. RUZ and own deadlines
  do not move it.
- **D46. Chat events do not repeat classes; a day without a time is an all-day event; one's own calendar shows the
  class type, 09.11.2026.** This changes §6.5 and D44. A chat's event of a regular kind (lecture, seminar, lab,
  class) that starts while a class of the same course, title or room goes on only repeats the class: the batch does
  not save it, and the feeds leave out one saved earlier or one whose class came to RUZ later. A move of a class the
  model was not shown is ignored instead of becoming a new event. The model sees the owner's own events, marked
  "own", so a chat does not tell them again; it still never moves them. A message that tells the day of an event
  but not its time ("tomorrow a consultation in S301") gives an all-day event: its start is a date alone, the feeds
  give a DTSTART date, Tablo reads it as midnight. When the same day already has an event of the same type and course
  (or title) with a time, the message only adds to it, as a new version: its room, or, the other way round, its
  time (an own event is left as it is). In `main.ics` and `optional.ics` a class's SUMMARY ends with its
  type ("Calculus · Seminar", "Midterm" for an interim assessment), since a calendar app shows the title alone;
  `tablo.ics` keeps the bare title, which Tablo takes for the course name.
- **D47. Deadline reminders, 09.11.2026.** The «Дедлайны» topic gets a reminder twice a day, Moscow time: on the eve
  (`reminder_eve`, 20:00) the deadlines due tomorrow, in the morning (`reminder_day`, 09:00) those due today. One
  message per slot holds the day's deadlines as the merged list shows them (hidden and done ones are not there), each
  line with its source, and a button per deadline: «Hide» («Delete» for an own one) and «Restore». Nothing due, no
  message. Each slot is sent once (kv `deadlines.reminded`); the first start takes the slots that have come as sent,
  and a slot missed while the service was down is sent when it starts again the same day. On, off and the times are
  in the Mini App, «Settings → Deadline reminders». The pinned list shows each deadline's source too, as the news
  list does: the chat with a link to the message, the letter's subject, RUZ, HSE App X or "own".
- **D48. One's own calendar links each event to its source and its card, 09.11.2026.** In `main.ics` and
  `optional.ics` DESCRIPTION ends with "From: <chat> <link to the message>" (the letter's subject, "RUZ", "you" for an
  own one) and "Edit: <link>", a link the notes show as one tap; no URL, which iOS shows under Location as a web page
  card above the room. It is a t.me link with the
  start parameter `ev-<key>`, the key the first 12 hex digits of SHA-1 of the UID (a UID has '@' and dots, which a
  start parameter does not allow); the Mini App opens the Timetable tab and the card of the entry with that key. With
  a main Mini App set in @BotFather the link is `?startapp=` and opens it at once; without one it is `?start=` and the
  bot answers with the Mini App's button. `tablo.ics` has neither: Tablo takes an event with URL for an online class.
- **D49. One card and one form for every timetable entry, 09.11.2026.** This changes D44. A RUZ class, an event from a
  chat or a letter and an own event have the same card in the Mini App (when, where, what, calendar, from, and "was"
  when it is changed) and the same three actions: **Edit**, **As it was** (when the owner or a chat changed it) and
  **Remove**. The one form has title, type, date, all day, from, to, where, required and cancelled for all of them.
  Behind it: a class gets an own correction, which now also renames it (its title is kept only when it differs from
  the class's, "" otherwise, so the dictionary goes on naming it), gives it another type (else the class's own kind)
  or puts it in the optional calendar; it applies while it changes anything, and RUZ changing the class still ends
  it. A chat's event gets an own copy as before, which now remembers the event it stands in for (`events.copy_of`):
  **As it was** withdraws the copy and shows the chat's event again; the hidden list leaves out an event with a copy.
  **Remove** cancels a class (it stays struck out), hides a chat's event or an own copy, and deletes an own event.
- **D50. A source links to its message's page in the Mini App, 09.11.2026.** This changes §8.3 and D48. The twin
  reads chats your own account may not be in, so a t.me link to the message does not open for you. Every "where it
  came from" link (the news, deadlines and timetable topics, the reminders, the sheets, the summaries' points, the
  calendar's "From:", the Mini App's cards) now opens the message's page: `m-<chat_id>-<msg_id>` as a start parameter
  (the mailbox is chat 0), through the bot as the **Edit** link of D48 is. The page (`GET /app/api/message`) shows
  the chat or the letter's subject, the sender, the time, the text, the files' names and the message it answers, as
  the twin read it, and **Open in Telegram** with the t.me link. After its 14 days (D29) only the chat and the link
  are left. Before the bot has started the links are t.me as before; `X-INFOMASTER-LINK` for Tablo stays t.me.
- **D51. A batch sees a few earlier messages of its chat, 09.11.2026.** This changes §6.1. Haiku saw only the new
  messages, so «её перенесли на субботу» or «ок, в R405» right after an earlier batch meant nothing to it. Before the
  new messages the prompt now gives up to 6 earlier, already parsed messages of the same chat and forum topic, from
  12 hours before the first new one, each cut to 300 characters (sender, time, reply, files' names, text; not what
  images or PDFs showed). They are context only: they give no items, and a new message that answers one of them is shown as a reply
  to it instead of with a quote. Letters get none. About 2,000 characters more per call.
- **D52. The models know who the student is and what a chat is about, 09.11.2026.** This changes §6.1 and §6.6.
  The prompts said only "first year, Software Engineering", so «у 2 подгруппы пара отменена» meant for another
  subgroup came as the student's own cancellation, and «ДЗ до пятницы» in a course chat had no course. Two texts go
  to the batches and the summaries when they are set: **About you** (`about_me`, at most 500 characters: group,
  subgroup, English group, electives) in «Settings → Batches and news», and a chat's **Note for the model** (`note`,
  at most 300: its course, who is who) on its screen. A message meant only for other groups is at most importance 2
  and gives no deadlines or events; a task that names no course gets the course of the note. The batch model also
  sees the deadlines passed in the last 7 days instead of 3, marked "passed", so an extension after the date moves the
  deadline instead of adding a second one; the merge still keeps 3 days.
- **D53. A quiet service with Claude as an optional door, 19.11.2026.** The owner actually uses deadlines and looks
  at grades and news in Tablo; the rest of the forum went unread and made the service one more thing to check. So
  only deadlines notify, summaries are gone, and everything else waits until it is opened. Thinking stays with the
  service (Haiku through the API key for batch parsing, as before), and Claude on the owner's subscription is let in
  through an MCP connector when the owner wants it: questions, changes, homework help, turning raw lecture material
  into a LaTeX knowledge base. The plan is §11.1 of [architecture.md](architecture.md).
- **D54. Grade sheets may be Dropbox links, 19.11.2026.** This changes §6.7. Some courses share the grade table as a
  Dropbox link to an `.xlsx` file instead of a Google Sheet. Such a link (`/scl/fi/…`, the older `/s/…`, or
  `dl.dropboxusercontent.com`) is downloaded with `dl=1`, the other parameters kept and redirects followed, and read
  exactly like Google's export. Its id is `dropbox-<id of the link>`, its title the file name without the extension
  (or the name in `Content-Disposition`). Folder links and files that are not `.xlsx` are refused when the sheet is
  added; a deleted or private link gives the usual "not a workbook" error. Only public share links, no Dropbox API.
- **D55. Only deadlines notify; the summaries and the Grades topic are gone, 19.11.2026.** This changes §6.6, §6.7,
  §8.3, §8.4, D17, D20, D23 and D50, and is stage 11 of §11.1 (D53). The forum was one more thing to check: grades,
  important news, the timetable and the sheets all notified, and the summaries went unread. Now a post notifies only
  for a new or moved deadline, a deadline reminder (D47), a source failure in System (a source unavailable for 12
  hours, a login needed, a grade sheet that cannot be read) and the spent LLM limit, since new deadlines stop then. A
  collector marks such a line as an `Alert`. Everything else is sent silently: the timetable, the sheets, a withdrawn
  deadline, the System lines about a recovery, a login or a connected sheet, the 80 % warning; pins never notify. The
  Grades topic is removed: grades are in the Mini App's Study tab and in `GET /api/grades` for Tablo. The News topic
  keeps only its pinned list: the separate messages for items of importance 4–5 are gone, those left from before are
  deleted at the next rebuild, and with them went the only way to hide a news item. The summaries of stage 7 are
  removed entirely: the Summaries topic, the daily summary, `/summary`, the Mini App screen, the `summary_time`
  setting, the chats' `summary` switch and the `summary` LLM task, so D50's list of links loses the summaries'
  points. Old data stays and is never read: settings rows, `messages.summarized_at`, `grade_events.post_id`, and the
  `summary` rows of the LLM journal, which the Mini App still shows.
- **D56. A sheet without a due date is due at the next seminar, 19.11.2026.** This changes §6.3 and §6.4 and is the
  first half of stage 12 of §11.1. Many sheets give no date and are simply due at the next seminar, so they were
  missing from the deadlines. Now such a sheet gets an assumed date: the first seminar or practical class of its
  course (matched through the dictionary) that starts after its message, within 21 days, as the feeds show the
  timetable, cancelled classes skipped. A course without seminars takes its next class of any kind, but not an interim
  assessment or an exam. The date is a deadline of its own (`tg`, the sheet's message, `<abbr> sheet <n>`,
  `assumed`), marked "≈ … (assumed)" in the caption, the Deadlines pin, its messages and reminders and the Mini App;
  `GET /api/deadlines` has `assumed`. Like a Midterm, it follows its class until the class starts: it moves with it,
  and is withdrawn when no class is left. A real date replaces it and clears the mark: the sheet's pages, a later
  message (the batch model sees the deadline marked "assumed" and moves it), or the owner in the Mini App. A newer
  version takes the deadline over. Sheets from before this change keep no date.
- **D57. Claude comes in through an MCP connector that infomaster authorizes itself, 19.11.2026.** This adds §8.5,
  changes §8.1 and is stage 13 of §11.1. infomaster serves a remote MCP server at `/mcp` on the public port, written
  directly in aiohttp (the official package would bring Starlette, uvicorn and pydantic into a 256 MB container). It
  answers both revision 2026-07-28 (per-request `_meta`, no sessions) and the initialize-based 2025 revisions,
  statelessly, because a client of either era may connect. It is its own OAuth 2.1 authorization server for one
  person: Dynamic Client Registration limited to claude.ai's callback and loopback addresses, PKCE S256, hashed
  rotating refresh tokens. Instead of a password, the bot sends a one-time code to the owner's private chat. Tools act
  through the Mini App's handlers, so no rule lives twice. Changes are marked destructive so the client asks first,
  and every change is journaled with an undo in the Mini App (Settings → Claude connector), because Claude reads
  messages strangers wrote. Secrets, the twin's and HSE App X's logins, the LLM limit and the model prices stay the
  owner's alone.

- **D58. The library: raw material and a knowledge base apart, files moved by signed links, 20.11.2026.** This adds
  §6.8 and is stage 14 of §11.1. Raw material (photos of the board, recordings, notes of the owner and classmates,
  slides, sheets, links, texts) is kept for good, per course and per RUZ class; an item whose course is not known
  waits in the inbox, and Claude sorts it, not the owner. Links are remembered, never fetched. The knowledge base is
  what Claude makes of it: a book per course, a document per lecture and seminar, and a cheat sheet for Claude
  (notation, formatting, topics covered); every version is kept, as its source (`.tex`, Markdown for the cheat
  sheet) and a PDF. The owner reads only the knowledge base; the raw store is material for Claude. Processing is
  manual, from Claude Code on the owner's machine: it transcribes audio and builds the PDF there, because the server
  has neither the processor time nor TeX (the PDF: until D84). Files never travel inside a tool's call (a recording is hundreds of
  megabytes): a tool gives a signed link that expires, and the file goes by `curl`. No backups: the owner accepted
  losing the library with the disk.

- **D59. Sheets behind links and in letters, 20.11.2026.** This changes §6.3 and finishes stage 12 of §11.1. On live
  data (05.11–19.11) every PDF sent to a chat was found; the sheets missed never came as one. They came as a Dropbox
  link in a Smart LMS letter, a public folder or file on Yandex Disk, or a Google Classroom assignment. Public links
  of study chats and letters are now followed to the PDF behind them, and a letter's PDF attachment is kept when the
  letter arrives. Classroom and Smart LMS pages need a login and are not tried: their deadlines still come from the
  letter's text. Four places where a sheet could be lost are closed: a PDF sent with a generic type, an old PDF of a
  chat that is not study after a pause of the twin, a download Telegram refused once, and an answer of the model that
  failed once. Downloads and answers are tried three times before they are given up.

- **D60. A course wiki is read as notices in the mailbox, 20.11.2026.** This adds a source to §5.2 and finishes stage
  15 of §11.1. The discrete mathematics course posts its sheets and homework only on its wiki page, with due dates by
  group in a table. Instead of a parser of its own for every course, a new or changed item of the page becomes a
  letter from "Course wiki" with the link and the table row, and the batches read it like any letter: the PDF behind
  the link is fetched (D59), the model finds the sheet and takes the due date of the student's group from `about_me`
  (D52). So the group is configured nowhere new, and the sheets, deadlines, library and forum need no change. Pages
  are listed in the `wiki_pages` setting (Mini App, «Batches and news»; the connector).

- **D61. A home-screen web app takes the place of the forum and the Mini App, 20.11.2026.** This adds §8.6, changes
  §3 and §8.1 and is stage 16 of §11.1. The app is built anew, not from the Mini App, around one feed: classes and
  deadlines on one time axis, a course as a lens on that feed rather than a tab, a «Check» queue that holds only what
  the service is unsure of (an assumed date, a likely duplicate, an item without a course), one card per item with
  its source and history, a «Machine» screen behind the status dot for the sources, the journal of changes and the
  settings, and one «+» for everything added by hand. Its look is called «Paper»: Lora for dates and headings, Nunito
  Sans for text, terracotta for actions, light or dark after the system. infomaster serves it on the public port under
  `/web/` as plain ES modules: Preact and htm are vendored and the fonts are kept in the repository, so the image and
  CI need no Node, and the app makes no third-party requests and opens offline. Only the owner logs in. The first
  login on a device is a code the bot sends to the owner's chat, as for the connector (D57); the device may then add
  a passkey (WebAuthn, the `webauthn` package) for its later logins. A session belongs to one device, can be ended
  from another, and its token sits hashed in the database and in an HttpOnly, SameSite=Strict cookie. Notifications
  move to Web Push for new and moved deadlines, reminders and failures. iOS shows them only for an app added to the
  Home Screen, and without action buttons. Deadlines keep a date without a time, so the app shows times for classes
  only. The forum and the Mini App work until the last step of the stage, then the forum is switched off; the bot
  stays for forwarded files and login codes.

- **D62. The web app's changes are the connector's tools, and «Check» asks only what the service is unsure of,
  20.11.2026.** This adds to §8.6 and is step 16.3 of §11.1. The web app does not get handlers of its own for
  deadlines: it calls the connector's write tools (§8.5), so one set of checks serves the Mini App, Claude and the app,
  and every change lands in one journal (`connector_changes.via`) with the undo the tool worked out; the app offers it
  right after the change, and «Machine» will list the journal (16.6). A hidden deadline now keeps why: done, not mine,
  or a duplicate of the one that stays. The merge, HA and Tablo still see one thing, a hidden deadline, so nothing
  changes for them; the reason is for the owner and for later screens. The «Check» queue holds three questions: an
  assumed date, a likely duplicate and a deadline without a course. A likely duplicate is narrower than "the same
  course and date": two works of a course often fall on one day, so the pair must be the same work as the merge
  compares it, or carry the same number, on dates up to a week apart; two items of HSE App X or two Midterms are never
  asked about, those sources list each work once. A question put aside is remembered by the deadlines' uids, so it
  comes back when one of them changes. Two connector tools come with it, `confirm_deadline_date` and
  `set_deadline_course`, and `hide_deadline` takes the reason.

- **D63. Web Push is sent by infomaster itself, to the devices that ask, 20.11.2026.** This adds to §8.6 and is step
  16.4 of §11.1. The notifier is one object called where the forum posts, so the forum can go at 16.7 and the
  notifications stay; it pushes exactly what the forum announces with a sound (D55), nothing the forum posts quietly.
  A subscription is a device's, bound to its web session: logging a device out, here or from another device, ends
  its notifications, with no second list to keep. The encryption (RFC 8291) and VAPID (RFC 8292) are a page of code
  on `cryptography`, which `webauthn` needed already and is now named in the manifest: the push libraries for Python
  use the synchronous `requests`, and the service runs on aiohttp. The VAPID keys are made once and kept in the
  database, not in `.env`: there is nothing for the owner to set, and new keys would make every device subscribe
  again. The endpoints are limited to the known push services, so the server posts nowhere else. Per-kind settings
  (only deadlines, quiet hours) wait for «Machine» (16.6).

- **D64. «+» adds by hand in one place, and the model only proposes, 20.11.2026.** This adds to §8.6 and is step
  16.5 of §11.1. Material goes to the class in progress, as the photos of a board are taken during it and a recording
  ends with it; 90 minutes after the end still count, and any class of the last week can be picked. One line is read
  by Haiku because typing a date, a course and a list is the slow part on a phone, but a model misreads «by
  Wednesday» or a course's nickname now and then, so the app shows what it understood with a sentence on the date and
  saves nothing before the owner confirms; the server drops whatever the model may not decide (an unknown course, a
  date that is not one). It is not optional (D12): the owner asked for it, so it runs below the 80 % mark too and stops
  only at the limit. Own deadlines get a course, the key of a lens, so what the owner adds stands in its course's lens
  like the rest. A shared file goes through the service worker rather than to the server directly: the share is a POST
  from the system, and the owner should still pick its class.

- **D65. «Machine» reads through the Mini App and changes through the connector, 20.11.2026.** This adds to §8.6 and
  is step 16.6 of §11.1. The web app's settings are the Mini App's moved over, not written again: its screens read
  through the Mini App's handlers, so the two show the same while both run, and every change of a setting is a
  connector's write tool, so it is checked once and lands in the one journal with its undo, as the owner's changes of
  deadlines do (D62). The journal shows the owner's and Claude's changes together, marked who made them: the owner
  wants one place to see what changed and take it back. What no tool does stays the owner's and is not journaled: the
  twin's and HSE App X's logins, the LLM limit, revoking Claude. A source shows its freshness as the time of its last
  good read and is «late» when that read is older than its poll allows, so a source that stopped quietly shows before
  it fails loudly. Notifications get the settings D63 put off: which kinds come and the quiet hours. A message of the
  quiet hours comes silent rather than not at all, so a failure at night is still on the lock screen in the morning.
  The model prices are not moved: they change with a new model, in the code's defaults.

- **D66. The switch waits for everything the forum and the Mini App give, 20.11.2026.** This changes stage 16 of
  §11.1. At step 16.7, the switch, the app still lacked the news, the problem sheets with their files, the library's
  raw material and inbox, the corrections of classes and events, the hidden lists with their restore and the
  corrections of grades. Switching off the forum and the Mini App then would have taken these from the owner until
  they were rebuilt, so they come first, as steps 16.7–16.9, and the switch is the last step, 16.10. The news come
  first: a third view next to the list and the week, not items in the feed, since the feed is one time axis of
  classes and deadlines (D61) and news are ranked, not dated. The view shows exactly what Tablo and the forum's list
  show, by the same rules. «Hide» is a connector tool, `hide_news`, so it is journaled and can be undone, which the
  forum's button could not do.

- **D67. The shelves keep a sheet's PDF in the app and change everything through the connector, 20.11.2026.** This
  adds to §8.6 and is step 16.8 of §11.1. The forum gives a sheet's PDF as a file in its post, and nothing else serves
  it: the sheets keep the file 14 days. The post goes with the forum at 16.10, so the app opens a version's PDF by
  the library's copy first (every sheet is copied into the library when it is read, and that copy is kept), then by
  the kept file behind the session, and shows «file gone» when both are gone. The kept file's route needs the session, which a link opened
  outside the app may lack on an iPhone, so there only the library's copy is certain to open; a signed link for the
  fallback is left for the switch (§11.1, 16.10). A course has two names, the model's on a sheet and the timetable's in the
  library, so the app speaks the lenses' keys and turns a key into a name before a tool is called: a sheet edited to
  another course stands in that course's lens, and a sorted item in that course's raw shelf. Every change is a
  connector tool, so Claude sees the same ones: the existing `edit_sheet`, `sort_material`, `update_material` and
  `remove_material` come with their undo, and `join_sheet` and `delete_sheet` are new. Neither can be undone, as the
  forum's post is deleted or its file replaced; the journal says so, and the app asks before it does either. An own
  sheet's PDF is a multipart upload that no tool takes, so it is kept as the Mini App keeps it and journaled by hand,
  with the undo that deletes it. «New material» stays in «+» (D64): the shelves sort and change what is there.

- **D68. Corrections live on the card and send every field, 20.11.2026.** This adds to §8.6 and is step 16.9 of
  §11.1. A class or an event is corrected where it is seen, on its card, not on a timetable screen of its own as in the
  Mini App: the card brings what stands behind the entry (RUZ's own time, the owner's correction, the chat's one, an
  event's id and whether it is an own one or an own copy). `correct_class` and `edit_event` reset a field they are not
  given (a move without the room blanks it, one without `required` puts the class back in the main calendar), so the
  card carries every field as it is now and the app sends all of them with the owner's change. A chat's correction of
  a class is ignored or applied again rather than deleted, as `set_event_options` hides it and the class's Midterm
  follows. What is hidden has nowhere else to be seen, so «Machine» gets a «Hidden» screen with the deadlines and the
  events ahead and «Restore» on each; an event an own copy stands in for is left out, as «As it was» on the copy
  brings it back. Grades are corrected in the course's lens, which now lists the grades added by hand as well. A grade's
  course is a short label, as a gradebook's, so «+» gives it the lens's key rather than the course's title, which the
  Mini App would refuse when it is longer than ten characters. All of it goes through the connector's existing tools,
  journaled with their undo.

- **D69. The switch: the web app takes the forum's and the Mini App's place, and both leave the code, 21.11.2026.**
  This finishes stage 16 of §11.1 (step 16.10) and changes §1–§4, §6, §7, §8 and §10. First the app got what D67 left
  for the switch: every published version of a knowledge document with a link of its own in the course's lens, the
  library's disk in «Machine», and a signed link to a sheet's PDF the sheets keep for 14 days (`/library/<token>`, the
  library's links), so it opens in Safari too, where an app on the iPhone's Home Screen sends a link and the session
  is not shared. The calendar's «Edit» link opens the entry's card in the app (`/web/#/c?uid=…`); its source line
  keeps the message's own link, as the card quotes the message, so the Mini App's page of a message is not rebuilt.
  Then the forum and the Mini App are removed, not switched off: kept behind a switch they would be a second
  interface to keep right with every change, and only the owner uses the service. The bot keeps two jobs, the login
  codes and the material the owner sends it, and answers `/start` with the app's address. What the topics did besides
  posting stays: the deadlines are compared with the list seen last and a new or moved one is pushed
  (`DeadlineWatch`), the reminders are pushed, and a sheet is published without a message (`SheetPublisher`; the
  status keeps the name `posted`, so versions chain as before). The Mini App's handlers were the one place of the
  rules that the connector and «Machine» call, so they stay as `actions.py`, the owner's actions; its page, its
  initData check and its routes go, and the tests mount the actions on their old paths. The setting
  `miniapp_language` becomes `language` (the stored value moves once), `forum_language` and the `bot_posts` table
  are dropped, and `TG_FORUM_ID` is no longer read. Web Push is now the only notification channel and has not yet
  been seen on a real device, so the server keeps the old image until it has; a rollback is the previous commit.

- **D70. After the first days on the phone: plain deleting, a course picker, a page per shelf, 21.11.2026.**
  Stage 17 of §11.1, from the owner's notes on the iPhone. The first round (17.1) fixed what misled: the icon was
  light paper with a dark «i», and the dark Home Screen of iOS darkens a light icon so the letter sank into it, so the
  icon is dark with a light «i»; the «Machine» dot was green whenever the feed had not said otherwise, also with no
  connection, so it has three states (all works, needs attention, a grey ring for no answer) and the News screen
  reads it too; a guessed date saved unchanged is «Right»; in «The same?» either card can be the one that stays (a
  tap on it), since the one the server keeps is only its guess; «N more» unfolds the questions in place; a card over
  the screen closes when pulled down and the screen behind it no longer scrolls; the shelves' screens drop the views
  bar; the header starts below the band iOS blurs under the status bar. The second round (17.2): deleting was spread
  over «Not mine», «Hide» and «Delete» by where an item came from, and the owner found none of it, so a deadline's
  and an event's card end with one «Delete»: an own item is deleted, a source's one is hidden (a deadline with the
  reason `not_mine`, an event by `set_event_options`), and the note says where to bring it back; a timetable class
  cannot be deleted, so its «Cancel» says what it does («Mark cancelled», struck through in the timetable). The
  library's inbox deletes the checked items at once: `remove_material` takes `ids` as well as `id`, one change with
  one undo, as `sort_material` does. The row of course chips grew past the screen, so the lens is «All» and a
  «Courses» button with the list in a card. The lens's four tiles each open a screen of their own (the knowledge
  base and the grades are new ones), and the shelf of all sheets is grouped by course, by the course key each sheet
  is matched with, so two names of one course make one group.

- **D71. A course's grades in full, in place of its gradebooks, 21.11.2026.** Stage 17.3 of §11.1. The lens showed one
  number per work, read from `grade_items`, and the owner wanted the gradebooks themselves no longer needed. The view
  every read keeps of a tab (D44: the own row's cells under their headers, the works laid out in them) already holds
  the rest, so the course's grades are built from it, one list for the lens's tile and the grades' screen, so their
  averages agree: every work in sheet order, tab by tab, with its points out of its maximum and its bonus; the points
  of its parts, the own row's cells between the work before it and its own column (a homework's problems, a test's
  tasks, a penalty), shown on a tap; the grades read, with the owner's changes over them, as before. A zero the sheet
  shows before any grade is still a placeholder (§6.7, Tablo's §3.6): it is listed «not graded yet» and counts in no
  average, and has no edit, since no grade stands behind it. A tab the layout skips that has cells (a summary,
  «Итоги») is shown as its headers and values. Each gradebook shows how its last read went, «Read now», its layout
  (Machine's screen, with the way back to the course) and the sheet; a gradebook is added from the course, its label
  the course's key. The app's static files now carry `Cache-Control: no-cache`: without it a browser kept an old
  module by its own guess after a deploy and ran the new page with it.

- **D72. A zero is real when the group's work is graded, 21.11.2026.** Stage 17.3 of §11.1, after D71 kept Tablo's
  rule that a zero before any grade is a placeholder: the owner saw the average ignore zeros. The real gradebooks
  showed why no rule on the own row can tell them apart: a homework missed (others graded, the own cell 0) and a
  homework not yet graded (every cell 0, made by the sheet's formula) look the same there, and the parts of a work
  are empty in both. What tells them apart is the column itself, so a read keeps, for each column of the own row,
  the share of the other students (rows whose name column holds a person's name) with points above zero; nothing
  else of another student is kept. A zero is the student's own once at least half of them have points
  (`GRADED_SHARE`): it is recorded, shown and counted, and a newly graded zero is a new grade like any other. With
  none it is a placeholder, «not graded yet». In between, a large stream graded group by group, it is «being graded»
  and counts in no average until most are graded.

- **D73. Icons for the kinds, and a sheet sent to the bot, 21.11.2026.** Stage 17.4 of §11.1. On the phone every
  class was the same dot and every deadline the same diamond, so a lecture and an exam looked alike until read: a
  row's mark is now a line icon of its kind, a class by its type, a deadline by an hourglass, a library item by its
  kind, in the colours the marks had (`mark` in `common.js`). Deadlines get one icon: they carry no kind, and a guess
  from the title would mislead. The owner sends the bot sheets the twin cannot see (forwarded from a chat it is not
  in, from a letter), and the bot put them in the library's inbox, where the sheets' rules never reached them. Every
  PDF the bot takes is now read as the batch reads a chat's (the file's name and first page, a scan as the page
  itself), with the owner's caption in place of the message, by one Haiku call (task `pdf`): a sheet becomes an own
  sheet with its course, number and due date, the caption kept with it (`sheets.caption`), shown on its card and given
  to the model that reads the sheet; it is published at once, journaled with an undo, and comes to the library as kind
  `sheet`, not to the inbox as well. Not every PDF is a sheet (slides, notes), so anything the model does not call
  one, and any PDF it could not be asked about (no key, the limit spent, a failure), goes to the inbox as before; the
  file is downloaded once either way. The same file as a sheet already is no new one, and the bot says so.

## Open questions

Collected in [architecture.md](architecture.md), §12.

## Handed over to Tablo

The move happens in one step (D35). First stop Tablo and copy the data (`import-tablo`, [setup.md](setup.md),
"Moving from Tablo"), then start infomaster with all sources, then the reworked Tablo. The work is done in the
`tablo-t75` repository. Done: Tablo was switched on 22.10.2026, and every item below is in place.

**The Tablo service (`service/tablo/`):**

- [x] **A new source, `sources/infomaster.py`.** Once a minute it calls `GET /api/deadlines`, `/api/news`,
  `/api/grades`, `/api/status` with infomaster's Bearer `API_TOKEN` at `http://infomaster:8081` (Tablo joins the
  `infomaster` network as an external one). The last successful answer is kept in `kv` and shown while infomaster
  is unavailable (§11.1).
- [x] **Deadlines** come as a finished list from `/api/deadlines` in screen order; Tablo only takes the first lines.
  Remove `todo.py`, the merge in `builder.py` (`merge_deadlines`, `drop_done`, `drop_hidden`) and the
  `/api/deadlines` routes.
- [x] **News** comes from `/api/news` (`full`, `short`, `importance`, `study`, `source`); the §6.6 layout stays.
  Remove `news.py` (Haiku parsing) and `sources/telegram.py`.
- [x] **Grades** come from `/api/grades` (`label`, `value`, `bonus`). Remove `sources/grades.py` and the layout
  detection.
- [x] **HSE App X and mail**: remove `sources/hseapp.py`, `sources/mail.py`, the `/hseapp` page,
  `/api/hseapp/login` and the callback; delete the HSE App X tokens from `kv`.
- [x] **The dictionary**: remove `subjects.py` and the manual entries from the config (`import-tablo` has already
  copied them).
- [x] **Haiku**: remove `llm.py`, `/api/llm/runs`, the `llm_runs` journal and the LLM block of the diagnostics
  screen, without a replacement (D40).
- [x] **Classes**: `ical.url` becomes `http://infomaster:8081/calendar/<CALENDAR_SECRET>/tablo.ics`. In `ical.py`:
  the title is `SUMMARY` as it is (already English, no dictionary); no more `<abbr> Midterm` from the feed (it comes
  in `/api/deadlines`); `STATUS:TENTATIVE` / `X-INFOMASTER-OPTIONAL:TRUE` becomes an optional mark (`ClassItem.mark`,
  a new value next to `moved` and `cancelled`, D36), shown with its own icon. A source icon
  (`X-INFOMASTER-SOURCE`) comes later.
- [x] **Failures per block** (D37) come from infomaster's `/api/status`, with the thresholds unchanged; the panel is told
  when infomaster goes and comes back (the "infomaster is unavailable" mark).
- [x] **Tablo's `GET /api/status`** takes `hseapp.login_needed` from infomaster's status (for the HA notification).
- [x] **`tablo.refresh_data`** polls infomaster and the weather again (infomaster has no "refresh now", D5).
- [x] **Config**: remove the `grades`, `hseapp`, `news`, `mail`, `subjects` sections; `ical` keeps the link. Add
  infomaster's address and token (the token goes in `.env`).
- [x] **Secrets**: remove `ANTHROPIC_API_KEY`, `TABLO_MAIL_USER`/`TABLO_MAIL_PASSWORD`, `TABLO_ICAL_URL` from Tablo's
  `.env` (mail and RUZ are now only here, as `MAIL_*` and `RUZ_ICAL_URL`).
- [x] **Texts on screen**: cut with "…" for now (D39).

**The HA integration (`ha-integration/`):**

- [x] Remove the `todo` platform (D34) and the `/api/deadlines` reads in the coordinator.
- [x] The Sources form keeps the weather coordinates and the iCal link; remove HSE App X, the full name, the sheets,
  the channels, the news lifetime, mail and the dictionary (D33).
- [x] The HSE App X login notification links to the bot (D38).
- [x] Remove the LLM sensors ("LLM run: …", "LLM cost today") without a replacement (D40).
- [x] Bring Tablo's documents (`docs/software/04`, `05`, `07`) in line with the new scheme.
- **D74. An old message's edit is news, 21.11.2026.** Stage 17.5 of §11.1. A channel's post edited weeks after it
  was posted (a moved due date) was lost: the twin dropped edits of messages past their 14 days (D29), since such a
  message would have been pruned at once. Now an enabled chat's message that is not stored, whatever its age and
  however long after its earlier edit, is stored on an edit whose `edit_date` is within the last 14 days, with
  `edited_at` set to it. Telegram also sends edits for reactions and link previews with the old `edit_date`: such an
  event brings back a message whose real edit the twin missed while offline, and never one last edited before its 14
  days. The 14 days of a message
  and of its file count from its last edit (`kept_from`), and so does the 24 hours of a non-study chat's news (D30).
  Its batch treats it as a message read before: what its earlier text gave and is still in force (news, deadlines,
  events, found by the message's id) is replaced or withdrawn, and the model sees the edit's time next to the
  message's, so a relative date counts from the edit. Edits made while the twin was offline are caught up only when such an event comes.

- **D75. A grade in full: comments, formulas and a calculator, 21.11.2026.** Stage 17.6 of §11.1. The owner wanted
  more of a grade than its points: the gradebooks hold the teachers' comments on the student's cells (why a problem
  lost points) and most of them the formulas that count a work from its problems and the course's grade from the
  works (0.35·Test 1 + 0.35·Test 2 + 0.3·homework, rounded; an exam weighted 0.5). Neither was read: openpyxl's
  read-only mode skips comments, and the values were read without formulas. A full load of the largest gradebook
  took more memory than the container has, so the read stays read-only: the formulas come in a second read-only
  pass over the own row, the comments from the file's comment parts, the own row's only and without the author. Of
  the rest of a tab only the header cells the formulas read are kept (a maximum, the stars of bonus problems); a
  formula reading another student's row is shown but not counted. A small evaluator of our own (no `eval`, no new
  dependency) counts the formulas as Excel does (`ROUND` half away from zero, `SUMIF` with wildcards, blanks in
  `AVERAGE` and `COUNT`); a function it does not know leaves that formula uncounted. Haiku writes each formula in a
  line for the card, since a chain of `IF(ISNUMBER(…))` written mechanically does not read; the mechanical form, with
  the names of the cells, stands when there is no answer. The calculator is the server's (one evaluator, tested):
  the course's grade by default (a formula cell on top of the most formulas, not a 0/1 flag; a header like «Итог»
  first), its works as inputs, a «what if» counted again on every change, and «How much is needed» by bisection over
  one input, which holds since a grade grows with its works. A new comment is not news. The grade card opens as a
  view, its edit behind a small «Change»; the lens bar lost its cross, which did what «All» does.

- **D76. A sheet's title instead of a number; notes that go, 21.11.2026.** Stage 17.7 of §11.1. Not every sheet has a
  number: a sheet of typical problems for a test could only be left without one. The sheet's `number` now holds its number or, without
  one, a short title (up to 60 characters, it was 20): one field, no new column, so the connector's `edit_sheet`, the
  journal and its undo stay as they were. Text that starts with a digit (after «№» or «#») is a number and shows as
  «№ 3», the deadline «Calc sheet 3», the library item «Sheet 3», as before; any other text shows as it is, the
  deadline «Calc: Test 1 problems». The batch gives such a title when a sheet has no number. The note on top after an
  action has a cross and goes when swiped up or aside, besides its 10 seconds. The empty due date of a sheet's form
  keeps the field's shape in Safari, with «Due date» beside it.

- **D77. «Back» returns where the owner came from; the wiki has its own screen, 21.11.2026.** Stage 17.8 of §11.1.
  The «Course wiki» row of «Machine» opened «Batches and news», where the wiki pages were the third section, below
  the screen: it looked like the wrong screen. The wiki pages now have a screen of their own (`#/machine/wiki`), and a
  route may name a section to scroll to (`#/machine/processing?at=images`). A pass over every route found «Back»
  fixed to a screen's parent wherever it was opened from: the library opened from «Machine» went back to the feed, a
  course's raw items opened from the shelves went back to the course. The client now keeps the screens the owner went
  through since the last top screen (the feed, a course, the week, the news; cards over a screen are not screens),
  and «Back» returns to the one before, the parent only for a link opened straight. The feed's search button, which
  never did anything, is gone; the news have the shelves' button as the feed has. An error of the wiki's own (its
  database down, `internal_api_error_*`) reads «the wiki's server failed», not as a Python exception.

- **D78. A pass over every screen's flows, 21.11.2026.** Stage 17.9 of §11.1. Every screen and card was traced in the
  code and walked on a local stand with made-up data at a phone's size. What changed:
  - Cards close without leaving themselves in the history: back to the screen under them when they were opened from
    it, else in place of their entry, so the phone's Back never opens a card again, a deleted thing's included.
  - A course carries through: the News view and «+» opened in a course keep it (the List returns to the course;
    «+» puts files on the course's shelf and fills the course in the deadline and grade forms).
  - The week never strands the owner: header and views bar while it loads or fails, «To this week» after paging,
    the day's deadline list reads again after a change. A class's correction keeps its card open and reads it again;
    an event's edit or removal still closes it (its copy has another uid).
  - One button per meaning: an own deadline has «Done» only (both deleted it); a dead «Next class» tile is no button;
    «Hide» only for live news; the feed's shelf button opens the inbox when it has something.
  - Confirmations where there is no undo or the loss is real: ending a session, logging out, removing a passkey (the
    last one says a code will be needed), revoking a connector, raising today's LLM limit, deleting an own grade,
    a dictionary entry, several inbox items or an item; «+» asks before dropping typed text.
  - Forms: labels above the fields of a sheet's edit, an end before the start not accepted, saving an unchanged sheet
    writes nothing; a field whose save was skipped shows the stored value again.
  - Smaller: course titles (not keys) in «Check», «Cancel» for «Other date», a Close while the queue loads, the class
    card's materials open its course's raw shelf, «Read now» follows the read up to a minute, the calculator recounts
    after a change, larger tap targets in the week and the grade edit, long file names wrap.
  Left as they are: overlapping classes in the week's grid, a «Hidden» link outside «Machine», days counted by the
  phone's clock in a card.
- **D79. A security review, 22.11.2026.** Stage 17.10 of §11.1. Nine reviewers went over the public port, the
  connector, the outbound fetches, the parsers, the model's answers, the client and the deployment; each finding was
  checked from three sides (can it be reached, does the code do it, what does it cost). What changed:
  - Files of the library are served as a safe image, audio, video or PDF, any text as plain text, the rest as a
    download, with `nosniff` and a sandbox CSP: a chat member chooses a file's type, and an HTML or SVG file opened
    inline ran next to the web app's session. The web app sends a CSP that runs only its own files and its import map
    (by hash), no framing, a same-origin referrer and `no-store` on the API.
  - The codes from the bot (the web app and the connector): three tries each, six codes an hour and twelve a day,
    counted before the bot is awaited. WebAuthn challenges are capped; a claude.ai redirect must be its MCP callback.
    A session ends a year after its login; the bot tells the owner when a passkey is added.
  - The public port reads at most 2 MB whole; uploads stream, /mcp reads its body after the token.
  - Links of chats and letters, wiki pages and grade sheets are fetched from public addresses only
    (`infomaster/net.py`: every host resolved and checked, on every redirect).
  - Memory within the 256 MB container: the twin checks a file's size of now before downloading; a sheet's PDF up
    to 20 MB, hashed in chunks, pypdf streams inflate to 20 MB; a letter over 15 MB is read by its headers, a poll
    holds 40 MB; a grade sheet up to 20 MB, refused when it unpacks past 120 MB, a tab read up to 5000×400; a
    formula's range up to 20000 cells, its nesting up to 64, odd arithmetic an error of the cell.
  - The model's answers: a letter from outside hse.ru (the wiki's own notices aside) adds but moves, cancels and
    replaces nothing; a chat that is no study one sees and changes only its own items; a withdrawn move gives back
    the deadline it moved; names in the prompt are quoted; fields are capped. The connector marks outside text and
    may not turn a chat on.
  - The HSE callback shows only an OAuth error code; the service's container does not hold the sidecar's key.
  - The service runs as a plain user whose id is `APP_UID` in `.env`, the host user that owns `./data`; the sidecar
    keeps its own state as itself.
  Left as they are: a cross-site POST to the share target (the service worker cannot tell it from Android's share),
  refresh-token reuse within its two minutes of grace, and the LLM budget a flood of messages can spend.

- **D80. A zero counts against the own group, 22.11.2026.** Amends D72. A homework's zero was dropped as «being
  graded» in a stream's gradebook where the student's group was graded and another group had not been yet: about
  half of the stream's rows had no points, so the share fell under `GRADED_SHARE`. When a column above the own row is
  headed «Группа» (exactly; «Действительная группа» is another column), each row belongs to the last group named at
  or above it, and the share is counted over the other students of the own group. Without such a column, or with no
  one else in the group, the whole tab counts as before. The zeros already shown keep their shares on the real
  gradebooks; a zero that now counts arrives as a new grade, as D72 says. gradebot takes the same rule.

- **D81. The service moves to Go, 22.11.2026.** One static binary instead of a Python image, with lower memory under
  the container's 256 MB. The port grows in `go/` (module `github.com/IvanIkra/infomaster/go`) next to the Python
  package, layer by layer: the foundation (store, clock, JSON and text as Python writes them, settings, the LLM
  client), then processing and the library, the collectors and outputs, the web app and the connector. Python stays
  the running service until Go matches it; then Go takes over the same `data/`, the same database and the same
  clients in one release, and the Python package goes. `go/CONTRACT.md` lists what must stay byte for byte (the
  schema, kv keys, routes and their JSON, uids and hashes, stored credentials); each module's pytest cases are ported
  as Go tests. Libraries: modernc SQLite (no cgo), anthropic-sdk-go, gotd for MTProto. The twin logs in again at the
  switch: gotd does not read Telethon's session; the bot gets a new session from its token.

- **D82. The switch to Go, 23.11.2026.** Before the switch a copy of the live database served by the Go image in shadow
  mode (`--shadow`: both ports, no Telegram, loops, push or LLM) answered the same as the running Python service on
  every route `compare` reads: the internal API, the iCal feeds, the public feeds, the OAuth metadata and the web
  page with its CSP. Then the Go image took over the same `data/`: the bot made a new session, the twin logged in
  again, the old Telethon sessions and a copy of the database stay on the server for a week as the way back. The
  Python package, its tests and `pyproject.toml`/`uv.lock` are removed from the repository at once (git keeps them);
  the web client moves to `go/web/static`, the root `Dockerfile` builds Go, CI runs the Go checks, a `node --check`
  of the client modules and the image. Compiling modernc's SQLite takes about 2 GB of memory, which the server does
  not spare next to Home Assistant and Tablo (a build there pushed it into swap): `scripts/deploy.sh` compiles on the
  developer's machine and builds the same runtime image on the host from the binaries (`Dockerfile.prebuilt`);
  `docker compose up -d --build` stays for hosts with the memory. Go uses 20–30 MB against Python's ~110 MB, and its
  image is 82 MB against 335 MB.
- **D83. One guide for the knowledge base, 23.11.2026.** Amends D58 and §6.8. The rules of the knowledge base (what goes
  into the book, a class's document and a cheat sheet; the LaTeX layout; the language and the style) were written
  nowhere, and the cheat sheets held only each course's notation. They now live in one document of kind `guide`, of no
  course: Markdown for Claude and the owner, versioned like the others, with an undo. The connector gives it by
  `get_guide` and also with every `get_cheatsheet`, which Claude already reads before writing a document, so the rules
  arrive without a step of their own; the server's instructions name it. The rules sit in the database and not in the
  repository, so the owner changes them from any client of the connector without a deploy. The rules themselves are
  not written yet: the owner writes them with Claude.
- **D84. The server builds PDFs, 24.11.2026.** Amends D58: there the server had neither the processor time nor TeX,
  so only Claude Code on the owner's Mac could publish a document. Now `build_document_version` sends a draft's
  LaTeX to a TeX builder on the server and keeps the PDF, so claude.ai and the phone publish too; a PDF built
  locally is taken as before. The builder is a container of its own, the boundary: no data, no secrets, no Docker
  socket, an internal network with only the service on it, a read-only root, no capabilities, one CPU, 512 MB, 64
  processes; TeX runs without shell escape, reads and writes only under the build's directory, under a minute,
  one build at a time (another is refused as busy). Measured on the host: about 2 GB of memory free and a build
  takes seconds, but unpacking TeX (~700 MB) once ran a CPU at full load and the owner felt the heat; so TeX is
  installed by `scripts/tex-image.sh` in a container held to one CPU at the lowest priority, once, and
  `scripts/deploy.sh` never rebuilds that image. CMU Serif is not in Alpine's TeX short of the 1 GB font
  collection: the four faces the guide uses are copied in. The source is one `.tex` file. Besides the LaTeX
  collections the builder has `mathscience` (`siunitx`, `physics`); `ADD=` installs another collection on top the
  same way, in a minute or two. `minted` does not work: it needs shell escape.
- **D85. The cheat sheet comes before homework, 25.11.2026.** Claude solved a problem sheet without the course's
  cheat sheet: the server's instructions named it only for processing a class, a sheet's answer said nothing of it,
  and `get_cheatsheet`'s description, which says to read it before homework, was not loaded (a client may load a
  tool's description only when it looks for the tool). Now the instructions, near their start, ask for
  `get_cheatsheet` before any homework or problem sheet; its description opens with when to call it; and a sheet
  read by `get_material` (kind sheet) or `get_sheet` carries `before_solving`, which names that call with the
  course. The field is there only when the course has a published cheat sheet. It is a pointer and not the text:
  the cheat sheet brings the whole guide with it, too much for every read of a sheet. The guide's rule that a
  seminar's problems are not solved stays a rule of documents, and the description says so.
- **D86. The text of a PDF is walked by the service itself, 25.11.2026.** A calculus sheet's Russian came out as
  garbage, and every TeX sheet came out without word spaces and with an Ω or a ⊗ after each piece of a formula.
  ledongthuc/pdf still parses the file and decodes its fonts, but the walk over a page's operators is now in
  `processing/pdf` (`text.go`), on the library's exported API: its own walk puts a "\n" through the current font
  after every TJ and ignores the gaps TeX writes for spaces. Glyphs are joined by where they stand (a gap over
  0.18 of the size is a space, a baseline moved over half the size is a line), with widths scaled for Type3 fonts
  and read from `W`/`DW` for Identity composites; where a font gives no widths, a TJ move of 200 or more is the
  space. pdfTeX's bitmap Cyrillic fonts carry no map to Unicode and name a glyph by its code: one with more codes
  from 192 on than ASCII letters is read as T2A, otherwise it is left alone (T1). Not covered: TeX's math fonts
  that come without a map to Unicode still give wrong signs (Δ, ∈, a minus). A sheet's kept text is read again at
  every start while its file is kept (14 days). The check was every PDF the server kept, old walk against new.
