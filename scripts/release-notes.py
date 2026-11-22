#!/usr/bin/env python3
"""Build a release body from reviewed changelog and intro inputs."""
import argparse
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import textwrap

ROOT = Path(subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip())
BASE = 'https://github.com/full-bars/urnetwork-3.23-fix'
os.chdir(ROOT)


def git(*args):
    return subprocess.check_output(['git', *args], text=True, stderr=subprocess.PIPE).strip()


def fail(message):
    sys.exit('release notes: ' + message)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('tag')
    parser.add_argument('--section', default='Unreleased')
    parser.add_argument('--out')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', args.tag):
        fail('invalid tag')
    intro = ROOT / 'releases' / '_intro' / (args.tag + '.md')
    if not intro.is_file():
        fail(f'missing intro input: {intro}')
    paragraphs = re.split(r'\n\s*\n', intro.read_text().strip())
    if len(paragraphs) != 2:
        fail('intro must contain a summary paragraph and an update paragraph')
    source = (ROOT / 'CHANGELOG.md').read_text()
    match = re.search(r'^## \[' + re.escape(args.section) + r'\][^\n]*\n(.*?)(?=^## \[|\Z)', source, re.M | re.S)
    if not match:
        fail(f'changelog section not found: {args.section}')
    section = match.group(1)
    target = args.tag if subprocess.run(['git', 'rev-parse', '--verify', '--quiet', args.tag + '^{commit}'], stdout=subprocess.DEVNULL).returncode == 0 else 'HEAD'
    previous_ref = target + '^' if target != 'HEAD' else target
    try:
        previous = git('describe', '--tags', '--abbrev=0', '--match', 'v*', previous_ref)
    except subprocess.CalledProcessError:
        fail('cannot find previous release tag')
    date_match = re.search(r'_Released (\d{4}-\d{2}-\d{2})', section)
    date = date_match.group(1) if date_match else git('show', '-s', '--format=%cs', target)
    mapping = {}
    mapfile = ROOT / 'releases' / '_map' / (args.tag + '.tsv')
    if mapfile.exists():
        for number, line in enumerate(mapfile.read_text().splitlines(), 1):
            if not line or line.startswith('#'):
                continue
            parts = line.split('\t')
            if len(parts) != 2 or parts[0] in mapping:
                fail(f'invalid mapping on line {number}')
            mapping[parts[0]] = parts[1].split(',')
    groups = {}
    category = None
    for line in section.splitlines():
        heading = re.match(r'^### (Added|Fixed|Changed|Security|Performance|CI|Known limits)\s*$', line)
        if heading:
            category = heading.group(1)
        elif line.startswith('### '):
            category = None
        elif line.startswith('- ') and category:
            groups.setdefault(category, []).append(line[2:].strip())
    if not any(k != 'Known limits' for k in groups):
        fail('changelog section has no change bullets')
    output = [f'# {args.tag}', '', f'**Released:** {date}.', '',
              f'Previous release: [{previous}]({BASE}/releases/tag/{previous}).', '',
              paragraphs[0], '', paragraphs[1], '',
              'Legend: **[redeploy]** needs the new provider, tool or container image. **[installer]** needs the updated installer or script. **[no redeploy]** covers documentation, CI and tests.', '',
              (ROOT / 'releases' / '_quickstart.md').read_text().rstrip(), '', "## What's changed"]
    actions = []
    pr_numbers = set()
    for category in ('Added', 'Fixed', 'Changed', 'Security', 'Performance', 'CI'):
        if category not in groups:
            continue
        output += ['', '### ' + category, '']
        for bullet in groups[category]:
            pr = re.search(r'\s*\(#(\d+)\)\s*$', bullet)
            body = bullet[:pr.start()].strip() if pr else bullet
            bold = re.match(r'^\*\*(.+?)\*\*\s*(.*)$', body)
            if bold:
                title = bold.group(1).rstrip('. ')
                description = bold.group(2).lstrip('. ').strip()
            else:
                title, sep, description = body.partition('. ')
                title = title.rstrip('.')
            refs = mapping.get(title)
            if not refs and pr:
                pr_numbers.add(int(pr.group(1)))
                messages = git('log', '--first-parent', '--merges', '--format=%H%x09%s', f'{previous}..{target}').splitlines()
                refs = [line.split('\t', 1)[0] for line in messages if re.search(r'\(#' + pr.group(1) + r'\)(?:\s|$)', line)]
            if not refs:
                fail(f'cannot resolve bullet: {title}')
            commits = []
            files = []
            for ref in refs:
                try:
                    sha = git('rev-parse', '--verify', ref.strip() + '^{commit}')
                    commits.append(sha)
                    files.extend(git('diff-tree', '--root', '--first-parent', '--no-commit-id', '--name-only', '-r', sha).splitlines())
                except subprocess.CalledProcessError:
                    fail(f'invalid commit {ref} for bullet: {title}')
            deploy = 'no redeploy'
            for f in files:
                name = Path(f).name
                if (f.endswith('.go') and not f.endswith('_test.go') and (f.startswith(('provider/', 'internal/')) or '/' not in f)) or name.startswith('Dockerfile') or re.match(r'^(docker-)?compose.*\.ya?ml$', name):
                    deploy = 'redeploy'
                    break
                if f.startswith('scripts/Provider_Install') or name.startswith('install-') and name.endswith(('.sh', '.ps1')):
                    deploy = 'installer'
            links = ', '.join(f'[{sha[:7]}]({BASE}/commit/{sha})' for sha in commits)
            line = f'- **{title}. [{deploy}]** {description} ({links})'
            output.extend(textwrap.wrap(line, width=580, subsequent_indent='  ', break_long_words=False, break_on_hyphens=False))
            if re.search(r'\b(restart|setting)\b', body, re.I):
                actions.append(title)
    try:
        raw = subprocess.check_output(['gh', 'pr', 'list', '-R', 'full-bars/urnetwork-3.23-fix', '--state', 'merged', '--label', 'needs-fleet-deploy', '--limit', '1000', '--json', 'number,title,mergeCommit'], text=True, stderr=subprocess.PIPE)
        prs = json.loads(raw)
    except (OSError, subprocess.CalledProcessError, ValueError) as exc:
        fail(f'cannot retrieve labelled operator actions: {exc}')
    for pr in prs:
        sha = (pr.get('mergeCommit') or {}).get('oid')
        if not sha:
            continue
        inside = subprocess.run(['git', 'merge-base', '--is-ancestor', sha, target], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
        before = subprocess.run(['git', 'merge-base', '--is-ancestor', sha, previous], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
        if inside and not before:
            actions.append(pr['title'])
    output += ['', '### Operator actions (needs-fleet-deploy)', '']
    output += [f'- {item}' for item in dict.fromkeys(actions)] if actions else ['None.']
    output += ['', '## Known limits', '']
    limits = ROOT / 'releases' / '_limits' / (args.tag + '.md')
    output.append(limits.read_text().strip() if limits.exists() else '\n'.join('- ' + x for x in groups.get('Known limits', [])) or 'None.')
    destination = Path(args.out) if args.out else ROOT / 'releases' / (args.tag + '.md')
    if destination.exists() and not args.out:
        fail(f'output already exists: {destination}')
    destination.write_text('\n'.join(output) + '\n')
    print(destination)


if __name__ == '__main__':
    main()
