// The screens (stages 16.2, 16.3): the feed with its lenses and the «Check» questions, a course's lens, the week, an
// item's card with its actions, and the «Check» queue one question at a time.
import { useEffect, useRef, useState } from "preact/hooks";
import { ApiError, CHANGED, ICONS, act, api, changed, go, html, icon, toast } from "./common.js";
import { day, kind, t, today } from "./i18n.js";

const hm = (iso) => iso.slice(11, 16);
const dateOf = (iso) => iso.slice(0, 10);
const daysBetween = (a, b) => Math.round((new Date(b + "T12:00:00") - new Date(a + "T12:00:00")) / 86400000);
const clock = (text) => Number(text.slice(0, 2)) * 60 + Number(text.slice(3, 5));  // "13:00" -> 780
const minutes = (iso) => clock(iso.slice(11, 16));
const capital = (text) => text.charAt(0).toUpperCase() + text.slice(1);

// Load a screen's JSON; again when the app comes back to the front, and every five minutes.
export function useLoad(path, onOut) {
  const [state, setState] = useState({ data: null, error: "" });
  useEffect(() => {
    if (!path) return undefined;
    let alive = true;
    const load = async () => {
      try {
        const data = await api("GET", path);
        if (alive) setState({ data, error: "" });
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) onOut();
        else if (alive) setState((s) => ({ data: s.data, error: e.message }));
      }
    };
    load();
    const visible = () => { if (document.visibilityState === "visible") load(); };
    document.addEventListener("visibilitychange", visible);
    window.addEventListener(CHANGED, load);
    const timer = setInterval(load, 300000);
    return () => {
      alive = false; clearInterval(timer);
      document.removeEventListener("visibilitychange", visible); window.removeEventListener(CHANGED, load);
    };
  }, [path]);
  return state;
}

const isIos = () => /iPhone|iPad/.test(navigator.userAgent);
const standalone = () => window.matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;

const openItem = (item) => go(item.kind === "deadline" ? "/d" : "/c", { uid: item.uid });

function dueWhen(due, todayIso) {
  const n = daysBetween(todayIso, due);
  return n === 0 ? t("todayDue") : n > 0 ? t("inDays", { n }) : t("daysAgo", { n: -n });
}

function placeOf(item) {
  if (!item.place) return "";
  return item.online ? t("online") : item.place;
}

// --- rows ----------------------------------------------------------------------------------------------------

function Row({ item, todayIso }) {
  if (item.kind === "class") {
    const small = [placeOf(item), item.cancelled && t("cancelledClass"), ...item.notes,
                   item.due.length && `${t("handIn")}: ${item.due.map((d) => d.title).join(", ")}`].filter(Boolean);
    const title = `${item.title} · ${kind(item.type)}`;
    return html`<button class=${"it" + (item.past ? " past" : "")} onClick=${() => openItem(item)}>
      <span class="t">${item.all_day ? "—" : hm(item.start)}</span><span class="k kc"></span>
      <span class="ttl">${item.cancelled ? html`<s>${title}</s>` : title}<small>${small.join(" · ")}</small></span>
    </button>`;
  }
  const small = [item.course_title, item.from].filter(Boolean);
  return html`<button class="it" onClick=${() => openItem(item)}>
    <span class="t due">${item.at_class ? hm(item.at_class.start) : ""}</span><span class="k kd"></span>
    <span class="ttl"><b>${item.title}</b><small>${small.join(" · ")}</small></span>
    ${item.assumed && html`<span class="chip">${t("assumed")}</span>`}
    ${!item.at_class && todayIso && html`<span class="mut small">${dueWhen(item.due, todayIso)}</span>`}
  </button>`;
}

function dayTitle(date, todayIso) {
  const n = daysBetween(todayIso, date);
  if (n === 0) return [t("today"), day(date, { weekday: "short", day: "numeric", month: "short" })];
  if (n === 1) return [t("tomorrow"), day(date, { weekday: "short", day: "numeric", month: "short" })];
  return [capital(day(date, { weekday: "long" })), day(date, { day: "numeric", month: "long" })];
}

