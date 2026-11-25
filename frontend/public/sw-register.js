// Existing clients need the retirement worker at the original script URL.
// Fresh installations use the browser's normal network path.
async function retireBambasticWorker() {
  if (!('serviceWorker' in navigator)) return;
  const registration = await navigator.serviceWorker.getRegistration('/');
  if (!registration || registration.scope !== location.origin + '/') return;
  const worker = registration.installing || registration.waiting || registration.active;
  if (worker?.scriptURL !== location.origin + '/sw.js') return;

  try {
    await registration.update();
  } catch {
    // The server can disappear during this visit. Retire the registration
    // locally so it cannot serve offline pages on a future browser session.
    await registration.unregister();
  }
}

// Prior kiosk sessions may have removed the registration but left cache entries.
async function clearBambasticCaches() {
  if (!('caches' in window)) return;
  const names = await window.caches.keys();
  await Promise.all(names
    .filter((name) => /^(bambastic|bambuddy)-(static-)?v\d+$/.test(name))
    .map((name) => window.caches.delete(name)));
}

// Back/forward navigation can resume a frozen document without contacting the server.
window.addEventListener('pageshow', (event) => {
  if (event.persisted) location.reload();
});

Promise.allSettled([retireBambasticWorker(), clearBambasticCaches()]);
