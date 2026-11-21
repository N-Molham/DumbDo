const CACHE_NAME = "DUMBDO_PWA_CACHE_V2";
const ASSETS_TO_CACHE = [];

const preload = async () => {
  console.log("Installing web app");
  return await caches.open(CACHE_NAME)
    .then(async (cache) => {
      console.log("caching index and important routes");
      const response = await fetch("/asset-manifest.json");
      const assets = await response.json();
      ASSETS_TO_CACHE.push(...assets);
      console.log("Assets Cached:", ASSETS_TO_CACHE);
      return cache.addAll(ASSETS_TO_CACHE);
  });
}

// Fetch asset manifest dynamically
globalThis.addEventListener("install", (event) => {
  event.waitUntil(preload());
});

globalThis.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    // Purge caches from previous service worker versions
    const names = await caches.keys();
    await Promise.all(names.filter(name => name !== CACHE_NAME).map(name => caches.delete(name)));
    await clients.claim();
  })());
});

globalThis.addEventListener("fetch", (event) => {
  event.respondWith(
    caches.match(event.request).then((cachedResponse) => {
      return cachedResponse || fetch(event.request);
    })
  );
});