function NowCard({ item, now }) {
  const going = item.start <= now;
  const left = minutes(item.start) - minutes(now);
  const label = going ? `${t("goingOn")} · ${t("until")} ${hm(item.end)}`
    : left <= 120 ? t("inMinutes", { n: left }) : `${t("laterToday")} · ${hm(item.start)}`;
  const small = [`${hm(item.start)}–${hm(item.end)}`, placeOf(item),
                 item.due.length && `${t("handIn")}: ${item.due.map((d) => d.title).join(", ")}`].filter(Boolean);
  return html`<button class="now" onClick=${() => openItem(item)}>
    <span class="lab">${label}</span><b>${item.title} · ${kind(item.type)}</b><p>${small.join(" · ")}</p>
  </button>`;
}

function Days({ days, todayIso, skipToday }) {
  return days.filter((d) => d.items.length).map((d) => {
    const [title, note] = dayTitle(d.date, todayIso);
    return html`<section key=${d.date}>
      <div class="dh">${title}<span>${note}</span></div>
      ${d.items.map((item) => html`<${Row} key=${item.kind + item.uid} item=${item} todayIso=${todayIso} />`)}
    </section>`;
  });
}

function Lenses({ courses, active, onPick }) {
  const ref = useRef(null);
  useEffect(() => {  // the chosen course in view, also when it is far to the right
    const chosen = ref.current && ref.current.querySelector('[aria-selected="true"]');
    if (chosen) chosen.scrollIntoView({ block: "nearest", inline: "center" });
  }, [active, courses.length]);
  return html`<div class="lenses" role="tablist" aria-label="Lens" ref=${ref}>
    <button class="lens" role="tab" aria-selected=${!active} onClick=${() => onPick(null)}>${t("all")}</button>
    ${courses.map((c) => html`<button key=${c.key} class="lens" role="tab" aria-selected=${active === c.key}
      title=${c.title} onClick=${() => onPick(c.key)}>${c.key}</button>`)}
  </div>`;
}

export function Bar({ view, course }) {
  return html`<div class="bar"><div>
    <div class="views" role="tablist" aria-label="View">
      <button role="tab" aria-selected=${view === "list"} onClick=${() => go(course ? "/course" : "/", { key: course })}>
        ${t("list")}</button>
      <button role="tab" aria-selected=${view === "week"} onClick=${() => go("/week", { course })}>
        ${t("weekView")}</button>
    </div>
    <a class="plus" href="#/add" aria-label=${t("add")}>${icon(ICONS.plus, 26)}</a>
  </div></div>`;
}

// --- the feed and a course's lens ----------------------------------------------------------------------------

const PUSH_HINT = "infomaster.pushHint";  // «Not now» on the feed's offer of notifications, for this browser

function PushHint() {
  const [shown, setShown] = useState(() => {
    try { return localStorage.getItem(PUSH_HINT) !== "no"; } catch { return true; }
  });
  if (!shown || !("PushManager" in window) || (isIos() && !standalone())) return null;
  const later = () => {
    try { localStorage.setItem(PUSH_HINT, "no"); } catch { /* private mode: it shows again next time */ }
    setShown(false);
  };
  return html`<div class="box stack" style="gap:10px">
    <span class="note">${t("pushHint")}</span>
    <div class="acts"><a class="sec" href="#/machine/push">${t("turnOn")}</a>
      <button class="sec" onClick=${later}>${t("notNow")}</button></div>
  </div>`;
}

