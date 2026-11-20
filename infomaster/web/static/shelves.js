// The shelves (stage 16.8): the problem sheets with their PDFs and versions, the library's raw material by course
// and the sorting of its inbox. A sheet and an item have a card over the screen, as news have. Every change is the
// connector's tool behind the API: the toast offers the undo, but for a sheet joined or deleted, which has none.
import { useEffect, useState } from "preact/hooks";
import { ApiError, ICONS, api, changed, go, html, icon, toast } from "./common.js";
import { day, getLang, t } from "./i18n.js";
import { Bar, useAct, useLoad } from "./screens.js";

const TEXTS = {
  shelves: ["Shelves", "Полки"],
  inboxRow: ["Library inbox", "Входящие библиотеки"],
  inboxN: ["{n} to sort", "разобрать: {n}"],
  inboxEmpty: ["Nothing to sort.", "Разбирать нечего."],
  sheetsTitle: ["Problem sheets", "Листочки"],
  sheetsN: ["{n} sheets", "листочков: {n}"],
  noSheets: ["No sheets yet.", "Листочков пока нет."],
  rawTitle: ["Raw material", "Сырьё"],
  rawCourses: ["Raw material by course", "Сырьё по курсам"],
  itemsN: ["{n} items", "записей: {n}"],
  newN: ["{n} new", "новых: {n}"],
  noItems: ["Nothing here yet. New material goes in with «+».", "Здесь пока пусто. Новое добавляется кнопкой «+»."],
  libraryOff: ["The library is off.", "Библиотека выключена."],
  versionsN: ["{n} versions", "версий: {n}"],
  yours: ["yours", "ваш"],
  waiting: ["not read yet", "ещё не прочитан"],
  notRead: ["not read", "не прочитан"],
  due: ["due", "срок"],
  noDue: ["no due date", "срока нет"],
  fileGone: ["file gone", "файла нет"],
  versions: ["Versions", "Версии"],
  summary: ["What it asks", "О чём"],
  editSheet: ["Edit", "Изменить"],
  course: ["Course", "Курс"],
  noCourse: ["No course", "Без курса"],
  number: ["Number", "Номер"],
  dueDate: ["Due date", "Срок"],
  summaryField: ["Summary", "Описание"],
  save: ["Save", "Сохранить"],
  join: ["It is a newer version of…", "Это новая версия другого листочка…"],
  joinWarn: ["It cannot be undone: this sheet's own message goes, and its file takes the place of the older one's.",
             "Отменить нельзя: собственное сообщение листочка удалится, а его файл встанет на место старого."],
  noCandidates: ["There is no other sheet to join it to.", "Присоединять не к чему: других листочков нет."],
  deleteSheet: ["Delete the sheet", "Удалить листочек"],
  deleteWarn: ["Deletes the sheet with all its versions. It cannot be undone.",
               "Удалит листочек со всеми версиями. Отменить нельзя."],
  sure: ["Sure", "Точно"],
  addSheet: ["Add your own PDF", "Добавить свой PDF"],
  choosePdf: ["Choose a PDF", "Выберите PDF"],
  upload: ["Upload", "Загрузить"],
  sheetSaved: ["Sheet saved", "Листочек сохранён"],
  sheetJoined: ["Joined as a newer version", "Присоединён как новая версия"],
  sheetDeleted: ["Sheet deleted", "Листочек удалён"],
  sheetAdded: ["Sheet added", "Листочек добавлен"],
  sorted: ["Sorted", "Разобрано"],
  itemSaved: ["Saved", "Сохранено"],
  itemRemoved: ["Removed", "Убрано"],
  moveTo: ["Move to", "Переместить"],
  wholeCourse: ["The course as a whole, no class", "Курс целиком, без пары"],
  backToInbox: ["Back to the inbox", "Обратно во входящие"],
  chooseCourse: ["Choose the course", "Выберите курс"],
  pickClass: ["Class", "Пара"],
  sortN: ["Sort {n}", "Разобрать: {n}"],
  looseItems: ["No class", "Без пары"],
  titleField: ["Title", "Название"],
  noteField: ["Note", "Заметка"],
  markProcessed: ["Mark processed", "Отметить обработанным"],
  markNew: ["Mark new", "Отметить новым"],
  remove: ["Remove", "Убрать"],
  openFile: ["Open the file", "Открыть файл"],
  openLink: ["Open the link", "Открыть ссылку"],
  transcripts: ["On it", "К нему"],
  fromWhere: ["from", "откуда"],
  statusNew: ["new", "новое"],
  statusProcessed: ["processed", "обработано"],
  lecture: ["lecture", "лекция"],
  seminar: ["seminar", "семинар"],
  classWord: ["class", "пара"],
};
const KINDS = { photo: ["photo", "фото"], audio: ["audio", "аудио"], video: ["video", "видео"],
                notes: ["notes", "конспект"], slides: ["slides", "слайды"], sheet: ["sheet", "листочек"],
                link: ["link", "ссылка"], text: ["text", "текст"], transcript: ["transcript", "расшифровка"],
                file: ["file", "файл"] };
