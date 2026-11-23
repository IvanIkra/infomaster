// What every screen uses: the API, icons, the hash routes.
import { h } from "preact";
import htm from "htm";
import { t } from "./i18n.js";

export const html = htm.bind(h);

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

export async function api(method, path, data) {
  let response;
  try {
    response = await fetch("/web/api" + path, {
      method, credentials: "same-origin",
      headers: data === undefined ? {} : { "Content-Type": "application/json" },
      body: data === undefined ? undefined : JSON.stringify(data),
    });
  } catch {
    throw new ApiError(0, t("offline"));
  }
  const answer = await response.json().catch(() => ({}));
  if (!response.ok) throw new ApiError(response.status, answer.error || t("failed"));
  return answer;
}

export const icon = (path, size = 20) => html`<svg class="ic" viewBox="0 0 24 24" aria-hidden="true"
  style=${{ width: size + "px", height: size + "px" }} dangerouslySetInnerHTML=${{ __html: path }}></svg>`;

export const ICONS = {
  back: '<path d="M15 18l-6-6 6-6"/>',
  next: '<path d="M9 6l6 6-6 6"/>',
  key: '<circle cx="8" cy="15" r="4"/><path d="M11 12l9-9M17 6l3 3M15 8l2 2"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/>',
  link: '<path d="M14 4h6v6M20 4l-9 9M18 14v6H4V6h6"/>',
  check: '<path d="M5 12l5 5L20 7"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16v4zM14 6l4 4"/>',
  cross: '<path d="M6 6l12 12M18 6L6 18"/>',
  merge: '<path d="M7 4v5a5 5 0 0 0 5 5h0a5 5 0 0 1 5 5v1M17 4v5a5 5 0 0 1-5 5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  camera: '<path d="M4 8h3l2-3h6l2 3h3v11H4z"/><circle cx="12" cy="13" r="3.5"/>',
  mic: '<rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5 11a7 7 0 0 0 14 0M12 18v3"/>',
  file: '<path d="M6 3h8l4 4v14H6zM14 3v4h4"/>',
  note: '<path d="M5 4h14v16H5zM8 9h8M8 13h8M8 17h5"/>',
  comment: '<path d="M4 5h16v11H9l-5 4V5z"/>',
  calc: '<rect x="5" y="3" width="14" height="18" rx="2"/><path d="M8 7h8M8 11h.01M12 11h.01M16 11h.01M8 15h.01M12 15h.01M16 15v3M8 18h.01M12 18h.01"/>',
  shelf: '<path d="M5 4h4v16H5zM11 4h4v16h-4zM17.5 5l3 .8-3.2 14-3-.8z"/>',
  sheet: '<path d="M6 3h8l4 4v14H6zM14 3v4h4M9 14l2 2 4-4"/>',
  video: '<rect x="3" y="6" width="13" height="12" rx="2"/><path d="M16 10l5-3v10l-5-3z"/>',
  slides: '<rect x="3" y="4" width="18" height="12" rx="1"/><path d="M12 16v4M8 20h8"/>',
};

