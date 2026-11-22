#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fixture=scripts/testdata/release-notes/good.md
lint=scripts/release-notes-lint.sh
"$lint" "$fixture"
fail_case() {
  local name=$1 line=$2
  cp "$fixture" "$tmp/bad.md"
  printf '\n%s\n' "$line" >> "$tmp/bad.md"
  if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
    echo "FAIL: $name was accepted" >&2
    exit 1
  fi
  echo "PASS: $name rejected"
}
fail_case 'em dash' 'Text — text.'
fail_case 'semicolon' 'Text; text.'
fail_case 'upstream link' '[link](https://github.com/urnetwork/connect)'
fail_case 'external link' '[link](https://example.com/)'
fail_case 'model name' 'The model is Codex.'
fail_case 'exactly' 'There are exactly two.'
fail_case 'contrast' 'Use this, not that.'
fail_case 'not but contrast' 'This is not one thing but another.'
fail_case 'bullet first person' '- I made it.'
fail_case 'unknown commit' '[commit](https://github.com/full-bars/urnetwork-3.23-fix/commit/0000000000000000000000000000000000000000)'
fail_case 'line length' "$(printf '%0601d' 0)"
sed 's/\[redeploy\]//' "$fixture" > "$tmp/bad.md"
if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
  echo 'FAIL: missing deploy tag accepted' >&2
  exit 1
fi
echo 'PASS: missing deploy tag rejected'
sed 's/I made the release/I I I made the release/' "$fixture" > "$tmp/bad.md"
if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
  echo 'FAIL: excessive first person accepted' >&2
  exit 1
fi
echo 'PASS: excessive first person rejected'
echo 'Lint fixture checks passed.'
repo="$tmp/repo"
mkdir -p "$repo/scripts" "$repo/releases/_intro" "$repo/releases/_map" "$repo/provider" "$repo/bin"
cp scripts/release-notes.sh scripts/release-notes.py scripts/release-notes-lint.sh scripts/private-host-patterns.txt "$repo/scripts/"
cp releases/_quickstart.md "$repo/releases/"
git -C "$repo" init -q
git -C "$repo" config user.name Test
git -C "$repo" config user.email test@example.invalid
printf 'base\n' > "$repo/README.md"
git -C "$repo" add README.md
git -C "$repo" commit -qm 'base'
git -C "$repo" tag v-old
printf 'package provider\n' > "$repo/provider/provider.go"
git -C "$repo" add provider/provider.go
git -C "$repo" commit -qm 'add provider'
sha=$(git -C "$repo" rev-parse HEAD)
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
# Changelog

## [Unreleased]

### Added

- **Feature.** New behavior.
CHANGELOG
printf 'I added a feature.\n\nUpdate and restart the provider.\n' > "$repo/releases/_intro/v-test.md"
printf 'Feature\t%s\n' "$sha" > "$repo/releases/_map/v-test.tsv"
printf '#!/usr/bin/env bash\nprintf "[]\\n"\n' > "$repo/bin/gh"
chmod +x "$repo/bin/gh"
(cd "$repo" && PATH="$repo/bin:$PATH" scripts/release-notes.sh v-test)
rg -q '^### Added$' "$repo/releases/v-test.md"
rg -q '\[redeploy\]' "$repo/releases/v-test.md"
rg -q "/commit/$sha" "$repo/releases/v-test.md"
(cd "$repo" && scripts/release-notes-lint.sh releases/v-test.md)
echo 'PASS: generator creates sections, deploy tag and commit link'
printf 'sensitive-example-pattern\n' > "$repo/scripts/private-host-patterns.txt"
printf '\nsensitive-example-pattern\n' >> "$repo/releases/v-test.md"
if (cd "$repo" && scripts/release-notes-lint.sh releases/v-test.md > /dev/null 2>&1); then
  echo 'FAIL: private pattern accepted' >&2
  exit 1
fi
echo 'PASS: private pattern rejected'
fail_case 'invalid commit name' '[commit](https://github.com/full-bars/urnetwork-3.23-fix/commit/no-such-commit)'
sed 's/\[redeploy\]/[redeploy] [installer]/' "$fixture" > "$tmp/bad.md"
if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
  echo 'FAIL: multiple deploy tags accepted' >&2
  exit 1
