// News (stage 16.7): the live news in rank order, as Tablo has them, and an item's card with
// the message it came from and «Hide» (journaled, with its undo).
import { ICONS, api, changed, go, html, icon, toast } from "./common.js";
import { day, t } from "./i18n.js";
import { Bar, MachineDot, Quote, useAct, useLoad } from "./screens.js";

const hm = (iso) => iso.slice(11, 16);
const posted = (iso, todayIso) => (iso.slice(0, 10) === todayIso ? hm(iso)
  : `${day(iso, { day: "numeric", month: "short" })}, ${hm(iso)}`);
const todayIso = () => {
  const now = new Date();
  return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
};
const fromName = (f) => f.kind === "mail" ? (f.name ? `${t("letter")} «${f.name}»` : t("letter")) : (f.name || "?");

function NewsRow({ item }) {
  return html`<button class="it news" onClick=${() => go("/n", { id: item.id })}>
    <span class="t">${posted(item.posted_at, todayIso())}</span>
    <span class=${"k " + (item.importance >= 4 ? "kd" : "kc")}></span>
    <span class="ttl">${item.importance >= 5 ? html`<b>${item.full}</b>` : item.full}
      <small>${fromName(item.from)}</small></span>
  </button>`;
}

export function NewsList({ course, onOut }) {
  const { data, error } = useLoad("/news", onOut);
  return html`<main class="screen">
    <header class="top">
      <div><div class="kicker">${data ? t("newsLive", { n: data.ttl_hours }) : ""}</div><h1>${t("newsTitle")}</h1></div>
      <a class="rb" href="#/shelf" aria-label=${t("shelves")}>${icon(ICONS.shelf)}</a>
      <${MachineDot} data=${data} error=${error} />
    </header>
    <div class="err" role="alert">${error}</div>
    ${data && html`<div class="feed">
      ${data.items.length === 0 ? html`<div class="empty">${t("noNews")}</div>`
        : html`<div>${data.items.map((item) => html`<${NewsRow} key=${item.id} item=${item} />`)}</div>`}
    </div>`}
    <${Bar} view="news" course=${course} />
  </main>`;
}

export function NewsCard({ id, onClose, onOut }) {
  const { data, error } = useLoad(`/news/${encodeURIComponent(id)}`, onOut);
  const act = useAct();
  const hide = () => act.run(async () => {
    const answer = await api("POST", `/news/${id}/hide`, {});
    onClose();
    await new Promise((done) => setTimeout(done, 50));
    changed();
    toast(t("newsHidden"), answer.change.undo ? answer.change.id : null);
  });
  return html`<div class="scrim" onClick=${onClose}></div>
    <section class="sheet" role="dialog" aria-label=${data ? data.short || data.full : ""}><div>
      <div class="grab" aria-hidden="true"></div>
      <div class="err" role="alert">${error || act.error}</div>
      ${data && html`
        <div>
          <div class="kicker">${posted(data.posted_at, todayIso())} · ${fromName(data.from)}
            ${data.importance >= 4 ? ` · ${t("important")}` : ""}</div>
          <h1 style="font-size:24px">${data.full}</h1>
        </div>
        ${!data.live && html`<div class="hint">${t("newsGone")}</div>`}
        <${Quote} quote=${data.quote} link=${data.link} from=${fromName(data.from)} />
        ${data.live && html`<div class="acts"><button class="sec" disabled=${act.busy} onClick=${hide}>
          ${icon(ICONS.cross, 18)}${t("hideNews")}</button></div>`}`}
      <button class="sec wide" onClick=${onClose}>${t("close")}</button>
    </div></section>`;
}
