// Keep the original script URL so browsers with an installed offline worker can update.
self.addEventListener('install', (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    try {
      await self.clients.claim();
      const names = await caches.keys();
      await Promise.all(names
        .filter((name) => /^(bambastic|bambuddy)-(static-)?v\d+$/.test(name))
        .map((name) => caches.delete(name)));
    } finally {
      await self.registration.unregister();
    }
  })());
});