const s = (key, values) => {
  const pair = TEXTS[key];
  let text = pair ? pair[getLang() === "ru" ? 1 : 0] : key;
  for (const [name, value] of Object.entries(values || {})) text = text.replace(`{${name}}`, value);
  return text;
};
const kindOf = (kind) => (KINDS[kind] ? KINDS[kind][getLang() === "ru" ? 1 : 0] : kind);
const capital = (text) => text.charAt(0).toUpperCase() + text.slice(1);
const hm = (iso) => iso.slice(11, 16);
const dayOf = (iso) => capital(day(iso, { weekday: "short", day: "numeric", month: "short" }));
const classLabel = (c) => `${dayOf(c.start)}, ${hm(c.start)} · ${c.kind ? s(c.kind) : s("classWord")}`;
const sheetName = (e) => [e.course_title, e.number && "№" + e.number].filter(Boolean).join(" · ") || e.name;
const dueText = (e) => (e.due ? `${s("due")} ${e.due_assumed ? "≈ " : ""}${day(e.due, { day: "numeric", month: "short" })}`
  : s("noDue"));

// A change's toast with its undo, after the screens read again.
const done = (answer, text) => {
  changed();
  toast(text, answer.change && answer.change.undo ? answer.change.id : null);
};

function Screen({ title, sub, back, error, children, view }) {
  return html`<main class="screen">
    <header class="top">
      <a class="rb" href=${back} aria-label=${t("back")}>${icon(ICONS.back)}</a>
      <div><h1>${title}</h1>${sub && html`<div class="mut small">${sub}</div>`}</div>
    </header>
    <div class="err" role="alert">${error}</div>
    <div class="feed">${children}</div>
    <${Bar} view=${view || "list"} />
  </main>`;
}

const Row = ({ onClick, href, title, sub, aside }) => {
  const body = html`<div><b>${title}</b>${sub && html`<div class="mut small">${sub}</div>`}</div>${aside}`;
  return href ? html`<a class="row" href=${href}>${body}</a>`
    : html`<button class="row plain" onClick=${onClick}>${body}</button>`;
};

// --- the entry --------------------------------------------------------------------------------------------------

