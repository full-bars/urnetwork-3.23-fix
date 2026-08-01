// dl.fullbars.xyz + install.fullbars.xyz
// - dl: proxies install/uninstall scripts + logs requests to dl-log
// - install: smart dispatcher (OS-detect bootstrapper) or browser landing page

const INSTALL_HOST = 'install.fullbars.xyz';
const SCRIPT_BASE = 'https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts';

const ROUTES = {
  '/install.sh': '/Provider_Install_Linux.sh',
  '/install-mac.sh': '/Provider_Install_Mac.sh',
  '/install-win.ps1': '/Provider_Install_Win32.ps1',
  '/uninstall.sh': '/Provider_Uninstall_Linux.sh',
  '/uninstall-win.ps1': '/Provider_Uninstall_Win32.ps1',
};

const INSTALL_ALIASES = {
  '/install.sh': 'https://dl.fullbars.xyz/install.sh',
  '/install-mac.sh': 'https://dl.fullbars.xyz/install-mac.sh',
  '/install-win.ps1': 'https://dl.fullbars.xyz/install-win.ps1',
  '/win.ps1': 'https://dl.fullbars.xyz/install-win.ps1',
};

const DISPATCHER = `#!/bin/sh
# URnetwork provider installer (auto-detect)
os="$(uname -s)"
case "$os" in
  Linux*)   script="https://dl.fullbars.xyz/install.sh" ;;
  Darwin*)  script="https://dl.fullbars.xyz/install-mac.sh" ;;
  MINGW*|MSYS*|CYGWIN*)
    echo "Detected Windows (Git Bash/MSYS/Cygwin). Launching PowerShell installer..."
    exec powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "irm https://dl.fullbars.xyz/install-win.ps1 | iex"
    ;;
  *)
    echo "Unsupported OS: $os"
    echo "Windows users - run this in PowerShell instead:"
    echo "  irm https://dl.fullbars.xyz/install-win.ps1 | iex"
    exit 1
    ;;
esac
echo "Detected $os -> $script"
tmp="$(mktemp)" || exit 1
trap 'rm -f "$tmp"' 0 1 2 15
curl -fsSL "$script" -o "$tmp" || { echo "download failed" >&2; exit 1; }
sh "$tmp" "$@"
`;

const LANDING_PAGE = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Install URnetwork Provider</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0d1117; color: #e6edf3; min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 24px;
  }
  .card { max-width: 680px; width: 100%; background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 32px; }
  h1 { font-size: 22px; margin: 0 0 6px; }
  p.sub { color: #8b949e; margin: 0 0 24px; }
  h2 { font-size: 15px; margin: 20px 0 8px; color: #58a6ff; }
  code { background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 2px 6px; font-size: 13px; }
  pre {
    background: #0d1117; border: 1px solid #30363d; border-radius: 8px; padding: 14px; overflow-x: auto; margin: 0;
    font-size: 13px; line-height: 1.5; user-select: all;
  }
  .note { color: #8b949e; font-size: 13px; margin-top: 8px; }
  a { color: #58a6ff; text-decoration: none; }
  a:hover { text-decoration: underline; }
</style>
</head>
<body>
  <div class="card">
    <h1>Install URnetwork Provider</h1>
    <p class="sub">One-liner installer for the full-bars provider fork. Always fetches the latest release.</p>

    <h2>Linux / macOS</h2>
    <pre>curl -fsSL https://install.fullbars.xyz | sh</pre>
    <div class="note">Runs the official Provider_Install_Linux.sh / Provider_Install_Mac.sh. Add <code>sudo</code> before <code>sh</code> if you want root-aware behavior (the installer will guide you).</div>

    <h2>Windows (PowerShell)</h2>
    <pre>irm https://install.fullbars.xyz/win.ps1 | iex</pre>

    <h2>Specify a version</h2>
    <pre>curl -fsSL https://install.fullbars.xyz | sh -s -- --version=v3.23.0-fix.26.4</pre>

    <div class="note" style="margin-top:20px">
      Direct script links: <a href="https://dl.fullbars.xyz/install.sh">install.sh</a> ·
      <a href="https://dl.fullbars.xyz/install-mac.sh">install-mac.sh</a> ·
      <a href="https://dl.fullbars.xyz/install-win.ps1">install-win.ps1</a>
    </div>
  </div>
</body>
</html>
`;

addEventListener('fetch', event => {
  const url = new URL(event.request.url);
  const path = url.pathname;

  if (url.hostname === INSTALL_HOST) {
    if (path === '/' || path === '') {
      const accept = event.request.headers.get('Accept') || '';
      if (accept.includes('text/html')) {
        event.respondWith(new Response(LANDING_PAGE, {
          headers: { 'Content-Type': 'text/html; charset=utf-8' },
        }));
      } else {
        event.respondWith(new Response(DISPATCHER, {
          headers: { 'Content-Type': 'text/x-shellscript; charset=utf-8' },
        }));
      }
      return;
    }
    const alias = INSTALL_ALIASES[path];
    if (alias) {
      event.respondWith(Response.redirect(alias, 302));
      return;
    }
    event.respondWith(new Response('Not Found', { status: 404 }));
    return;
  }

  // dl.fullbars.xyz handling (existing behavior)
  const suffix = ROUTES[path];
  if (!suffix) {
    event.respondWith(new Response('Not Found', { status: 404 }));
    return;
  }

  const target = SCRIPT_BASE + suffix;
  event.respondWith(fetch(target, { headers: { 'User-Agent': 'cloudflare-worker' } }));

  const cf = event.request.cf || {};
  const params = new URLSearchParams({
    method: event.request.method,
    uri: path,
    ip: event.request.headers.get('CF-Connecting-IP') || '-',
    ua: event.request.headers.get('User-Agent') || '-',
    country: cf.country || '--',
  });
  event.waitUntil(
    fetch('https://dl-log.fullbars.xyz/?' + params.toString(), { method: 'GET', signal: AbortSignal.timeout(2000) }).catch(() => {})
  );
});