export function Feed({ course, push, onOut }) {
  const { data, error } = useLoad("/feed" + (course ? "?course=" + encodeURIComponent(course) : ""), onOut);
  const info = useLoad(course ? "/course?key=" + encodeURIComponent(course) : null, onOut).data;
  const pick = (key) => go(key ? "/course" : "/", { key });
  const { weekday, day: date } = today();
  const header = course
    ? html`<div><div class="kicker">${t("course")}</div><h1>${(info && info.title) || course}</h1></div>`
    : html`<div><div class="kicker">${weekday}${data ? ` · ${hm(data.now)}` : ""}</div>
        <h1>${date}</h1></div>`;
  return html`<main class="screen">
    <header class="top">
      ${header}
      <button class="rb" aria-label=${t("search")} disabled>${icon(ICONS.search)}</button>
      <a class="rb" href="#/machine" aria-label=${data && data.problems ? t("machineBad", { n: data.problems })
        : t("machineOk")}><span class=${data && data.problems ? "dot bad" : "dot"}></span></a>
    </header>
    ${data && html`<${Lenses} courses=${data.courses} active=${course} onPick=${pick} />`}
    <div class="err" role="alert">${error}</div>
    ${data && html`<div class="feed">
      ${!course && isIos() && !standalone() && html`<div class="hint">${t("installIos")}</div>`}
      ${!course && push === false && html`<${PushHint} />`}
      ${course && info && info.key && html`<${CourseHead} info=${info} todayIso=${data.today} />`}
      ${data.check.length > 0 && html`<${CheckBox} items=${data.check} course=${course} />`}
      ${!course && data.current && html`<${NowCard} item=${data.current} now=${data.now} />`}
      ${data.overdue.length > 0 && html`<section class="overdue">
        <div class="lab" style="color:var(--rev-fg)">${t("overdue")} · ${data.overdue.length}</div>
        ${data.overdue.map((item) => html`<${Row} key=${item.uid} item=${item} todayIso=${data.today} />`)}
      </section>`}
      ${course && html`<h2>${t("ahead")}</h2>`}
      <${Days} days=${data.days} todayIso=${data.today} />
      ${!data.days.some((d) => d.items.length) && !data.overdue.length &&
        html`<div class="empty">${t("nothingAhead")}</div>`}
    </div>`}
    <${Bar} view="list" course=${course} />
  </main>`;
}

function CourseHead({ info, todayIso }) {
  const [open, setOpen] = useState(null);
  const g = info.grades, n = info.next;
  const toggle = (name) => setOpen(open === name ? null : name);
  const shelf = (name, title, note, badge) => html`<button class="shelf" aria-expanded=${open === name}
    onClick=${() => toggle(name)}><b>${title}${badge ? html`<span class="count">${badge}</span>` : ""}</b>
    <span>${note}</span></button>`;
  return html`<div class="stack">
    <div class="stats">
      <button class="stat dark" onClick=${() => toggle("grades")}>
        <span class="lab">${t("byCourse")}</span>
        <big>${g.average === null ? "—" : String(g.average).replace(".", getDecimal())}<small> / 10</small></big>
        <small>${g.count ? t("worksN", { n: g.count }) + (g.new ? " · " + t("newN", { n: g.new }) : "")
          : t("noGrades")}</small>
      </button>
      <button class="stat" onClick=${() => n && openItem(n)}>
        <span class="lab">${t("nextClass")}</span>
        ${n ? html`<em>${capital(day(n.start, { weekday: "short", day: "numeric" }))}, ${hm(n.start)}</em>
          <small>${[kind(n.type), placeOf(n), n.due.length && `${t("handIn")}: ${n.due.map((d) => d.title)
            .join(", ")}`].filter(Boolean).join(" · ")}</small>` : html`<small>${t("noNext")}</small>`}
      </button>
    </div>
    <h2>${t("shelves")}</h2>
    <div class="shelves">
      ${shelf("docs", t("knowledge"), t("docsN", { n: info.documents.length }))}
      ${shelf("sheets", t("sheets"), t("sheetsN", { n: info.sheets.count }))}
      ${shelf("grades", t("grades"), g.count ? t("worksN", { n: g.count }) : t("noGrades"), g.new)}
      ${shelf("raw", t("raw"), t("rawN", { n: info.materials.count }), info.materials.new)}
    </div>
    ${open === "docs" && html`<div class="box">${info.documents.length ? info.documents.map((d) => html`
      <div class="doc" key=${d.id}><span><b>${d.title}</b><div class="mut small">${d.kind === "book" ? t("book")
        : d.kind === "cheatsheet" ? t("cheatsheet") : d.day ? day(d.day, { day: "numeric", month: "long" }) : ""}
        </div></span>${d.url && html`<a class="sec" href=${d.url} target="_blank" rel="noopener">PDF</a>`}</div>`)
      : html`<div class="note">—</div>`}</div>`}
    ${open === "grades" && html`<div class="box">${g.works.length ? g.works.map((w, i) => html`
      <div class="doc" key=${i}><span>${w.label}${w.new ? html` <span class="chip">new</span>` : ""}</span>
        <b>${w.value === null ? "—" : String(w.value).replace(".", getDecimal())}</b></div>`)
      : html`<div class="note">${t("noGrades")}</div>`}</div>`}
    ${(open === "sheets" || open === "raw") && html`<div class="note">${t("soon")}</div>`}
  </div>`;
}

