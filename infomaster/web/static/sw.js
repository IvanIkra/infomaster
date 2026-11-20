// The web app's service worker (§8.6). The app's own files go network first, then the cache: an update is seen on
// the next start, and the app still opens without a connection. /web/api is never cached. Pushes come in stage 16.4.
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
