// «+» (stage 16.5): material for the class in progress, one line read by the model and confirmed, and a deadline,
// an event or a grade by hand. Android's «Share» opens it with the shared files (sw.js keeps them in SHARE_CACHE).
import { useEffect, useRef, useState } from "preact/hooks";
import { ApiError, ICONS, api, changed, html, icon, mark, toast } from "./common.js";
import { day, kind, t } from "./i18n.js";
import { useAct, useLoad } from "./screens.js";

const SHARE_CACHE = "infomaster-share";
const hm = (iso) => iso.slice(11, 16);
const capital = (text) => text.charAt(0).toUpperCase() + text.slice(1);

// Files to the library (multipart, the place first): the answer has the change, for its undo.
async function upload(files, place) {
  const form = new FormData();
  if (place.class_uid) form.append("class_uid", place.class_uid);
  else if (place.course) form.append("course", place.course);
  for (const file of files) form.append("file", file, file.name || "file");
  let response;
  try {
    response = await fetch("/web/api/add/files", { method: "POST", credentials: "same-origin", body: form });
  } catch {
    throw new ApiError(0, t("offline"));
  }
  const answer = await response.json().catch(() => ({}));
  if (!response.ok && response.status !== 207) throw new ApiError(response.status, answer.error || t("failed"));
  return answer;
}

async function sharedFiles() {
  // What sw.js kept of a «Share»: the files, and the text or link as /text.
  if (!("caches" in window)) return { files: [], text: "" };
  const cache = await caches.open(SHARE_CACHE);
  const files = [];
  let text = "";
  for (const request of await cache.keys()) {
    const response = await cache.match(request);
    if (new URL(request.url).pathname.endsWith("/text")) {
      text = await response.text();
    } else {
      const blob = await response.blob();
      files.push(new File([blob], decodeURIComponent(response.headers.get("X-Name") || "file"), { type: blob.type }));
    }
  }
  return { files, text };
}

const forgetShared = async () => { if ("caches" in window) await caches.delete(SHARE_CACHE); };

function classLabel(c) {
  return `${c.title} · ${kind(c.type)}`;
}

function Place({ data, place, setPlace }) {
  const [picking, setPicking] = useState(false);
  const chosen = place.class_uid ? data.classes.find((c) => c.uid === place.class_uid) : null;
  const going = chosen && data.current && chosen.uid === data.current.uid;
  return html`<div class="box stack" style="gap:8px">
    <div style="display:flex;gap:10px;align-items:center">
      <div style="flex:1;min-width:0">
        ${chosen ? html`<span class="lab">${going ? t("goingNow") : capital(day(chosen.start, { weekday: "short",
            day: "numeric", month: "short" })) + ", " + hm(chosen.start)}</span>
          <div><b>${classLabel(chosen)}</b></div>
          <div class="mut small">${t("attachHere")}</div>`
        : html`<span class="lab">${t("noClassNow")}</span><div class="mut small">${t("toInbox")}</div>`}
      </div>
      <button class="sec" onClick=${() => setPicking(!picking)}>${chosen ? t("another") : t("pickClass")}</button>
    </div>
    ${picking && html`<div class="stack" style="gap:0">
      ${data.classes.map((c) => html`<button key=${c.uid} class="it" onClick=${() => {
        setPlace({ class_uid: c.uid }); setPicking(false);
      }}><span class="t">${hm(c.start)}</span>${mark(c.type)}
        <span class="ttl">${classLabel(c)}<small>${capital(day(c.start, { weekday: "short", day: "numeric",
          month: "short" }))}</small></span></button>`)}
      <button class="it" onClick=${() => { setPlace({}); setPicking(false); }}>
        <span class="t"></span><span class="k ki">${icon(ICONS.shelf, 16)}</span><span class="ttl">${t("inboxChoice")}</span></button>
    </div>`}
  </div>`;
}

function CourseSelect({ courses, value, onChange, required }) {
  return html`<select class="field" value=${value || ""} required=${required}
    onChange=${(e) => onChange(e.target.value || null)}>
    ${!required && html`<option value="">${t("noCourse")}</option>`}
    ${required && html`<option value="" disabled>${t("chooseCourse")}</option>`}
    ${courses.map((c) => html`<option key=${c.key} value=${c.key}>${c.title}</option>`)}
  </select>`;
}