const getDecimal = () => (document.documentElement.lang === "ru" ? "," : ".");

// --- the week ------------------------------------------------------------------------------------------------

const PPM = 0.62;  // pixels a minute in the week's grid

export function Week({ start, course, onOut }) {
  const query = new URLSearchParams(Object.entries({ start, course }).filter(([, v]) => v)).toString();
  const { data, error } = useLoad("/week" + (query ? "?" + query : ""), onOut);
  const [opened, setOpened] = useState(null);  // a day whose deadlines are listed
  const shift = (n) => {
    const monday = new Date(data.start + "T12:00:00");
    monday.setDate(monday.getDate() + 7 * n);
    go("/week", { start: monday.toISOString().slice(0, 10), course });
  };
  if (!data) return html`<main class="screen"><div class="err">${error}</div></main>`;
  const classes = data.days.flatMap((d) => d.items.filter((i) => i.kind === "class" && !i.all_day));
  const first = Math.min(9 * 60, ...classes.map((c) => minutes(c.start)));
  const last = Math.max(18 * 60, ...classes.map((c) => minutes(c.end)));
  const top = Math.floor(first / 60) * 60;
  const height = (Math.ceil(last / 60) * 60 - top) * PPM;
  const starts = [...new Set(classes.map((c) => c.start.slice(11, 16)))].sort();
  const sunday = data.days[6].date;
  const range = `${day(data.start, { day: "numeric" })} – ${day(sunday, { day: "numeric", month: "long" })}`;
  return html`<main class="screen">
    <header class="top">
      <div><div class="kicker">${data.days.some((d) => d.date === data.today) ? t("thisWeek") : t("weekView")}</div>
        <h1>${range}</h1></div>
      <button class="rb" aria-label=${t("prevWeek")} onClick=${() => shift(-1)}>${icon(ICONS.back)}</button>
      <button class="rb" aria-label=${t("nextWeek")} onClick=${() => shift(1)}>${icon(ICONS.next)}</button>
    </header>
    <${Lenses} courses=${data.courses} active=${course} onPick=${(key) => go("/week", { start: data.start, course: key })} />
    <div class="err" role="alert">${error}</div>
    <div class="wk">
      <div class="wk-head"><span></span>${data.days.map((d) => html`<div key=${d.date}
        class=${d.date === data.today ? "today" : ""}>${day(d.date, { weekday: "short" })}
        <b>${Number(d.date.slice(8))}</b></div>`)}</div>
      <div class="wk-due"><span>${t("deadlinesShort").toUpperCase()}</span>${data.days.map((d) => {
        const due = d.items.filter((i) => i.kind === "deadline");
        return html`<div key=${d.date}>${due.length > 0 && html`<button class=${d.date < data.today ? "past" : ""}
          onClick=${() => (due.length === 1 ? openItem(due[0]) : setOpened(d))}>
          <span class="k kd" style="width:6px;height:6px"></span>${due.length}</button>`}</div>`;
      })}</div>
      <div class="wk-grid" style=${{ height: height + "px" }}>
        ${starts.map((s) => html`<span key=${s} class="wk-time" style=${{ top: (clock(s) - top) * PPM + "px" }}>${s}</span>
          <div class="wk-line" style=${{ top: (clock(s) - top) * PPM + "px" }}></div>`)}
        <span></span>
        ${data.days.map((d) => html`<div key=${d.date} class=${"wk-col" + (d.date === data.today ? " today" : "")}>
          ${d.items.filter((i) => i.kind === "class" && !i.all_day).map((c) => html`<button key=${c.uid}
            class=${"wk-class" + (c.past ? " past" : "") + (c.due.length ? " hand" : "") + (c.cancelled ? " cancelled" : "")}
            style=${{ top: (minutes(c.start) - top) * PPM + 1 + "px",
                      height: Math.max(22, (minutes(c.end) - minutes(c.start)) * PPM - 2) + "px" }}
            title=${c.title} onClick=${() => openItem(c)}>
            <b>${c.course || c.title}</b><span>${kind(c.type)}</span></button>`)}
          ${d.date === data.today && minutes(data.now) >= top && minutes(data.now) <= top + height / PPM &&
            html`<div class="wk-now" style=${{ top: (minutes(data.now) - top) * PPM + "px" }}></div>`}
        </div>`)}
      </div>
    </div>
    ${opened && html`<div class="scrim" onClick=${() => setOpened(null)}></div>
      <section class="sheet" role="dialog"><div>
        <div class="grab"></div>
        <h2>${capital(day(opened.date, { weekday: "long", day: "numeric", month: "long" }))}</h2>
        <div>${opened.items.filter((i) => i.kind === "deadline").map((i) => html`<${Row} key=${i.uid} item=${i} />`)}</div>
      </div></section>`}
    <${Bar} view="week" course=${course} />
  </main>`;
}

