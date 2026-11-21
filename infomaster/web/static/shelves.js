// The shelves (stage 16.8): the problem sheets with their PDFs and versions, the library's raw material by course
// and the sorting of its inbox. A sheet and an item have a card over the screen, as news have. Every change is the
// connector's tool behind the API: the toast offers the undo, but for a sheet joined or deleted, which has none.
import { useEffect, useState } from "preact/hooks";
import { ApiError, ICONS, api, changed, go, html, icon, mark, toast } from "./common.js";
import { day, getLang, t } from "./i18n.js";
import { getDecimal, useAct, useLoad } from "./screens.js";
import { GradeRow } from "./fixes.js";
import { fresh, m } from "./machine.js";

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
  caption: ["Your caption", "Ваша подпись"],
  editSheet: ["Edit", "Изменить"],
  course: ["Course", "Курс"],
  noCourse: ["No course", "Без курса"],
  pendingN: ["{n} not graded yet", "ещё не оценено: {n}"],
  bookSettings: ["Layout", "Разметка"],
  byHand: ["Added by hand", "Добавлены вручную"],
  bookAdded: ["Gradebook added: it is read in a minute", "Таблица добавлена: прочитается через минуту"],
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
  remove: ["Delete", "Удалить"],
  noCourse: ["No course", "Без курса"],
  pendingN: ["{n} not graded yet", "ещё не оценено: {n}"],
  bookSettings: ["Layout", "Разметка"],
  byHand: ["Added by hand", "Добавлены вручную"],
  bookAdded: ["Gradebook added: it is read in a minute", "Таблица добавлена: прочитается через минуту"],
  noDocs: ["Nothing in the knowledge base yet.", "В базе знаний пока ничего нет."],
  removeN: ["Delete {n}", "Удалить: {n}"],
  removed: ["Deleted", "Удалено"],
  openFile: ["Open the file", "Открыть файл"],
  openLink: ["Open the link", "Открыть ссылку"],
  transcripts: ["On it", "К нему"],
  fromWhere: ["from", "откуда"],
  statusNew: ["new", "новое"],
  statusProcessed: ["processed", "обработано"],
  lecture: ["lecture", "лекция"],
  seminar: ["seminar", "семинар"],
  classWord: ["class", "пара"],
  calc: ["Calculator", "Калькулятор"],
  calcTarget: ["Count", "Считать"],
  calcFrom: ["Counted from", "Из чего считается"],
  calcSheet: ["the sheet has {v}", "в таблице {v}"],
  calcReset: ["As the sheet has it", "Как в таблице"],
  calcNeed: ["How much is needed", "Сколько нужно"],
  calcFor: ["for", "чтобы получить"],
  calcOn: ["on", "по"],
  calcNeedIs: ["at least {x} of {max}", "не меньше {x} из {max}"],
  calcNever: ["not reachable even with the maximum, {max}", "не получится даже с максимумом, {max}"],
  calcAlready: ["reached already, whatever you get", "уже есть при любом результате"],
  ofMax: ["of {max}", "из {max}"],
  notYetShort: ["not graded", "не оценено"],
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

// A screen under the shelves: a way back, and no views bar (List, Week, News belong to the top screens).
function Screen({ title, sub, back, error, children }) {
  return html`<main class="screen">
    <header class="top">
      <a class="rb" href=${back} aria-label=${t("back")}>${icon(ICONS.back)}</a>
      <div><h1>${title}</h1>${sub && html`<div class="mut small">${sub}</div>`}</div>
    </header>
    <div class="err" role="alert">${error}</div>
    <div class="feed">${children}</div>
  </main>`;
}

const Row = ({ onClick, href, title, sub, aside, lead }) => {
  const body = html`${lead}<div><b>${title}</b>${sub && html`<div class="mut small">${sub}</div>`}</div>${aside}`;
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
      ${!data.sheets.length ? html`<div class="box"><div class="note">${s("noSheets")}</div></div>`
        : course ? html`<${SheetRows} sheets=${data.sheets} />`
        : byCourse(data.sheets, data.courses).map(([key, title, rows]) => html`<section key=${key || "-"}
            class="stack" style="gap:6px"><h2>${title}</h2><${SheetRows} sheets=${rows} /></section>`)}`}
  <//>`;
}

// All the sheets by course: the lenses' courses in their order, then other known courses, then no course.
function byCourse(sheets, courses) {
  const order = new Map(courses.map((c, i) => [c.key, i]));
  const groups = new Map();
  for (const e of sheets) {
    const key = e.course || null;
    if (!groups.has(key)) groups.set(key, [key, key ? e.course_title || key : s("noCourse"), []]);
    groups.get(key)[2].push(e);
  }
  const rank = (key) => (key === null ? Infinity : order.has(key) ? order.get(key) : courses.length);
  return [...groups.values()].sort((a, b) => rank(a[0]) - rank(b[0]));
}

