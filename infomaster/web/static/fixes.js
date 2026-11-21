// The corrections (stage 16.9): a class moved, cancelled or put in the optional calendar, a chat's change of it
// ignored, an event changed, hidden or deleted, the hidden deadlines and events with «Restore», a grade corrected.
// Every change is the connector's tool behind /fix, journaled: the toast offers its undo. A class's correction takes
// every field at once (the tool resets one it is not given), so the card's fields go with the owner's changes.
import { useEffect, useState } from "preact/hooks";
import { ApiError, ICONS, api, backTo, changed, html, icon, toast } from "./common.js";
import { day, getLang, kind, t } from "./i18n.js";

const TEXTS = {
  change: ["Change", "Изменить"],
  cancelClass: ["Mark cancelled", "Отметить отменённой"],
  cancelEvent: ["Marked cancelled", "Отмечено отменённым"],
  notCancelled: ["Not cancelled after all", "Всё-таки не отменена"],
  cancelHint: ["A cancelled one stays in the timetable, struck through.",
               "Отменённая остаётся в расписании зачёркнутой."],
  asItWas: ["As it was", "Как было"],
  asItWasHint: ["Takes your change back: the class is as the timetable and the chats have it.",
                "Убирает вашу правку: пара — как в РУЗ и в чатах."],
  copyHint: ["Your copy stands for the chat's event; «As it was» brings the chat's one back.",
             "Вместо события из чата — ваша копия; «Как было» вернёт событие из чата."],
  optional: ["In the optional calendar", "В необязательном календаре"],
  deleteEvent: ["Delete", "Удалить"],
  chatMoved: ["A chat changed it: {from}", "Изменено в чате: {from}"],
  chatIgnored: ["A chat's change is ignored: {from}", "Изменение из чата не учитывается: {from}"],
  ignore: ["Ignore it", "Не учитывать"],
  applyAgain: ["Apply it again", "Учитывать снова"],
  ruzHas: ["The timetable has: {when}", "В РУЗ: {when}"],
  title: ["Title", "Название"],
  date: ["Date", "Дата"],
  from: ["From", "Начало"],
  to: ["To", "Конец"],
  allDay: ["All day", "Весь день"],
  room: ["Room or link", "Аудитория или ссылка"],
  save: ["Save", "Сохранить"],
  cancel: ["Cancel", "Отмена"],
  saved: ["Saved", "Сохранено"],
  classCancelled: ["Class cancelled", "Пара отменена"],
  classBack: ["The class is back", "Пара возвращена"],
  hidden: ["Hidden", "Скрыто"],
  deletedHidden: ["Deleted. To bring it back: Machine → Hidden", "Удалено. Вернуть: Машина → Скрытое"],
  shown: ["Shown again", "Снова показано"],
  deleted: ["Deleted", "Удалено"],
  calendarOptional: ["In the optional calendar now", "Теперь в необязательном календаре"],
  calendarMain: ["In the main calendar now", "Теперь в основном календаре"],
  hiddenTitle: ["Hidden", "Скрытое"],
  hiddenSub: ["What you hid, to bring back", "Что вы скрыли — чтобы вернуть"],
  deadlines: ["Deadlines", "Сроки"],
  events: ["Events", "События"],
  noHiddenDeadlines: ["No hidden deadlines.", "Скрытых сроков нет."],
  noHiddenEvents: ["No hidden events ahead.", "Скрытых событий впереди нет."],
  restore: ["Restore", "Вернуть"],
  restored: ["Restored", "Возвращено"],
  grade: ["Grade, 0 to 10", "Оценка, от 0 до 10"],
  label: ["Work", "Работа"],
  sheetHas: ["The sheet has {value}", "В таблице — {value}"],
  points: ["{raw} of {max}", "{raw} из {max}"],
  bonusN: ["bonus {n}", "бонус {n}"],
  pending: ["not graded yet", "ещё не оценено"],
  checking: ["being graded", "проверяется"],
  checkingNote: ["Some of the group have points for it and you have 0: it is being graded, or yours is not yet. In no average until most of the group is graded.",
                 "У части группы уже есть баллы, у вас 0: работу проверяют, или вашу ещё не проверили. В среднее не идёт, пока не проверят большинство."],
  pendingNote: ["The sheet shows 0 and no one in the group has points for it yet: not graded, so in no average.",
                "В таблице 0, и баллов за неё пока нет ни у кого в группе: не проверена, в среднее не идёт."],
  parts: ["By problem", "По задачам"],
  comments: ["Comments", "Комментарии"],
  howCounted: ["How it counts", "Как считается"],
  hasComment: ["a comment", "есть комментарий"],
  asSheet: ["As the sheet has it", "Как в таблице"],
  deleteGrade: ["Delete", "Удалить"],
  gradeSaved: ["Grade saved", "Оценка сохранена"],
  gradeReset: ["The sheet's grade is back", "Оценка — как в таблице"],
  gradeDeleted: ["Grade deleted", "Оценка удалена"],
  deleteGradeAsk: ["Delete this grade?", "Удалить эту оценку?"],
  yours: ["yours", "ваша"],
  edited: ["corrected", "исправлена"],
};
export const f = (key, values) => {
  const pair = TEXTS[key];
  let text = pair ? pair[getLang() === "ru" ? 1 : 0] : key;
  for (const [name, value] of Object.entries(values || {})) text = text.replace(`{${name}}`, value);
  return text;
};
const hm = (iso) => iso.slice(11, 16);
const capital = (text) => text.charAt(0).toUpperCase() + text.slice(1);
const shortWhen = (start, end) => capital(day(start, { weekday: "short", day: "numeric", month: "short" })) +
  (start.length > 10 ? `, ${hm(start)}${end ? "–" + hm(end) : ""}` : "");