// --- an item's card ------------------------------------------------------------------------------------------

export function Card({ kind: what, uid, onClose, onOut }) {
  const { data, error } = useLoad(`/${what === "d" ? "deadline" : "class"}?uid=${encodeURIComponent(uid)}`, onOut);
  return html`<div class="scrim" onClick=${onClose}></div>
    <section class="sheet" role="dialog" aria-label=${data ? data.title : ""}><div>
      <div class="grab" aria-hidden="true"></div>
      <div class="err" role="alert">${error}</div>
      ${data && (what === "d" ? html`<${DeadlineCard} d=${data} onClose=${onClose} />` : html`<${ClassCard} c=${data} />`)}
      <button class="sec wide" onClick=${onClose}>${t("close")}</button>
    </div></section>`;
}

// Run a change; its error stays on the screen, and `after` runs once it is done.
export function useAct(after) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const run = async (job) => {
    setBusy(true); setError("");
    try { await job(); if (after) after(); } catch (e) { setError(e.message); } finally { setBusy(false); }
  };
  return { busy, error, run };
}

function CourseLink({ course, title }) {
  if (!course) return null;
  return html`<button class="sec" style="min-height:34px" onClick=${() => go("/course", { key: course })}>
    ${title || course}${icon(ICONS.next, 16)}</button>`;
}

function Quote({ quote, link, from }) {
  if (!quote && !link) return null;
  return html`<div class="stack" style="gap:8px">
    <span class="lab">${t("from")}</span>
    ${quote && quote.text ? html`<blockquote>${quote.text}</blockquote>`
      : quote && html`<div class="note">${t("messageGone")}</div>`}
    <div style="display:flex;justify-content:space-between;gap:10px;font-size:12px" class="mut">
      <span>${[from, quote && quote.sender, quote && quote.at && day(quote.at, { day: "numeric", month: "short" })]
        .filter(Boolean).join(" · ")}</span>
      ${link && html`<a class="acc" href=${link} target="_blank" rel="noopener">${t("openMessage")}</a>`}
    </div>
  </div>`;
}

function DeadlineActions({ d, onClose }) {
  const [mode, setMode] = useState(null);  // null, "edit" or "twin"
  const { busy, error, run } = useAct();
  const doIt = (action, text, extra = {}) => run(() => act(action, { uid: d.uid, ...extra }, t(text), onClose));
  return html`<div class="stack" style="gap:10px">
    <div class="err" role="alert">${error}</div>
    ${mode === null && html`<div class="acts">
      <button class="btn" disabled=${busy} onClick=${() => doIt("done", d.own ? "deleted" : "markedDone")}>
        ${icon(ICONS.check)}${t("done")}</button>
      ${d.assumed && html`<button class="sec" disabled=${busy} onClick=${() => doIt("confirm", "confirmed")}>
        ${t("dateRight")}</button>`}
      <button class="sec" disabled=${busy} onClick=${() => setMode("edit")}>${icon(ICONS.edit, 18)}${t("edit")}</button>
      <button class="sec" disabled=${busy} onClick=${() => doIt("not-mine", d.own ? "deleted" : "hiddenNotMine")}>
        ${d.own ? t("delete") : t("notMine")}</button>
      <button class="sec" disabled=${busy} onClick=${() => setMode("twin")}>${t("isDuplicate")}</button>
    </div>`}
    ${mode === "edit" && html`<${EditForm} d=${d} busy=${busy} onCancel=${() => setMode(null)}
      onSave=${(fields) => doIt("edit", "saved", fields)} />`}
    ${mode === "twin" && html`<div class="box stack" style="gap:6px">
      <div class="note">${t("twinOf")}</div>
      ${d.twins.length ? d.twins.map((x) => html`<button key=${x.uid} class="it" disabled=${busy}
          onClick=${() => doIt("merge", "merged", { keep: x.uid })}>
          <span class="k kd"></span><span class="ttl"><b>${x.title}</b><small>${[shortDay(x.due), x.course_title,
            x.from].filter(Boolean).join(" · ")}</small></span>${icon(ICONS.merge, 18)}</button>`)
        : html`<div class="note">${t("noTwins")}</div>`}
      <button class="sec wide" onClick=${() => setMode(null)}>${t("cancel")}</button>
    </div>`}
  </div>`;
}

