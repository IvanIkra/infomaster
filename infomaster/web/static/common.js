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
