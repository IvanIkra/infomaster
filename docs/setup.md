# Setup guide

A detailed guide to the first start, the sources and the outputs of infomaster. The short version is in the
[README](../README.md). Interface names are given as they appear in English; with Russian selected, the web app
uses the Russian names.

## First start

1. **Bot.** Create one with @BotFather and put its token in `TG_BOT_TOKEN`, and your own Telegram id in
   `TG_OWNER_ID`. Take `TG_API_ID` and `TG_API_HASH` from my.telegram.org. The bot only sends you login codes and
   takes files you send it for the library (D69).
2. **Tailscale (a personal tailnet).** In the tailnet admin console turn on HTTPS certificates and Funnel (the
   `funnel` attribute in the policy). Create an auth key and put it in `TS_AUTHKEY`. After the first start put the
   address `https://infomaster.<tailnet>.ts.net` in `PUBLIC_URL` and restart.
3. **RUZ and calendars** (optional). Put the link to your RUZ feed (`https://api.hseapp.ru/v3/ruz/cal/<uuid>`) in
   `RUZ_ICAL_URL`. The secret of the internal feeds goes in `CALENDAR_SECRET`
   (`python3 -c "import secrets; print(secrets.token_urlsafe(24))"`); the feeds for your own calendar need a
   different secret in `CALENDAR_PUBLIC_SECRET`. Without the link the timetable holds only events from chats;
   without a secret its feeds are off.
4. Fill in the rest of `.env` following `.env.example` and run `docker compose up -d --build`.
5. Send `/start` to the bot: it answers with the web app's address. Open it and log in (see [Web app](#web-app)).
6. **Twin.** A separate Telegram account (neither your main one nor a bot). Log it in from the web app under
   **Machine → Twin and chats**: phone, code (it comes to the twin's Telegram or by SMS; do not forward it in
   Telegram, or it stops working) and the cloud password if there is one. Chats are read at once, with no restart;
   the session is kept in `data/twin.session`. **Log out** on the same screen closes the session in Telegram too;
   the chats and their settings stay. The fallback is the terminal:
   `docker compose run --rm infomaster python -m infomaster login`, then `docker compose restart infomaster`.
7. Under **Machine → Twin and chats** turn on the chats you need and mark the study ones. New chats appear turned
   off.
8. **HSE App X** (optional). If Tablo polls HSE App X itself, turn that off first (`hseapp.enabled: false` in its
   config): the `refresh_token` rotates on every refresh, and two clients would log each other out. Then log in under
   **Machine → HSE App X**: **Log in** opens the HSE sign-in page in the browser; after signing in the browser
   returns to `PUBLIC_URL/hseapp/callback` on its own. If it does not, the same screen has a section for pasting the
   return address by hand. If you do not need HSE App X, turn off **Poll HSE App X** there.
9. **Mail** (optional). Put the `@edu.hse.ru` address in `MAIL_USER` and a Yandex 360 app password in
   `MAIL_PASSWORD`. If Tablo reads the mailbox itself, turn that off there (`mail.enabled: false`), or letters are
   processed twice.
10. **Grades** (optional). If Tablo reads grade sheets itself, turn that off (remove `grades.sheets` from its
    config), or grades arrive twice. Under **Machine → Gradebooks** enter your full name as the sheets spell it and
    add the sheet links (shared with "anyone with the link") with a short course name.

What you should see: «Machine» lists the sources with their freshness and the twin's chats, and messages from the
enabled chats pile up in the database (`messages`). Until the twin logs in, the service runs without chats and the
dot at the top of the feed is red.

## Sources and outputs

**Batch processing** (needs `ANTHROPIC_API_KEY`). Every enabled chat is processed at hours divisible by N (3 by
default: 0, 3, 6…). A chat marked urgent is processed soon after a pin, a PDF or a message from an important person:
once the chat has been quiet for a minute, but no later than 5 minutes. The live news of the last 24 hours are in the
web app's **News** view. The queue and the last error are in `GET /api/status` (`batches`).

**Course dictionary and timetable.** RUZ is polled once an hour. Haiku translates course names (in one call,
together with the whole dictionary), and every midterm gets a `<abbr> Midterm` deadline (not pushed: the first poll
is not news); the deadline follows its class, including a move or a cancellation from a chat. Consultations and
events from chats, their moves and cancellations, and moves and cancellations of classes announced in chats join the
timetable. If RUZ later changes the class itself, the change from the chat stops applying. The web app's week and
the cards show the same as the feeds; a class's or an event's card moves, cancels, makes optional, removes or
corrects it (D68). In `main.ics` and `optional.ics` an event's notes end with its source (the chat with a link to the
message, the letter, RUZ, you) and an **Edit** link to its card in the web app (D69). **Machine → Course dictionary** holds
manual entries (Haiku leaves them alone; Haiku names a deleted entry again; `<abbr> Midterm` is renamed right after
an edit). If `RUZ_ICAL_URL` changes, the next poll counts as the first one. RUZ state is in `GET /api/status` (`ruz`:
the number of classes in the last feed, the poll error without the link, the translation error).

