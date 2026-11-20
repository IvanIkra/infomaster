// «Machine → Notifications» (stage 16.4): Web Push on this device, on or off, and a test message.
import { useEffect, useState } from "preact/hooks";
import { ICONS, api, html, icon } from "./common.js";
import { t } from "./i18n.js";

const supported = () => "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
const isIos = () => /iPhone|iPad/.test(navigator.userAgent);
const standalone = () => window.matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;

function bytes(base64url) {
  const text = atob(base64url.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - base64url.length % 4) % 4));
  return Uint8Array.from(text, (c) => c.charCodeAt(0));
}

const sameKey = (subscription, key) => {
  const own = subscription.options && subscription.options.applicationServerKey;
  if (!own) return true;  // the browser does not say: take it as it is
  const a = new Uint8Array(own), b = bytes(key);
  return a.length === b.length && a.every((v, i) => v === b[i]);
};

export function Notifications({ onOut, onChange }) {
  const [state, setState] = useState(null);
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const run = async (job) => {
    setBusy(true); setError(""); setNote("");
    try { const next = await job(); if (next) setState(next); } catch (e) {
      if (e.status === 401) onOut(); else setError(e.message);
    } finally { setBusy(false); }
  };
  useEffect(() => { run(() => api("GET", "/push")); }, []);

  const on = () => run(async () => {
    const permission = await Notification.requestPermission();  // first: iOS asks only inside the tap
    if (permission !== "granted") throw new Error(t("pushDenied"));
    const registration = await navigator.serviceWorker.ready;
    let subscription = await registration.pushManager.getSubscription();
    if (subscription && !sameKey(subscription, state.key)) {
      await subscription.unsubscribe();
      subscription = null;
    }
    subscription = subscription || await registration.pushManager.subscribe(
      { userVisibleOnly: true, applicationServerKey: bytes(state.key) });
    const next = await api("POST", "/push", { subscription: subscription.toJSON() });
    onChange();
    return next;
  });
  const off = () => run(async () => {
    const registration = await navigator.serviceWorker.ready;
    const subscription = await registration.pushManager.getSubscription();
    if (subscription) await subscription.unsubscribe();
    const next = await api("DELETE", "/push");
    onChange();
    return next;
  });
  const test = () => run(async () => { await api("POST", "/push/test", {}); setNote(t("testSent")); });

  const blocked = supported() && Notification.permission === "denied";
  return html`<main class="screen">
    <header class="top">
      <a class="rb" href="#/machine" aria-label=${t("back")}>${icon(ICONS.back)}</a>
      <div><h1>${t("notifications")}</h1></div>
    </header>
    <div class="stack">
      ${!supported() ? html`<div class="hint">${isIos() && !standalone() ? t("pushIos") : t("pushUnsupported")}</div>`
        : state && html`
        <div class="box stack" style="gap:6px">
          <b>${state.on ? t("pushOn") : t("pushOff")}</b>
          <span class="mut small">${t("pushDevicesN", { n: state.devices })}</span>
        </div>
        ${blocked && html`<div class="hint">${t("pushDenied")}</div>`}
        ${state.on
          ? html`<div class="acts">
              <button class="btn" disabled=${busy} onClick=${test}>${t("sendTest")}</button>
              <button class="sec wide danger" disabled=${busy} onClick=${off}>${t("turnOff")}</button></div>`
          : html`<button class="btn" disabled=${busy || blocked} onClick=${on}>${t("turnOn")}</button>`}
        <div class="note">${note}</div>`}
      <div class="err" role="alert">${error}</div>
      <h2>${t("pushWhat")}</h2>
      <div class="note">${t("pushWhatList").split(" · ").map((line) => html`<div key=${line}>— ${line.charAt(0).toUpperCase() + line.slice(1)}</div>`)}</div>
    </div>
  </main>`;
}