function EditForm({ d, busy, onSave, onCancel }) {
  const [title, setTitle] = useState(d.title);
  const [due, setDue] = useState(d.due);
  const [category, setCategory] = useState(d.category);
  const submit = (event) => {
    event.preventDefault();
    const fields = {};
    if (title.trim() !== d.title) fields.title = title.trim();
    if (due !== d.due) fields.due = due;
    if (category !== d.category) fields.category = category;
    if (Object.keys(fields).length) onSave(fields); else onCancel();
  };
  return html`<form class="box stack" style="gap:10px" onSubmit=${submit}>
    <label class="lab" for="e-title">${t("title")}</label>
    <input id="e-title" class="field" value=${title} maxlength="200" required
           onInput=${(e) => setTitle(e.target.value)} />
    <label class="lab" for="e-due">${t("date")}</label>
    <input id="e-due" class="field" type="date" value=${due} required onInput=${(e) => setDue(e.target.value)} />
    <div class="views" role="radiogroup" style="box-shadow:none;align-self:flex-start">
      ${["study", "personal"].map((c) => html`<button key=${c} type="button" role="radio"
        aria-checked=${category === c} aria-selected=${category === c} onClick=${() => setCategory(c)}>
        ${t(c)}</button>`)}
    </div>
    ${!d.own && html`<div class="note small">${t("editNote")}</div>`}
    <div class="acts">
      <button class="btn" type="submit" disabled=${busy || !title.trim() || !due}>${t("save")}</button>
      <button class="sec" type="button" onClick=${onCancel}>${t("cancel")}</button>
    </div>
  </form>`;
}

export const shortDay = (iso) => capital(day(iso, { weekday: "short", day: "numeric", month: "short" }));

function DeadlineCard({ d, onClose }) {
  const when = [capital(day(d.due, { weekday: "short", day: "numeric", month: "long" })),
                d.at_class && `${t("atClass")} ${hm(d.at_class.start)}`].filter(Boolean).join(", ");
  const what = { found: t("found"), moved: t("movedFrom"), hseapp: t("fromHseApp"), own: t("ownAdded") };
  return html`
    <div style="display:flex;justify-content:space-between;align-items:center;gap:10px">
      <span class="lab" style="color:var(--due);display:flex;align-items:center;gap:8px">
        <span class="k kd"></span>${t("deadline")}</span>
      <${CourseLink} course=${d.course} title=${d.course_title} />
    </div>
    <div>
      <h1>${d.title}</h1>
      <div class="when">${when} · ${dueWhen(d.due, todayIso())}</div>
      ${d.assumed && html`<div style="margin-top:8px"><span class="chip">${t("assumed")}</span></div>`}
    </div>
    <${Quote} quote=${d.quote} link=${d.link} from=${d.from} />
    ${d.history.length > 0 && html`<div class="stack" style="gap:8px">
      <span class="lab">${t("history")}</span>
      <ol class="history">${d.history.map((h, i) => html`<li key=${i}>
        <span>${h.at ? day(h.at, { day: "numeric", month: "short" }) : "—"}</span>
        <span>${what[h.what] || h.what}${h.what === "moved"
          ? html`: <s class="mut">${day(h.was, { day: "numeric", month: "short" })}</s> → ${day(h.due,
            { day: "numeric", month: "short" })}` : ""}${h.from && h.what !== "hseapp" ? html` · ${h.from}` : ""}</span>
      </li>`)}</ol>
    </div>`}
    <${DeadlineActions} d=${d} onClose=${onClose} />`;
}

