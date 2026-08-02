function edgeLatencyMs(cf) {
  if (cf.clientTcpRtt !== undefined && cf.clientTcpRtt !== null) return cf.clientTcpRtt;
  if (cf.clientQuicRtt !== undefined && cf.clientQuicRtt !== null) return cf.clientQuicRtt;
  return null;
}

addEventListener("fetch", event => {
  const request = event.request;
  const cf = request.cf;
  const rtt = edgeLatencyMs(cf);
  const data = {
    ip: request.headers.get("CF-Connecting-IP"),
    city: cf.city,
    region: cf.region,
    country: cf.country,
    loc: cf.latitude && cf.longitude ? cf.latitude + "," + cf.longitude : null,
    org: cf.asOrganization || null,
    AS: cf.asn || null,
    postal: cf.postalCode,
    timezone: cf.timezone,
    colo: cf.colo,
    edgeLatency: rtt !== null ? rtt + "ms" : null
  };
  event.respondWith(new Response(JSON.stringify(data, null, 2), {
    headers: { "content-type": "application/json", "Cache-Control": "no-store" }
  }));
});