export function ShelfHome({ onOut }) {
  const { data, error } = useLoad("/shelf", onOut);
  return html`<${Screen} title=${s("shelves")} back="#/" error=${error}>
    ${data && html`
      <div class="box list">
        ${data.library && html`<${Row} href="#/inbox" title=${s("inboxRow")}
          sub=${data.inbox ? s("inboxN", { n: data.inbox }) : s("inboxEmpty")}
          aside=${data.inbox > 0 && html`<span class="count">${data.inbox}</span>`} />`}
        ${data.sheets !== null && html`<${Row} href="#/sheets" title=${s("sheetsTitle")}
          sub=${s("sheetsN", { n: data.sheets })} />`}
      </div>
      ${data.library && html`<h2>${s("rawCourses")}</h2>
        <div class="box list">${data.courses.length ? data.courses.map((c) => html`<${Row} key=${c.key}
          href=${"#/raw?course=" + encodeURIComponent(c.key)} title=${c.title}
          sub=${[s("itemsN", { n: c.items }), c.new && s("newN", { n: c.new })].filter(Boolean).join(" · ")} />`)
          : html`<div class="note">${s("noItems")}</div>`}</div>`}
      ${!data.library && html`<div class="note">${s("libraryOff")}</div>`}`}
  <//>`;
}

// --- the sheets -------------------------------------------------------------------------------------------------

function CourseSelect({ courses, value, onChange, required }) {
  return html`<select class="field" value=${value || ""} required=${required}
    onChange=${(e) => onChange(e.target.value || null)}>
    <option value="" disabled=${required}>${required ? s("chooseCourse") : s("noCourse")}</option>
    ${courses.map((c) => html`<option key=${c.key} value=${c.key}>${c.title}</option>`)}
  </select>`;
}

function AddSheet({ courses, course, onDone }) {
  const [file, setFile] = useState(null);
  const [form, setForm] = useState({ course: course || null, number: "", due: "" });
  const act = useAct();
  const send = (e) => {
    e.preventDefault();
    act.run(async () => {
      const body = new FormData();
      if (form.course) body.append("course", form.course);
      body.append("number", form.number.trim());
      body.append("due", form.due);
      body.append("file", file, file.name);
      let response;
      try {
        response = await fetch("/web/api/shelf/sheets", { method: "POST", credentials: "same-origin", body });
      } catch {
        throw new ApiError(0, t("offline"));
      }
      const answer = await response.json().catch(() => ({}));
      if (!response.ok) throw new ApiError(response.status, answer.error || t("failed"));
      onDone();
      done(answer, s("sheetAdded"));
    });
  };
  return html`<form class="box stack" style="gap:10px" onSubmit=${send}>
    <div class="err" role="alert">${act.error}</div>
    <input class="field" type="file" accept="application/pdf" required
           onChange=${(e) => setFile(e.target.files[0] || null)} />
    <${CourseSelect} courses=${courses} value=${form.course} onChange=${(v) => setForm({ ...form, course: v })} />
    <input class="field" placeholder=${s("number")} maxlength="20" value=${form.number}
           onInput=${(e) => setForm({ ...form, number: e.target.value })} />
    <input class="field" type="date" aria-label=${s("dueDate")} value=${form.due}
           onInput=${(e) => setForm({ ...form, due: e.target.value })} />
    <button class="btn" type="submit" disabled=${act.busy || !file}>${s("upload")}</button>
  </form>`;
}

export function SheetList({ course, onOut }) {
  const { data, error } = useLoad("/shelf/sheets" + (course ? "?course=" + encodeURIComponent(course) : ""), onOut);
  const [adding, setAdding] = useState(false);
  return html`<${Screen} title=${(data && data.title) || s("sheetsTitle")} sub=${course ? s("sheetsTitle") : null}
      back=${course ? "#/course?key=" + encodeURIComponent(course) : "#/shelf"} error=${error}>
    ${data && html`
      ${adding ? html`<${AddSheet} courses=${data.courses} course=${course} onDone=${() => setAdding(false)} />`
        : html`<button class="sec" onClick=${() => setAdding(true)}>${icon(ICONS.plus, 18)}${s("addSheet")}</button>`}
      <div class="box list">${data.sheets.length ? data.sheets.map((e) => html`<${Row} key=${e.first}
        onClick=${() => go("/s", { id: e.id })} title=${sheetName(e)}
        sub=${[dueText(e), e.versions > 1 && s("versionsN", { n: e.versions }), e.own ? s("yours") : e.chat]
          .filter(Boolean).join(" · ")}
        aside=${e.status !== "posted" && html`<span class="chip plain">${e.status === "failed" ? s("notRead")
          : s("waiting")}</span>`} />`) : html`<div class="note">${s("noSheets")}</div>`}</div>`}
  <//>`;
}

