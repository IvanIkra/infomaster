# Tablo's data sources: classes, deadlines, news, grades

> How Tablo collected and processed its data before infomaster. infomaster took its collection and processing logic
> from here, so references like §5.5 in the code and in [architecture.md](architecture.md) point to this document.
> It is a condensed description: section numbers are kept, and Tablo-specific details that infomaster did not take
> over are shortened.

Tablo is a home dashboard service with an e-ink screen. This document describes four of its data sources and what a
separate data service (infomaster) had to provide in their place.

- §1–§9 describe how Tablo collected and processed the data, the logic the new service could take over in full or in
  part.
- §10 summarises Tablo's Haiku prompts.
- §11 lists what Tablo needs as input for each block and the decisions to make before the work.

Checked against Tablo's `service/tablo/` code on 20.10.2026.

Contents:

1. [Overview](#1-overview)
2. [Common components: Haiku, the course dictionary, the deadline model](#2-common-components)
3. [Grades: Google Sheets](#3-grades-google-sheets)
4. [Classes: the RUZ iCal feed](#4-classes-the-ruz-ical-feed)
5. [Deadlines: five sources and the merge](#5-deadlines)
6. [News: Telegram, mail, Haiku](#6-news)
7. [Settings and secrets](#7-settings-and-secrets)
8. [Tablo's HTTP API](#8-tablos-http-api)
9. [SQLite storage](#9-sqlite-storage)
10. [Appendix: Haiku prompts](#10-appendix-haiku-prompts)
11. [An external data service: what Tablo needs as input](#11-an-external-data-service-what-tablo-needs-as-input)

---

## 1. Overview

```
Google Sheets ─► header layout (Haiku, cached) ─► grade_items / grade_events ─► Grades

RUZ iCal ──────► kv "ical.events" ─┬─► Classes (the next 3 classes)
                                   └─► Deadlines: "<abbr> Midterm"

HSE App X ─────► kv "hseapp.deadlines" (active + completed) ─► Deadlines
                     labels by Haiku, cached

Telegram t.me/s ─┐
IMAP mail ───────┴─► news_posts ─► Haiku (in batches) ─┬─► news_items ─► News
                                                       └─► news_deadlines ─► Deadlines

HA to-do ──────► own_deadlines / hidden_deadlines ─► Deadlines

The subjects dictionary (Haiku + manual corrections) is shared by Classes, Deadlines and News.
```

**Rules common to all sources:**

- **One asyncio process.** Every source has its own `run()` loop: poll, then sleep `poll_minutes`. A "refresh now"
  command (`refetch`, §8) wakes the loop and the poll runs at once.
- **The last good data is kept in SQLite.** After a restart the screen shows the saved data at once, without waiting
  for the first poll. A failure never erases data.
- **A failure is not shown at once.** While the saved data is younger than a threshold (each source has its own),
  the screen looks normal. Past the threshold the block shows a short note about the problem.
- **Time zone:** `display.tz_name`, `Europe/Moscow` by default. All dates and times in the database are local,
  without a zone. The exception is the Haiku journal `llm_runs`, which is in UTC.
- **The screen is in English.** Haiku translates Russian names.
- **Haiku does not see other people's data.** From grade sheets it gets only the header and the own row.

---

## 2. Common components

### 2.1 Calling Haiku

| Parameter | Value |
|---|---|
| Model | `claude-haiku-5-5` |
| SDK | `anthropic.AsyncAnthropic(timeout=120, max_retries=3)` |
| Key | `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`; without it the source gets an "ANTHROPIC_API_KEY is not set" error and keeps its data |
| Answer format | structured output: `output_config={"effort": "low", "format": {"type": "json_schema", "schema": …}}` |
| Messages | `system` = the instruction, one `user` message = the data |
| Errors | `stop_reason == "refusal"`, `max_tokens` or an answer without text count as errors; the tokens spent are recorded anyway |
| Journal | every call, failed ones included, goes to `llm_runs`: the task (`news`, `grades`, `deadlines`, `subjects`), tokens, cost, duration |
| Network | from Russia the Anthropic API is reachable only through a VPN |

Prices for the cost, in $ per million tokens: input 0.10, output 0.50; for prompts over 100,000 tokens 0.50 and 2.50.
A cache write costs ×1.25 of the input price, a cache read ×0.1.

**Length check by font.** Screen texts (names, labels, news) are measured with the real font (Inter 16) after Haiku
answers. What does not fit goes back to Haiku **once more** with the reason ("Your previous … was too wide…; make it
shorter"). If the second answer does not fit either, the renderer cuts the text with "…". Character budgets for the
prompts are computed as `width in px / 8.1`:

| What | Width | Budget |
|---|---|---|
| A news line | 234 px | 28 characters |
| A deadline title | 118 px | 14–15 characters |
| A course in Classes | the column width | ≈12 characters |
| A grade label | — | 23 characters minus the course `label` and a space, at least 8 |

### 2.2 The course dictionary `subjects`

One dictionary for all blocks. Without it a Russian course name from HSE App X, *Calculus* from iCal and a slang
abbreviation from a letter would never merge into one course.

**An entry:** `name` (the name as in the source, after cleaning) → `title` (a short English name for Classes) +
`abbr` (up to 6 characters, the deadline prefix: `Calc`, `DM`) + `manual` (1 if set by hand).

**Cleaning a name (`clean`):** whitespace is collapsed, and a trailing `(рус)` is removed.

**`resolve(names)`:**

1. Runs under a lock: sources are polled at the same time on start, and without a lock one course would get two
   different answers.
2. Picks the names not yet in the dictionary.
3. Sends the new names to Haiku **in one call** together with the whole dictionary ("Already in the dictionary"), so
   Haiku sees that *Algebra* is the same course as its Russian name and gives the same `title` and `abbr`.
4. Measures `title` by the font; one retry if it does not fit.
5. Saves the entry with `manual=0`; `abbr` is cut to 6 characters.
6. If Haiku is unavailable, the source shows the name as it is and tries again on the next poll.

**Manual entries** come from the config (`subjects: [{name, title, abbr}]`, sent by HA). The service keeps the set of
manual entries exactly equal to the config: an entry removed from the config is translated by Haiku again. Haiku never
overwrites a manual entry (`upsert` with `where excluded.manual or not subjects.manual`).

The prompt is summarised in [§10.1](#101-the-course-dictionary-task-subjects).

### 2.3 The deadline model

All sources bring their deadlines to one structure:

```python
Deadline(
    title: str,          # "Calc HW 5", a finished English label
    due: date,           # a date only, no time
    category: str,       # "study" | "personal"
    source: str,         # "hseapp" | "ical" | "mail" | "tg" | "ha"
    course: str | None,  # abbr from the dictionary, for merging duplicates
    key: str = "",       # own deadlines from HA only: "ha-<id>"
)
```

---

## 3. Grades: Google Sheets

Code: `sources/grades.py`.

### 3.1 Access

- Sheets are shared by link; no Google login is needed.
- The sheet id comes from the link by the regex `/spreadsheets/d/([A-Za-z0-9_-]+)`.
- Data: `GET https://docs.google.com/spreadsheets/d/{id}/export?format=xlsx`, 120 s total timeout.
- The sheet title (needed only without a `label`): `GET …/{id}/htmlview`, the `<title>` without the
  " - Google Drive" suffix.
- A private sheet would need OAuth `spreadsheets.readonly` with the Google Cloud app in production (otherwise the
  token lives 7 days). Not implemented.

### 3.2 Settings

```yaml
grades:
  student: "Ivanov Ivan"      # surname and first name as the sheets spell them; required with sheets
  sheets:
    - url: https://docs.google.com/spreadsheets/d/…/edit
      label: Calc             # a short course name; without it, the first 8 characters of the sheet title
  window_days: 7              # how many days a grade counts as new
  poll_minutes: 30
  include_baseline: false     # for debugging: also show the grades of the first poll
```

### 3.3 Reading the workbook (`read_views`)

The workbook is opened with openpyxl (`read_only`, `data_only`: values, not formulas). Hidden tabs are skipped. For
every visible tab:

1. **Find the own row.** Cell text is normalised: lower case, `ё` → `е`, punctuation except `-` → space, whitespace
   collapsed. A cell matches if its first words are the words of `student`, so "Ivanov Ivan" finds
   "IVANOV Ivan Petrovich". The first match wins; its row and the name column are remembered. No own row means the
   tab is skipped.
2. **Where the header ends.** In the name column, the first row that looks like a person: 2–4 words, each
   capitalised and made of letters (`-` and `.` allowed). The header is up to 6 rows above it.
3. **Pick columns** that are non-empty in the header or in the own row.
4. **The result, `TabView`:** the tab name; the header as a list of `column letter → text` maps; the own row as
   `letter → raw value` without the name column; the name column's letter.

**To text:** a date without a time → `2026-09-17`, a date with a time → `2026-09-17 10:00`, an integral float → no
`.0`.

### 3.4 Tab layout by Haiku

- Haiku gets the tab name, the name column, the header rows and the own row as `C=value | D=…`. **It does not see
  other rows.**
- It returns JSON: `skip` (skip the tab), `reason`, `items[]` = `{column, label, max, bonus_column}`.
- **Cache:** the key `grades.layout:<sha256(tab, header, LAYOUT_VERSION)[:24]>:<budget>` in `kv`. While the header
  does not change, Haiku is not called. A prompt change bumps `LAYOUT_VERSION=2`, and the old cache stops matching.
- `max_tokens` 4000.

The prompt is summarised in [§10.2](#102-the-grade-tab-layout-task-grades).

**What the layout must produce on the real sheets** (this is the test): one column per dated homework (`HW Sep 17`)
while totals and activity tabs are skipped; seminars, project, LMS and exam columns; every work separately with its
"score" column, the 0–10 scale from the maximum in the second row and a separate `+1` bonus column, while per-problem
points, totals, statistics and archive tabs are skipped; the grade column after every quiz and homework while
per-problem points and averages are skipped.

### 3.5 Values (`extract`)

For every layout item:

1. The own row's value becomes a number (a string with `,` works too). Not a number: the item is skipped.
2. The maximum: `max` from the layout, 10 if `null`. A maximum ≤ 0 skips the item.
3. The 0–10 scale: with a maximum of 10 the value is taken as it is, otherwise `raw / max × 10`, rounded to 2 places.
4. The bonus is the number in `bonus_column`; zero means no bonus.
5. The item key is `"{sheet_id}|{tab}|{column}|{header text of the column}"`. It does not depend on `label`, so
   Haiku renaming a label does not create a "new grade".
6. The label is `"{course label} {work label}"` → `DM HW 1A`.

### 3.6 What counts as a new grade (`apply_changes`)

Three tables: `grade_sheets` (each sheet's `baseline_at`, the time of the first successful poll), `grade_items` (the
current value of every key) and `grade_events` (the journal of new grades).

- **The first poll of a sheet** only fills `grade_items` and sets `baseline_at`, without events. Otherwise the first
  day would dump everything on the screen.
- **Placeholder zeros.** A 0 without a bonus is skipped if the key did not exist before or was 0 too: it means "not
  graded yet". The price: a real zero never reaches the screen. A deliberate compromise.
- **A change** is a key that did not exist, or a changed value or bonus: a `grade_events` row with
  `seen_at = now`.
- Keys no longer in the sheet are deleted from `grade_items`.

### 3.7 Output

- `grade_events` with `seen_at` within the last `window_days` days, the latest event per key, newest first.
- With `include_baseline`, the other `grade_items` follow: in config order of the sheets, then by position, from the
  end.
- **On screen:** `<label>` on the left, the grade on the right; an integer without a fraction, otherwise one digit;
  the bonus after a space: `DM HW 1B 9.5 +1`, `Calc Quiz 2 6`.
- What does not fit becomes a `+N` counter in the heading. Empty: *no new grades*.

### 3.8 Failures

Failures are counted per sheet, in `status.grades.sheets[id]` (`error`, `ok_at`, `tabs`, `items`). The block shows:

- a Haiku key error → *no Haiku API key*;
- no sheet can be read → *sheets unavailable*;
- some cannot be read → *N sheet(s) failing*.

---

## 4. Classes: the RUZ iCal feed

Code: `sources/ical.py`.

### 4.1 The feed

- A URL like `https://api.hseapp.ru/v3/ruz/cal/<uuid>`, without a token. The link is **secret**. Set in `ical.url`
  (sent by HA), falling back to the `TABLO_ICAL_URL` variable.
- Polled every `poll_minutes` (60), 60 s timeout.
- `If-None-Match: <saved ETag>` is sent when both an ETag and saved events exist; a 304 means the saved events.
- API limit: 30 requests a minute (`x-ratelimit-reset: 60`).
- The feed covers about 5 weeks. `RRULE` and `STATUS` do not occur in it.
- **The language is mixed.** Past weeks are in Russian with a "(рус)" suffix and Russian class types; later weeks are
  in long English (*Introduction into Software Engineering*, type *Lecture*). Hence the dictionary (§2.2).

### 4.2 Parsing an event (the `icalendar` library, every `VEVENT`)

| Field | How it is obtained |
|---|---|
| skipped | `STATUS:CANCELLED`, an empty `SUMMARY` or no `DTSTART` |
| `subject` | `clean(SUMMARY)`, without "(рус)" |
| `start`, `end` | `DTSTART`/`DTEND` → local time without a zone; no `DTEND` → `end = start`; an all-day event → midnight |
| `kind` | the first line of `DESCRIPTION`: a lecture, *Seminar*, *Ongoing interim assessment* |
| `room` | a non-empty `LOCATION` that is not a link → the part before the first comma (`R609, <street address>` → `R609`); a link in `LOCATION`, an `http(s)://` link in `DESCRIPTION` or a `URL` → `online`; otherwise empty |
| `uid` | `UID` |

Events are sorted by (start, subject) and saved in `kv["ical.events"]`:
`{fetched_at, etag, events: [{uid, start, end, subject, room, kind}]}`.

**After every poll**, even a 304, `subjects.resolve(all courses)` runs, so names not translated while Haiku was
unavailable get another try.

### 4.3 Picking for the screen (`pick`, on every redraw, once a minute)

1. If today has classes with `end > now`: the first 3, heading *today*. A class in progress is an ordinary line.
2. Otherwise the nearest later day with classes, its first 3. The heading by the difference in days: 1 → *tomorrow*,
   2–6 → the weekday (*Tue*), 7 or more → the date (*Nov 3*).
3. No future classes: empty.

**A line:** `10:00 Algebra R401`: the start time, the dictionary `title` (or the original name without a
translation), the room.

### 4.4 Midterms → deadlines

If `kind` (case-insensitive) contains the Russian for "midterm assessment" or "interim assessment", the event stays a
class and **also** gives a deadline:

```
Deadline(title=f"{abbr} Midterm", due=start.date(), category="study", source="ical", course=abbr)
```

`abbr` comes from the dictionary. Without a dictionary entry the original name stands in for `abbr`, and
`course = None`.

### 4.5 Failures and what is not done

- No link → *calendar link not set*.
- The poll fails and the saved data is older than 12 h (or missing) → *timetable unavailable* instead of
  *no classes*.
- **Only iCal changes classes.** *moved* and *cancelled* marks appear only if the feed itself gives them. Letters and
  posts about moved classes reach only the news.
- A fallback if iCal stops being enough: the RUZ API in HSE App X (`GET /ruz/lessons`, needs a token, §5.1). Not
  used.

---

## 5. Deadlines

### 5.1 HSE App X

Code: `sources/hseapp.py`.

#### Authorisation (Keycloak, OpenID Connect)

| Parameter | Value |
|---|---|
| Realm | `https://saml.hse.ru/realms/hse/protocol/openid-connect` |
| Authorisation | `…/auth` |
| Tokens | `…/token` |
| `client_id` | `app-x-android`, a public client without a secret |
| Default `redirect_uri` | `ru.hse.hseappx://saml.hse.ru/authorize_callback` |
| `scope` | `profile email` |
| `access_token` lifetime | 3 h |
| `refresh_token` lifetime | 21 days, the window **slides** on every refresh |

**The first login: PKCE in a browser.**

1. `verifier = token_urlsafe(48)`, `challenge = base64url(sha256(verifier))` without `=`,
   `state = token_urlsafe(16)`. The service remembers `state → (verifier, redirect_uri)` pairs, the last 5.
2. The browser opens `…/auth?client_id=app-x-android&redirect_uri=<…>&response_type=code&scope=profile+email&state=<…>&code_challenge=<…>&code_challenge_method=S256`.
3. The client accepts **any http(s) `redirect_uri`**: localhost, local network addresses, any https. So the service
   sets its own `http://<service>/hseapp/callback`, and the browser comes back there after the login. Other custom
   schemes and `urn:ietf:wg:oauth:2.0:oob` are rejected.
4. `code` and `state` are taken from the return address, then `POST …/token` (form): `client_id`,
   `grant_type=authorization_code`, `code`, `redirect_uri`, `code_verifier`.
5. A fallback if HSE closes arbitrary `redirect_uri`s: log in with the `ru.hse.hseappx://…` address and paste the
   return address by hand. Safari loses it; Chrome with the console is needed.

**Refreshing the token:**

- `POST …/token` (form): `client_id=app-x-android`, `grant_type=refresh_token`, `refresh_token=<…>`.
- A refresh happens when less than 10 minutes of `access_token` life is left, and on a 401 from the API.
- **The `refresh_token` rotates**, so every answer is saved at once.
- The service polls every 15 minutes, so the token never expires.
- A 400 or 401 on refresh (`invalid_grant`: 21 days without a refresh, or a logout) erases the tokens; the state
  becomes "login needed".
- **Two instances must never refresh the same token:** rotation logs one of them out.

Stored in `kv["hseapp.auth"]`: `{access_token, expires_at, refresh_token, refresh_expires_at, refreshed_at}`, times
in unix seconds. `logged_in` means a `refresh_token` exists and `refresh_expires_at` is still in the future.

#### Fetching deadlines

```
POST https://api.hseapp.ru/v2/deadlines[?cursor=<next_cursor>]
Authorization: Bearer <access_token>
Accept-Language: ru
Content-Type: application/json

{"is_completed": false}      # the second request: {"is_completed": true}
```

- The answer: `{"next_cursor": null | "<str>", "items": [...]}`. Pagination by `next_cursor`, at most 10 pages; one
  is enough in practice.
- On a 401 the token is refreshed by force and the request repeated once.
- 60 s timeout.
- Other methods: one deadline `GET /v2/deadlines/{id}`, disciplines `GET /v2/deadlines/disciplines`, groups
  `GET /v2/deadlines/groups`.

**Item fields:**

| Field | Use |
|---|---|
| `id` | the key; `title` if missing |
| `title` | the deadline text in Russian, whitespace collapsed; empty skips the item |
| `discipline` | the course or `null` (personal ones). Cut **before the first " ("**, which drops the academic year, modules and the teacher's name |
| `expires_at` | ISO UTC; the end of a day is `20:59Z`, i.e. 23:59 MSK. Converted to the local zone, the **date** is taken. Invalid skips the item |
| `completion_status` | `ACTIVE`, `COMPLETED`, `COMPLETED_LATE` |
| the rest | `description`, `is_pinned`, `created_at`, `updated_at`, `groups`, `emails`, `created_by`, `preview_*`: unused |

**Filter:** from `is_completed:false` only `ACTIVE`; from `is_completed:true` the statuses starting with `COMPLETED`.
`is_completed:false` also returns overdue ones (a past due date); the screen picks among them (§5.6).

#### English labels

1. `discipline` goes through the dictionary (§2.2) for its `abbr`.
2. All deadlines, active and completed, **without a cached label** go to Haiku in one call, one line each:
   `- id <id>: "<title>" (course: <discipline>, abbreviation: <abbr>)`.
3. The answer: `{"items": [{"id", "label"}]}`. A label wider than 118 px gets one retry.
4. **Cached forever** under `hseapp.label:<sha256(version, budget, title, discipline, abbr)[:16]>`; a prompt change
   bumps `LABEL_VERSION=2`.
5. Haiku unavailable → the original Russian `title` is shown.

The prompt is summarised in [§10.3](#103-hse-app-x-deadline-labels-task-deadlines).

**Stored** in `kv["hseapp.deadlines"]`:

```json
{"fetched_at": "2026-10-20T12:00:00",
 "items": [{"id": "…", "label": "DM HW 5", "due": "2026-10-22", "course": "DM"}],
 "done":  [{"id": "…", "label": "Safety HW 2", "due": "2026-10-13", "course": "Safety"}]}
```

The lists are sorted by (date, title) and become `Deadline(label, due, "study", "hseapp", course)`.

**Failures:**

- `hseapp.enabled: false` → the source is silent.
- No login → *HSE login needed* as the block's first line, `hseapp.login_needed: true` in `/api/status`, and a
  `tablo_changed {kind: "status"}` event in HA.
- A network or API error with data older than 3 h → *HSE App unavailable*.

### 5.2 iCal

`<abbr> Midterm` deadlines from midterm events, see §4.4.

### 5.3 Mail and Telegram

Haiku extracts deadlines while parsing news (§6.4) and saves them to `news_deadlines`. The list takes rows without
`moved_by` and with `due ≥ today − 3 days`, mail first, then Telegram (the order matters for the merge), by date
within each.

`Deadline(label, due, "study", "mail" | "tg", course)`, where `course` is the dictionary `abbr`.

### 5.4 Own deadlines and hiding (HA to-do lists)

Code: `todo.py`. In HA all deadlines from the screen are visible as two to-do lists, "Tablo · Study" and
"Tablo · Personal".

**An item's `uid`:**

- own: `ha-<row id>`;
- the rest: `<source>-<sha1("source|lower-cased title with collapsed whitespace|due")[:10]>`.

| Action in HA | What the service does |
|---|---|
| An item with a due date added | a row in `own_deadlines` (title, due, category by list): `Deadline(…, source="ha", key="ha-<id>")` |
| An own item completed or deleted | the row is deleted |
| An item from a source completed or deleted | a `hidden_deadlines` row (uid, source, course, title, due). The item **never comes back**, even if the source sends it again; this is how Haiku's mistakes are removed |
| An item from a source edited | the original is hidden and an own copy with the new title or date created |
| An own item edited | the row is updated |

**Cleanup:** hidden and own items more than 30 days past their due date are deleted on every change of the lists.

After any change the screen is redrawn at once. When the lists or the HSE login state change, HA gets a
`tablo_changed` event with `kind` = `deadlines` or `status`.

### 5.5 The merge (`hub.deadlines()`)

```python
merged = merge_deadlines(
    hseapp.current(),                 # 1, the most reliable
    ical.deadlines(),                 # 2
    *((d,) for d in news.deadlines()),# 3 mail, then 4 Telegram; EACH as its own group
    todo.own(),                       # 5, own ones from HA
)
result = drop_hidden(drop_done(merged, hseapp.done()), todo.hidden())
```

**Comparing works:**

- `course(d)` is `d.course` or, without it, the first word of `title`, lower-cased.
- `work(d)` is the lower-cased `title` with collapsed whitespace, without the `course + " "` prefix:
  "Prog Midterm" → "midterm".
- `same_work(a, b)` is true if:
  - the `title`s match entirely (ignoring case and whitespace), **or**
  - the `course` matches, both `work`s are non-empty, and one contains the other **as whole words**, checked with
    `(?<!\w)part(?!\w)`: `hw 2` is in `hw 2 late` but not in `hw 20` or `hw 2b`.
- `same(a, b)` = `a.due == b.due` **and** `same_work(a, b)`.

**Steps:**

1. **`merge_deadlines`.** Groups are walked in order. An item is added unless it is `same` as one already accepted.
   Items within one group are not compared with each other, so news deadlines are passed one per group: two posts
   can bring the same work, and it must be merged.
2. **`drop_done`.** Drops ical, mail and tg deadlines for which HSE App X has a completed one with `same_work` and a
   date difference of **at most 14 days**. The window covers extensions: a letter carries the new date while HSE App
   X keeps the old one (this happened with a homework due on the 13th in the app and the 15th in a letter). The step
   leaves `hseapp` and `ha` deadlines alone.
3. **`drop_hidden`.** Drops deadlines (except `ha`) that are `same` as any of `hidden_deadlines`.

### 5.6 Picking and ordering for the screen (`visible_deadlines`)

- **Upcoming:** `due ≥ today`, by ascending date.
- Then **overdue:** `today − N ≤ due < today`, by descending date. N = 3 days, and 1 day for `mail` and `tg`: those
  sources cannot know whether the work was submitted.
- The same order goes to the HA to-do lists (`GET /api/deadlines`), where there can be more than 5 deadlines.

**A line:** the category icon on the left (study `mdi:book-open-variant`, personal `mdi:account`) and the source icon,
then the title (a 118 px budget), the due date on the right: `overdue`, `today`, `tomorrow`, otherwise a date like
`Oct 22`. Today's and tomorrow's lines are inverted. The block holds 5 lines of 22 px (one goes to a problem note if
there is one); the rest is `+N` in the heading.

| Source | Source icon |
|---|---|
| hseapp | `mdi:school` |
| mail | `mdi:email-outline` |
| tg | `mdi:send` |
| ical | `mdi:calendar` |
| ha | `mdi:home-outline` |

---

## 6. News

Code: `sources/telegram.py` and `sources/mail.py` collect, `news.py` parses with Haiku and serves. Collection and
parsing are separate: sources only add new records to `news_posts`, and Haiku reads them in batches.

### 6.1 Telegram: the web preview

- `GET https://t.me/s/<channel>`, without an account, 30 s timeout; it returns about the last 20 posts.
- **A channel setting:** `{name, study}`. The name can be given as `@name`, `name` or `https://t.me/name`; the service
  keeps `name`. Case-insensitive duplicates are rejected.
- **Frequency:** study channels (`study: true`) every 5 minutes, others every 15. The loop checks every minute which
  channels are due.

**Parsing the HTML** (the standard `HTMLParser`):

- **A post** is a `div` with `data-post="channel/<number>"` and the class `tgme_widget_message`; the class
  `service_message` marks a service post ("channel photo updated" and the like).
- **The time** is the first `<time datetime="…">` in the post (UTC); a post without one is skipped.
- **Fields** by the class of the nested `div`: `js-message_text` is the text (`<br>` becomes a line break),
  `js-message_reply_text` the text of the post replied to, `tgme_widget_message_forwarded_from` the forward origin
  (without "Forwarded from"), `tgme_widget_message_document_title` the file names (there can be several).
- Strings are cleaned: whitespace collapsed, at most two empty lines in a row.
- A page without posts is an error ("the channel is closed, renamed or has no web preview").

**The text for Haiku (`body`):**

```
Forwarded from: <…>          # if forwarded
Reply to: <up to 200 chars…> # if a reply
Files: a.pdf, b.docx         # if files
<text>
```

Cut to 8,000 characters. A service post or a post without text and files (a bare photo) is empty, and empty ones are
not saved.

**What is new:** `kv["telegram.last"]` holds the highest post number seen per channel; new posts have higher numbers.
**The first poll of a channel** (no number yet) takes only posts younger than `ttl_hours` (24 h); the rest only move
the number. The record key is `tg:<channel>/<number>`, the time is local, and `study` comes from the channel setting.

### 6.2 Mail: IMAP

| Parameter | Value |
|---|---|
| Mailbox | `@edu.hse.ru` only (Yandex 360) |
| Server | `imap.yandex.ru:993`, SSL, 60 s timeout |
| Login | `TABLO_MAIL_USER` and `TABLO_MAIL_PASSWORD` (an app password) |
| Folder | `INBOX`, opened **read-only** (`select … readonly=True`) |
| Reading | `BODY.PEEK[]`, letters are not marked as read |
| Polling | every 5 minutes |
| Enabled | `mail.enabled` and a login with a password |

**Which letters are new.** State: `kv["mail.state"] = {uidvalidity, last}`.

1. After `SELECT`, `UIDVALIDITY` and `UIDNEXT` (via `STATUS`) are read; `top = UIDNEXT − 1`.
2. The same `UIDVALIDITY` and a known `last`: if `top ≤ last`, there is nothing new; otherwise
   `UID SEARCH UID <last+1>:*`, dropping UIDs ≤ `last` from the result, because `n:*` always returns the newest letter.
3. Otherwise (the first start or a changed `UIDVALIDITY`): `UID SEARCH SINCE <the date 24 h ago>`, keeping only
   letters younger than 24 h by their `Date` header.
4. Every letter is `UID FETCH <uid> (BODY.PEEK[])`. At the end `last = max(top, the UIDs found)`.

**A letter → text:**

- **Sender:** the display name from `From`, or the address. **Subject:** `Subject`. **Date:** `Date`; without a zone
  it is UTC, unparseable means now. **Attachments:** file names.
- **Body:** `text/plain`. If fewer than 20 characters remain after cleaning, `text/html` converted to text: block tags
  give line breaks; `script`, `style`, `head`, `title`, `blockquote` and elements with the classes `gmail_quote` or
  `yandex_quote` are dropped.
- **Cutting quotes.** The text ends at the first line that looks like a quote header (`On … wrote:`, its Russian
  forms, the Yandex style `…<a@b.ru>:`, `--- Original Message ---` and its Russian form, a line of 10+ `_` from
  Outlook) or is the signature separator `-- `. Lines starting with `>` are dropped.
- **Cleaning:** invisible characters are removed (`U+00AD`, `U+034F`, `U+200B–200F`, `U+2060–2064`, `U+FEFF`, the
  padding in newsletter previews), a no-break space becomes a space, whitespace is collapsed, at most two empty lines
  in a row. An unreadable encoding is decoded as UTF-8 with replacement.

**The text for Haiku:**

```
Subject: <subject>
From: <sender>
Files: <attachments>   # if any

<body>
```

Cut to 8,000 characters. The key is `mail:<uidvalidity>/<uid>`. **Every letter of this mailbox is study**
(`study=1`). One broken letter does not stop the rest.

### 6.3 When Haiku is called

- A source saved new records → `wake(study)`. Parsing starts **at once** if they include study ones or the news
  column is empty; otherwise on a timer, **once an hour**.
- **Stale records:** a non-study record older than `ttl_hours` is marked read without calling Haiku, since it would
  disappear at once anyway. Old study records are read for their deadlines.
- Nothing waiting: Haiku is not called.
- **A batch is up to 20 records** (≈250 output tokens per record), `max_tokens` 16,000; more waiting means several
  calls in a row. The context is recomputed before every call.
- Haiku sees every record **once**: `parsed_at` is set after the call, even if the record is missing from the answer.
- A Haiku error (`LLMError`) leaves the records queued until next time.

### 6.4 The prompt and the answer

**The user message** (`prompt`):

```
Today: Tue 2026-10-20 14:05
Budgets: full at most 2 lines of 28 characters; short at most 28 characters; deadline label at most 14 characters.

Course dictionary:
- "<course name>" → Calc
- …                                   # or (empty)

Current news:
- tg:<channel>/1234: "<full>"         # live ones without replaced_by; or (none)

Upcoming deadlines from mail and Telegram:
- d12: "DM HW 3" 2026-10-21           # news_deadlines without moved_by, due ≥ today−3; or (none)

New posts:
- id tg:<channel>/23 · <channel> · study · posted Sat 2026-10-17 18:50
  <the text line by line, indented by 2 spaces>
  Your previous answer did not fit: <reason>. Write it shorter.   # on a retry only
```

The system prompt is summarised in [§10.4](#104-news-task-news).

**The answer, an item per record:**

```json
{"items": [{
  "id": "tg:<channel>/23",
  "full": "Calc KR-1 Oct 22/23 · Alg KR-1 & IDZ-1 Nov 7 18:10",
  "short": "Calc & Alg tests: Oct 22, Nov 7",
  "importance": 5,
  "replaces": "",
  "deadlines": [{"course": "<course name>", "label": "Calc KR-1", "due": "2026-10-22", "moves": ""}]
}]}
```

| Field | Meaning |
|---|---|
| `full` | up to 2 column lines, English, telegraphic |
| `short` | 1 line |
| `importance` | 1–5, the scale below |
| `replaces` | the `id` of a live item on the same topic or of an earlier record in this batch; `""` for none |
| `deadlines[].course` | the name **as in the dictionary** or `""` |
| `deadlines[].label` | `<abbr> <work>` or just the work (`NIS choice`) |
| `deadlines[].due` | `YYYY-MM-DD`; relative dates ("on Friday") count from the record's date; of several slots, the earliest |
| `deadlines[].moves` | the `dN` of a listed deadline that moved, or `""` |

Haiku does not return the source or the study flag: the service knows them.

**The `importance` scale:**

| | |
|---|---|
| 5 | something to do for studies soon: a deadline, a test, a sign-up or a choice with a date, a cancelled or moved class |
| 4 | study information; from city news only what changes plans today: closed metro stations, mobile internet switched off, a downpour or snowfall that stops transport |
| 3 | a notable item: the city in general, blocked services, other cities |
| 2 | background: drone and air defence reports, airport delays and closures, fog, minor news |
| 1 | ads, memes, posts with no content; not thrown away, but shown only if space is left |

Fellow students' questions and replies in a group mailing list that add no new facts get 1–2, and no deadlines are
taken from them.

**The service checks the answer:**

- Items with an `id` not from the batch or with empty `full` and `short` are dropped.
- If one of `full`/`short` is missing, the other stands in.
- `importance` is clamped to 1–5; 3 if missing.
- A deadline without a `label` or with an invalid date is dropped.
- **A retry by length:** an overlong `full` (over 2 lines), `short` (wider than a line) or deadline `label` → one retry
  of those records only. The reason names the target length: `full` ≈ `2 × 28 − 8` characters, `short` ≈ `28 − 3`.
- **After the retry:** if `full` still does not fit and `short` does, `full = short`, and the other way round. The
  renderer's "…" stays the last resort.

### 6.5 Saving (`_save`)

For every record of the batch:

1. `news_posts.parsed_at = now`.
2. `news_items` ← `(id = the record key, source, study, posted_at, full, short, importance)`.
3. **Replacing a topic.** If `replaces` points to an `id` that was in the prompt or earlier in this batch, the old item
   gets `replaced_by = the new id`. The old one disappears, and the new one's lifetime counts from its own time.
   Recurring reports on one topic (say, drones overnight and over the week) are one topic with one line on screen.
4. **Deadlines.** Each goes to `news_deadlines(item, source, course, label, due)`:
   - `course` = the dictionary `abbr` by name; without the name in the dictionary, the first word of `label` if it
     matches some `abbr`; otherwise `NULL`.
   - With `moves = dN` and `N` in the prompt, deadline `N` gets `moved_by` and is hidden.
   - Only mail and Telegram deadlines reach the prompt, so news **never moves** HSE App X or iCal deadlines.

### 6.6 Output

- **Live news:** `replaced_by IS NULL` and `posted_at ≥ now − ttl_hours` (24 h).
- **Rank:** `importance + study` (+1 for study); the newer one higher on a tie.
- **Laying out** the column: 20 px lines, 10 px gaps; a one-line item takes 30 px, a two-line one 50 px. Order:
  1. items with rank ≥ 5, as `full`;
  2. the rest by rank while they fit, as `short`;
  3. the remaining space: `full` for the rest by rank;
  4. `importance = 1` only fills what is left.
- The source icon on the left. Empty: *no news*.

### 6.7 What is not done

- Moved and cancelled classes are not parsed separately: that is ordinary news, and only iCal changes the timetable.
- Work mail is not connected.
- **Later:** reading Telegram chats through Telethon (a user account). This became infomaster's twin.

---

## 7. Settings and secrets

The config comes from the HA integration (`PUT /api/config`) and is stored in `data/config.yaml`; unknown keys are an
error. Secrets live in `.env`, and environment variables win over the file.

```yaml
display:
  tz_name: Europe/Moscow
grades:
  student: "Ivanov Ivan"
  sheets: [{url: "…", label: Calc}]
  window_days: 7
  poll_minutes: 30
  include_baseline: false
hseapp:
  enabled: true
  poll_minutes: 15            # the login itself is stored in the database, not in the config
ical:
  url: ""                     # empty → TABLO_ICAL_URL
  poll_minutes: 60
news:
  channels: [{name: <course_channel>, study: true}, <news_channel>]
  ttl_hours: 24               # news lifetime and the depth of the first poll
  study_poll_minutes: 5
  poll_minutes: 15
mail:
  enabled: true
  host: imap.yandex.ru
  port: 993
  folder: INBOX
  poll_minutes: 5             # user/password from the environment
subjects:                     # manual dictionary entries
  - {name: "<course name>", title: "Discrete Math", abbr: DM}
```

| Variable | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` (or `ANTHROPIC_AUTH_TOKEN`) | Haiku |
| `TABLO_ICAL_URL` | the RUZ link if not set in the config |
| `TABLO_MAIL_USER`, `TABLO_MAIL_PASSWORD` | IMAP |
| `TABLO_API_TOKEN` | Bearer for the service's HTTP API |

HSE App X tokens live in SQLite (`kv["hseapp.auth"]`), not in `.env`.

---

## 8. Tablo's HTTP API

The address on the host is `http://<host-ip>:8765`. Where a token is needed, the header is
`Authorization: Bearer <TABLO_API_TOKEN>`; without it the answer is 401. Bodies are JSON.

| Method and path | Token | What it does |
|---|---|---|
| `GET /api/status` | no | the state of all sources (below) |
| `GET /api/deadlines` | yes | the final deadline list after merging and hiding, in screen order, **without the 5-line limit** |
| `POST /api/deadlines` | yes | an own deadline: `{"title", "due": "YYYY-MM-DD", "category": "study"\|"personal"}` → `{"uid": "ha-12"}` |
| `PATCH /api/deadlines/{uid}` | yes | `{"done": true}` hides or deletes → `{"uid": null}`; `{"title"?, "due"?}` edits → `{"uid": "<new>"}`, and one from a source becomes an own copy |
| `DELETE /api/deadlines/{uid}` | yes | delete an own one or hide one from a source |
| `POST /api/refresh` | yes | `{"refetch": true}` or `{"sources": ["grades","hseapp","ical","news","weather"]}` polls now and redraws; `{"full": true}` also does a full screen refresh |
| `GET /api/config`, `PUT /api/config` | yes | the whole config |
| `GET /api/llm/runs?limit=&task=` | yes | the Haiku journal, newest first |
| `GET /hseapp` | no | the HSE App X login page |
| `GET /api/hseapp/login` | yes | `{"url": <a login link with the service's callback>, "logged_in"}` |
| `POST /api/hseapp/login` | yes | paste the return address by hand |
| `GET /hseapp/callback` | no | where the browser returns after the login |
| `GET /frame.png` | no | the current screen frame as an image |

**An example `GET /api/deadlines`:**

```json
{"items": [
  {"uid": "hseapp-3f2a9c01be", "title": "DM HW 5", "due": "2026-10-22",
   "category": "study", "source": "hseapp", "overdue": false}
]}
```

**`GET /api/status`, the source fields:**

```json
{
  "grades":  {"llm": true, "sheets": {"<sheet_id>": {"title", "ok_at", "error", "tabs", "items"}}},
  "hseapp":  {"logged_in", "ok_at", "error", "items", "done", "labels_error", "problem", "login_needed"},
  "ical":    {"ok_at", "error", "events", "subjects_error"},
  "news":    {"ok_at", "error", "items", "waiting"},
  "mail":    {"configured", "ok_at", "error", "last"},
  "telegram":{"waiting", "channels": {"<name>": {"ok_at", "error", "last"}}},
  "llm":     {…}, "synced_at": "…"
}
```

**The HA event:** `tablo_changed` with `{"kind": "deadlines"}` when the final deadline list changed, or
`{"kind": "status"}` when the HSE login state changed. If an event is lost, the integration still sees the change on
its own poll.

There are no endpoints for grades, classes and news: that data is visible only on the frame. The HA to-do lists work
through `/api/deadlines` and had to stay as they were when moving to an external service (§11.3).

---

## 9. SQLite storage

The file `data/tablo.db`. Times are local ISO without a zone, except `llm_runs.at` (UTC).

| Table | Columns | Purpose |
|---|---|---|
| `kv` | `key` PK, `value` JSON | source state and caches (keys below) |
| `grade_sheets` | `sheet_id` PK, `title`, `baseline_at` | when a sheet was first read |
| `grade_items` | `key` PK (`sheet\|tab\|col\|header`), `sheet_id`, `label`, `value` 0–10, `bonus`, `position` | current grades |
| `grade_events` | `id` PK, `key`, `label`, `value`, `bonus`, `seen_at` | the journal of new and changed grades |
| `subjects` | `name` PK, `title`, `abbr`, `manual` | the course dictionary |
| `news_posts` | `key` PK (`tg:chan/N`, `mail:V/UID`), `source` `tg`\|`mail`, `origin`, `posted_at`, `body`, `study`, `parsed_at` | the queue of records for Haiku |
| `news_items` | `id` PK (= the record key), `source`, `study`, `posted_at`, `full`, `short`, `importance`, `replaced_by` | parsed news |
| `news_deadlines` | `id` PK, `item`, `source`, `course` (abbr), `label`, `due`, `moved_by` | deadlines from mail and Telegram |
| `own_deadlines` | `id` PK, `title`, `due`, `category` | own deadlines from HA |
| `hidden_deadlines` | `uid` PK, `source`, `course`, `title`, `due` | hidden in HA |
| `llm_runs` | `id`, `at`, `task`, `model`, `ok`, `error`, tokens, `cost_usd`, `duration_s` | the Haiku journal |

**`kv` keys:**

| Key | Content |
|---|---|
| `ical.events` | `{fetched_at, etag, events[]}` |
| `hseapp.auth` | HSE App X tokens (**secret**) |
| `hseapp.deadlines` | `{fetched_at, items[], done[]}` |
| `hseapp.label:<hash>` | a label cache entry |
| `grades.layout:<hash>:<budget>` | a tab layout cache entry |
| `telegram.last` | `{channel: number}` |
| `mail.state` | `{uidvalidity, last}` |

---

## 10. Appendix: Haiku prompts

Every call uses `effort: low` and a JSON Schema answer. infomaster's own prompts live in its code
(`infomaster/processing/`); below is what Tablo's prompts asked for.

### 10.1 The course dictionary (task `subjects`)

`max_tokens` 2000. Name university courses for the dashboard: for every name, in Russian or English, return a short
English `title` within the character budget and an `abbr` of at most 6 characters used as the deadline prefix
("Calc HW 5"). Plain English without articles; shorten words rather than cut them ("Intro to Prog", not
"Introduction t"); keep the usual name when it fits. A fixed list of abbreviations for the common courses (Alg, Calc,
DM, Prog, SE, Eng, Hist, Safety, PE). The same course in Russian and in English gets the same title and abbr, and
courses already in the dictionary are listed so that their title and abbr are reused.

The user message gives the title budget, the existing entries and the new courses with ids (plus the reason on a
retry). The answer: `{"items": [{"id": string, "title": string, "abbr": string}]}`, all fields required, no extra
ones.

### 10.2 The grade tab layout (task `grades`)

`max_tokens` 4000. `{budget}` = 23 − the course label length − 1, at least 8. Map one tab of a teacher's sheet to the
graded works, seeing only the header rows and the own row as `column=value` pairs. A graded work is the overall result
of one homework, quiz, test, seminar, project, exam or extra sheet; per-problem points, totals, sums, averages, final
grades, attendance, activity points, comments and penalties are not. When a work has per-problem columns plus a total
column, the total is the work; when columns are bare numbers, the tab name says what the work is. Skip summary,
statistics and archive tabs. For every work: `column`, `max` (from a header row anywhere, 10 for a column that is
already 0–10, `null` if unknown), `bonus_column` and an English telegraphic `label` within the budget ("HW 1A",
"Quiz 2", "HW Sep 17"), with Cyrillic letter suffixes mapped by alphabet position (а→A, б→B, в→C); labels must differ.

The answer: `{"skip": boolean, "reason": string, "items": [{"column": string, "label": string, "max": number|null,
"bonus_column": string|null}]}`, all fields required, no extra ones.

### 10.3 HSE App X deadline labels (task `deadlines`)

`max_tokens` 2000. Write a short English label within the budget for every deadline from the university app (a Russian
title and, when known, the course). Telegraphic English, digits instead of words; start with the course abbreviation
(exactly the one given, if any), then the work ("DM HW 3"); fixed abbreviations for common works (HW, Quiz, Test,
Midterm, Exam, Lab, Essay, Project, Sem); drop words that add nothing on a deadline list; keep the work's numbers and
letters ("HW 2B"); known acronyms are expanded ("Course survey"). The answer: `{"items": [{"id": string,
"label": string}]}`.

### 10.4 News (task `news`)

`max_tokens` 16,000; the user message is described in §6.4. Write the news column of the dashboard from new Telegram
posts and university letters: for every post `full` (at most 2 lines), `short` (1 line), `importance` 1–5,
`replaces` and `deadlines`.

- **Style:** telegraphic English, no articles, digits, the essential fact first; fixed abbreviations; never name the
  channel or sender (an icon shows the source); no emoji, links or hashtags; stay within the budgets.
- **Importance:** the scale of §6.4, with the test "does this change what a student in the city does today? If not,
  it is not 4". A reply from a teacher or the study office, or one written to the student personally, is rated by its
  content.
- **Replacing:** a post that repeats or extends a current item about the same event replaces it, and the new text must
  tell the whole story, newest fact first, with the topic named; recurring reports on one topic are one running story;
  different events on a similar topic are not replaced; old and new text are never glued together.
- **Deadlines:** only work with a date (homework, tests, projects, sign-ups, choices, forms); the course exactly as in
  the dictionary; `<abbr> <work>` labels; relative dates count from the post date, the earliest of several slots; a
  deadline already listed with the same date is left out; a post that moves a listed deadline gives the new date and
  its id in `moves`.

The prompt ends with worked examples: a study post with several tests and a survey, a choice with a deadline, a moved
homework, a grading formula, a chain of airport reports replacing each other, drone reports, a metro closure, a wrong
and a right replacement, a classmate's question, weather small talk and an ad.

The answer: `{"items": [{"id": string, "full": string, "short": string, "importance": integer (1–5),
"replaces": string, "deadlines": [{"course": string, "label": string, "due": string, "moves": string}]}]}`, all fields
required, no extra ones.

---

## 11. An external data service: what Tablo needs as input

This section lists the requirements for the future data service: **which data** Tablo needs, **where** it enters
Tablo's processing, and **what Tablo does with it**. The exchange format (HTTP, WebSocket, push or polling) was left
to be decided with the code.

### 11.1 Requirements common to all blocks

- **Stable identifiers.** Every record (class, deadline, post, grade) needs an id that does not change between
  requests: "new grade", "new post", hiding deadlines in HA and replacing news depend on it.
- **Times with a zone or explicitly in Moscow time.** Tablo works in `Europe/Moscow`. Deadlines are needed as a
  **date** (no time) in Moscow: a 23:59 MSK deadline is that day, not the next one by UTC.
- **Freshness and errors per block.** The time of the last successful update (`ok_at`) and the current error, by which
  Tablo decides when to show *timetable unavailable* or *HSE login needed* instead of data (thresholds in §3.8, §4.5,
  §5.1). Plus a separate "HSE App X login needed" flag.
- **The last good data is never lost.** If the data service is unavailable, Tablo shows what it saved, so Tablo keeps
  a copy, and the service serves the full current state rather than only changes. The exception is new posts and new
  grades, which need a cursor (§11.4, §11.5).
- **"Refresh now".** `tablo.refresh_data` in HA (`POST /api/refresh`, §8) asks to poll the sources at once; the
  service must accept such a request.
- **Language.** Everything on screen is English, with length budgets by font (§2.1). Who translates is a decision of
  §11.6.
- **Other people's data.** Only the header and the own row of grade sheets leave the service. The rule moves to the
  service if it takes over grades.

### 11.2 Classes

**The minimum input** is the events for the coming weeks (the RUZ feed gives about 5):

| Field | Required | Note |
|---|---|---|
| `id` | yes | like the iCal `UID` |
| `start`, `end` | yes | local time |
| `subject` | yes | the name as in the source; Tablo translates it by the dictionary |
| `room` | yes | `R609`, `online` or empty; rules in §4.2 |
| `kind` | yes | the class type, by which Tablo finds midterms; can be replaced by an explicit `midterm` flag |
| `mark` | no | `moved` or `cancelled`; cancelled classes are simply dropped now, and if the service knows about a cancellation, Tablo can show a mark |

**Tablo then does itself:** the course dictionary, picking three classes and the heading (*today*, *tomorrow*…), the
`<abbr> Midterm` deadline (§4.3–4.4).

**Can be handed to the service:** translating names (then it serves `title` and `abbr`, and the dictionary lives
there, §11.6).

### 11.3 Deadlines

**The minimum input** is two lists from every external source: active and completed.

| Field | Required | Note |
|---|---|---|
| `id` | yes | stable |
| `title` | yes | a finished English label of up to ~15 characters, **or** the original text that Tablo then translates (§5.1) |
| `due` | yes | a Moscow date |
| `course` | desirable | the course name or `abbr`; without it duplicates merge only by the first word of the label (§5.5) |
| `source` | yes | it decides the icon, the reliability order in the merge and how many days an overdue one stays (§5.5–5.6); new sources mean new values and icons |
| `category` | yes | `study` or `personal` |
| `done` | yes | completed ones are not for display: they hide copies of the same work from less reliable sources (the 14-day window, §5.5) |

**Tablo keeps:**

- the HA to-do lists: own deadlines and hiding by `uid` (§5.4). This is the user interface, and it does not move;
- the final merge, dropping completed and hidden ones, picking and ordering for the screen (§5.5–5.6).

**To decide in advance:**

- **Who merges duplicates.** If the service merges, Tablo gets one list; if Tablo merges, the service serves lists per
  source and the §5.5 rules stay as they are. Hiding from HA is applied last, in Tablo, either way.
- **Deadlines from news.** If the service parses news (§11.4, level B), deadlines from posts and moves (`moves`) come
  from it too.
- **HSE App X in one place only.** The `refresh_token` rotates on every refresh (§5.1). If the service talks to HSE
  App X, Tablo's polling is turned off (`hseapp.enabled: false`) and the `/hseapp` login page moves to the service.

### 11.4 News

It can be connected at one of two levels.

**Level A: raw records, parsing stays in Tablo.** The service only collects; Tablo puts records into the
`news_posts` queue and parses them with Haiku as now (§6.3–6.5).

| Field | Note |
|---|---|
| `key` | stable and unique: `tg:channel/number`, `mail:…` |
| `source` | the icon; new sources mean new icons |
| `origin` | the channel or the sender, which Haiku sees |
| `posted_at` | the record's time |
| `body` | clean text up to 8,000 characters: no HTML, quotes or signatures (§6.1–6.2) |
| `study` | whether it is study, which affects the parsing speed and the rank |

A cursor is needed: Tablo takes only records newer than the last one received. The first start takes the last 24 h.

**Level B: finished news.** The service calls Haiku itself; Tablo only ranks and lays out (§6.6).

| Field | Note |
|---|---|
| `id`, `source`, `study`, `posted_at` | as at level A |
| `full`, `short` | up to 2 lines and 1 line of the column (28 characters a line) |
| `importance` | 1–5 on the scale of §6.4 |
| `replaced` | a flag that the item was replaced by a fresher one; such items are not shown |
| `deadlines[]` | go to deadlines (§11.3) |

At level B the service must know the screen budgets. The exact font check exists only in Tablo, so Tablo would cut a
long text with "…" and could not ask Haiku to rewrite it.

### 11.5 Grades

It can be connected at one of three levels.

| Level | What the service serves | What Tablo does |
|---|---|---|
| A | links to or files of the sheets | everything as now (§3) |
| B | a list of works: a stable `key`, the course (`label`), the work (`label`), points, maximum, bonus | the 0–10 scale, the snapshot, placeholder zeros, the journal of new ones, the 7-day window (§3.5–3.7) |
| C | finished "new grades" with `seen_at` | display only |

At level B or C the Haiku tab layout moves to the service together with the rule about other people's data (§11.1).
The `key` must depend on the position in the sheet, not on the label, or a renamed label gives a false "new grade"
(§3.5). The first export is a snapshot without events (§3.6).

### 11.6 Decisions before the work

1. **The processing boundary for every block.** Where the service ends and Tablo begins: the levels of §11.4–11.5,
   who merges deadlines (§11.3).
2. **Where the course dictionary lives.** It is one for all blocks (§2.2) and must stay one: either in Tablo, and the
   service serves original names, or in the service, and it serves `title` and `abbr` the same everywhere. The manual
   corrections from HA (`subjects`) must go to the same place.
3. **Where Haiku is.** If the service calls Haiku, it needs an Anthropic key and a way out through a VPN (the API is
   unavailable from Russia). The journal of calls and spending (`llm_runs`) then lives there too.
4. **Where the secrets are.** The HSE App X token, the mail password, the RUZ link, the sheet links, the full name.
   Every secret in exactly one place.
5. **New sources.** New deadline or news sources need a place in the reliability order (§5.5), an icon and an overdue
   display period (§5.6).
6. **Configuration from HA.** The HA integration configures sources through `PUT /api/config` in Tablo (§7). Decide
   whether those forms stay in Tablo, which passes the settings on to the service, or the integration talks to the
   service itself.