const round = (value) => Math.round(value * 100) / 100;
const mark = (value) => (value === null || value === undefined ? "—"
  : String(value).replace(".", getLang() === "ru" ? "," : "."));

// A change through /fix: `before` runs first (a card closes: an event's copy has another uid), then the screens read
// again and the toast offers the undo, with the tool's note when it has one.
async function fixIt(path, data, text, before) {
  const answer = await api("POST", "/fix" + path, data);
  if (before) {
    before();
    await new Promise((done) => setTimeout(done, 50));
  }
  changed();
  toast(answer.change.note ? `${text}: ${answer.change.note}` : text, answer.change.undo ? answer.change.id : null);
  return answer;
}

function useRun() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const run = async (job) => {
    setBusy(true); setError("");
    try { await job(); } catch (e) { setError(e.message); } finally { setBusy(false); }
  };
  return { busy, error, run };
}

// --- a class's or an event's card --------------------------------------------------------------------------------

function TimeForm({ fields, withTitle, busy, onSave, onCancel }) {
  const whole = fields.end === null;
  const [title, setTitle] = useState(fields.title || "");
  const [date, setDate] = useState(fields.start.slice(0, 10));
  const [allDay, setAllDay] = useState(whole);
  const [from, setFrom] = useState(whole ? "10:00" : hm(fields.start));
  const [to, setTo] = useState(whole ? "11:20" : hm(fields.end));
  const [room, setRoom] = useState(fields.room || "");
  const submit = (event) => {
    event.preventDefault();
    const span = allDay ? { start: date, end: null } : { start: `${date}T${from}`, end: `${date}T${to}` };
    onSave({ ...(withTitle ? { title: title.trim() } : {}), ...span, room: room.trim() });
  };
  return html`<form class="box stack" style="gap:10px" onSubmit=${submit}>
    ${withTitle && html`<label class="lab" for="x-title">${f("title")}</label>
      <input id="x-title" class="field" value=${title} maxlength="80" required
             onInput=${(e) => setTitle(e.target.value)} />`}
    <label class="lab" for="x-date">${f("date")}</label>
    <input id="x-date" class="field" type="date" value=${date} required onInput=${(e) => setDate(e.target.value)} />
    ${withTitle && html`<label style="display:flex;gap:8px;align-items:center">
      <input type="checkbox" checked=${allDay} onChange=${(e) => setAllDay(e.target.checked)} />${f("allDay")}</label>`}
    ${!allDay && html`<div style="display:flex;gap:10px">
      <div class="stack" style="gap:4px;flex:1"><label class="lab" for="x-from">${f("from")}</label>
        <input id="x-from" class="field" type="time" value=${from} required onInput=${(e) => setFrom(e.target.value)} />
      </div>
      <div class="stack" style="gap:4px;flex:1"><label class="lab" for="x-to">${f("to")}</label>
        <input id="x-to" class="field" type="time" value=${to} required onInput=${(e) => setTo(e.target.value)} />
      </div>
    </div>`}
    <label class="lab" for="x-room">${f("room")}</label>
    <input id="x-room" class="field" value=${room} maxlength="200" onInput=${(e) => setRoom(e.target.value)} />
    <div class="acts">
      <button class="btn" type="submit" disabled=${busy || !date || (withTitle && !title.trim())
        || (!allDay && to <= from)}>${f("save")}</button>
      <button class="sec" type="button" onClick=${onCancel}>${f("cancel")}</button>
    </div>
  </form>`;
}