function SheetEdit({ sheet, courses, onDone }) {
  const [form, setForm] = useState({ course: sheet.course, number: sheet.number || "", due: sheet.due || "",
                                      summary: sheet.summary || "" });
  const act = useAct();
  const save = (e) => {
    e.preventDefault();
    act.run(async () => {
      const answer = await api("PATCH", `/shelf/sheets/${sheet.id}`, {
        course: form.course || "", number: form.number.trim(), due: form.due || null, summary: form.summary.trim() });
      onDone();
      done(answer, s("sheetSaved"));
    });
  };
  return html`<form class="stack" style="gap:10px" onSubmit=${save}>
    <div class="err" role="alert">${act.error}</div>
    <${CourseSelect} courses=${courses} value=${form.course} onChange=${(v) => setForm({ ...form, course: v })} />
    <input class="field" placeholder=${s("number")} maxlength="20" value=${form.number}
           onInput=${(e) => setForm({ ...form, number: e.target.value })} />
    <input class="field" type="date" aria-label=${s("dueDate")} value=${form.due}
           onInput=${(e) => setForm({ ...form, due: e.target.value })} />
    <textarea class="field" style="min-height:90px;padding:10px 12px" maxlength="600" placeholder=${s("summaryField")}
              value=${form.summary} onInput=${(e) => setForm({ ...form, summary: e.target.value })}></textarea>
    <div class="acts"><button class="btn" type="submit" disabled=${act.busy}>${s("save")}</button>
      <button class="sec" type="button" onClick=${onDone}>${t("cancel")}</button></div>
  </form>`;
}

export function SheetCard({ id, onClose, onOut }) {
  const { data, error } = useLoad(`/shelf/sheets/${encodeURIComponent(id)}`, onOut);
  const [mode, setMode] = useState(null);  // null, "edit", "join" or "delete"
  const act = useAct();
  const run = (job, text, closes) => act.run(async () => {
    const answer = await job();
    if (closes) { onClose(); await new Promise((ok) => setTimeout(ok, 50)); }
    setMode(null);
    done(answer, text);
  });
  return html`<div class="scrim" onClick=${onClose}></div>
    <section class="sheet" role="dialog" aria-label=${data ? sheetName(data) : ""}><div>
      <div class="grab" aria-hidden="true"></div>
      <div class="err" role="alert">${error || act.error}</div>
      ${data && html`
        <div><div class="kicker">${[dueText(data), data.own ? s("yours") : data.chat].filter(Boolean).join(" · ")}</div>
          <h1 style="font-size:24px">${sheetName(data)}</h1></div>
        ${data.status === "failed" && html`<div class="err">${s("notRead")}${data.error ? ": " + data.error : ""}</div>`}
        ${data.summary && html`<div class="stack" style="gap:4px"><span class="lab">${s("summary")}</span>
          <div>${data.summary}</div></div>`}
        <div class="stack" style="gap:0"><span class="lab">${s("versions")}</span>
          ${data.versions_list.map((v) => html`<div class="doc" key=${v.id}>
            <span><b>${v.name}</b><div class="mut small">${[v.version > 1 && "v" + v.version,
              dayOf(v.created_at)].filter(Boolean).join(" · ")}</div>
              ${v.changes && html`<div class="small">${v.changes}</div>`}</span>
            ${v.file_url ? html`<a class="sec" href=${v.file_url} target="_blank" rel="noopener">PDF</a>`
              : html`<span class="chip plain">${s("fileGone")}</span>`}</div>`)}</div>
        ${mode === null && html`<div class="acts">
          <button class="sec" onClick=${() => setMode("edit")}>${icon(ICONS.edit, 18)}${s("editSheet")}</button>
          ${data.status === "posted" && data.versions === 1 && html`<button class="sec"
            onClick=${() => setMode("join")}>${icon(ICONS.merge, 18)}${s("join")}</button>`}
          ${data.own && html`<button class="sec danger" onClick=${() => setMode("delete")}>
            ${icon(ICONS.cross, 18)}${s("deleteSheet")}</button>`}</div>`}
        ${mode === "edit" && html`<${SheetEdit} sheet=${data} courses=${data.courses}
          onDone=${() => setMode(null)} />`}
        ${mode === "join" && html`<div class="stack" style="gap:8px">
          <div class="note">${s("joinWarn")}</div>
          ${data.candidates.length ? data.candidates.map((c) => html`<button class="sec" key=${c.id} disabled=${act.busy}
            style="justify-content:flex-start" onClick=${() => run(
              () => api("POST", `/shelf/sheets/${data.id}/join`, { previous: c.id }), s("sheetJoined"), true)}>
            ${sheetName(c)}${c.versions > 1 ? ` · ${s("versionsN", { n: c.versions })}` : ""}</button>`)
            : html`<div class="note">${s("noCandidates")}</div>`}
          <button class="sec" onClick=${() => setMode(null)}>${t("cancel")}</button></div>`}
        ${mode === "delete" && html`<div class="stack" style="gap:8px">
          <div class="note">${s("deleteWarn")}</div>
          <div class="acts"><button class="btn" disabled=${act.busy} onClick=${() => run(
            () => api("DELETE", `/shelf/sheets/${data.id}`), s("sheetDeleted"), true)}>${s("sure")}</button>
            <button class="sec" onClick=${() => setMode(null)}>${t("cancel")}</button></div></div>`}`}
      <button class="sec wide" onClick=${onClose}>${t("close")}</button>
    </div></section>`;
}

