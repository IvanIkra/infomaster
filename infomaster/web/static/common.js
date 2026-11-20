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
  shelf: '<path d="M5 4h4v16H5zM11 4h4v16h-4zM17.5 5l3 .8-3.2 14-3-.8z"/>',
};

// #/path?query: the screen and its parameters
export function route() {
  const [path, query] = (location.hash.slice(1) || "/").split("?");
  return { path: path || "/", params: new URLSearchParams(query || "") };
}

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
