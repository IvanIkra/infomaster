# Setup guide

A detailed guide to the first start, the sources and the outputs of infomaster. The short version is in the
[README](../README.md). Interface names are given as they appear in English; with Russian selected, the bot and the
Mini App use the Russian names.

## First start

1. **Bot.** Create one with @BotFather and put its token in `TG_BOT_TOKEN`. Take `TG_API_ID` and `TG_API_HASH` from
   my.telegram.org.
2. **Forum.** Create a group and turn topics on. Add the bot as an admin with the rights to manage topics, pin
   messages and delete messages. Put the group id (`-100…`) in `TG_FORUM_ID` and your own id in `TG_OWNER_ID`.
   The bot creates the topics System, News, Deadlines, Timetable, Sheets, Grades and Summaries itself.
3. **Tailscale (a personal tailnet).** In the tailnet admin console turn on HTTPS certificates and Funnel (the
   `funnel` attribute in the policy). Create an auth key and put it in `TS_AUTHKEY`. After the first start put the
   address `https://infomaster.<tailnet>.ts.net` in `PUBLIC_URL` and restart.
4. **RUZ and calendars** (optional). Put the link to your RUZ feed (`https://api.hseapp.ru/v3/ruz/cal/<uuid>`) in
   `RUZ_ICAL_URL`. The secret of the internal feeds goes in `CALENDAR_SECRET`
   (`python3 -c "import secrets; print(secrets.token_urlsafe(24))"`); the feeds for your own calendar need a
   different secret in `CALENDAR_PUBLIC_SECRET`. Without the link the timetable holds only events from chats;
   without a secret its feeds are off.
5. Fill in the rest of `.env` following `.env.example` and run `docker compose up -d --build`.
6. Send `/start` to the bot: an **Open** button and a **Settings** menu button appear; both open the Mini App.
7. **Twin.** A separate Telegram account (neither your main one nor a bot). Log it in from the Mini App under
   **Settings → Twin and chats**: phone, code (it comes to the twin's Telegram or by SMS; do not forward it in
   Telegram, or it stops working) and the cloud password if there is one. Chats are read at once, with no restart;
   the session is kept in `data/twin.session`. **Log out** on the same screen closes the session in Telegram too;
   the chats and their settings stay. The fallback is the terminal:
   `docker compose run --rm infomaster python -m infomaster login`, then `docker compose restart infomaster`.
8. Under **Settings → Twin and chats** turn on the chats you need and mark the study ones. New chats appear turned
   off, and the bot reports each one in System.
9. **HSE App X** (optional). If Tablo polls HSE App X itself, turn that off first (`hseapp.enabled: false` in its
   config): the `refresh_token` rotates on every refresh, and two clients would log each other out. Then log in:
   System holds a "login needed" message with a **Log in to HSE App X** button. It opens the chat with the bot, and
   the bot answers with a Mini App button for the HSE App X screen (also **Settings → HSE App X**). **Log in** opens
   the HSE sign-in page in the browser; after signing in the browser returns to `PUBLIC_URL/hseapp/callback` on its
   own. If it does not, the same screen has a section for pasting the return address by hand. If you do not need
   HSE App X, turn off **Poll HSE App X** there.
10. **Mail** (optional). Put the `@edu.hse.ru` address in `MAIL_USER` and a Yandex 360 app password in
    `MAIL_PASSWORD`. If Tablo reads the mailbox itself, turn that off there (`mail.enabled: false`), or letters are
    processed twice.
11. **Grades** (optional). If Tablo reads grade sheets itself, turn that off (remove `grades.sheets` from its
    config), or grades arrive twice. In the Mini App (**Settings → Gradebooks**) enter your full name as the sheets
    spell it and add the sheet links (shared with "anyone with the link") with a short course name.

12. **Own emoji** (optional, the bot's owner needs Telegram Premium). At start the bot creates the emoji pack
    `infomaster_by_<bot>` in the owner's name from `infomaster/outputs/emoji-pack/` (MDI glyphs, as on Tablo's
    screen) and shows the emoji of its posts as the pack's ones, in the colour of the text. Without Premium Telegram
    shows the ordinary emoji. A bot may set only Telegram's own topic icons, so the pack's ones are set by hand:
    a topic → **Edit** → its icon → the pack (System ⚙️, News 📰, Deadlines ⏰, Timetable 📅, Sheets 📄, Grades 📊,
    Summaries 📝).

