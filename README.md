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
requests. The reconciler compares consecutive snapshots and cancels an active review when its merge
request disappears from the authenticated reviewer's selection.