function DeadlineForm({ courses, start, busy, onSave }) {
  const [title, setTitle] = useState(start.title || "");
  const [due, setDue] = useState(start.due || "");
  const [course, setCourse] = useState(start.course || null);
  const [category, setCategory] = useState(start.category || "study");
  return html`<form class="stack" style="gap:10px" onSubmit=${(e) => {
    e.preventDefault(); onSave({ title: title.trim(), due, course, category });
  }}>
    <input class="field" placeholder=${t("title")} value=${title} maxlength="200" required
           onInput=${(e) => setTitle(e.target.value)} />
    <input class="field" type="date" value=${due} required onInput=${(e) => setDue(e.target.value)} />
    <${CourseSelect} courses=${courses} value=${course} onChange=${setCourse} />
    <div class="views" role="radiogroup" style="box-shadow:none;align-self:flex-start">
      ${["study", "personal"].map((c) => html`<button key=${c} type="button" role="radio" aria-checked=${category === c}
        onClick=${() => setCategory(c)}>${t(c)}</button>`)}
    </div>
    <button class="btn" type="submit" disabled=${busy || !title.trim() || !due}>${t("saveDeadline")}</button>
  </form>`;
}

function EventForm({ start, busy, onSave }) {
  const [title, setTitle] = useState(start.title || "");
  const [date, setDate] = useState((start.start || "").slice(0, 10));
  const [from, setFrom] = useState((start.start || "").slice(11, 16));
  const [to, setTo] = useState((start.end || "").slice(11, 16));
  const [room, setRoom] = useState(start.room || "");
  const submit = (e) => {
    e.preventDefault();
    const values = { title: title.trim(), start: from ? `${date}T${from}` : date };
    if (from && to) values.end = `${date}T${to}`;
    if (room.trim()) values.room = room.trim();
    onSave(values);
  };
  return html`<form class="stack" style="gap:10px" onSubmit=${submit}>
    <input class="field" placeholder=${t("title")} value=${title} maxlength="120" required
           onInput=${(e) => setTitle(e.target.value)} />
    <input class="field" type="date" value=${date} required onInput=${(e) => setDate(e.target.value)} />
    <div class="acts">
      <input class="field" style="flex:1 1 100px;width:auto" type="time" value=${from} aria-label=${t("timeFrom")}
             onInput=${(e) => setFrom(e.target.value)} />
      <input class="field" style="flex:1 1 100px;width:auto" type="time" value=${to} aria-label=${t("timeTo")}
             onInput=${(e) => setTo(e.target.value)} />
    </div>
    <input class="field" placeholder=${t("room")} value=${room} maxlength="200" onInput=${(e) => setRoom(e.target.value)} />
    <button class="btn" type="submit" disabled=${busy || !title.trim() || !date}>${t("saveEvent")}</button>
  </form>`;
}

function GradeForm({ courses, busy, onSave }) {
  const [course, setCourse] = useState(null);
  const [label, setLabel] = useState("");
  const [value, setValue] = useState("");
  const number = Number(value.replace(",", "."));
  const ok = course && label.trim() && value !== "" && number >= 0 && number <= 10;
  return html`<form class="stack" style="gap:10px" onSubmit=${(e) => {
    e.preventDefault(); onSave({ course, label: label.trim(), value: number });
  }}>
    <${CourseSelect} courses=${courses} value=${course} onChange=${setCourse} required />
    <input class="field" placeholder=${t("gradeWhat")} value=${label} maxlength="100" required
           onInput=${(e) => setLabel(e.target.value)} />
    <input class="field" inputmode="decimal" placeholder=${t("gradeValue")} value=${value}
           onInput=${(e) => setValue(e.target.value)} />
    <button class="btn" type="submit" disabled=${busy || !ok}>${t("saveGrade")}</button>
  </form>`;
}