function Optional({ on, busy, onChange }) {
  return html`<label style="display:flex;justify-content:space-between;align-items:center;gap:10px">
    <span>${f("optional")}</span>
    <span class="switch"><input type="checkbox" role="switch" checked=${on} disabled=${busy}
      aria-label=${f("optional")} onChange=${(e) => onChange(e.target.checked)} /><span></span></span>
  </label>`;
}

function ClassFix({ c, onClose }) {
  const fix = c.fix;
  const [editing, setEditing] = useState(false);
  const { busy, error, run } = useRun();
  // A class keeps its uid through every correction: the card stays open and reads it again.
  const correct = (changes, text) => run(async () => {
    await fixIt("/class", { uid: c.uid, ...fix.fields, ...changes }, text);
    setEditing(false);
  });
  const option = (id, flags, text) => run(() => fixIt("/options", { id, ...flags }, text));
  const ruz = fix.ruz;
  const movedFromRuz = ruz.start !== fix.fields.start || ruz.room !== fix.fields.room;
  return html`<div class="stack" style="gap:10px">
    <div class="err" role="alert">${error}</div>
    ${fix.chat && html`<div class="box stack" style="gap:8px">
      <div class="note">${f(fix.chat.hidden ? "chatIgnored" : "chatMoved", { from: fix.chat.from || "—" })}</div>
      <button class="sec" disabled=${busy} onClick=${() => option(fix.chat.id, { hidden: !fix.chat.hidden },
        f(fix.chat.hidden ? "shown" : "hidden"))}>${f(fix.chat.hidden ? "applyAgain" : "ignore")}</button>
    </div>`}
    ${(movedFromRuz || fix.own) && html`<div class="note small">${f("ruzHas", {
      when: shortWhen(ruz.start, ruz.end) + (ruz.room ? ` · ${ruz.room}` : "") })}</div>`}
    ${editing ? html`<${TimeForm} fields=${fix.fields} busy=${busy} onCancel=${() => setEditing(false)}
        onSave=${(span) => correct(span, f("saved"))} />`
      : html`<div class="acts">
        <button class="sec" disabled=${busy} onClick=${() => setEditing(true)}>${icon(ICONS.edit, 18)}${f("change")}
        </button>
        <button class="sec" disabled=${busy} onClick=${() => correct({ cancelled: !fix.fields.cancelled },
          f(fix.fields.cancelled ? "classBack" : "classCancelled"))}>
          ${fix.fields.cancelled ? f("notCancelled") : f("cancelClass")}</button>
        ${fix.own && html`<button class="sec" disabled=${busy} title=${f("asItWasHint")}
          onClick=${() => run(() => fixIt("/remove", { id: fix.own }, f("asItWas")))}>${f("asItWas")}</button>`}
      </div>`}
    ${!editing && !fix.fields.cancelled && html`<div class="note small">${f("cancelHint")}</div>`}
    <${Optional} on=${!fix.fields.required} busy=${busy}
      onChange=${(on) => correct({ required: !on }, f(on ? "calendarOptional" : "calendarMain"))} />
  </div>`;
}