**HSE App X.** The app is polled every 15 minutes and the login renews itself; tokens stay in the database (`kv`) and
go nowhere else: not to the status, the log or the web app. Haiku translates deadline names, in one call for all new
ones, cached forever; while it is unavailable, new deadlines keep their Russian names. Active deadlines go to the
common list, completed ones remove their copies from mail, chats and RUZ (§5.5); the first poll pushes nothing. If
the login expires (three weeks without polling, or a logout), you get one notification; if the API is unavailable
for 3 hours, one, and one more when it is back. State is under **Machine → HSE App X** and in `GET /api/status`
(`hseapp`: `logged_in`, `login_needed`, `ok_at`, `error`, `items`, `done`, `labels_error`).

**Deadlines.** The web app's feed holds the common list after the merge (§5.5): HSE App X, RUZ midterms, mail and
chats, your own; Tablo gets the same list in the same order. Every new or moved deadline from any source is pushed;
what you did (an own deadline, hiding, an edit) is not, and neither is a rename (the English HSE App X label,
`<abbr> Midterm` after the dictionary) or a copy of the same task from another source. On a deadline's card:
**Done**, **Not mine**, **Edit**, **It is a duplicate…** (D62): a hidden deadline does not come back even if the
source sends it again; **Machine → Hidden deadlines and events** restores it. Own and hidden deadlines are deleted 30 days after the due
date.

**Mail.** The mailbox is checked every 5 minutes, read-only (letters are not marked as read); the first poll takes
the letters of the last 24 hours. Letters are processed with the chats on the common N-hour schedule, after the
study chats and before the rest; every letter counts as study. News, deadlines and events from letters carry the
subject instead of a link. If the mailbox does not answer for 3 hours, you get one notification (the password never
appears in it, in the log or in the status). State is in `GET /api/status` (`mail`: `configured`, `ok_at`, `error`,
`last`, `letters`).