const SheetRows = ({ sheets }) => html`<div class="box list">${sheets.map((e) => html`<${Row} key=${e.first}
  onClick=${() => go("/s", { id: e.id })} lead=${mark("sheet")} title=${sheetName(e)}
  sub=${[dueText(e), e.versions > 1 && s("versionsN", { n: e.versions }), e.own ? s("yours") : e.chat]
    .filter(Boolean).join(" · ")}
  aside=${e.status !== "posted" && html`<span class="chip plain">${e.status === "failed" ? s("notRead")
    : s("waiting")}</span>`} />`)}</div>`;

// --- a course's knowledge base and grades: the lens's tiles open these ------------------------------------------

const lensBack = (course) => "#/course?key=" + encodeURIComponent(course || "");

export function DocsList({ course, onOut }) {
  const { data, error } = useLoad("/course?key=" + encodeURIComponent(course || ""), onOut);
  return html`<${Screen} title=${(data && data.title) || course} sub=${t("knowledge")} back=${lensBack(course)}
      error=${error}>
    ${data && html`<div class="box">${data.documents.length ? data.documents.map((d) => html`
      <div class="doc" key=${d.id}><span><b>${d.title}</b><div class="mut small">${d.kind === "book" ? t("book")
        : d.kind === "cheatsheet" ? t("cheatsheet") : d.day ? day(d.day, { day: "numeric", month: "long" }) : ""}
        </div>${d.versions.length > 1 && html`<div class="mut small">${t("versions")}: ${d.versions.slice(1)
          .map((v) => v.url ? html`<a key=${v.version} href=${v.url} target="_blank" rel="noopener">v${v.version}</a> `
            : html`<span key=${v.version}>v${v.version} </span>`)}</div>`}
        </span>${d.url && html`<a class="sec" href=${d.url} target="_blank" rel="noopener">${d.kind === "cheatsheet"
          ? "MD" : "PDF"} v${d.versions[0].version}</a>`}</div>`)
      : html`<div class="note">${s("noDocs")}</div>`}</div>`}
  <//>`;
}

