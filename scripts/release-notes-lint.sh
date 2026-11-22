#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: scripts/release-notes-lint.sh <file>" >&2
  exit 2
fi
root=$(git rev-parse --show-toplevel)
python3 - "$1" "$root" <<'PY'
import pathlib, re, subprocess, sys

file, root = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
try:
    lines = file.read_text(encoding='utf-8').splitlines()
    patterns = (root / 'scripts/private-host-patterns.txt').read_text().splitlines()
    forbidden = (root / 'scripts/release-notes-forbidden-names.txt').read_text().splitlines()
except OSError as exc:
    sys.exit(str(exc))
errors = 0
fence = False
changed = False
intro_started = False
intro_done = False
intro = []
# The repository is read from origin so the same scripts serve every fork.
try:
    url = subprocess.run(['git', '-C', str(root), 'config', '--get', 'remote.origin.url'], text=True, capture_output=True).stdout.strip()
except OSError:
    url = ''
match = re.search(r'github\.com[:/]+([^/]+)/([^/]+?)(?:\.git)?/?$', url)
base = 'https://github.com/' + (f'{match.group(1)}/{match.group(2)}' if match else 'full-bars/urnetwork-3.23-fix')
legacy_lines = []
# A release note file that is already committed keeps its unchanged long lines
# with a warning. Only lines added or edited in this working copy must fit.
try:
    relative = file.resolve().relative_to(root.resolve())
except ValueError:
    relative = None
if relative is not None:
    result = subprocess.run(['git', '-C', str(root), 'show', f'HEAD:{relative.as_posix()}'], text=True, capture_output=True)
    if result.returncode == 0:
        legacy_lines = result.stdout.splitlines()

def error(n, message):
    global errors
    errors += 1
    print(f'{file}:{n}: {message}', file=sys.stderr)

for n, line in enumerate(lines, 1):
    if re.match(r'^\s*(```|~~~)', line):
        fence = not fence
        continue
    if fence:
        continue
    prose = re.sub(r'(`+).*?\1', '', line)
    if line.startswith('## '):
        changed = line == "## What's changed"
    if line.startswith("### Operator actions"):
        changed = False
    if not intro_done and not intro_started and line.strip() and not line.startswith(('#', '**Released:**', 'Previous release:')):
        intro_started = True
    if intro_started and not intro_done:
        if not line.strip():
            intro_done = True
        else:
            intro.append(prose)
    if '—' in line:
        error(n, 'em dash')
    if ';' in prose:
        error(n, 'semicolon in prose')
    if re.search(r'github\.com/urnetwork/', line, re.I):
        error(n, 'upstream URL')
    for link in re.findall(r'\]\(([^)]+)\)', line):
        if not (link == base or link.startswith(base + '/')):
            error(n, f'link outside this repository: {link}')
        match = re.fullmatch(re.escape(base) + r'/commit/([^/?#]+)', link)
        if match and (not re.fullmatch(r'[0-9a-fA-F]{7,40}', match.group(1)) or subprocess.run(['git', '-C', str(root), 'cat-file', '-e', match.group(1) + '^{commit}'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode):
            error(n, f'unknown commit: {match.group(1)}')
    for pattern in patterns:
        if pattern.strip() and not pattern.lstrip().startswith('#') and re.search(pattern, prose, re.I):
            error(n, 'private host or fleet pattern')
    for name in forbidden:
        name = name.strip()
        if name and not name.startswith('#') and re.search(r'\b' + re.escape(name) + r'\b', prose, re.I):
            error(n, 'model or tool name')
            break
    if re.search(r'\bexactly\b', prose, re.I):
        error(n, 'word exactly in prose')
    if re.search(r',\s+not\b', prose, re.I):
        error(n, 'comma followed by not')
    if re.search(r'\bnot\s+[^.!?]+?\s+but\s+', prose, re.I):
        error(n, 'not X but Y contrast')
    if not line.lstrip().startswith('|') and len(line) > 600:
        if n <= len(legacy_lines) and line == legacy_lines[n - 1]:
            print(f'{file}:{n}: warning: unchanged historical line exceeds 600 characters', file=sys.stderr)
        else:
            error(n, 'line exceeds 600 characters')
    if re.match(r'^\s*[-*+]\s+', line):
        if re.search(r'\bI\b', prose):
            error(n, 'first person in bullet')
        if changed and not line.startswith('### '):
            tags = re.findall(r'\[(?:redeploy|installer|no redeploy)\]', line)
            if len(tags) != 1:
                error(n, "What's changed bullet must have one deploy tag")
count = len(re.findall(r'\bI\b', ' '.join(intro)))
if count == 0:
    print('warning: intro has no first-person I', file=sys.stderr)
elif count > 2:
    error(1, 'intro has more than two uses of I')
if errors:
    sys.exit(1)
print(f'{file}: release notes lint passed')
PY
