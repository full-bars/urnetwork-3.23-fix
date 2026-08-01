const GITHUB_API = 'https://api.github.com/repos/full-bars/urnetwork-3.23-fix';
const GITHUB_DL = 'https://github.com/full-bars/urnetwork-3.23-fix';

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

    if (url.pathname === '/latest-version') {
      return getLatestVersion(ctx);
    }

    if (url.pathname.startsWith('/releases/download/')) {
      return proxyRelease(request, url);
    }

    return new Response('Not found', { status: 404 });
  }
};

async function getLatestVersion(ctx) {
  const cacheKey = 'https://dl.fullbars.xyz/latest-version';
  const cache = caches.default;
  let cached = await cache.match(cacheKey);
  if (cached) return cached;

  const resp = await fetch(`${GITHUB_API}/releases/latest`, {
    headers: { 'User-Agent': 'fullbars-dl-worker', 'Accept': 'application/vnd.github.v3+json' }
  });
  if (!resp.ok) return new Response('failed', { status: 502 });

  const data = await resp.json();
  const tagName = data.tag_name;
  if (!tagName) return new Response('failed', { status: 502 });

  const response = new Response(tagName + '\n', {
    headers: { 'Content-Type': 'text/plain', 'Cache-Control': 'public, max-age=300' }
  });
  ctx.waitUntil(cache.put(cacheKey, response.clone()));
  return response;
}

const FORWARD_REQUEST_HEADERS = ['range', 'if-none-match', 'if-modified-since'];

async function proxyRelease(request, url) {
  const githubUrl = `${GITHUB_DL}${url.pathname}${url.search}`;

  const forwardHeaders = new Headers();
  for (const name of FORWARD_REQUEST_HEADERS) {
    const value = request.headers.get(name);
    if (value) forwardHeaders.set(name, value);
  }
  forwardHeaders.set('User-Agent', 'fullbars-dl-worker');

  let resp;
  try {
    resp = await fetch(githubUrl, { method: request.method, headers: forwardHeaders });
  } catch (err) {
    return new Response('failed', { status: 502 });
  }
  return new Response(resp.body, { status: resp.status, statusText: resp.statusText, headers: resp.headers });
}