// --- raw material and the inbox ---------------------------------------------------------------------------------

const ItemRow = ({ item, aside, onClick }) => html`<${Row} onClick=${onClick || (() => go("/i", { id: item.id }))}
  title=${item.title || item.name || item.url || kindOf(item.kind)}
  sub=${[kindOf(item.kind), item.day && day(item.day, { day: "numeric", month: "short" }),
    item.course_title].filter(Boolean).join(" · ")}
  aside=${aside || (item.status === "new" && html`<span class="chip plain">${s("statusNew")}</span>`)} />`;

export function RawList({ course, onOut }) {
  const { data, error } = useLoad("/shelf/raw?course=" + encodeURIComponent(course || ""), onOut);
  const sections = data ? data.classes.filter((c) => c.items_list.length) : [];
  return html`<${Screen} title=${(data && data.title) || s("rawTitle")} sub=${s("rawTitle")}
      back=${"#/course?key=" + encodeURIComponent(course || "")} error=${error}>
    ${data && html`
      ${sections.map((c) => html`<section key=${c.uid} class="stack" style="gap:6px"><h2>${classLabel(c)}</h2>
        <div class="box list">${c.items_list.map((item) => html`<${ItemRow} key=${item.id} item=${item} />`)}</div>
      </section>`)}
      ${data.items.length > 0 && html`<section class="stack" style="gap:6px"><h2>${s("looseItems")}</h2>
        <div class="box list">${data.items.map((item) => html`<${ItemRow} key=${item.id} item=${item} />`)}</div>
      </section>`}
      ${!sections.length && !data.items.length && html`<div class="empty">${s("noItems")}</div>`}`}
  <//>`;
}