fi
echo 'PASS: multiple deploy tags rejected'
mkdir -p "$repo/docs"
printf 'documentation\n' > "$repo/docs/change.md"
git -C "$repo" add docs/change.md
git -C "$repo" commit -qm 'docs'
docsha=$(git -C "$repo" rev-parse HEAD)
printf '#!/bin/sh\n' > "$repo/scripts/Provider_Install_Test.sh"
git -C "$repo" add scripts/Provider_Install_Test.sh
git -C "$repo" commit -qm 'installer'
installsha=$(git -C "$repo" rev-parse HEAD)
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
# Changelog

## [Unreleased]

### Changed

- **Docs.** Explain the behavior.
- **Installer.** Update the installer.
- **Combined.** Update both.
CHANGELOG
printf 'Docs\t%s\nInstaller\t%s\nCombined\t%s,%s\n' "$docsha" "$installsha" "$sha" "$installsha" > "$repo/releases/_map/v-test.tsv"
(cd "$repo" && PATH="$repo/bin:$PATH" scripts/release-notes.sh v-test --out "$tmp/generated.md")
rg -q '^\- \*\*Docs\. \[no redeploy\]' "$tmp/generated.md"
rg -q '^\- \*\*Installer\. \[installer\]' "$tmp/generated.md"
rg -q '^\- \*\*Combined\. \[redeploy\]' "$tmp/generated.md"
echo 'PASS: documentation and installer classification and redeploy precedence'
# Merge a branch so the changelog suffix resolves from first-parent history.
git -C "$repo" checkout -qb feature
printf 'package provider\n// feature\n' > "$repo/provider/merge.go"
git -C "$repo" add provider/merge.go
git -C "$repo" commit -qm 'feature'
git -C "$repo" checkout -q master
git -C "$repo" merge -q --no-ff feature -m 'Merge feature (#42)'
merge_sha=$(git -C "$repo" rev-parse HEAD)
rm "$repo/releases/_map/v-test.tsv"
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
## [Unreleased]

### Added

- **Merge feature.** The feature works. (#42)
CHANGELOG
cat > "$repo/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
[ "$1" = pr ] && [ "$2" = list ]
case "${GH_STUB_MODE:-ok}" in
  ok) printf '[{"number":42,"title":"Restart the provider","mergeCommit":{"oid":"%s"}}]' "$GH_MERGE_SHA" ;;
  empty) printf '[]' ;;
  paginated) python3 -c 'import json,os; print(json.dumps([dict(number=i,title="Action "+str(i),mergeCommit={"oid":os.environ["GH_MERGE_SHA"]}) for i in range(1,151)]))' ;;
  fail) printf 'partial page' ; exit 1 ;;
  capped) python3 -c 'import json; print(json.dumps([{"number":1,"title":"x","mergeCommit":{"oid":"x"}}]*1000))' ;;
esac
STUB
chmod +x "$repo/bin/gh"
(cd "$repo" && PATH="$repo/bin:$PATH" GH_MERGE_SHA="$merge_sha" scripts/release-notes.sh v-test --out "$tmp/merge.md")
expect() {
  local name=$1 file=$2 pattern=$3
  if rg -q -- "$pattern" "$file"; then echo "PASS: $name"; else echo "FAIL: $name" >&2; exit 1; fi
}
expect 'merge commit link' "$tmp/merge.md" "/commit/$merge_sha"
expect 'labelled operator action' "$tmp/merge.md" '^- Restart the provider$'
expect 'merge of provider code needs redeploy' "$tmp/merge.md" '^\- \*\*Merge feature\. \[redeploy\]'
# A merge that only carries documentation stays no redeploy.
mkdir -p "$repo/docs"
git -C "$repo" checkout -qb docs-feature
printf 'documentation\n' > "$repo/docs/merge.md"
git -C "$repo" add docs/merge.md
git -C "$repo" commit -qm 'docs feature'
git -C "$repo" checkout -q master
git -C "$repo" merge -q --no-ff docs-feature -m 'Merge docs (#43)'
docs_merge_sha=$(git -C "$repo" rev-parse HEAD)
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
## [Unreleased]

### Added

