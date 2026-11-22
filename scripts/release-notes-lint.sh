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
except OSError as exc:
    sys.exit(str(exc))
errors = 0
fence = False
changed = False
intro_started = False
intro_done = False
intro = []
base = 'https://github.com/full-bars/urnetwork-3.23-fix'

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
        match = re.fullmatch(re.escape(base) + r'/commit/([0-9a-fA-F]+)', link)
        if match and subprocess.run(['git', '-C', str(root), 'cat-file', '-e', match.group(1) + '^{commit}'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
            error(n, f'unknown commit: {match.group(1)}')
    for pattern in patterns:
        if pattern.strip() and not pattern.lstrip().startswith('#') and re.search(pattern, prose, re.I):
            error(n, 'private host or fleet pattern')
    if re.search(r'\b(?:claude|gpt|terra|sol|luna|gemini|deepseek|codex|hermes|copilot|anthropic|openai)\b', prose, re.I):
        error(n, 'model or tool name')
    if re.search(r'\bexactly\b', prose, re.I):
        error(n, 'word exactly in prose')
    if re.search(r',\s+not\b', prose, re.I):
        error(n, 'comma followed by not')
    if not line.lstrip().startswith('|') and len(line) > 600:
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