// The mark of an item's kind in a row (D73): a class by its type, a deadline, a library item by its kind.
const KIND_ICONS = {
  Lecture: '<path d="M3 4h18M5 4v10h14V4M12 14v3M8 21l4-4 4 4"/>',
  Seminar: '<circle cx="8" cy="8" r="3"/><circle cx="16" cy="8" r="3"/><path d="M3 20c0-3 2-5 5-5s5 2 5 5M11 20c0-3 2-5 5-5s5 2 5 5"/>',
  Lab: '<path d="M9 3h6M10 3v6l-6 10a1.5 1.5 0 0 0 1.3 2h13.4a1.5 1.5 0 0 0 1.3-2L14 9V3M7 15h10"/>',
  Class: '<rect x="4" y="5" width="16" height="15" rx="2"/><path d="M4 10h16M9 3v4M15 3v4"/>',
  "Interim assessment": '<path d="M9 3h6v3H9zM7 4.5H5V21h14V4.5h-2M9 14l2 2 4-4"/>',
  Exam: '<path d="M2 9l10-5 10 5-10 5zM6 11v5c3 2 9 2 12 0v-5M22 9v6"/>',
  Consultation: '<path d="M4 5h16v11H9l-5 4z"/>',
  Event: '<path d="M5 21V4h12l-2 4 2 4H5"/>',
  Talk: ICONS.mic,
  Deadline: '<path d="M7 3h10M7 21h10M8 3v3l4 6 4-6V3M8 21v-3l4-6 4 6v3"/>',
  photo: ICONS.camera, audio: ICONS.mic, video: ICONS.video, notes: ICONS.note, slides: ICONS.slides,
  sheet: ICONS.sheet, link: ICONS.link, text: ICONS.note, transcript: ICONS.note, file: ICONS.file,
};
export const mark = (kind, size = 16) => html`<span class=${"k ki" + (kind === "Deadline" ? " due" : "")}>
  ${icon(KIND_ICONS[kind] || KIND_ICONS.Class, size)}</span>`;

// #/path?query: the screen and its parameters
export function route() {
  const [path, query] = (location.hash.slice(1) || "/").split("?");
  return { path: path || "/", params: new URLSearchParams(query || "") };
}

// The screens the owner went through since the last top screen (the feed, a course, the week, the news): «Back»
// returns to the one before, where they came from; with none (a link opened straight), to the screen's own parent.
// Cards over a screen are not screens. Runs before the app reads the new route (registered first).
const TOP = new Set(["/", "/week", "/news", "/course"]);
const OVER = new Set(["/d", "/c", "/n", "/s", "/i", "/check", "/add"]);
let trail = [];
let over = false;  // the card on screen was opened over the screen under it, one history entry ago
const step = (event) => {
  const at = location.hash.slice(1) || "/", path = at.split("?")[0] || "/";
  if (OVER.has(path)) {
    const from = event && event.oldURL ? (new URL(event.oldURL).hash.slice(1) || "/") : null;
    over = from !== null && from === trail[trail.length - 1];
    return;
  }
  over = false;
  if (TOP.has(path)) trail = [at];
  else if (trail.length > 1 && trail[trail.length - 2] === at) trail.pop();
  else if (trail[trail.length - 1] !== at) trail.push(at);
};
step();
window.addEventListener("hashchange", step);
// A card closes to the screen under it: back in the history when it was opened from there (so the phone's Back
// does not open it again), else in place of the card's entry.
export const closeTo = (hash) => {
  if (over) history.back();
  else location.replace(hash);
};
export const backTo = (parent) => (trail.length > 1 ? "#" + trail[trail.length - 2] : parent);

export function go(path, params = {}) {
  const query = new URLSearchParams(Object.entries(params).filter(([, v]) => v)).toString();
  location.hash = path + (query ? "?" + query : "");
}

// After a change: every loaded screen reads its data again, and a note with «Undo» shows at the bottom.
export const CHANGED = "infomaster:changed";
export const TOAST = "infomaster:toast";
export const changed = () => window.dispatchEvent(new Event(CHANGED));
export const toast = (text, changeId = null) =>
  window.dispatchEvent(new CustomEvent(TOAST, { detail: { text, changeId } }));

// One of the owner's changes of a deadline (POST /deadlines/{action}): `before` runs first (a card closes, so it does
// not ask for a deadline that is gone), then the screens reload and the note offers its undo.
export async function act(action, data, text, before) {
  const answer = await api("POST", `/deadlines/${action}`, data);
  if (before) {
    before();
    await new Promise((done) => setTimeout(done, 50));  // the hash changes, and the card is gone, in a later task
  }
  changed();
  toast(text, answer.change.undo ? answer.change.id : null);
  return answer;
}
