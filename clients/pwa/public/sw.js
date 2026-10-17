// Shell-only cache. API responses are never cached, so balances and states are always live.
const SHELL = "spade-shell-v1";
self.addEventListener("install", e => e.waitUntil(caches.open(SHELL).then(c => c.addAll(["/", "/manifest.webmanifest", "/icon.svg"]))));
self.addEventListener("fetch", e => {
  const u = new URL(e.request.url);
  if (e.request.method !== "GET" || u.origin !== location.origin) return;
  e.respondWith(fetch(e.request).catch(() => caches.match(e.request).then(r => r || caches.match("/"))));
});