function ClassCard({ c }) {
  const when = c.all_day ? capital(day(c.start, { weekday: "long", day: "numeric", month: "long" }))
    : `${capital(day(c.start, { weekday: "short", day: "numeric", month: "long" }))}, ${hm(c.start)}–${hm(c.end)}`;
  return html`
    <div style="display:flex;justify-content:space-between;align-items:center;gap:10px">
      <span class="lab">${kind(c.type)}${c.cancelled ? " · " + t("cancelledClass") : ""}</span>
      <${CourseLink} course=${c.course} title=${c.course_title} />
    </div>
    <div>
      <h1>${c.cancelled ? html`<s>${c.title}</s>` : c.title}</h1>
      <div class="when cls">${when}</div>
      ${c.place && html`<div class="mut" style="margin-top:4px">${c.online
        ? html`<a class="acc" href=${c.place} target="_blank" rel="noopener">${t("online")}</a>` : c.place}</div>`}
    </div>
    ${c.notes.length > 0 && html`<div class="note">${c.notes.join(" · ")}</div>`}
    ${c.due.length > 0 && html`<div class="stack" style="gap:4px"><span class="lab">${t("handedHere")}</span>
      ${c.due.map((d) => html`<button key=${d.uid} class="it" onClick=${() => go("/d", { uid: d.uid })}>
        <span class="k kd"></span><span class="ttl"><b>${d.title}</b></span></button>`)}</div>`}
    <${Quote} quote=${c.quote} link=${c.link} from=${c.origin} />
    ${c.materials > 0 && html`<div class="note">${t("materialsN", { n: c.materials })}</div>`}`;
}

const todayIso = () => {
  const now = new Date();
  return new Date(now.getTime() - now.getTimezoneOffset() * 60000).toISOString().slice(0, 10);
};

// --- the «Check» queue ---------------------------------------------------------------------------------------

function question(q) {
  if (q.kind === "duplicate") return t("sameQ", { a: q.items[0].title, b: q.items[1].title });
  if (q.kind === "assumed") return t("assumedQ", { title: q.item.title });
  return t("courseQ", { title: q.item.title });
}

// The feed's box: how many questions, the first two with their quick answer; a tap opens the queue at it.
function CheckBox({ items, course }) {
  const { busy, error, run } = useAct();
  const quick = (q) => {
    if (q.kind === "duplicate") {
      const drop = q.items.find((x) => x.uid !== q.keep);
      return [t("merge"), () => run(() => act("merge", { uid: drop.uid, keep: q.keep }, t("merged")))];
    }
    if (q.kind === "assumed") return [t("right"), () => run(() => act("confirm", { uid: q.item.uid }, t("confirmed")))];
    return [t("choose"), () => go("/check", { id: q.id, course })];
  };
  return html`<section class="check">
    <div class="lab">${t("checkPlease")} · ${items.length}</div>
    ${items.slice(0, 2).map((q) => {
      const [label, answer] = quick(q);
      return html`<div class="cq" key=${q.id}>
        <button class="cq-text" onClick=${() => go("/check", { id: q.id, course })}>${question(q)}</button>
        <button class="sec" disabled=${busy} onClick=${answer}>${label}</button>
      </div>`;
    })}
    ${items.length > 2 && html`<button class="acc cq-more" onClick=${() => go("/check", { course })}>
      ${t("moreN", { n: items.length - 2 })}</button>`}
    <div class="err" role="alert">${error}</div>
  </section>`;
}