function EventFix({ c, onClose }) {
  const fix = c.fix;
  const [editing, setEditing] = useState(false);
  const { busy, error, run } = useRun();
  const edit = (fields, text) => run(() => fixIt("/event", { uid: c.uid, ...fields }, text, onClose));
  // An edit of a chat's event makes an own copy with another uid, and a removed one is gone: those close the card;
  // the calendar switch keeps it.
  const option = (flags, text, close = true) => run(() => fixIt("/options", { id: fix.id, ...flags }, text,
                                                                 close ? onClose : undefined));
  const remove = (text) => run(() => fixIt("/remove", { id: fix.id }, text, onClose));
  return html`<div class="stack" style="gap:10px">
    <div class="err" role="alert">${error}</div>
    ${fix.copy && html`<div class="note small">${f("copyHint")}</div>`}
    ${editing ? html`<${TimeForm} fields=${fix.fields} withTitle busy=${busy} onCancel=${() => setEditing(false)}
        onSave=${(fields) => edit(fields, f("saved"))} />`
      : html`<div class="acts">
        <button class="sec" disabled=${busy} onClick=${() => setEditing(true)}>${icon(ICONS.edit, 18)}${f("change")}
        </button>
        <button class="sec" disabled=${busy} onClick=${() => edit({ cancelled: !fix.fields.cancelled },
          f(fix.fields.cancelled ? "classBack" : "cancelEvent"))}>
          ${fix.fields.cancelled ? f("notCancelled") : f("cancelClass")}</button>
        ${fix.copy && html`<button class="sec" disabled=${busy} onClick=${() => remove(f("asItWas"))}>
          ${f("asItWas")}</button>`}
      </div>`}
    <${Optional} on=${!fix.required} busy=${busy}
      onChange=${(on) => option({ required: !on }, f(on ? "calendarOptional" : "calendarMain"), false)} />
    ${!editing && html`<button class="sec danger wide" disabled=${busy} onClick=${() => (fix.own && !fix.copy
      ? remove(f("deleted")) : option({ hidden: true }, f("deletedHidden")))}>${icon(ICONS.cross, 18)}
      ${f("deleteEvent")}</button>`}
  </div>`;
}

// The card's corrections: a RUZ class's or an event's, as the server says what stands behind it.
export function EntryFix({ c, onClose }) {
  if (!c.fix) return null;
  return c.fix.of === "class" ? html`<${ClassFix} c=${c} onClose=${onClose} />`
    : html`<${EventFix} c=${c} onClose=${onClose} />`;
}

// --- the hidden lists -------------------------------------------------------------------------------------------

export function Hidden({ onOut }) {
  const [data, setData] = useState(null);
  const { busy, error, run } = useRun();
  const [loadError, setLoadError] = useState("");
  const load = async () => {
    try { setData(await api("GET", "/fix/hidden")); setLoadError(""); } catch (e) {
      if (e instanceof ApiError && e.status === 401) onOut(); else setLoadError(e.message);
    }
  };
  useEffect(() => {
    load();
    window.addEventListener("infomaster:changed", load);
    return () => window.removeEventListener("infomaster:changed", load);
  }, []);
  const restore = (path, body) => run(() => fixIt(path, body, f("restored")));
  return html`<main class="screen">
    <header class="top">
      <a class="rb" href=${backTo("#/machine")} aria-label=${t("back")}>${icon(ICONS.back)}</a>
      <div><h1>${f("hiddenTitle")}</h1><div class="mut small">${f("hiddenSub")}</div></div>
    </header>
    <div class="err" role="alert">${loadError || error}</div>
    ${data && html`<div class="stack mach">
      <section class="stack" style="gap:6px"><h2>${f("deadlines")}</h2>
        ${data.deadlines.length ? html`<div class="box list">${data.deadlines.map((d) => html`<div class="row"
            key=${d.uid}><div><b>${d.title}</b><div class="mut small">${[shortWhen(d.due), d.course, d.reason, d.from]
              .filter(Boolean).join(" · ")}</div></div>
            <button class="sec" disabled=${busy} onClick=${() => restore("/hidden/deadline", { uid: d.uid })}>
              ${f("restore")}</button></div>`)}</div>`
          : html`<div class="note">${f("noHiddenDeadlines")}</div>`}
      </section>
      <section class="stack" style="gap:6px"><h2>${f("events")}</h2>
        ${data.events.length ? html`<div class="box list">${data.events.map((e) => html`<div class="row" key=${e.id}>
            <div><b>${e.title}</b><div class="mut small">${[shortWhen(e.start, e.end), kind(e.type), e.chat]
              .filter(Boolean).join(" · ")}</div></div>
            <button class="sec" disabled=${busy} onClick=${() => restore("/options", { id: e.id, hidden: false })}>
              ${f("restore")}</button></div>`)}</div>`
          : html`<div class="note">${f("noHiddenEvents")}</div>`}
      </section>
    </div>`}
  </main>`;
}

// --- a grade in the course's lens -------------------------------------------------------------------------------