function Understood({ p, busy, onSave, onNote, onEdit }) {
  const rows = [[t("what"), p.title], [t("courseLabel"), p.course_title || "—"]];
  if (p.kind === "deadline") {
    rows.push([t("dueLabel"), capital(day(p.due, { weekday: "short", day: "numeric", month: "long" }))]);
    rows.push([t("remind"), t("remindWhen")]);
  } else if (p.kind === "event") {
    rows.push([t("when"), capital(day(p.start, { weekday: "short", day: "numeric", month: "long" }))
      + (p.start.length > 10 ? `, ${hm(p.start)}${p.end ? "–" + hm(p.end) : ""}` : "")]);
    if (p.room) rows.push([t("room"), p.room]);
  }
  const save = { deadline: t("saveDeadline"), event: t("saveEvent"), note: t("saveNote") }[p.kind];
  return html`<div class="box stack" style="gap:10px">
    <span class="lab">${t("understood")}</span>
    <div class="mut small">«${p.line}»</div>
    <dl class="pairs">${rows.map(([k, v]) => html`<dt key=${k}>${k}</dt><dd>${v}</dd>`)}</dl>
    ${p.why && html`<div class="note small">${p.why}</div>`}
    <button class="btn" disabled=${busy} onClick=${onSave}>${icon(ICONS.check)}${save}</button>
    <div class="acts">
      ${p.kind !== "note" && html`<button class="sec" disabled=${busy} onClick=${onNote}>${t("asNote")}</button>`}
      ${p.kind !== "note" && html`<button class="sec" disabled=${busy} onClick=${onEdit}>${t("edit")}</button>`}
    </div>
  </div>`;
}

