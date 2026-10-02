<p align="center">
  <a href="https://orpheus-agents.github.io/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/orpheus-logo.svg">
      <img src=".github/orpheus-logo-light.svg" alt="Orpheus" width="240">
    </picture>
  </a>
</p>

# orpheus-gitlab-mr-review

Review Gitlab MRs with Orpheus

## Runtime configuration

Copy `.env.dist` to `.env` and configure GitLab and Orpheus credentials. The connector requires:

- `GITLAB_BASE_URL` and `GITLAB_TOKEN`;
- `ORPHEUS_BASE_URL` and `ORPHEUS_API_KEY`;
- `ORPHEUS_AGENT_PROFILE` and `ORPHEUS_SANDBOX_TEMPLATE`;
- `ORPHEUS_AGENT_INSTRUCTION_FILES`, a comma-separated ordered list of mounted `.md` files.

The instruction files contain user-owned developer instructions, such as Kubernetes compliance and
project-specific rules. The connector-owned operating mode and review protocol remain embedded in
the binary. Instruction files are loaded once during startup and must be non-empty English-language
UTF-8 Markdown.

The sandbox template must provide Git, Python 3, `base64`, `gzip`, and `sha256sum`. It must also
provide read-only GitLab authentication for repository cloning and `glab` queries.

Timeout variables use explicit seconds: `HTTP_TIMEOUT_SECONDS`, `POLL_INTERVAL_SECONDS`,
`SHUTDOWN_TIMEOUT_SECONDS`, `RUN_TIMEOUT_SECONDS`, and `HOOK_TIMEOUT_SECONDS`.

`RECONCILE_WORKER_COUNT` controls parallel Orpheus lifecycle calls and
`RECONCILE_QUEUE_CAPACITY` bounds the in-memory queue of complete poll snapshots. GitLab polling
submits a snapshot only after every candidate was fetched successfully and never waits for Orpheus
requests. Before admission, the reconciler restores active workflow sessions from Orpheus and
compares them with current GitLab state. A transient read error prevents all creates for that tick.
`MAX_CONCURRENT_REVIEWS` bounds active Orpheus review sessions; excess candidates remain assigned and
are reconsidered on the next poll.

Completed runs are published idempotently from the validated `after_run` bundle. Findings,
recommendations, recurrence replies, and the completion note use bot-owned trailing markers. The
connector checks the current diff before every mutation and removes only its own reviewer after the
completion marker is visible. Publication resumes from GitLab markers after a retry or restart.

Recurring findings carry an explicit reference to the previous bot-owned discussion. An open thread
is retained, a thread marked by GitLab as `resolved_by_push` is replaced at the current diff
position, and an explicitly human-resolved thread receives a finding-specific reply before it is
reopened. If GitLab does not expose the resolution origin, publication fails closed instead of
guessing.
