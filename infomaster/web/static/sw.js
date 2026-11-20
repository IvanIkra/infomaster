// The web app's service worker (§8.6). The app's own files go network first, then the cache: an update is seen on
// the next start, and the app still opens without a connection. /web/api is never cached. A push (stage 16.4) shows a
// notification; a tap on it brings the app forward on the screen it names.
const CACHE = "infomaster-web";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

// Android's «Share» (the manifest's share_target): the files and the text are kept here, and «+» opens with them,
// so the owner picks the class before they are sent.
const SHARE_CACHE = "infomaster-share";

async function keepShared(request) {
  const form = await request.formData();
  await caches.delete(SHARE_CACHE);
  const cache = await caches.open(SHARE_CACHE);
  let n = 0;
  for (const file of form.getAll("files")) {
    if (!(file instanceof File) || !file.size) continue;
    await cache.put(`/web/share/file/${n++}`, new Response(file, {
      headers: { "Content-Type": file.type || "application/octet-stream", "X-Name": encodeURIComponent(file.name) },
    }));
  }
  const text = ["title", "text", "url"].map((name) => form.get(name)).filter((v) => typeof v === "string" && v.trim())
    .filter((v, i, all) => all.indexOf(v) === i).join("\n");
  if (text) await cache.put("/web/share/text", new Response(text));
  return Response.redirect("/web/#/add?shared=1", 303);
}

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (event.request.method === "POST" && url.origin === location.origin && url.pathname === "/web/share") {
    event.respondWith(keepShared(event.request));
    return;
  }
  if (event.request.method !== "GET" || url.origin !== location.origin || !url.pathname.startsWith("/web/")
      || url.pathname.startsWith("/web/api/")) {
    return;
  }
  event.respondWith((async () => {
    const cache = await caches.open(CACHE);
    try {
      const response = await fetch(event.request);
      if (response.ok) await cache.put(event.request, response.clone());
      return response;
    } catch (error) {
      const cached = await cache.match(event.request, { ignoreSearch: true });
      if (cached) return cached;
      throw error;
    }
  })());
});

self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = { body: event.data ? event.data.text() : "" };
  }
  event.waitUntil(self.registration.showNotification(data.title || "infomaster", {
    body: data.body || "", tag: data.tag, renotify: Boolean(data.tag), lang: "",
    icon: "/web/static/icons/icon-192.png", data: { url: data.url || "/web/" },
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data && event.notification.data.url || "/web/", location.origin);
  event.waitUntil((async () => {
    const open = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const app = open.find((client) => new URL(client.url).pathname.startsWith("/web/"));
    if (app) {
      app.postMessage({ open: url.hash });  // the app moves to the screen without loading again
      return app.focus();
    }
    return self.clients.openWindow(url.href);
  })());
});