**Internal API** (Bearer token) for Tablo, answers shaped like Tablo's own (§8): `GET /api/deadlines` returns
`{"items": [{uid, title, due, category, source, overdue, assumed}]}` (`assumed`: a sheet's date taken from its
course's next seminar), read-only: own and hidden deadlines are managed in the web app and by Claude. Live news for
Tablo is `GET /api/news`: `{"items": [{id, source, study, posted_at, full, short, importance}], "ttl_hours"}` in rank
order, the same items as the web app's news (without replaced, withdrawn and hidden ones).

**Problem sheets.** The twin downloads a PDF from an enabled chat when batch processing reaches its message (files
over 20 MB are skipped); the first page is read on the spot, and a scan goes to the model as an image. Batch
processing decides whether a PDF is a sheet (homework, problems, a variant); the sheet is copied into the library and
shown on the web app's **Sheets** shelf with its course, number, due date, source and PDF. A sheet without a due date
is due before the next seminar of its course in RUZ, marked as assumed until a real date comes (architecture.md,
§6.3). The same file a second time is not a new sheet. A new version (the same chat and course and the same number or
almost the same file name, the old sheet at most 30 days old; or the same message with the file replaced) is chained
to the old one with what changed. With **Whole PDF** in the chat settings Haiku reads the sheet (up to 10 pages and
30k tokens, set under **Machine → Batches and news**): a summary, deadlines from all pages go to the common list, and a new
version says what changed (only the difference of the texts goes to the model). Reading is an optional task: above
80 % of the daily limit the sheet arrives without it. On a sheet's card an unrecognised version can be attached to an
old sheet, and a sheet's course, number, due date and description corrected; you can upload your own PDF as a sheet
(D67). Downloaded files are deleted after 14 days; the library's copy stays. The queue and the reading error are in
`GET /api/status` (`sheets`: `waiting`, `error`).

**Grades.** Teachers' Google Sheets are read every 30 minutes (and right after an edit under
**Machine → Gradebooks** or on **Read now**). A sheet is added by its link: a Google Sheet open to anyone with the
link, or a Dropbox share link to an `.xlsx` file (`https://www.dropbox.com/scl/fi/…`, the older
`https://www.dropbox.com/s/…`, or `dl.dropboxusercontent.com`); a Dropbox sheet is named after its file. A Dropbox
folder link or a file of another kind is refused when it is added. In every visible tab the service finds your row by
full name; Haiku sees the top of the tab (up to 15 rows above yours, other students' included) and your row, finds the
header itself and says which columns are graded tasks, their maximum and the bonus. The answer is cached with the
header: while the header does not change, Haiku is not called, however other students' grades change. Grades are
brought to a 0–10 scale. The first read of a sheet is a snapshot. Grades are not pushed (D55): they are in the course
lenses of the web app and in Tablo. A zero for a task that did not exist before counts as "not graded yet". A sheet
that cannot be read for 12 hours (closed, a deleted Dropbox link, no own row, the LLM limit) is pushed once; its
grades stay meanwhile. Removing a sheet or changing the name erases its grades: the next read is a snapshot again.
Every sheet has its own screen in «Machine»: name, check and the tab layout, which can be corrected (yours overrides
Haiku's answer); in the course's lens a grade can be changed (it holds until the sheet itself changes it) or your own
added. New grades of the last 7 days (the window is configurable), your own included, are served by
`GET /api/grades` on the internal port; sheet state is in `GET /api/status` (`grades`).

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

**LLM spending** is under **Machine → LLM spending**: today, the week and the month by task and chat, the daily limit and a
raise for today. Tablo and HA do not show spending (D40).

**All settings** are in **Machine**: sources, processing (batches and news, deadline reminders, PDFs, images, the
dictionary), the LLM limit, notifications and the language; at the top, **Needs attention** lists what is broken.
`.env` holds only secrets. The model prices are the code's defaults.

**Language** (D41): English or Russian, English by default, switched at the bottom of «Machine». It covers the web
app, the errors and states the service sends, the bot's answers, the page shown after the HSE App X login, and the
sheet summary and "what changed" (Haiku writes them in that language). News, deadline labels, course names and the
iCal feeds are always in English, because Tablo receives them too.

## Web app

The home-screen app (architecture §8.6) is infomaster's interface: the feed, the week, the cards and «Check», «+»,
the news, the shelves, notifications, and «Machine» with the sources, the journal of changes and the settings.
Nothing is set in `.env`: it uses `PUBLIC_URL`.

1. Make sure you have pressed **Start** in your private chat with the bot: the login code comes there.
2. Open `https://infomaster.<tailnet>.ts.net/web/` on the phone, tap **Send a code**, type the six digits the bot
   sent and tap **Log in**.
3. Create a passkey when the app offers it: later logins on that device take Face ID, Touch ID or the device's PIN.
   A passkey works only at this address; if the Funnel name ever changes, log in with a code again.
4. Add the app to the Home Screen: on iPhone, **Share → Add to Home Screen** (notifications on iOS need this); on
   Android, the browser's **Install app**.
5. Turn notifications on when the feed offers it, or under **Machine → Notifications**, and send a test.

**Machine → Login and devices** lists where you are logged in and your passkeys; a lost phone is logged out there
from another device. A device unused for 90 days is logged out by itself.

**Machine** is behind the dot at the top of the feed; the dot turns red when something needs you, and Machine says
what and where to fix it. **Journal of changes** lists what you and Claude changed, with **Undo**.
**Machine → Notifications** picks what comes to the phone (new and moved deadlines, reminders, failures and logins,
the LLM limit) and the quiet hours, when notifications come without a sound.

If the bot had a main Mini App set in @BotFather (**Bot Settings → Configure Mini App**) from before, turn it off
there: the Mini App is gone (D69).

## Claude connector

infomaster is a remote MCP server (architecture §8.5): add it to claude.ai as a custom connector, and Claude in the
web, desktop and phone apps can read your deadlines, timetable, grades, news, messages and sheets, and change what
the web app changes. It works on any plan, Free included (one custom connector there). Nothing is set in `.env`.

1. Make sure you have pressed **Start** in your private chat with the bot: the sign-in code comes there.
2. In the web app, **Machine → Claude connector**: **Copy** the address, `https://infomaster.<tailnet>.ts.net/mcp`.
3. In claude.ai: **Customize → Connectors → Add → Add custom connector**. Name it `infomaster`, paste the address and
   click **Continue**. Keep the options marked **Detected**; if the dialog asks how Claude identifies itself, choose
   **Register automatically** (infomaster supports Dynamic Client Registration, not Claude's published identity),
   leave the client secret and the request headers empty, and click **Add**.
4. Click **Connect**. A page "Connect to infomaster" opens: check that it says **Claude** and goes back to
   `claude.ai`, click **Send a code to Telegram**, type the code the bot sent and click **Allow**.
5. In a chat, turn the connector on under **+ → Connectors**. The phone app uses the same connector.
6. Under **Customize → Connectors → infomaster** you can set each tool's permission. Keep the changing tools on
   approval rather than **Always allow**: Claude reads messages strangers wrote.

Everything Claude changes is listed in **Machine → Journal of changes** with **Undo**; **Machine → Claude connector**
revokes a connection (Claude asks you to sign in again). An access token lives an hour and is refreshed by Claude on
its own; a connection unused for 30 days lapses.

**Claude Code** (on your machine) connects to the same address: `claude mcp add --transport http infomaster
https://infomaster.<tailnet>.ts.net/mcp`, then `/mcp` in Claude Code to sign in the same way; the browser goes back
to a loopback address, which the page says.

If claude.ai says it cannot reach the server, check that `PUBLIC_URL` is the Funnel address and that
`curl https://infomaster.<tailnet>.ts.net/.well-known/oauth-protected-resource/mcp` answers from outside the tailnet.

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
new `uid`s (`ha-<id>`). A manual dictionary entry already set wins over Tablo's. The copied own deadlines are not
pushed as new. Then start infomaster with all sources, then the reworked Tablo; what to change in Tablo and in the HA
integration is listed under "Handed over to Tablo" in [decisions.md](decisions.md).
