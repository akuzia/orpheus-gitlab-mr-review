<p align="center">
  <a href="https://orpheus-agents.github.io/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/orpheus-logo.svg">
      <img src=".github/orpheus-logo-light.svg" alt="Orpheus" width="240">
    </picture>
  </a>
</p>

# orpheus-gitlab-mr-review

A CLI connector that triggers GitLab merge request review through Orpheus.

- assign the connector's GitLab user as an MR reviewer
- service creates an Orpheus Session
- publishes the validated findings
- removes itself from reviewers when the lifecycle is complete

## Requirements

- GitLab bot user with an API token and access to the projects it should review;
- Orpheus API key, agent profile, and sandbox template;
- one or more mounted Markdown files with general review instructions;
- read-only repository and `glab` authentication inside the Orpheus sandbox template;
- Git, Python 3, `base64`, `gzip`, and `sha256sum` in the sandbox.

The connector communicates only with GitLab and the Orpheus API. AgentBox execution is managed by
Orpheus.

## Configuration

Copy the example configuration:

```shell
cp .env.dist .env
```

Set the required values:

```dotenv
APP_MODE=prod

GITLAB_BASE_URL=https://gitlab.example.com
GITLAB_TOKEN=<bot-token>

ORPHEUS_BASE_URL=https://orpheus.example.com
ORPHEUS_WEB_BASE_URL=https://orpheus.example.com
ORPHEUS_API_KEY=<api-key>
ORPHEUS_AGENT_PROFILE=<profile>
ORPHEUS_SANDBOX_TEMPLATE=<sandbox-template>
ORPHEUS_AGENT_INSTRUCTION_FILES=/var/app/instructions/review-policy.md
```

`ORPHEUS_AGENT_INSTRUCTION_FILES` selects the custom review instructions described below.
`ORPHEUS_AGENT_MODEL` may be set to override the model selected by the Orpheus profile.

Timeout settings use integer seconds. The defaults in `.env.dist` are suitable for an initial run.

## Custom review instructions

Custom instructions define the general review policy applied to every merge request processed by the
connector instance. They are not project-level rules.

An instruction file can contain rules such as:

```markdown
# Review policy

- Report only actionable issues introduced by the current diff.
- Prioritize correctness, security, data loss, and backward compatibility.
- Do not report subjective style preferences unless they hide a concrete defect.
- Write all findings and recommendations in English.
```

Configure one file or an ordered, comma-separated list:

```dotenv
ORPHEUS_AGENT_INSTRUCTION_FILES=/var/app/instructions/review-policy.md,/var/app/instructions/security-policy.md
```

Files are appended in the configured order and passed verbatim to Orpheus as developer instructions.
Each path must be unique and point to a regular, non-empty UTF-8 file with the `.md` extension. The
instructions should be written in English and must not contradict the embedded operating mode or
publication protocol.

For a container deployment, mount the files read-only:

```shell
--volume /host/review-instructions:/var/app/instructions:ro
```

Instructions are loaded once when the connector starts. Restart the process after changing them;
existing Orpheus Sessions retain the workflow revision with which they were created.

## Run locally

Go 1.27 or newer is required.

```shell
go run .
```

For development with Air:

```shell
make up
docker compose logs -f air
```

The process runs until it receives `SIGINT` or `SIGTERM` and performs bounded graceful shutdown.

All application logs are newline-delimited JSON on stdout, including startup failures before
configuration is loaded. Each record contains `timestamp`, `level`, and `msg`; errors include an
`error` field. Configuration, initialization, and runtime failures exit with code 1. Startup failures
are reported regardless of `LOG_LEVEL`.

## Run with Docker

The instruction path in `.env` must match its path inside the container:

```shell
docker run --detach \
  --name orpheus-gitlab-mr-review \
  --restart unless-stopped \
  --env-file .env \
  --volume /host/path/instructions.md:/var/app/instructions/review-policy.md:ro \
  retailcrm/orpheus-gitlab-mr-review:latest
```

For this example, configure:

```dotenv
ORPHEUS_AGENT_INSTRUCTION_FILES=/var/app/instructions/review-policy.md
```

Run exactly one connector instance for a GitLab installation and bot identity. Concurrent instances
are unsupported because admission does not use a distributed lock.

## Request a review

1. Add the bot user to the GitLab project.
2. Assign the bot as reviewer on an open merge request.
3. Wait for the next poll. The connector creates or resumes the matching Orpheus Session.
4. After successful analysis, findings and a completion note are published in GitLab.
5. The connector removes only itself from the reviewer list.

For a new review after completion or failure, assign the bot as reviewer again. Project scope is
controlled by GitLab access and reviewer assignment; no project allowlist is required.

The service exposes no HTTP API or health endpoints. Use process status and structured logs for
operations and diagnostics.

See [ARCHITECTURE.md](ARCHITECTURE.md) for lifecycle, idempotency, stale-result handling, and restart
recovery details.