// Where to put items: a course, and a class of it or none. `onPick` gets {course, class_uid} for the API.
function Place({ courses, inbox, onPick, label, busy }) {
  const [course, setCourse] = useState(null);
  const [back, setBack] = useState(false);  // «Back to the inbox»
  const [classes, setClasses] = useState([]);
  const [klass, setKlass] = useState("");
  useEffect(() => {
    setKlass("");
    if (!course) { setClasses([]); return undefined; }
    let alive = true;
    api("GET", "/shelf/classes?course=" + encodeURIComponent(course))
      .then((answer) => { if (alive) setClasses(answer.classes); }).catch(() => {});
    return () => { alive = false; };
  }, [course]);
  const ready = back || course;
  return html`<div class="stack" style="gap:8px">
    <select class="field" value=${back ? "-" : course || ""} aria-label=${s("moveTo")}
      onChange=${(e) => { const v = e.target.value; setBack(v === "-"); setCourse(v && v !== "-" ? v : null); }}>
      <option value="" disabled>${s("chooseCourse")}</option>
      ${courses.map((c) => html`<option key=${c.key} value=${c.key}>${c.title}</option>`)}
      ${!inbox && html`<option value="-">${s("backToInbox")}</option>`}
    </select>
    ${course && html`<select class="field" value=${klass} aria-label=${s("pickClass")}
      onChange=${(e) => setKlass(e.target.value)}>
      <option value="">${s("wholeCourse")}</option>
      ${classes.map((c) => html`<option key=${c.uid} value=${c.uid}>${classLabel(c)}</option>`)}
    </select>`}
    <button class="btn" disabled=${busy || !ready}
      onClick=${() => onPick(back ? { course: null } : klass ? { class_uid: klass } : { course })}>${label}</button>
  </div>`;
}

export function Inbox({ onOut }) {
  const { data, error } = useLoad("/shelf/inbox", onOut);
  const [chosen, setChosen] = useState([]);
  const act = useAct();
  const toggle = (id) => setChosen((old) => (old.includes(id) ? old.filter((i) => i !== id) : [...old, id]));
  const sort = (place) => act.run(async () => {
    const answer = await api("POST", "/shelf/sort", { ids: chosen, ...place });
    setChosen([]);
    done(answer, s("sorted"));
  });
  return html`<${Screen} title=${s("inboxRow")} back="#/shelf" error=${error || act.error}>
    ${data && html`
      <div class="box list">${data.items.length ? data.items.map((item) => html`<div class="row" key=${item.id}>
        <input type="checkbox" checked=${chosen.includes(item.id)} aria-label=${item.title || item.name || ""}
               onChange=${() => toggle(item.id)} style="width:22px;height:22px;flex:0 0 auto" />
        <button style="flex:1;min-width:0;text-align:left;border:0;background:none;padding:0;font:inherit"
                onClick=${() => go("/i", { id: item.id })}>
          <b>${item.title || item.name || item.url || kindOf(item.kind)}</b>
          <div class="mut small">${[kindOf(item.kind), item.day && day(item.day, { day: "numeric", month: "short" }),
            item.author].filter(Boolean).join(" · ")}</div></button></div>`)
        : html`<div class="note">${s("inboxEmpty")}</div>`}</div>
      ${chosen.length > 0 && html`<div class="box"><${Place} courses=${data.courses} inbox=${true} busy=${act.busy}
        label=${s("sortN", { n: chosen.length })} onPick=${sort} /></div>`}`}
  <//>`;
}

