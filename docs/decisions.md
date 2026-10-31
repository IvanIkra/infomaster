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
    Batch processing does not see own items: the model cannot move them, and a chat message cannot replace them.
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

## Open questions

Collected in [architecture.md](architecture.md), §12.

## Handed over to Tablo

The move happens in one step (D35). First stop Tablo and copy the data (`import-tablo`, [setup.md](setup.md),
"Moving from Tablo"), then start infomaster with all sources, then the reworked Tablo. The work is done in the
`tablo-t75` repository.

**The Tablo service (`service/tablo/`):**

- [ ] **A new source, `sources/infomaster.py`.** Once a minute it calls `GET /api/deadlines`, `/api/news`,
  `/api/grades`, `/api/status` with infomaster's Bearer `API_TOKEN` at `http://infomaster:8081` (Tablo joins the
  `infomaster` network as an external one). The last successful answer is kept in `kv` and shown while infomaster
  is unavailable (§11.1).
- [ ] **Deadlines** come as a finished list from `/api/deadlines` in screen order; Tablo only takes the first lines.
  Remove `todo.py`, the merge in `builder.py` (`merge_deadlines`, `drop_done`, `drop_hidden`) and the
  `/api/deadlines` routes.
- [ ] **News** comes from `/api/news` (`full`, `short`, `importance`, `study`, `source`); the §6.6 layout stays.
  Remove `news.py` (Haiku parsing) and `sources/telegram.py`.
- [ ] **Grades** come from `/api/grades` (`label`, `value`, `bonus`). Remove `sources/grades.py` and the layout
  detection.
- [ ] **HSE App X and mail**: remove `sources/hseapp.py`, `sources/mail.py`, the `/hseapp` page,
  `/api/hseapp/login` and the callback; delete the HSE App X tokens from `kv`.
- [ ] **The dictionary**: remove `subjects.py` and the manual entries from the config (`import-tablo` has already
  copied them).
- [ ] **Haiku**: remove `llm.py`, `/api/llm/runs`, the `llm_runs` journal and the LLM block of the diagnostics
  screen, without a replacement (D40).
- [ ] **Classes**: `ical.url` becomes `http://infomaster:8081/calendar/<CALENDAR_SECRET>/tablo.ics`. In `ical.py`:
  the title is `SUMMARY` as it is (already English, no dictionary); no more `<abbr> Midterm` from the feed (it comes
  in `/api/deadlines`); `STATUS:TENTATIVE` / `X-INFOMASTER-OPTIONAL:TRUE` becomes an optional mark (`ClassItem.mark`,
  a new value next to `moved` and `cancelled`, D36). **To do in Tablo: an icon for optional events.** A source icon
  (`X-INFOMASTER-SOURCE`) comes later.
- [ ] **Failures per block** (D37) come from infomaster's `/api/status`, with the thresholds unchanged. **To do in
  Tablo: an "infomaster is unavailable" icon.**
- [ ] **Tablo's `GET /api/status`** takes `hseapp.login_needed` from infomaster's status (for the HA notification).
- [ ] **`tablo.refresh_data`** polls infomaster and the weather again (infomaster has no "refresh now", D5).
- [ ] **Config**: remove the `grades`, `hseapp`, `news`, `mail`, `subjects` sections; `ical` keeps the link. Add
  infomaster's address and token (the token goes in `.env`).
- [ ] **Secrets**: remove `ANTHROPIC_API_KEY`, `TABLO_MAIL_USER`/`TABLO_MAIL_PASSWORD`, `TABLO_ICAL_URL` from Tablo's
  `.env` (mail and RUZ are now only here, as `MAIL_*` and `RUZ_ICAL_URL`).
- [ ] **Texts on screen**: cut with "…" for now (D39).

**The HA integration (`ha-integration/`):**

- [ ] Remove the `todo` platform (D34) and the `/api/deadlines` reads in the coordinator.
- [ ] The Sources form keeps the weather coordinates and the iCal link; remove HSE App X, the full name, the sheets,
  the channels, the news lifetime, mail and the dictionary (D33).
- [ ] The HSE App X login notification links to the bot (D38).
- [ ] Remove the LLM sensors ("LLM run: …", "LLM cost today") without a replacement (D40).
- [ ] Bring Tablo's documents (`docs/software/04`, `05`, `07`) in line with the new scheme.
