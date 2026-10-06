const GITHUB_API = 'https://api.github.com/repos/full-bars/urnetwork-3.23-fix';
const GITHUB_DL = 'https://github.com/full-bars/urnetwork-3.23-fix';

// Bound each upstream lookup so a hung connection falls through to the next path
// instead of holding the request until the Worker's own limit. Built per call —
// an AbortSignal.timeout starts counting at creation, not at fetch time.
const LOOKUP_TIMEOUT_MS = 5000;

// A successful tag is cached for this long. This is the only window in which the
// endpoint can report a tag that has just been superseded upstream.
const POSITIVE_TTL_SECONDS = 300;

// A failed lookup is cached briefly so a GitHub outage does not make every request
// retry upstream. This only ever short-circuits a FAILED lookup — a tag is never
// negatively cached, so it can never pin a stale release.
const NEGATIVE_TTL_SECONDS = 30;

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

    if (url.pathname === '/latest-version') {
      return getLatestVersion(env, ctx);
    }

    if (url.pathname.startsWith('/releases/download/')) {
      return proxyRelease(request, url);
    }

    return new Response('Not found', { status: 404 });
  }
};

async function getLatestVersion(env, ctx) {
  const cacheKey = 'https://dl.fullbars.xyz/latest-version';
  const cache = caches.default;
  const cached = await cache.match(cacheKey);
  if (cached) {
    // A cached failure is short-lived and must not be cached by clients.
    return cached.ok ? cached : lookupFailedResponse();
  }

  const tagName = await resolveLatestTag(env);
  if (!tagName) {
    ctx.waitUntil(cache.put(cacheKey, new Response('failed', {
      status: 502,
      headers: { 'Cache-Control': `public, max-age=${NEGATIVE_TTL_SECONDS}` }
    })));
    return lookupFailedResponse();
  }

  const response = new Response(tagName + '\n', {
    headers: { 'Content-Type': 'text/plain', 'Cache-Control': `public, max-age=${POSITIVE_TTL_SECONDS}` }
  });
  ctx.waitUntil(cache.put(cacheKey, response.clone()));
  return response;
}

function lookupFailedResponse() {
  return new Response('failed', { status: 502, headers: { 'Cache-Control': 'no-store' } });
}

// Resolve the latest release tag WITHOUT depending on the rate-limited GitHub API.
// api.github.com allows only 60 anonymous requests/hour per source IP, and Workers
// egress from shared Cloudflare IPs, so that budget is exhausted by unrelated
// traffic and the API path returns 403. That made this endpoint answer 502 for
// everyone, which defeats its whole purpose (it exists to rescue clients whose own
// GitHub API call was rate-limited). The /releases/latest web endpoint 302s to the
// tag URL and is not subject to the API rate limit, so it is the primary path.
//
// Caveat inherited from GitHub: /releases/latest (web and API alike) ignores
// releases marked as prerelease or draft. If the newest provider release is ever
// flagged prerelease, both paths 404 and this endpoint answers 502.
async function resolveLatestTag(env) {
  // Two redirect strategies, so a surprise in either cannot take the endpoint down:
  // 'manual' returns the raw 3xx with Location readable (documented: "the 3xx
  // redirect response will be returned to the caller as-is") at one round-trip and
  // zero body bytes; 'follow' reads the final URL off resp.url.
  for (const redirect of ['manual', 'follow']) {
    try {
      const resp = await fetch(`${GITHUB_DL}/releases/latest`, {
        redirect,
        headers: { 'User-Agent': 'fullbars-dl-worker' },
        signal: AbortSignal.timeout(LOOKUP_TIMEOUT_MS)
      });
      const target = resp.headers.get('Location') || resp.url || '';
      if (resp.body) await resp.body.cancel();
      if (resp.ok || (resp.status >= 300 && resp.status < 400)) {
        const match = target.match(/\/releases\/tag\/([^/?#]+)/);
        if (match && match[1]) return decodeURIComponent(match[1]);
      }
    } catch (err) {
      // try the next strategy
    }
  }

  // Fallback: the GitHub API. Set the GITHUB_TOKEN secret to raise the anonymous
  // 60/hr ceiling to 5000/hr so a shared egress IP cannot exhaust it.
  try {
    const headers = {
      'User-Agent': 'fullbars-dl-worker',
      'Accept': 'application/vnd.github.v3+json'
    };
    if (env && env.GITHUB_TOKEN) {
      headers['Authorization'] = `Bearer ${env.GITHUB_TOKEN}`;
    }
    const resp = await fetch(`${GITHUB_API}/releases/latest`, {
      headers,
      signal: AbortSignal.timeout(LOOKUP_TIMEOUT_MS)
    });
    if (resp.ok) {
      const data = await resp.json();
      if (data.tag_name) return data.tag_name;
    }
  } catch (err) {
    // fall through
  }

  return null;
}

const FORWARD_REQUEST_HEADERS = ['range', 'if-none-match', 'if-modified-since'];

// NOTE: no timeout here on purpose — these are the asset downloads, and aborting
// mid-stream would truncate a legitimate large tarball.
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