export function CheckQueue({ start, course, onClose, onOut }) {
  const { data, error } = useLoad("/check" + (course ? "?course=" + encodeURIComponent(course) : ""), onOut);
  const [later, setLater] = useState([]);  // put off for this visit
  const [first, setFirst] = useState(start);
  const { busy, error: failed, run } = useAct();
  if (!data) {
    return html`<div class="scrim" onClick=${onClose}></div><section class="sheet" role="dialog"><div>
      <div class="err">${error}</div></div></section>`;
  }
  const open = data.items.filter((q) => !later.includes(q.id));
  const at = Math.max(0, open.findIndex((q) => q.id === first));
  const q = open[at];
  const next = () => setFirst(open[at + 1] ? open[at + 1].id : null);
  const answer = (action, data, text) => run(async () => { await act(action, data, t(text)); next(); });
  const aside = () => run(async () => {
    const answered = await api("POST", "/check/put-aside", { id: q.id });
    next();
    changed();
    toast(t("putAside"), answered.change.id);
  });
  return html`<div class="scrim" onClick=${onClose}></div>
    <section class="sheet" role="dialog" aria-label=${t("checkPlease")}><div>
      <div class="grab" aria-hidden="true"></div>
      ${q ? html`<span class="lab">${t("checkOf", { i: at + 1, n: open.length })}</span>
        ${q.kind === "duplicate" && html`<${SameQuestion} q=${q} busy=${busy}
          onMerge=${() => answer("merge", { uid: q.items.find((x) => x.uid !== q.keep).uid, keep: q.keep }, "merged")}
          onDifferent=${aside} />`}
        ${q.kind === "assumed" && html`<${AssumedQuestion} q=${q} busy=${busy}
          onRight=${() => answer("confirm", { uid: q.item.uid }, "confirmed")}
          onDate=${(due) => answer("edit", { uid: q.item.uid, due }, "saved")} />`}
        ${q.kind === "course" && html`<${CourseQuestion} q=${q} courses=${data.courses} busy=${busy}
          onPick=${(key) => answer("course", { uid: q.item.uid, course: key }, "courseSet")} onOutside=${aside} />`}
        <div class="err" role="alert">${failed || error}</div>
        <button class="sec wide" disabled=${busy} onClick=${() => { setLater([...later, q.id]); next(); }}>
          ${t("laterDecide")}</button>`
      : html`<h1>${later.length ? t("checkLater") : t("allChecked")}</h1>
        <button class="sec wide" onClick=${onClose}>${t("close")}</button>`}
    </div></section>`;
}

function Choice({ d, kept }) {
  return html`<div class=${"choice" + (kept ? " kept" : "")}>
    <b>${d.title}</b>
    <small>${[d.from, d.course_title].filter(Boolean).join(" · ")}</small>
    <span class="when">${shortDay(d.due)}${d.at_class ? `, ${t("atClass")} ${hm(d.at_class.start)}` : ""}</span>
    ${d.assumed && html`<span><span class="chip">${t("assumed")}</span></span>`}
  </div>`;
}

function SameQuestion({ q, busy, onMerge, onDifferent }) {
  const keep = q.items.find((x) => x.uid === q.keep);
  return html`<h1>${t("sameTitle")}</h1>
    <div class="note">${t("sameText", { course: keep.course_title || q.course })}</div>
    <div class="pair">${q.items.map((d) => html`<${Choice} key=${d.uid} d=${d} kept=${d.uid === q.keep} />`)}</div>
    <div class="box"><span class="lab">${t("stays")}</span>
      <div><b>${keep.title}</b> · ${shortDay(keep.due)}${keep.from ? ` · ${keep.from}` : ""}</div></div>
    <div class="acts">
      <button class="btn" disabled=${busy} onClick=${onMerge}>${icon(ICONS.merge)}${t("merge")}</button>
      <button class="sec" disabled=${busy} onClick=${onDifferent}>${t("different")}</button>
    </div>`;
}

function AssumedQuestion({ q, busy, onRight, onDate }) {
  const [due, setDue] = useState(null);
  return html`<h1>${t("assumedTitle")}</h1>
    <div class="note">${t("assumedText")}</div>
    <${Choice} d=${q.item} />
    ${due === null ? html`<div class="acts">
        <button class="btn" disabled=${busy} onClick=${onRight}>${icon(ICONS.check)}${t("right")}</button>
        <button class="sec" disabled=${busy} onClick=${() => setDue(q.item.due)}>${t("otherDate")}</button>
      </div>`
      : html`<form class="acts" onSubmit=${(e) => { e.preventDefault(); onDate(due); }}>
        <input class="field" type="date" value=${due} required onInput=${(e) => setDue(e.target.value)} />
        <button class="btn" type="submit" disabled=${busy || !due || due === q.item.due}>${t("save")}</button>
      </form>`}`;
}

function CourseQuestion({ q, courses, busy, onPick, onOutside }) {
  return html`<h1>${t("courseTitle")}</h1>
    <div class="note">${t("courseText")}</div>
    <${Choice} d=${q.item} />
    <div class="picks">${courses.map((c) => html`<button key=${c.key} class="lens" disabled=${busy} title=${c.title}
      onClick=${() => onPick(c.key)}>${c.key}</button>`)}</div>
    <button class="sec wide" disabled=${busy} onClick=${onOutside}>${t("outside")}</button>`;
}
