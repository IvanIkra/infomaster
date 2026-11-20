// The web app's service worker (§8.6). The app's own files go network first, then the cache: an update is seen on
// the next start, and the app still opens without a connection. /web/api is never cached. A push (stage 16.4) shows a
// notification; a tap on it brings the app forward on the screen it names.
const CACHE = "infomaster-web";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
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
