// The web app (§8.6, D61): the login (a code from the bot, then a passkey), the devices, and the frame that routes
// to the screens (screens.js).
import { render } from "preact";
import { useEffect, useState } from "preact/hooks";
import { ApiError, ICONS, api, go, html, icon, route } from "./common.js";
import { getLang, setLang, t, when } from "./i18n.js";
import * as passkey from "./passkey.js";
import { Card, Feed, Week } from "./screens.js";

const LANG_KEY = "infomaster.lang";  // the login screen's choice, before the server's setting is known

const passkeyError = (e) => (e.name === "NotAllowedError" || e.name === "AbortError") ? t("cancelled")
  : (e instanceof ApiError ? e.message : t("failed"));


// --- the login ---------------------------------------------------------------------------------------------

function Login({ onIn }) {
  const [sent, setSent] = useState(false);
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [, redraw] = useState(0);

  const run = async (job) => {
    setBusy(true); setError("");
    try { await job(); } catch (e) { setError(passkeyError(e)); } finally { setBusy(false); }
  };
  const send = () => run(async () => { await api("POST", "/login/code", {}); setSent(true); setCode(""); });
  const check = (event) => {
    event.preventDefault();
    run(async () => { await api("POST", "/login/code/check", { code }); onIn(true); });
  };
  const withPasskey = () => run(async () => {
    const options = await api("POST", "/login/passkey/options", {});
    const credential = await passkey.get(options);
    await api("POST", "/login/passkey", { credential });
    onIn(false);
  });
  const choose = (value) => {
    setLang(value);
    try { localStorage.setItem(LANG_KEY, value); } catch { /* private mode: the choice lasts this visit */ }
    redraw((n) => n + 1);
  };

  return html`<main class="screen">
    <div class="brand">
      <div class="logo" aria-hidden="true">im</div>
      <h1>infomaster</h1>
      <p>${t("tagline")}</p>
    </div>
    <div class="grow"></div>
    <div class="stack">
      ${passkey.supported() && html`
        <button class="btn" disabled=${busy} onClick=${withPasskey}>${icon(ICONS.key, 22)}${t("passkeyLogin")}</button>
        <div class="mut small" style="text-align:center">${t("passkeyHint")}</div>
        <div class="or">${t("newDevice")}</div>`}
      <form class="box stack" onSubmit=${check}>
        <div class="note">${sent ? t("codeSent") : t("codeIntro")}</div>
        ${sent && html`
          <label class="lab" for="code">${t("codeLabel")}</label>
          <input id="code" class="code" inputmode="numeric" autocomplete="one-time-code" maxlength="6"
                 pattern="[0-9]{6}" value=${code} onInput=${(e) => setCode(e.target.value.replace(/\D/g, ""))}
                 autofocus />
          <button class="btn" type="submit" disabled=${busy || code.length !== 6}>${t("logIn")}</button>`}
        <button class="sec wide" type="button" disabled=${busy} onClick=${send}>
          ${sent ? t("sendAgain") : t("sendCode")}</button>
      </form>
      <div class="err" role="alert">${error}</div>
      <div class="langs">
        <button aria-pressed=${getLang() === "ru"} onClick=${() => choose("ru")}>Русский</button> ·
        <button aria-pressed=${getLang() === "en"} onClick=${() => choose("en")}>English</button>
      </div>
    </div>
  </main>`;
}

function OfferPasskey({ onDone }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const create = async () => {
    setBusy(true); setError("");
    try {
      const options = await api("POST", "/passkeys/options", {});
      await api("POST", "/passkeys", { credential: await passkey.create(options) });
      onDone();
    } catch (e) { setError(passkeyError(e)); } finally { setBusy(false); }
  };
  return html`<main class="screen">
    <div class="brand">
      <div class="logo" aria-hidden="true">${icon(ICONS.key, 34)}</div>
      <h1 style="font-size:26px">${t("offerTitle")}</h1>
      <p>${t("offerText")}</p>
    </div>
    <div class="grow"></div>
    <div class="stack">
      <div class="err" role="alert">${error}</div>
      <button class="btn" disabled=${busy} onClick=${create}>${icon(ICONS.key, 22)}${t("createPasskey")}</button>
      <button class="sec wide" disabled=${busy} onClick=${onDone}>${t("later")}</button>
    </div>
  </main>`;
}

// --- after the login ---------------------------------------------------------------------------------------

function Machine() {
  return html`<main class="screen">
    <header class="top">
      <a class="rb" href="#/" aria-label=${t("back")}>${icon(ICONS.back)}</a>
      <div><h1>${t("machine")}</h1></div>
    </header>
    <a class="link" href="#/machine/login">${t("login")}<span>${icon(ICONS.next, 16)}</span></a>
  </main>`;
}