export function ItemCard({ id, onClose, onOut }) {
  const { data, error } = useLoad(`/shelf/items/${encodeURIComponent(id)}`, onOut);
  const [mode, setMode] = useState(null);  // null, "move" or "edit"
  const [form, setForm] = useState(null);
  const act = useAct();
  const run = (job, text, closes) => act.run(async () => {
    const answer = await job();
    if (closes) { onClose(); await new Promise((ok) => setTimeout(ok, 50)); }
    setMode(null);
    done(answer, text);
  });
  const edit = () => { setForm({ title: data.title || "", note: data.note || "" }); setMode("edit"); };
  return html`<div class="scrim" onClick=${onClose}></div>
    <section class="sheet" role="dialog" aria-label=${data ? data.title || data.name || "" : ""}><div>
      <div class="grab" aria-hidden="true"></div>
      <div class="err" role="alert">${error || act.error}</div>
      ${data && html`
        <div><div class="kicker">${[kindOf(data.kind), data.course_title, data.class && classLabel(data.class),
          data.day && !data.class && day(data.day, { day: "numeric", month: "long" })].filter(Boolean).join(" · ")}</div>
          <h1 style="font-size:24px;overflow-wrap:anywhere">${data.title || data.name || data.url || kindOf(data.kind)}</h1>
          <div class="mut small">${[data.author, data.status === "new" ? s("statusNew") : s("statusProcessed")]
            .filter(Boolean).join(" · ")}</div></div>
        ${data.note && html`<div class="note">${data.note}</div>`}
        ${data.text && html`<div style="white-space:pre-wrap;overflow-wrap:anywhere;max-height:40vh;overflow:auto">
          ${data.text}</div>`}
        <div class="acts">
          ${data.file_url && html`<a class="sec" href=${data.file_url} target="_blank" rel="noopener">
            ${icon(ICONS.file, 18)}${s("openFile")}</a>`}
          ${data.url && /^https?:/.test(data.url) && html`<a class="sec" href=${data.url} target="_blank"
            rel="noopener">${icon(ICONS.link, 18)}${s("openLink")}</a>`}</div>
        ${data.children.length > 0 && html`<div class="stack" style="gap:0"><span class="lab">${s("transcripts")}</span>
          ${data.children.map((c) => html`<div class="doc" key=${c.id}><span><b>${c.title || kindOf(c.kind)}</b>
            <div class="mut small">${c.text}</div></span></div>`)}</div>`}
        ${mode === null && html`<div class="acts">
          <button class="sec" onClick=${() => setMode("move")}>${icon(ICONS.next, 18)}${s("moveTo")}</button>
          <button class="sec" onClick=${edit}>${icon(ICONS.edit, 18)}${t("edit")}</button>
          <button class="sec" disabled=${act.busy} onClick=${() => run(() => api("PATCH", `/shelf/items/${data.id}`,
            { status: data.status === "new" ? "processed" : "new" }), s("itemSaved"))}>
            ${data.status === "new" ? s("markProcessed") : s("markNew")}</button>
          <button class="sec danger" disabled=${act.busy} onClick=${() => run(
            () => api("DELETE", `/shelf/items/${data.id}`), s("itemRemoved"), true)}>
            ${icon(ICONS.cross, 18)}${s("remove")}</button></div>`}
        ${mode === "move" && html`<div class="stack" style="gap:8px">
          <${Place} courses=${data.courses} inbox=${!data.course} busy=${act.busy} label=${s("moveTo")}
            onPick=${(place) => run(() => api("POST", "/shelf/sort", { ids: [data.id], ...place }), s("sorted"))} />
          <button class="sec" onClick=${() => setMode(null)}>${t("cancel")}</button></div>`}
        ${mode === "edit" && form && html`<form class="stack" style="gap:10px" onSubmit=${(e) => {
          e.preventDefault();
          run(() => api("PATCH", `/shelf/items/${data.id}`, { title: form.title.trim(), note: form.note.trim() }),
              s("itemSaved"));
        }}>
          <input class="field" placeholder=${s("titleField")} maxlength="200" value=${form.title}
                 onInput=${(e) => setForm({ ...form, title: e.target.value })} />
          <input class="field" placeholder=${s("noteField")} maxlength="500" value=${form.note}
                 onInput=${(e) => setForm({ ...form, note: e.target.value })} />
          <div class="acts"><button class="btn" type="submit" disabled=${act.busy}>${s("save")}</button>
            <button class="sec" type="button" onClick=${() => setMode(null)}>${t("cancel")}</button></div></form>`}`}
      <button class="sec wide" onClick=${onClose}>${t("close")}</button>
    </div></section>`;
}
