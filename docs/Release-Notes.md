# Release notes

End each changelog bullet with its merged pull request number, for example `(#123)`. Start each bullet with a bold title followed by its description. Use a separate bullet for each change.

Create `releases/_intro/<tag>.md` with two paragraphs. Write a short summary in the first paragraph. Use first person sparingly. Put the update and restart instruction in the second paragraph.

The generator uses local merge history and reads merged pull request labels with the GitHub CLI. Sign in to the GitHub CLI before running it. Add known limits to `releases/_limits/<tag>.md` when needed. Review the resulting notes and commit them before creating a tag.

```sh
scripts/release-notes.sh <tag>
scripts/release-notes-lint.sh releases/<tag>.md
scripts/pre-tag-check.sh <tag>
```

The release date comes from the selected changelog section when it carries a date. For an existing tag without a section date it uses the tag creation date, and for a tag that does not exist yet it uses the UTC date at generation time. The previous release is the nearest earlier `v*` tag before the target. Check the date and the previous release before the tag is created.

The generator and the lint read the repository name from `remote.origin.url`, so the same scripts serve every fork. They fall back to the repository they were written for when origin is not a GitHub url.

Private host and fleet patterns are configured per repository in `scripts/private-host-patterns.txt`. The checked-in file is empty until the repository owner adds local patterns. Names that must not appear in public notes are listed one per line in `scripts/release-notes-forbidden-names.txt`; the lint reads that file, so the list can change without editing the check. Both files contain the very strings the scanners search for, so any attribution scan run over a branch needs an exact-path exemption for these two files.

A release note file that is already committed keeps its unchanged long lines: the lint reports them as warnings and fails only on long lines that this change adds or edits.