export function AddSheet({ shared, onClose, onOut }) {
  const { data, error: loadError } = useLoad("/add", onOut);
  const [place, setPlace] = useState(null);           // {class_uid} or {} (the inbox); null until loaded
  const [mode, setMode] = useState(null);             // note, deadline, event, grade
  const [line, setLine] = useState("");
  const [proposal, setProposal] = useState(null);
  const [form, setForm] = useState({});               // a manual form's first values (from the model)
  const [note, setNote] = useState("");
  const [incoming, setIncoming] = useState(null);     // shared {files, text}
  const [progress, setProgress] = useState("");
  const { busy, error, run } = useAct();
  const inputs = { photo: useRef(null), audio: useRef(null), file: useRef(null) };

  useEffect(() => {
    if (data && place === null) setPlace(data.current ? { class_uid: data.current.uid } : {});
  }, [data]);
  useEffect(() => {
    if (shared !== "1") return;
    sharedFiles().then((found) => {
      setIncoming(found);
      if (found.text) { setNote(found.text); setMode("note"); }
    });
  }, [shared]);

  const done = (answer, text, keepOpen = false) => {
    changed();
    toast(text, answer.change && answer.change.undo ? answer.change.id : null);
    if (!keepOpen) onClose();
  };
  const send = (files, fromShare = false) => run(async () => {
    if (!files.length) return;
    setProgress(t("uploadingN", { n: files.length }));
    try {
      const answer = await upload(files, place || {});
      if (answer.error) throw new ApiError(400, answer.error);
      const notePending = fromShare && mode === "note" && note.trim();
      if (fromShare) setIncoming({ files: [], text: "" });
      if (!notePending) await forgetShared();
      done(answer, t("addedN", { n: answer.added.length }), notePending);  // a shared link waits to be saved
    } finally { setProgress(""); }
  });
  const pick = (name) => inputs[name].current && inputs[name].current.click();
  const chosen = (event) => { const files = [...event.target.files]; event.target.value = ""; send(files); };
  const saveNote = (text) => run(async () => {
    done(await api("POST", "/add/note", { text, ...(place || {}) }), t("noteSaved"));
    await forgetShared();
  });
  const read = (event) => {
    event.preventDefault();
    run(async () => setProposal(await api("POST", "/add/line", { text: line })));
  };
  const byHand = (what, values) => run(async () => done(await api("POST", `/add/${what}`, values), t("saved")));
  const saveProposal = () => {
    const p = proposal;
    if (p.kind === "deadline") byHand("deadline", { title: p.title, due: p.due, course: p.course, category: p.category });
    else if (p.kind === "event") byHand("event", { title: p.title, start: p.start, end: p.end, room: p.room });
    else saveNote(p.line);
  };

  return html`<div class="scrim" onClick=${onClose}></div>
    <section class="sheet" role="dialog" aria-label=${t("add")}><div>
      <div class="grab" aria-hidden="true"></div>
      <h1>${t("add")}</h1>
      <div class="err" role="alert">${loadError}</div>
      ${shared === "lost" && html`<div class="hint">${t("shareLost")}</div>`}
      ${data && place !== null && html`
        ${data.library && html`<${Place} data=${data} place=${place} setPlace=${setPlace} />`}
        ${incoming && incoming.files.length > 0 && html`<div class="box stack" style="gap:8px">
          <span class="lab">${t("sharedN", { n: incoming.files.length })}</span>
          <div class="mut small">${incoming.files.map((f) => f.name).join(", ")}</div>
          <button class="btn" disabled=${busy} onClick=${() => send(incoming.files, true)}>${t("addThem")}</button>
        </div>`}
        ${data.library && html`<div class="grid4">
          <button class="tile" disabled=${busy} onClick=${() => pick("photo")}>${icon(ICONS.camera, 24)}${t("photo")}</button>
          <button class="tile" disabled=${busy} onClick=${() => pick("audio")}>${icon(ICONS.mic, 24)}${t("recording")}</button>
          <button class="tile" disabled=${busy} onClick=${() => pick("file")}>${icon(ICONS.file, 24)}${t("fileTile")}</button>
          <button class="tile" disabled=${busy} aria-pressed=${mode === "note"}
                  onClick=${() => setMode(mode === "note" ? null : "note")}>${icon(ICONS.note, 24)}${t("noteLink")}</button>
          <input ref=${inputs.photo} type="file" accept="image/*" capture="environment" multiple hidden onChange=${chosen} />
          <input ref=${inputs.audio} type="file" accept="audio/*" capture multiple hidden onChange=${chosen} />
          <input ref=${inputs.file} type="file" multiple hidden onChange=${chosen} />
        </div>`}
        ${progress && html`<div class="note">${progress}</div>`}
        ${mode === "note" && html`<form class="stack" style="gap:10px" onSubmit=${(e) => {
          e.preventDefault(); saveNote(note);
        }}>
          <textarea class="field area" rows="4" placeholder=${t("notePlaceholder")} value=${note}
                    onInput=${(e) => setNote(e.target.value)}></textarea>
          <button class="btn" type="submit" disabled=${busy || !note.trim()}>${t("saveNote")}</button>
        </form>`}
        <span class="lab">${t("oneLine")}</span>
        ${data.line ? html`<form class="acts" onSubmit=${read}>
            <input class="field" style="flex:1 1 200px;width:auto" placeholder=${t("linePlaceholder")} value=${line}
                   maxlength="500" onInput=${(e) => { setLine(e.target.value); setProposal(null); }} />
            <button class="btn" style="flex:0 0 auto;padding:0 18px" type="submit"
                    disabled=${busy || !line.trim()}>${t("readLine")}</button>
          </form>
          <div class="mut small">${t("lineHint")}</div>`
          : html`<div class="note small">${t("lineOff")}</div>`}
        ${proposal && html`<${Understood} p=${proposal} busy=${busy} onSave=${saveProposal}
          onNote=${() => saveNote(proposal.line)}
          onEdit=${() => { setForm(proposal); setMode(proposal.kind); setProposal(null); }} />`}
        <span class="lab">${t("byHand")}</span>
        <div class="acts">
          ${["deadline", "event", ...(data.grades ? ["grade"] : [])].map((what) => html`<button key=${what}
            class="sec" aria-pressed=${mode === what} onClick=${() => { setForm({}); setMode(mode === what ? null : what); }}>
            ${t({ deadline: "deadline", event: "eventWord", grade: "gradeWord" }[what])}</button>`)}
        </div>
        ${mode === "deadline" && html`<${DeadlineForm} key=${JSON.stringify(form)} courses=${data.courses}
          start=${form} busy=${busy} onSave=${(v) => byHand("deadline", v)} />`}
        ${mode === "event" && html`<${EventForm} key=${JSON.stringify(form)} start=${form} busy=${busy}
          onSave=${(v) => byHand("event", v)} />`}
        ${mode === "grade" && html`<${GradeForm} courses=${data.courses} busy=${busy}
          onSave=${(v) => byHand("grade", v)} />`}`}
      <div class="err" role="alert">${error}</div>
      <button class="sec wide" onClick=${onClose}>${t("close")}</button>
    </div></section>`;
}

