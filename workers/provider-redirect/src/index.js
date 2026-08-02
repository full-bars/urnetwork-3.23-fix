// provider.fullbars.xyz -> https://github.com/full-bars/urnetwork-3.23-fix
// 301 redirect preserving path + query. Bound to route provider.fullbars.xyz/*.
// DNS: proxied A record provider.fullbars.xyz -> 192.0.2.1 (TEST-NET placeholder).
addEventListener('fetch', event => {
  event.respondWith(Response.redirect("https://github.com/full-bars/urnetwork-3.23-fix" + (new URL(event.request.url).pathname + new URL(event.request.url).search), 301));
})
