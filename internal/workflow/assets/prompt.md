Review GitLab merge request `!{{ .MergeRequestIID }}` in project `{{ .ProjectPath }}`.

## Immutable session context

- MR key: `{{ .MRKey }}`
- Title: `{{ .Title }}`
- URL: {{ .MergeRequestURL }}
- State: `{{ .State }}`
- Draft: `{{ .Draft }}`
- Source -> target: `{{ .SourceBranch }}` -> `{{ .TargetBranch }}`
- Reviewer bot: `{{ .ReviewerUsername }}` (user ID `{{ .ReviewerUserID }}`)
- Diff fingerprint: `{{ .DiffFingerprint }}`
- Review fingerprint: `{{ .ReviewFingerprint }}`
- Review artifacts: `{{ .ArtifactsPath }}`
- Diff refs:
  - base: `{{ .BaseSHA }}`
  - start: `{{ .StartSHA }}`
  - head: `{{ .HeadSHA }}`

## Merge request description

{{ .Description }}

## Operating mode

1. This is an unattended session. Do not ask the user questions or wait for interactive input.
2. Read and modify project files only inside the provided workspace.
3. GitLab mutations are forbidden. You may use `glab` only for read-only requests. The connector
   owns comments, discussions, resolution, approval, and reviewer changes.
4. Use Git only inside the current checkout. Do not switch the checkout to another commit.
5. Never print or write tokens, SSH keys, or other secrets.
6. Do not expand the scope. Review only the pinned diff described by this prompt and environment.
7. Do not install plugins or request approval to install them. If a required check cannot run,
   record the exact reason without requesting interactive help.

## Pinned diff

Always start with:

```sh
git diff "$ORPHEUS_GITLAB_DIFF_BASE_SHA"..."$ORPHEUS_GITLAB_DIFF_HEAD_SHA"
git log "$ORPHEUS_GITLAB_DIFF_BASE_SHA".."$ORPHEUS_GITLAB_DIFF_HEAD_SHA" --oneline
```

If the refs cannot be resolved or the diff cannot be reviewed, do not report a false clean result.
Record an unfinished finding with the blocking reason. The validation hook will fail the workflow.

When needed, read current merge request metadata, notes, and discussions through `glab` in read-only
mode. Code analysis and findings must still refer only to the pinned diff. Do not substitute newer
branch state for the pinned SHAs.

## Review artifacts

Work only in `{{ .ArtifactsPath }}`:

```text
findings/
confirmed/
rejected/
recommendations/
resolutions/
```

Write every potential finding immediately as a separate Markdown file in `findings/`. Use this
required format:

```md
---
id: F-0001
path: internal/service.go
line: 42
severity: warning
title: Incomplete validation
source: AGENTS.md
---

A self-contained explanation of the problem, evidence, checks performed, and the recommended fix.
```

Every finding requires a unique `id`, a repository-relative `path`, a positive line number in the
new version, `severity` (`info`, `warning`, or `error`), `title`, `source`, and a non-empty body.

If a confirmed finding is the same issue as an Orpheus finding from an earlier review, add all four
fields below to its front matter. Copy the discussion, exact note, and exact trailing marker from
GitLab. Write a concise, finding-specific `recurrence_comment` explaining what you rechecked, why
the defect still exists, and what still has to be fixed. Do not use a stock sentence.

```md
previous_discussion_id: discussion-id
previous_note_id: 123
previous_marker: "<!-- orpheus-review-finding:... -->"
recurrence_comment: "The nil path is still reachable from ParseConfig; guard the lookup before dereferencing it."
```

After the initial analysis, verify every finding again against the pinned diff and surrounding code.
Then atomically move it to `confirmed/` if the issue is valid, or to `rejected/` if it is not. Add the
exact rejection reason to every rejected finding. `findings/` must be empty before successful
completion. An invalid or unfinished finding fails the workflow instead of producing partial output.

Store important rules that should be documented as separate files in `recommendations/`. Do not
modify the target repository documentation only to record such a recommendation during review.

Create a resolution only for an Orpheus finding whose cause is demonstrably fixed in the pinned
diff. Store the intent in `resolutions/`:

```md
---
discussion_id: discussion-id
note_id: 123
marker: "<!-- orpheus-review-finding:... -->"
---

The verified path and line, the cause of the original finding, and evidence that it is fixed.
```

Do not create resolutions for another author's discussion, a discussion without an Orpheus marker,
or a change without evidence. The connector independently verifies author identity, the marker, and
the current diff.

## Existing discussions

Use discussions read through `glab` to verify previous findings. For every previous Orpheus finding:

- If it still reproduces, create a confirmed finding at its current diff position and include the
  complete previous-finding reference above. Do this whether the old thread is open, manually
  resolved, or automatically made outdated. The connector decides whether to retain, reopen, or
  replace that thread without creating a semantic duplicate.
- If it was explicitly resolved by a person with an explanation that accepts the risk or rejects
  the finding, do not recreate or reopen it unless new evidence makes it a materially different
  defect.
- If its cause is fixed, create a resolution intent only when the thread is still open.

Do not reply in GitLab or mutate discussions yourself. Never claim a previous marker that is not an
exact trailing marker on a note authored by the configured reviewer account.

## Completion

The final response is only for the session log and is not a publication channel for findings.
Briefly state that the artifacts for the current diff fingerprint were verified. Do not return final
JSON, publish anything to GitLab, or claim that the merge request is approved. The validation hook
validates the artifacts, and the connector independently validates the bundle and performs
idempotent publication and lifecycle operations.