function Devices({ onOut }) {
  const [state, setState] = useState(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const run = async (job) => {
    setBusy(true); setError("");
    try { const next = await job(); if (next) setState(next); } catch (e) {
      if (e.status === 401) onOut(); else setError(passkeyError(e));
    } finally { setBusy(false); }
  };
  useEffect(() => { run(() => api("GET", "/devices")); }, []);

  const end = (id) => run(async () => {
    const next = await api("DELETE", `/sessions/${id}`);
    if (id === state.current) { onOut(); return null; }
    return next;
  });
  const remove = (id) => run(() => api("DELETE", `/passkeys/${id}`));
  const add = () => run(async () => {
    const options = await api("POST", "/passkeys/options", {});
    return api("POST", "/passkeys", { credential: await passkey.create(options) });
  });
  const logout = () => run(async () => { await api("POST", "/logout", {}); onOut(); return null; });

  return html`<main class="screen">
    <header class="top">
      <a class="rb" href="#/machine" aria-label=${t("back")}>${icon(ICONS.back)}</a>
      <div><h1>${t("login")}</h1></div>
    </header>
    <div class="err" role="alert">${error}</div>
    ${state && html`<div class="stack">
      <h2>${t("devices")}</h2>
      <div>${state.sessions.map((s) => html`<div class="row" key=${s.id}>
        <div><b>${s.device}</b>${s.id === state.current && html`<span class="tag">${t("thisDevice")}</span>`}
          <div class="mut small">${t("lastUsed")} ${when(s.used_at)}</div></div>
        <button class="sec" disabled=${busy} onClick=${() => end(s.id)}>${t("endSession")}</button>
      </div>`)}</div>
      <h2>${t("passkeys")}</h2>
      <div>${state.passkeys.length === 0 ? html`<div class="note">${t("noPasskeys")}</div>`
        : state.passkeys.map((p) => html`<div class="row" key=${p.id}>
          <div><b>${p.device}</b><div class="mut small">${t("added")} ${when(p.created_at)}${p.used_at
            ? html` · ${t("lastUsed")} ${when(p.used_at)}` : ""}</div></div>
          <button class="sec danger" disabled=${busy} onClick=${() => remove(p.id)}>${t("remove")}</button>
        </div>`)}</div>
      ${passkey.supported() && html`<button class="sec wide" disabled=${busy} onClick=${add}>
        ${icon(ICONS.key, 18)}${t("addPasskey")}</button>`}
      <button class="sec wide danger" disabled=${busy} onClick=${logout}>${t("logOut")}</button>
    </div>`}
  </main>`;
}

// --- the frame ---------------------------------------------------------------------------------------------

let base = { path: "/", params: new URLSearchParams() };  // the screen under a card

function App() {
  const [me, setMe] = useState(undefined);   // undefined: asking; null: logged out
  const [offer, setOffer] = useState(false); // a passkey to offer after a login by code
  const [at, setAt] = useState(route());

  const load = async () => {
    try {
      const answer = await api("GET", "/me");
      setLang(answer.lang);
      setMe(answer);
    } catch {
      setMe(null);
    }
  };
  useEffect(() => {
    load();
    const changed = () => setAt(route());
    window.addEventListener("hashchange", changed);
    return () => window.removeEventListener("hashchange", changed);
  }, []);

  if (me === undefined) return null;
  if (me === null) {
    return html`<${Login} onIn=${(byCode) => { setOffer(byCode && passkey.supported()); load(); }} />`;
  }
  if (offer) return html`<${OfferPasskey} onDone=${() => { setOffer(false); load(); }} />`;
  const out = () => { setMe(null); location.hash = ""; };
  if (at.path === "/machine") return html`<${Machine} />`;
  if (at.path === "/machine/login") return html`<${Devices} onOut=${out} />`;

  const card = at.path === "/d" || at.path === "/c";
  if (!card) base = at;
  const close = () => go(base.path, Object.fromEntries(base.params));
  const screen = base.path === "/week"
    ? html`<${Week} start=${base.params.get("start")} course=${base.params.get("course")} onOut=${out} />`
    : html`<${Feed} course=${base.path === "/course" ? base.params.get("key") : null} onOut=${out} />`;
  return html`${screen}${card && html`<${Card} key=${at.params.get("uid")} kind=${at.path.slice(1)}
    uid=${at.params.get("uid")} onClose=${close} onOut=${out} />`}`;
}

let saved = null;
try { saved = localStorage.getItem(LANG_KEY); } catch { /* no storage: the browser's language decides */ }
setLang(saved || (navigator.language.startsWith("ru") ? "ru" : "en"));
render(html`<${App} />`, document.getElementById("app"));

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/web/sw.js", { scope: "/web/" }).catch(() => {});
}
