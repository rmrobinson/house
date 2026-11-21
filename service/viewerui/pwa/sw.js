// House service worker. viewerui is a live control surface, so this worker
// deliberately caches almost nothing: a cached dashboard would show stale
// light/door state. All it does is swap a failed page load for a small
// self-contained "can't reach the house" page. Commands (POST), the SSE
// streams and the WHEP endpoints are never touched - the browser handles them
// directly.
//
// Bump CACHE if /offline or its icon changes.
const CACHE = 'house-shell-v1';
const SHELL = ['/offline', '/static/icons/icon-192.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET' || req.mode !== 'navigate') return;
  const offline = () => caches.match('/offline');
  e.respondWith(
    fetch(req).then(
      // A reverse proxy answers 502/503/504 when viewerui itself is down.
      (res) => (res.status >= 502 && res.status <= 504 ? offline().then((o) => o || res) : res),
      () => offline().then((o) => o || Response.error())
    )
  );
});