What you should see: "infomaster is up." in the System topic, the Mini App shows LLM spending and all the twin's
chats, and messages from the enabled chats pile up in the database (`messages`). Until the twin logs in, the service
runs without chats and reminds you about the login in System.

## Sources and outputs

**Batch processing** (needs `ANTHROPIC_API_KEY`). Every enabled chat is processed at hours divisible by N (3 by
default: 0, 3, 6…). A chat marked urgent is processed soon after a pin, a PDF or a message from an important person:
once the chat has been quiet for a minute, but no later than 5 minutes. The News topic holds a pinned list of live
news for the last 24 hours, which the bot edits in place, plus a short notification for each item of importance
4–5; **Hide** under a notification removes the item for good. The queue and the last error are in
`GET /api/status` (`batches`).

**Course dictionary and timetable.** RUZ is polled once an hour. After the first poll with classes, System reports
how many classes the timetable holds. Haiku translates course names (in one call, together with the whole
dictionary), and every midterm gets a `<abbr> Midterm` deadline in the pinned Deadlines list (without separate
messages: the first poll is not news); the deadline follows its class, including a move or a cancellation from a
chat. After that the Timetable topic receives changes: what RUZ itself moved, cancelled or removed (one message per
poll), and consultations and events from chats, their moves and cancellations, and moves and cancellations of
classes announced in chats. Each comes with a link to the message and the buttons **Main / Optional** and **Hide**
(**Don't apply** for a change to a class). If RUZ later changes the class itself, the change from the chat stops
applying. The Mini App's Timetable tab shows the same as the feeds: classes with changes, events and your own; a tap
opens a card where an item can be moved, cancelled, made optional, removed, or a chat's change corrected, and **+**
adds your own event (D44). **Settings → Course dictionary** holds manual entries (Haiku leaves them alone; Haiku
names a deleted entry again; `<abbr> Midterm` is renamed right after an edit). If `RUZ_ICAL_URL` changes, the next
poll counts as the first one: System reports the new number of classes, without a stream of "removed" messages in
Timetable. RUZ state is in `GET /api/status` (`ruz`: the number of classes in the last feed, the poll error without
the link, the translation error).

**HSE App X.** After the login System confirms that deadlines are updating. The app is polled every 15 minutes and
the login renews itself; tokens stay in the database (`kv`) and go nowhere else: not to the status, the log or the
Mini App. Haiku translates deadline names, in one call for all new ones, cached forever; while it is unavailable,
new deadlines keep their Russian names. Active deadlines go to the common list, completed ones remove their copies
from mail, chats and RUZ (§5.5); the first poll sends no separate messages, the deadlines simply appear in the
Deadlines pin. If the login expires (three weeks without polling, or a logout), System gets one message with the same
button; if the API is unavailable for 3 hours, one message, and one more when it is back. State is on the HSE App X
screen and in `GET /api/status` (`hseapp`: `logged_in`, `login_needed`, `ok_at`, `error`, `items`, `done`,
`labels_error`).

**Deadlines.** The Deadlines topic holds the pinned common list after the merge (§5.5): HSE App X, RUZ midterms,
mail and chats, your own; Tablo gets the same list in the same order. Each line has a source icon (🎓 HSE App X,
📅 RUZ, ✉️ mail, 💬 chat, 🏠 own), the title, the due date ("today", "tomorrow", "overdue (Mon 19.10)", or a day and
date) and the course; overdue ones stay visible for 3 days (1 day for mail and chats). The bot edits the list in
place. Every new or moved deadline from any source arrives as its own message, and one withdrawn by its source as a
"withdrawn" message; what the owner did (an own deadline, hiding, an edit in the Mini App) only edits the list, and
so does a rename (the English HSE App X label, `<abbr> Midterm` after the dictionary); a copy of the same task from
another source is not news. Under a message is **Hide** (**Delete** for your own): a hidden deadline does not come
back even if the source sends it again; **Restore** undoes it. Your own, hidden and the list itself are on the Mini
App's Deadlines tab, each with the chat or letter it came from: there you add, edit or delete your own deadline,
edit someone else's (it becomes yours), hide and restore. Own and hidden deadlines are deleted 30 days after the due
date.

**Mail.** The mailbox is checked every 5 minutes, read-only (letters are not marked as read); the first poll takes
the letters of the last 24 hours. Letters are processed with the chats on the common N-hour schedule, after the
study chats and before the rest; every letter counts as study. News, deadlines and events from letters are marked ✉️
and carry the subject instead of a link. The Mini App settings show the number of letters over 14 days and the time
of the last check; if the mailbox does not answer for 3 hours, System gets one message (the password never appears
in it, in the log or in the status). State is in `GET /api/status` (`mail`: `configured`, `ok_at`, `error`, `last`,
`letters`).

**Internal API** (Bearer token) for Tablo, answers shaped like Tablo's own (§8): `GET /api/deadlines` returns
`{"items": [{uid, title, due, category, source, overdue}]}`, read-only: own and hidden deadlines are managed in the
Mini App and with the forum buttons. Live news for Tablo is `GET /api/news`:
`{"items": [{id, source, study, posted_at, full, short, importance}], "ttl_hours"}` in rank order, the same items as
the News pin (without replaced, withdrawn and hidden ones).

**Problem sheets.** The twin downloads a PDF from an enabled chat when batch processing reaches its message (files
over 50 MB are skipped); the first page is read on the spot, and a scan goes to the model as an image. Batch
processing decides whether a PDF is a sheet (homework, problems, a variant), and the bot posts the file to the Sheets
topic with a caption: course and number, due date, a link to the message and a course hashtag (#Calc). The same file
a second time is not a new sheet. A new version (the same chat and course and the same number or almost the same file
name, the old sheet at most 30 days old; or the same message with the file replaced) replaces the file in the old
message, and a "sheet updated" reply follows. With **Whole PDF** in the chat settings Haiku reads the sheet (up to 10
pages and 30k tokens, set under **Settings → Whole PDF**): a summary goes in the caption, deadlines from all pages go
to the common list, and a new version says what changed (only the difference of the texts goes to the model).
Reading is an optional task: above 80 % of the daily limit the sheet arrives without it. In the Mini App
(**Study → Sheets**) an unrecognised version can be attached to an old sheet, a sheet's course, number, due date and
description can be corrected (the message caption is edited), and **+** uploads your own PDF as a sheet (D44).
Downloaded files are deleted after 14 days; the sheets stay in the forum. The queue and the reading error are in
`GET /api/status` (`sheets`: `waiting`, `error`).

**Summaries.** At 21:00 Moscow time (set under **Settings → Daily summary**) the Summaries topic receives a message
for every enabled chat with the summary switch on: what happened there since the last summary, as points in the
forum's language, each with a "→" link to its message. Chats with nothing important are gathered in one line. A
summary is built from raw messages and does not wait for a batch; Haiku first compresses a long period day by day. A
chat filtered to "important only" contributes only the selected messages. The daily summary is an optional task:
above 80 % of the daily limit it is skipped, and its messages go into the next one. On demand, in a private chat
with the bot or in the forum: `/summary [chat] [period] [language]`. The chat is part of its name; the period is
`3h`, `2d`, `20.10` or `20.10-22.10` (at most 7 days; without it, since this chat's last summary); the language is
`en` or `ru` at the end, before or after the period (without it, the forum's language; the chat buttons remember
it). Without a chat the bot shows the enabled chats as buttons; you can also forward the bot a message from a chat
and reply `/summary` to it. An on-demand summary also goes to the Summaries topic, and the bot answers with a link to
it; it works above 80 % of the limit too, and one too large for what is left is simply refused without stopping
batch processing. Only a "since the last one" summary moves the start of the next. State is in `GET /api/status`
(`summaries`: `ok_at`, `error`, `done`, `time`).

**Grades.** Teachers' Google Sheets are read every 30 minutes (and right after an edit under
**Settings → Gradebooks** or on **Read now**). In every visible tab the service finds your row by full name; Haiku
sees the top of the tab (up to 15 rows above yours, other students' included) and your row, finds the header itself
and says which columns are graded tasks, their maximum and the bonus. The answer is cached with the header: while the
header does not change, Haiku is not called, however other students' grades change. Grades are brought to a 0–10
scale. The first read of a sheet is a snapshot: System reports the sheet as connected, without grade messages. After
that every new or changed grade goes to the Grades topic, one message per sheet: the label (`DM HW 1A`), the grade,
the bonus, the sheet's points and the previous grade (`DM HW 1A — 9 +1 (6.3 of 7; was 8.4)`). A zero for a task that
did not exist before counts as "not graded yet". A sheet that cannot be read for 12 hours (closed, no own row, the
LLM limit) gets one line in System, and one when it is read again; its grades stay meanwhile. Removing a sheet or
changing the name erases its grades: the next read is a snapshot again. Every sheet has its own Mini App screen:
name, check and the tab layout, which can be corrected (yours overrides Haiku's answer); on the Study tab a grade can
be changed (it holds until the sheet itself changes it) or your own added (D44). New grades of the last 7 days (the
window is configurable), your own included, are served by `GET /api/grades` on the internal port; sheet state is in
`GET /api/status` (`grades`).

**iCal feeds** are served without Bearer, by the secret in the path. The internal port serves
`http://infomaster:8081/calendar/<CALENDAR_SECRET>/main.ics` (classes and wherever one has to be), `optional.ics`
(where one may go) and `tablo.ics` (everything, optional items with `STATUS:TENTATIVE` and
`X-INFOMASTER-OPTIONAL:TRUE`, every event with `X-INFOMASTER-SOURCE` and `X-INFOMASTER-LINK`). Titles are in
English, the first line of the description is the class type, times are in UTC. Tablo reads `tablo.ics` with its
`ical.py` unchanged.

**Your own calendar** (D43). With `CALENDAR_PUBLIC_SECRET` the same `main.ics` and `optional.ics` (not `tablo.ics`)
are also served on the public port, through Funnel:
`https://infomaster.<tailnet>.ts.net/calendar/<CALENDAR_PUBLIC_SECRET>/main.ics`. Add the link as a subscription: in
Apple Calendar, File → New Calendar Subscription (on iPhone, Settings → Calendar → Accounts → Add Account → Other →
Add Subscribed Calendar, or open the link with `webcal://` instead of `https://`); in Google Calendar, Other
calendars → From URL. Apple refreshes the feed about once an hour, Google every few hours and ignores the feed's
hint. The secret is the only protection, so do not share the link. If it leaks, replace `CALENDAR_PUBLIC_SECRET` in
`.env`, run `docker compose up -d` and subscribe again; Tablo is not affected. To turn it on, generate the secret on
the server and take the link from `grep ^CALENDAR_PUBLIC_SECRET= ~/infomaster/.env`:

```bash
cd ~/infomaster && printf "\nCALENDAR_PUBLIC_SECRET=%s\n" "$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')" >> .env && docker compose up -d --build
```

**LLM spending** is under **Settings → Spending and limit** in the Mini App: today, the week and the month with a
forecast, a 30-day chart with the limit line, tasks and chats for the chosen period, the daily limit and a raise for
today. Tablo and HA no longer show spending (D40).

**All settings** are on the Mini App's Settings tab: sources, processing (batches and news, the summary, PDFs, images,
the dictionary), LLM spending and model prices, languages; at the top, **Needs attention** lists what is broken.
`.env` holds only secrets.

**Languages** (D41): English or Russian, English by default, with two switches under **Settings → Language**. The
**Mini App** switch covers the whole interface, the errors and states the service sends, and the page shown after the
HSE App X login; it applies at once. The **forum** switch covers everything the bot writes: topics, pins, messages,
buttons, button replies and `/summary`, System, and also the daily summary, the sheet summary and "what changed"
(Haiku writes them in that language). It applies to what the bot writes after the switch: old messages stay as they
were, the News and Deadlines pins are rewritten on their next edit, existing topics are not renamed, and the
command description and the menu button change after a restart. News, deadline labels, course names and the iCal
feeds are always in English, because Tablo receives them too.

## Moving from Tablo

The move happens in one step, with a full stop (D35). Own and hidden deadlines and the manual dictionary entries live
in Tablo's database; copy them once, with Tablo stopped (infomaster is stopped too, and Tablo's database is opened
read-only):

```sh
docker compose stop infomaster
cp ~/tablo/data/tablo.db data/tablo.db
docker compose run --rm infomaster python -m infomaster import-tablo --tablo-db data/tablo.db
rm data/tablo.db && docker compose start infomaster
```

The command prints how many items were copied and skipped; running it again duplicates nothing. Own deadlines get
new `uid`s (`ha-<id>`). A manual dictionary entry already set in the Mini App wins over Tablo's. The copied own
deadlines appear in the Deadlines pin without separate messages. Then start infomaster with all sources, then the
reworked Tablo; what to change in Tablo and in the HA integration is listed under "Handed over to Tablo" in
[decisions.md](decisions.md).
