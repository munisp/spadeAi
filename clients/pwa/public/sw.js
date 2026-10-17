// Shell-only cache. API responses are never cached or substituted, so balances and states are always live.
const SHELL = "spade-shell-v2";
self.addEventListener("install", e => e.waitUntil(caches.open(SHELL).then(c => c.addAll(["/", "/manifest.webmanifest", "/icon.svg"]))));
self.addEventListener("activate", e => e.waitUntil(caches.keys().then(ks => Promise.all(ks.filter(k => k !== SHELL).map(k => caches.delete(k))))));
self.addEventListener("fetch", e => {
  const u = new URL(e.request.url);
  if (e.request.method !== "GET" || u.origin !== location.origin || /^\/(v1|internal|healthz)(\/|$)/.test(u.pathname)) return;
  // Only page navigations may fall back to the cached shell; other assets fail as real network errors.
  e.respondWith(fetch(e.request).catch(() => caches.match(e.request).then(r => r || (e.request.mode === "navigate" ? caches.match("/") : Response.error()))));
});