export function GradeRow({ w }) {
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(w.value === null ? "" : String(w.value));
  const [label, setLabel] = useState(w.label);
  const { busy, error, run } = useRun();
  const number = Number(value.replace(",", "."));
  const valid = value.trim() !== "" && Number.isFinite(number) && number >= 0 && number <= 10;
  const save = (event) => {
    event.preventDefault();
    const body = { id: w.id, value: number };
    if (w.own && label.trim() && label.trim() !== w.label) body.label = label.trim();
    run(async () => { await fixIt("/grade", body, f("gradeSaved")); setEditing(false); });
  };
  const reset = () => run(async () => {
    await fixIt("/grade/reset", { id: w.id }, f(w.own ? "gradeDeleted" : "gradeReset"));
    setEditing(false);
  });
  const edit = () => { setValue(w.value === null ? "" : String(w.value)); setLabel(w.label); setEditing(!editing); };
  const bonus = Number(String(w.bonus || "").replace(",", "."));
  const parts = w.parts || [];
  const said = [w.note && { name: "", text: w.note }, ...parts.filter((p) => p.note).map((p) => ({ name: p.header,
    text: p.note }))].filter(Boolean);
  const notes = [w.raw !== null && w.raw !== undefined && w.max && w.max !== 10 && f("points",
    { raw: mark(round(w.raw)), max: mark(w.max) }), bonus > 0 && f("bonusN", { n: mark(bonus) }),
    w.own && f("yours"), w.edited && f("edited")].filter(Boolean).join(" · ");
  return html`<div class=${"grade" + (w.pending ? " pending" : "")}>
    <button class="doc plain" style="width:100%;text-align:left" aria-expanded=${open}
      onClick=${() => { setOpen(!open); setEditing(false); }}>
      <span>${w.label}${w.new ? html` <span class="chip">new</span>` : ""}
        ${notes && html`<div class="mut small">${notes}</div>`}</span>
      ${said.length > 0 && html`<span class="said" title=${f("hasComment")}>${icon(ICONS.comment, 16)}</span>`}
      ${w.pending ? html`<span class="chip plain">${f(w.checking ? "checking" : "pending")}</span>`
        : html`<b>${mark(w.value)}</b>`}</button>
    ${open && html`<div class="gview">
      ${parts.length > 0 && html`<div class="parts" aria-label=${f("parts")}>
        ${parts.map((p, i) => html`<span key=${i} class=${p.note ? "noted" : ""}><small>${p.header || "—"}</small>
          <b>${p.value || "—"}</b></span>`)}</div>`}
      ${said.length > 0 && html`<div class="said-list" aria-label=${f("comments")}>${said.map((c, i) => html`
        <p key=${i}>${c.name && html`<b>${c.name}</b>`}${c.text}</p>`)}</div>`}
      ${w.how && html`<div class="how"><span class="lab">${f("howCounted")}</span>
        ${w.how.text && html`<div>${w.how.text}</div>`}
        ${w.how.formula && html`<code>${w.how.formula}</code>`}</div>`}
      ${w.pending && html`<div class="note small">${f(w.checking ? "checkingNote" : "pendingNote")}</div>`}
      ${!w.pending && !editing && html`<div class="gacts">
        <button class="sec sm" onClick=${edit}>${icon(ICONS.edit, 14)}${f("change")}</button></div>`}
      ${!w.pending && editing && html`<form class="gedit" onSubmit=${save}>
        <div class="err" role="alert">${error}</div>
        ${w.own && html`<input class="field" aria-label=${f("label")} value=${label} maxlength="40"
               onInput=${(e) => setLabel(e.target.value)} />`}
        <div class="row-in">
          <input class="field narrow" aria-label=${f("grade")} inputmode="decimal" value=${value}
                 onInput=${(e) => setValue(e.target.value)} />
          <button class="sec sm acc" type="submit" disabled=${busy || !valid}>${f("save")}</button>
          <button class="sec sm" type="button" onClick=${() => setEditing(false)}>${f("cancel")}</button>
        </div>
        ${(w.edited || w.own) && html`<button class=${"sec wide" + (w.own ? " danger" : "")} type="button"
          disabled=${busy} onClick=${() => (!w.own || confirm(f("deleteGradeAsk"))) && reset()}>
          ${w.own ? f("deleteGrade") : f("asSheet")}</button>`}
        ${w.edited && html`<div class="note small">${f("sheetHas", { value: mark(w.sheet_value) })}</div>`}
      </form>`}
    </div>`}
  </div>`;
}
