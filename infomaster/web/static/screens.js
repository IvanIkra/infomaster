// The screens that read (stage 16.2): the feed with its lenses, a course's lens, the week, an item's card.
import { useEffect, useRef, useState } from "preact/hooks";
import { ApiError, ICONS, api, go, html, icon } from "./common.js";
import { day, kind, t, today } from "./i18n.js";

const hm = (iso) => iso.slice(11, 16);
const dateOf = (iso) => iso.slice(0, 10);
const daysBetween = (a, b) => Math.round((new Date(b + "T12:00:00") - new Date(a + "T12:00:00")) / 86400000);
const clock = (text) => Number(text.slice(0, 2)) * 60 + Number(text.slice(3, 5));  // "13:00" -> 780
const minutes = (iso) => clock(iso.slice(11, 16));
const capital = (text) => text.charAt(0).toUpperCase() + text.slice(1);

// Load a screen's JSON; again when the app comes back to the front, and every five minutes.
function useLoad(path, onOut) {
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
    const timer = setInterval(load, 300000);
    return () => { alive = false; document.removeEventListener("visibilitychange", visible); clearInterval(timer); };
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
  </div></div>`;
}

// --- the feed and a course's lens ----------------------------------------------------------------------------

export function Feed({ course, onOut }) {
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
      <a class="rb" href="#/machine" aria-label=${t("machineOk")}><span class="dot"></span></a>
    </header>
    ${data && html`<${Lenses} courses=${data.courses} active=${course} onPick=${pick} />`}
    <div class="err" role="alert">${error}</div>
    ${data && html`<div class="feed">
      ${!course && isIos() && !standalone() && html`<div class="hint">${t("installIos")}</div>`}
      ${course && info && info.key && html`<${CourseHead} info=${info} todayIso=${data.today} />`}
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
      ${data && (what === "d" ? html`<${DeadlineCard} d=${data} />` : html`<${ClassCard} c=${data} />`)}
      <button class="sec wide" onClick=${onClose}>${t("close")}</button>
    </div></section>`;
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

function DeadlineCard({ d }) {
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
    </div>`}`;
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
