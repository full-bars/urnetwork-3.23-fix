# Release notes

End each changelog bullet with its merged pull request number, for example `(#123)`. Start each bullet with a bold title followed by its description. Use a separate bullet for each change.

Create `releases/_intro/<tag>.md` with two paragraphs. Write a short summary in the first paragraph. Use first person sparingly. Put the update and restart instruction in the second paragraph.

The generator uses local merge history and reads merged pull request labels with the GitHub CLI. Sign in to the GitHub CLI before running it. Add known limits to `releases/_limits/<tag>.md` when needed. Review the resulting notes and commit them before creating a tag.

```sh
scripts/release-notes.sh <tag>
scripts/release-notes-lint.sh releases/<tag>.md
scripts/pre-tag-check.sh <tag>
```

The release date comes from the selected changelog section when present. Otherwise it comes from the target commit. The generator uses `HEAD` until the tag exists. The previous release is the nearest earlier tag. Check both dates and the previous release before the tag is created.

Private host and fleet patterns are configured per repository in `scripts/private-host-patterns.txt`. The checked-in file is empty until the repository owner adds local patterns. Keep private names out of public notes. The lint warns for unchanged long lines in the published 36.0 notes and rejects long lines in new notes.

The release date comes from the changelog section date when present. For an existing tag without a section date, it uses the tag creation date. For a new tag it uses the UTC date when notes are generated. Check the date before tagging.