// A course's grades in full, in place of its gradebooks (stage 17.3): the average over the graded works, then each
// gradebook with its state and actions, its works tab by tab (points, bonus, the points of every problem on a tap,
// a zero before any grade «not graded yet»), and what its summary tabs show; the grades added by hand; a gradebook
// added for the course. The tile in the lens reads the same list, so the numbers agree.
export function GradesList({ course, onOut }) {
  const { data, error } = useLoad("/course?key=" + encodeURIComponent(course || ""), onOut);
  const status = useLoad("/machine/grades", onOut).data;
  const act = useAct();
  const g = data && data.grades;
  const read = (sid) => act.run(async () => {
    await api("POST", `/machine/grades/${encodeURIComponent(sid)}/read`, {});
    toast(m("reading"));
    setTimeout(changed, 5000);
  });
  const state = (sid) => (status ? status.sheets.find((b) => b.id === sid) : null);
  return html`<${Screen} title=${(data && data.title) || course} sub=${t("grades")} back=${lensBack(course)}
      error=${error || act.error}>
    ${g && html`<div class="stat dark grade-hero">
        <span class="lab">${t("byCourse")}</span>
        <big>${g.average === null ? "—" : String(g.average).replace(".", getDecimal())}<small> / 10</small></big>
        <small>${[g.count > 0 && t("worksN", { n: g.count }), g.pending > 0 && s("pendingN", { n: g.pending })]
          .filter(Boolean).join(" · ") || t("noGrades")}</small></div>
      ${g.books.map((b) => {
        const st = state(b.id), works = g.works.filter((w) => w.book === b.id);
        const tabs = [...new Set(works.map((w) => w.tab))];
        const back = encodeURIComponent(location.hash);
        return html`<section key=${b.id} class="stack" style="gap:8px">
          <h2>${b.name || (st && st.title) || b.label}</h2>
          <div class=${st && st.error ? "err small" : "mut small"}>${st && st.error ? st.error
            : st && st.ok_at ? `${m("read")} ${fresh(st.ok_at)}` : m("notYet")}</div>
          <div class="acts">
            <button class="sec" disabled=${act.busy} onClick=${() => read(b.id)}>${m("readNow")}</button>
            <a class="sec" href=${`#/machine/gradebook?id=${encodeURIComponent(b.id)}&back=${back}`}>
              ${icon(ICONS.edit, 16)}${s("bookSettings")}</a>
            <a class="sec" href=${b.url} target="_blank" rel="noopener">${icon(ICONS.link, 16)}${m("open")}</a>
          </div>
          ${tabs.map((tab) => html`<div key=${tab} class="box">
            ${tab && html`<div class="lab" style="padding-top:10px">${tab}</div>`}
            ${works.filter((w) => w.tab === tab).map((w) => html`<${GradeRow} key=${w.id} w=${w} />`)}</div>`)}
          ${b.summaries.map((sum) => html`<div key=${sum.tab} class="box">
            <div class="lab" style="padding-top:10px">${sum.tab}</div>
            <dl class="pairs">${sum.cells.map((c, i) => html`<dt key=${"t" + i}>${c.header || "—"}</dt>
              <dd key=${"d" + i}>${c.value}${c.how && html`<div class="how"><div class="mut small">
                ${c.how.text || html`<code>${c.how.formula}</code>`}</div></div>`}
                ${c.note && html`<div class="said-list"><p>${c.note}</p></div>`}</dd>`)}</dl></div>`)}
          ${b.calc && html`<${Calculator} course=${course} book=${b.id} />`}
        </section>`;
      })}
      ${g.works.some((w) => w.own) && html`<section class="stack" style="gap:8px"><h2>${s("byHand")}</h2>
        <div class="box">${g.works.filter((w) => w.own).map((w) => html`<${GradeRow} key=${w.id} w=${w} />`)}</div>
      </section>`}
      ${!g.works.length && !g.books.length && html`<div class="box"><div class="note">${t("noGrades")}</div></div>`}
      <${AddBook} course=${course} labelChars=${status ? status.label_chars : 10} />`}
  <//>`;
}

// The calculator of a gradebook (17.6): a formula cell of the own row (the course's grade by default), the works
// and cells it is counted from with the sheet's values, any of them changed to see the grade counted again as the
// sheet would, and «How much is needed»: the least result on one of them for a grade. Nothing is saved.
const dec = (v) => String(v).replace(".", getDecimal());

function Calculator({ course, book }) {
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState(null);
  const [values, setValues] = useState({});
  const [texts, setTexts] = useState({});
  const [need, setNeed] = useState({ cell: "", goal: "" });
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const goal = Number(String(need.goal).replace(",", "."));
  const solve = need.cell && need.goal !== "" && Number.isFinite(goal) ? { cell: need.cell, goal } : null;
  useEffect(() => {
    if (!open) return undefined;
    let alive = true;
    const timer = setTimeout(async () => {
      try {
        const answer = await api("POST", "/course/calc", { key: course, book, target, values, solve });
        if (!alive) return;
        setData(answer); setError("");
        if (!need.cell) {
          const first = answer.inputs.find((i) => i.pending) || answer.inputs[answer.inputs.length - 1];
          if (first) setNeed((n) => ({ ...n, cell: first.cell }));
        }
      } catch (e) {
        if (alive) setError(e instanceof ApiError ? e.message : String(e));
      }
    }, 250);
    return () => { alive = false; clearTimeout(timer); };
  }, [open, target, JSON.stringify(values), need.cell, need.goal]);
  if (!open) return html`<button class="sec" onClick=${() => setOpen(true)}>${icon(ICONS.calc, 18)}${s("calc")}</button>`;
  const put = (cell, text) => {
    setTexts({ ...texts, [cell]: text });
    const number = Number(text.replace(",", "."));
    const next = { ...values };
    if (text.trim() === "") next[cell] = null;
    else if (Number.isFinite(number)) next[cell] = number;
    setValues(next);
  };
  const reset = () => { setValues({}); setTexts({}); };
  const num = (v) => (v === null || v === undefined || v === "" ? "—" : typeof v === "number"
    ? dec(Math.round(v * 100) / 100) : String(v));
  const changedAny = Object.keys(values).length > 0;
  const sol = data && data.solve;
  const solIn = data && sol && data.inputs.find((i) => i.cell === sol.cell);
  return html`<div class="box calc">
    <div class="lab">${s("calc")}</div>
    <div class="err" role="alert">${error}</div>
    ${data && html`
      <label class="lab" for=${"ct-" + book}>${s("calcTarget")}</label>
      <select id=${"ct-" + book} class="field" value=${data.target}
        onChange=${(e) => { setTarget(e.target.value); setValues({}); setTexts({}); setNeed({ cell: "", goal: need.goal }); }}>
        ${data.targets.map((tg) => html`<option key=${tg.cell} value=${tg.cell}>${tg.tab} · ${tg.name}</option>`)}
      </select>
      <div class="res"><big>${num(data.value)}</big>
        ${changedAny && html`<span class="mut small">${s("calcSheet", {
          v: num((data.targets.find((tg) => tg.cell === data.target) || {}).value) })}</span>`}</div>
      ${data.how && html`<div class="how"><code>${data.how}</code></div>`}
      <div class="lab">${s("calcFrom")}</div>
      <div class="ins">${data.inputs.map((i) => html`
        <label key=${"l" + i.cell} for=${"ci-" + i.cell}>${i.name}<small>${[i.tab, i.max && s("ofMax", { max: dec(i.max) }),
          i.pending && s("notYetShort")].filter(Boolean).join(" · ")}</small></label>
        <input key=${"i" + i.cell} id=${"ci-" + i.cell} class=${"field" + (i.changed ? " changed" : "")}
          inputmode="decimal" value=${i.cell in texts ? texts[i.cell] : (i.sheet === null ? "" : num(i.sheet))}
          onInput=${(e) => put(i.cell, e.target.value)} />`)}</div>
      ${changedAny && html`<div><button class="sec sm" onClick=${reset}>${s("calcReset")}</button></div>`}
      <div class="lab">${s("calcNeed")}</div>
      <div class="need">${s("calcFor")}
        <input class="field narrow" inputmode="decimal" aria-label=${s("calcFor")} value=${need.goal}
          onInput=${(e) => setNeed({ ...need, goal: e.target.value })} />
        ${s("calcOn")}
        <select class="field" aria-label=${s("calcOn")} value=${need.cell}
          onChange=${(e) => setNeed({ ...need, cell: e.target.value })}>
          ${data.inputs.map((i) => html`<option key=${i.cell} value=${i.cell}>${i.name} (${i.tab})</option>`)}
        </select></div>
      ${sol && html`<div class="answer">${sol.need === null ? s("calcNever", { max: dec(sol.max) })
        : sol.need === 0 ? s("calcAlready")
        : s("calcNeedIs", { x: dec(sol.need), max: dec(solIn && solIn.max ? solIn.max : sol.max) })}</div>`}`}
  </div>`;
}

function AddBook({ course, labelChars }) {
  const [open, setOpen] = useState(false);
  const [url, setUrl] = useState("");
  const [label, setLabel] = useState((course || "").slice(0, labelChars));
  const act = useAct();
  const add = (event) => {
    event.preventDefault();
    act.run(async () => {
      const answer = await api("POST", "/machine/grades", { url: url.trim(), label: label.trim() });
      setUrl(""); setOpen(false);
      done(answer, s("bookAdded"));
    });
  };
  if (!open) return html`<button class="sec" onClick=${() => setOpen(true)}>${icon(ICONS.plus, 18)}${m("addBook")}</button>`;
  return html`<form class="box stack" style="gap:8px;padding:12px" onSubmit=${add}>
    <div class="err" role="alert">${act.error}</div>
    <input class="field" placeholder=${m("bookUrl")} value=${url} autocomplete="off" spellcheck="false" required
      onInput=${(e) => setUrl(e.target.value)} />
    <input class="field" placeholder=${m("bookLabel")} value=${label} maxlength=${labelChars}
      onInput=${(e) => setLabel(e.target.value)} />
    <div class="acts">
      <button class="btn" type="submit" disabled=${act.busy || !url.trim()}>${m("addBook")}</button>
      <button class="sec" type="button" onClick=${() => setOpen(false)}>${t("cancel")}</button>
    </div>
  </form>`;
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
        ${data.caption && html`<div class="stack" style="gap:4px"><span class="lab">${s("caption")}</span>
          <div style="white-space:pre-line">${data.caption}</div></div>`}
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
  lead=${mark(item.kind)} title=${item.title || item.name || item.url || kindOf(item.kind)}
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
  const remove = () => act.run(async () => {
    const answer = await api("POST", "/shelf/remove", { ids: chosen });
    setChosen([]);
    done(answer, s("removed"));
  });
  return html`<${Screen} title=${s("inboxRow")} back="#/shelf" error=${error || act.error}>
    ${data && html`
      <div class="box list">${data.items.length ? data.items.map((item) => html`<div class="row" key=${item.id}>
        <input type="checkbox" checked=${chosen.includes(item.id)} aria-label=${item.title || item.name || ""}
               onChange=${() => toggle(item.id)} style="width:22px;height:22px;flex:0 0 auto" />
        ${mark(item.kind)}
        <button style="flex:1;min-width:0;text-align:left;border:0;background:none;padding:0;font:inherit"
                onClick=${() => go("/i", { id: item.id })}>
          <b>${item.title || item.name || item.url || kindOf(item.kind)}</b>
          <div class="mut small">${[kindOf(item.kind), item.day && day(item.day, { day: "numeric", month: "short" }),
            item.author].filter(Boolean).join(" · ")}</div></button></div>`)
        : html`<div class="note">${s("inboxEmpty")}</div>`}</div>
      ${chosen.length > 0 && html`<div class="box"><${Place} courses=${data.courses} inbox=${true} busy=${act.busy}
        label=${s("sortN", { n: chosen.length })} onPick=${sort} />
        <button class="sec danger wide" style="margin-top:8px" disabled=${act.busy} onClick=${remove}>
          ${icon(ICONS.cross, 18)}${s("removeN", { n: chosen.length })}</button></div>`}`}
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