- **Merge feature.** The feature works. (#42)
- **Merge docs.** The text changed. (#43)
CHANGELOG
(cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-test --out "$tmp/merge2.md")
expect 'provider merge stays redeploy with two bullets' "$tmp/merge2.md" '^\- \*\*Merge feature\. \[redeploy\]'
expect 'documentation merge stays no redeploy' "$tmp/merge2.md" "^\\- \\*\\*Merge docs\\. \\[no redeploy\\].*/commit/$docs_merge_sha"
# A changelog bullet must not carry the deploy tag that the generator adds.
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
## [Unreleased]

### Added

- **Tagged.** The text. [redeploy] (#42)
CHANGELOG
if (cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-test --out "$tmp/tagged.md" 2>"$tmp/err"); then
  echo 'FAIL: changelog deploy tag accepted' >&2
  exit 1
fi
rg -q 'carries its own deploy tag' "$tmp/err"
test ! -e "$tmp/tagged.md"
echo 'PASS: changelog deploy tag rejected'
# A bullet listed without a commit fails and prints the recorded reason.
printf 'Unmapped\t-\tno merge commit for the text change\n' > "$repo/releases/_map/v-test.tsv"
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
## [Unreleased]

### Added

- **Unmapped.** The text.
CHANGELOG
if (cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-test --out "$tmp/unmapped.md" 2>"$tmp/err"); then
  echo 'FAIL: unmapped bullet accepted' >&2
  exit 1
fi
rg -q 'unmapped bullet: Unmapped: no merge commit for the text change' "$tmp/err"
test ! -e "$tmp/unmapped.md"
echo 'PASS: unmapped bullet fails with its reason'
# A release reads its own changelog section once that section exists.
rm "$repo/releases/_map/v-test.tsv"
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
## [Unreleased]

### Added

- **Pending.** Not released yet.

## [v-test] - 2002-03-04

### Added

- **Merge feature.** The feature works. (#42)
CHANGELOG
(cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-test --out "$tmp/section.md")
expect 'tag section is used when present' "$tmp/section.md" '^\- \*\*Merge feature\.'
if rg -q 'Pending' "$tmp/section.md"; then
  echo 'FAIL: Unreleased bullet leaked into the tag section' >&2
  exit 1
fi
expect 'tag section date wins' "$tmp/section.md" '^\*\*Released:\*\* 2002-03-04\.$'
# The tag section is removed again so the later cases read Unreleased.
cat > "$repo/CHANGELOG.md" <<'CHANGELOG'
## [Unreleased]

### Added

- **Merge feature.** The feature works. (#42)
CHANGELOG
(cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-test --out "$tmp/empty.md")
rg -q '^None\.$' "$tmp/empty.md"
(cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=paginated GH_MERGE_SHA="$merge_sha" scripts/release-notes.sh v-test --out "$tmp/pages.md")
rg -q '^- Action 1$' "$tmp/pages.md"
rg -q '^- Action 150$' "$tmp/pages.md"
for mode in fail capped; do
  if (cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE="$mode" scripts/release-notes.sh v-test --out "$tmp/$mode.md" > /dev/null 2>&1); then
    echo "FAIL: gh $mode accepted" >&2
    exit 1
  fi
  test ! -e "$tmp/$mode.md"
done
echo 'PASS: first-parent merge, labelled actions, empty labels and closed failures'
# Untagged releases use the current UTC date. Section dates override tags.
today=$(date -u +%F)
rg -q "^\*\*Released:\*\* $today\.$" "$tmp/merge.md"
sed 's/\[Unreleased\]/[Unreleased] - 2001-02-03/' "$repo/CHANGELOG.md" > "$tmp/dated-changelog"
cp "$tmp/dated-changelog" "$repo/CHANGELOG.md"
(cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-test --out "$tmp/dated.md")
rg -q '^\*\*Released:\*\* 2001-02-03\.$' "$tmp/dated.md"
git -C "$repo" tag -a v-tagged -m tagged
printf 'I added a feature.\n\nUpdate and restart the provider.\n' > "$repo/releases/_intro/v-tagged.md"
sed 's/\[Unreleased\] - 2001-02-03/[Unreleased]/' "$repo/CHANGELOG.md" > "$tmp/undated-changelog"
cp "$tmp/undated-changelog" "$repo/CHANGELOG.md"
(cd "$repo" && PATH="$repo/bin:$PATH" GH_STUB_MODE=empty scripts/release-notes.sh v-tagged --out "$tmp/tagged.md")
rg -q "^\*\*Released:\*\* $(git -C "$repo" for-each-ref --format='%(creatordate:short)' refs/tags/v-tagged)\.$" "$tmp/tagged.md"
echo 'PASS: release date precedence and annotated tags'
