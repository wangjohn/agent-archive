# Cloud session capture: engineering specification

Status: proposed plan for review; not scheduled for implementation. Prepared 2026-09-24. This document describes a target design, not the current implementation. Claude Code on the web facts marked *verified* come from live probes on 2026-09-23 and 2026-09-24; everything else about vendor products comes from vendor documentation read on 2026-09-23 and is marked *documented* or *unknown*.

## Purpose

The archive covers only sessions that run on a configured Mac. The [product specification](../archive-spec.md#scope-and-non-goals) says: "Remote and cloud-hosted sessions require a collector in their execution environment and are not automatically covered by a Mac installation." Coding agents increasingly run in vendor-hosted VMs (Claude Code on the web, Codex cloud, Cursor Cloud Agents) or on CI runners. Those sessions are invisible to the archive today.

This specification adds cloud capture for the three harnesses the archive already supports, Claude Code, Codex and Cursor, wherever the vendors make it possible, without weakening the archive's existing guarantees: filtered native records, fresh-start eligibility, content-addressed sources, metadata written last, no secrets on disk or in output, and nothing on the agent's critical path that can block it.

## Why the Mac design does not reach the cloud

| Mac assumption | Cloud reality |
| --- | --- |
| Hooks in user-level app settings (`~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.cursor/hooks.json`) | Cloud agents load repository-committed hook files (plus team or managed ones); user-level hooks do not run |
| A LaunchAgent collector retries later | The VM is reclaimed after the session; nothing runs after the last hook |
| R2 credentials in Keychain; S3 through a named AWS profile | No Keychain; the only secret channel is the vendor's environment configuration |
| `machine_id` is a durable Mac identity; retention is swept by the registering machine | Each session gets a fresh VM; no machine ever returns to sweep |
| `project_id` hashes the local project root | The cloud checkout path (`/home/user/<repo>`) differs from the Mac path for the same repository |
| Releases are macOS-only (`scripts/build-release.sh` builds `GOOS=darwin` with cgo) | Cloud VMs are Linux x86_64 |

## Capture strategies

Three strategies exist. They differ in fidelity and in where credentials live.

1. **In-environment push.** A repository-committed hook runs the `agent-archive` binary inside the cloud VM. At each stop event it filters the native transcript and uploads it synchronously. This preserves full native-record fidelity and reuses the existing adapters. It needs upload credentials inside the VM.
2. **CI-runner push.** The same as in-environment push, on a runner the user controls (GitHub Actions running `claude-code-action` or `codex-action`). Hooks and transcripts are documented there, and secrets are ordinary CI secrets. This is the easiest path and is included in every phase that ships in-environment push.
3. **After-the-fact pull.** The Mac fetches a finished session from a vendor API and archives the result. No credentials enter the VM, but vendor APIs return derived views, not native transcripts, so fidelity is lower and each API needs its own adapter.

Push is the primary strategy. Pull is a fallback only where push is impossible or unreliable.

## Support matrix

| Agent | Hooks run in cloud | Native transcript in VM | Upload path | Credential channel | Pull fallback | Plan |
| --- | --- | --- | --- | --- | --- | --- |
| Claude Code on the web | *Verified*: repo `.claude/settings.json`, single-repository sessions only | *Verified*: `/root/.claude/projects/<cwd-slug>/<session_id>.jsonl` | *Verified* reachable: S3 and R2 through the proxy's TLS tunnel (default Trusted level) | Environment variables, readable by anyone using the environment | `claude --teleport` to a Mac (interactive, undocumented format); no public transcript API | Phase 1 |
| Claude Code in CI (`claude-code-action`, `claude -p`) | *Documented*: repo settings and a `settings` input | *Documented*: `transcript_path` | Runner egress | CI secrets | Not needed | Phase 1 |
| Cursor Cloud Agents | *Documented*: repo `.cursor/hooks.json`; `sessionStart` and `sessionEnd` do not run; hooks skip early read-only turns | *Unknown*; `transcript_path` may be null ("if transcripts disabled") | *Documented*: all egress allowed by default | Runtime Secrets (redacted from transcripts) | *Documented*: v0 `GET /v0/agents/{id}/conversation` (messages only, no tool calls); v1 run stream (retention-limited) | Phase 0 probe, Phase 2 |
| Codex cloud | *Unknown*: hooks documented for the CLI only; project hooks require trust review | *Unknown* | *Documented*: agent-phase internet off by default; an optional GET/HEAD/OPTIONS-only mode would block uploads | Secrets are removed before the agent phase; only plain environment variables remain | *None*: `codex cloud` returns status and diffs, not transcripts | Phase 0 probe, Phase 3 if the probe passes |
| Codex in CI (`codex-action`) | *Documented*: normal CLI hooks | *Documented*: rollout files under a configurable `codex-home` | Runner egress | CI secrets | Not needed | Phase 3 |

Other cloud agents, such as GitHub Copilot's cloud agent, Gemini CLI, Google Jules, Devin and Amp, are out of scope. Supporting any of them would mean a new harness adapter, not cloud capture for an existing one.

## Verified behavior: Claude Code on the web

Probes ran as one-off routines on the default Anthropic cloud environment, Claude Code 2.1.281.

- The VM is Ubuntu 24.04 x86_64, running as root with `HOME=/root` and `CLAUDE_CODE_REMOTE=true`. `agent-archive` builds there with Go 1.24 (`linux/amd64`) and `status` runs.
- Repository hooks fired for `SessionStart`, `UserPromptSubmit`, `SubagentStop` and `Stop`. `SessionEnd` could not be observed, because it runs after the last turn.
- `SessionStart` arrived with `source: "startup"` and a transcript that existed with 0 bytes, so the existing fresh-start proof in `provesFreshSessionStart` passes unchanged.
- `transcript_path` was always absolute, and its basename equalled `session_id`. At `Stop` the transcript already contained the final assistant records.
- `SubagentStop` carried `agent_transcript_path` at `…/<session_id>/subagents/agent-<id>.jsonl`, and the file existed. That is the layout the Claude child capture already expects.
- The transcript contains record types the Claude adapter does not allow (`atis-latch`, `attachment`, `last-prompt`, `queue-operation`) and new top-level keys (`wireToolInputs`, `turnOrigin`, `atis`, `classifierBoundary`, `apiBlockIndex`, `queueSkipAttachments`, `rendered`, `sourceToolAssistantUUID`). Today these are dropped with `unknown_record_type` and `unknown_field_omitted` gaps, so nothing fails, but coverage is incomplete.
- The egress proxy passes `*.amazonaws.com` and `<account>.r2.cloudflarestorage.com` through a CONNECT tunnel, so the client sees the provider's own certificate and SigV4 signing works unchanged. An unauthenticated HEAD returns 405 from S3 and 400 from R2. A made-up R2 account hostname fails the TLS handshake everywhere, so a reachability check must use a real account ID.
- Anthropic's own `launcher-settings.json` installs a `Stop` hook; repository hooks run alongside it.

## Design

### 1. Linux build

Add `linux/amd64` and `linux/arm64` release artifacts built with `CGO_ENABLED=0`, with checksums, next to the macOS artifacts. Keychain stays behind its existing `!darwin` stub. Mac-only commands (`setup`, the LaunchAgent paths in `internal/cli/env_defaults.go`, `uninstall`) return a clear "not supported in cloud mode" error on Linux instead of shelling out to `launchctl`. CI already runs tests on Ubuntu; add a Linux build and an end-to-end cloud-mode test against MinIO (the local end-to-end recipe already uses MinIO).

### 2. Cloud mode configuration

Cloud mode is selected only by an explicit environment variable, never inferred:

| Variable | Meaning |
| --- | --- |
| `AGENT_ARCHIVE_CLOUD=1` | Enables cloud mode. Without it, the cloud hook exits 0 immediately |
| `AGENT_ARCHIVE_PROVIDER` | `r2` or `s3` |
| `AGENT_ARCHIVE_BUCKET`, `AGENT_ARCHIVE_PREFIX` | Destination. The default prefix is `agent-archive/cloud/` |
| `AGENT_ARCHIVE_R2_ENDPOINT` | `https://<account>.r2.cloudflarestorage.com` |
| `AGENT_ARCHIVE_REGION` | S3 only |
| `AGENT_ARCHIVE_ACCESS_KEY_ID`, `AGENT_ARCHIVE_SECRET_ACCESS_KEY` | Static credentials for either provider |

In cloud mode the configuration is built in memory from these variables; no `config.json` or `setup` is involved. The Mac rule "do not silently use an unrelated environment credential" still holds: cloud mode reads only the `AGENT_ARCHIVE_*` names above, never ambient `AWS_*` credentials, and it never runs on a Mac install's configuration. An S3 user who wants web identity or OIDC (for example Cursor's documented OIDC token) can add it later as a separate, explicit variable.

The gate matters because repository hook files also run on every collaborator's laptop. With `AGENT_ARCHIVE_CLOUD` unset, the committed hook is a no-op, so it neither double-captures a session that the local install already captures nor fails on a machine without the binary.

### 3. Ephemeral capture command

Add an internal entry point, `agent-archive _hook --harness <h> --cloud`, that the committed hook files call.

- **Start events** register the session exactly as on the Mac (same fresh-start proof, same session index) under `AGENT_ARCHIVE_HOME`, which defaults to `$HOME/.local/share/agent-archive` inside the VM and lives as long as the VM.
- **Stop events** (`Stop`, `SubagentStop`, Cursor `stop` and `afterAgentResponse`, and Codex `Stop` if its probe passes) run a synchronous capture of that one session: filter, build the bundle, upload the source, read it back, then write metadata. This requires a new exported collector function, for example `collector.CaptureSession(ctx, local, store, registrationID, opts)`, wrapping the unexported `processSession` and the request and pending bookkeeping that `Run` does today. The urgent-request path already bypasses the 3-minute rate limit.
- **Retry within the VM.** A capture that fails leaves its `pending/` snapshot; the next stop event retries it first, exactly as the collector does. After the VM is gone there is no retry, so the last stop's success is the durability boundary.
- **Idempotence.** Each stop publishes a snapshot of the whole transcript so far. The source key is content-addressed, so an unchanged transcript uploads nothing new.
- **Never block the agent.** The command always exits 0, never returns a block decision, prints no transcript content or secrets, and writes one short status line to stderr on failure. The committed hook entry sets an explicit timeout (30 seconds proposed, versus 2 seconds for Mac hooks) because it uploads synchronously; start and prompt events stay local and fast.
- **No activation gate.** On a Mac, a project must be configured and activated before sessions register. In cloud mode, committing the hook file and configuring the environment variables is the opt-in, so every session in that repository with a proven fresh start is eligible. A resumed or teleported session still fails the fresh-start proof and is not captured.

### 4. Identity and metadata

The metadata schema has `additionalProperties: false`, so cloud fields require `schema_version: 2` of `metadata.schema.json`. Readers must accept versions 1 and 2.

- `execution`: `{ "kind": "local" | "cloud" | "ci", "provider": "claude_code_web" | "cursor_cloud" | "codex_cloud" | "github_actions" | …, "remote_session_id": "…" }`. `remote_session_id` comes from vendor variables such as `CLAUDE_CODE_REMOTE_SESSION_ID`, so a session can be linked back to the vendor UI.
- `machine_id`: in cloud mode, a stable pseudo-machine per provider and environment, for example `cloud-claude_code_web`, never the VM's random identity. This keeps `list` grouping meaningful and makes clear that the machine will not sweep its own sessions.
- `repo_key`: `sha256(normalized origin remote URL)[:16]`, as already proposed in the [handoff specification](../handoff.md). It is written for local and cloud sessions alike, so the same repository groups together even though `project_id` (a path hash) differs between a Mac and a VM.

`list` and `status` show the execution kind and provider. `show` and `handoff` are unchanged.

### 5. Adapter coverage

- **Claude.** Review each cloud-only record type and key above against the [privacy rules](../../agent-archive-privacy.md): allow what carries conversation structure (likely `attachment` with filtered contents), keep bookkeeping types as counted gaps, and bump the filter version. Add sanitized fixtures from a cloud transcript.
- **Cursor.** Reuse the desktop rules: register at the first `beforeSubmitPrompt` when `transcript_path` is null or empty, and adopt the path later. If the Phase 0 probe shows transcripts are disabled on cloud VMs, push capture cannot work and Cursor falls back to pull.
- **Codex.** Depends entirely on the Phase 0 probe.

### 6. Credentials and privacy

Cloud credentials are the weakest point of this design and are handled as follows.

- **Separate destination.** Use a separate bucket (preferred for R2, where tokens cannot be narrower than a bucket) or a separate prefix, so a leaked cloud credential cannot read the Mac archive.
- **Least privilege.** The ephemeral path needs `PutObject` and `GetObject` on its prefix: `Get` is needed for the source read-back and the published-metadata check. It does not need `List` or `Delete`. S3 policies can enforce that; R2 Object Read & Write tokens scoped to one bucket are the closest equivalent.
- **Visibility.** Claude Code environment variables are readable by anyone using that environment, and the agent itself can read them, so a prompt-injected agent could misuse them. Documentation must say so plainly and recommend a per-user environment. Cursor Runtime Secrets are redacted from transcripts and are the preferred channel there. Codex removes secrets before the agent phase, so only plain environment variables work.
- **Redaction.** Before filtering, cloud mode replaces any exact occurrence of the `AGENT_ARCHIVE_ACCESS_KEY_ID` and `AGENT_ARCHIVE_SECRET_ACCESS_KEY` values in the transcript with a fixed marker, and it records a `credential_redacted` gap. The general filter remains best effort.
- **Shared environments.** A team environment with one owner's credentials would archive teammates' sessions into that owner's bucket. `cloud provision` (below) warns about this, and `status` on the Mac shows each cloud provider and environment that has published sessions.
- **Supply chain.** The committed setup script installs a pinned release version and verifies its SHA-256 before running it. It never uses `curl | sh` from a moving URL.

### 7. Retention without a returning machine

Two mechanisms, both driven from the Mac, which has `List` and `Delete`:

1. **Cloud sweep.** A configured Mac (opt-in with a new `cloud_retention` setting) lists sessions under the cloud prefix, reads remote metadata, deletes superseded sources that metadata no longer references once they are past the existing 24-hour grace period, and expires whole sessions after `RetentionDays`. It uses the same metadata-first deletion order as `retention.Sweep`. Remote metadata replaces the local registration as proof of ownership, limited to `execution.kind` of `cloud` or `ci` under the cloud prefix.
2. **Bucket lifecycle rule** as a backstop. Setup guidance recommends an object lifecycle rule on the cloud prefix at `RetentionDays` plus a margin. Both R2 and S3 support lifecycle rules.

Without either mechanism, superseded snapshots accumulate: one full source per stop event. The sweep is required before cloud mode is recommended for regular use.

### 8. Provisioning, repository setup and verification

Setup has two halves. The storage half and the repository half can be automated. Getting credentials into each vendor's cloud environment can only partly be automated, because most vendors document no API for it. The goal is that a user pastes one block of variables per vendor, once, and never touches it again until the credentials are rotated.

What vendor documentation (read 2026-09-24) says can be configured without a web UI:

| Target | Programmatic? | Mechanism |
| --- | --- | --- |
| R2 bucket | Yes | `wrangler r2 bucket create`, or `POST /accounts/{account_id}/r2/buckets` |
| R2 lifecycle rule | Yes | `wrangler r2 bucket lifecycle add <bucket> … --expire-days N`, or `PUT /accounts/{account_id}/r2/buckets/{bucket}/lifecycle` |
| R2 bucket-scoped credential | Yes, API only (no wrangler command) | `POST /accounts/{account_id}/tokens` with the "Workers R2 Storage Bucket Item Write" permission group on the one bucket; the S3 access key ID is the token `id`, and the secret access key is the SHA-256 of the token `value` |
| R2 short-lived credential | Yes | `POST /accounts/{account_id}/r2/temp-access-credentials`: bucket and optional prefix scope, at most 7 days |
| S3 bucket, IAM user or role | Yes | AWS CLI (`aws s3api`, `aws iam`); OIDC roles for GitHub Actions |
| GitHub Actions secrets | Yes | `gh secret set`, or the Actions secrets REST API |
| Cursor agents launched through the API or SDK | Per launch only | `POST /v1/agents` accepts `envVars` (session-scoped, deleted with the agent) |
| Claude Code hosted environment variables | No | Environment dialog on claude.ai/code; server-managed settings are admin-UI only |
| Cursor saved secrets (agents started from the app) | No | Dashboard only |
| Codex cloud environment variables | No | Codex environment settings only |

The routines API used for the live probes carries an undocumented `environment_variables` field. Anthropic states the routines API has no public token management, so this design does not rely on it.

Commands:

- **`agent-archive cloud provision [--harness claude,cursor,codex] [--ci github]`**, run on a Mac inside a repository. It is optional; every step can also be done by hand from the printed instructions.
  1. **Storage.** For R2, it asks once for a bootstrap Cloudflare API token with permission to create account API tokens (Account API Tokens: Edit), stores it in Keychain under its own reference, and uses it only for `provision` and `rotate`. It creates the cloud bucket if missing (default `agent-archive-cloud`), adds a lifecycle rule at the cloud retention period plus a margin, and mints a bucket-scoped object token. For S3 it creates a bucket-prefix-scoped IAM user and access key with `PutObject` and `GetObject` only, or, for GitHub Actions, an OIDC role with no stored key. It then runs the synthetic write, read-back and delete test.
  2. **Repository files.** It writes or merges the committed files: `.claude/settings.json`, `.cursor/hooks.json`, `.codex/hooks.json`, and `scripts/agent-archive-cloud-setup.sh`, which installs the pinned Linux binary after checking its SHA-256. It uses the same owned-entry merge as `internal/hooks`, so unrelated hooks are preserved and `cloud remove` strips only its own entries. The user reviews and commits the files; nothing is committed automatically.
  3. **Environments.** With `--ci github`, it sets the repository secrets through `gh`. For each vendor without an API, it prints one block of `AGENT_ARCHIVE_*` variables in `.env` form and the settings location, and states who can read the values there (for example, anyone using a Claude Code environment, and the agent itself). The secret is printed once, to the terminal, never to a file.
- **`agent-archive cloud rotate`** mints a new scoped credential, updates every target it can update programmatically, prints the paste block for the rest, and deletes the old credential only after the user confirms that the paste is done. A `cloud verify` pass, run in a new cloud session after the paste, confirms that the environment has switched to the new credential.
- **`agent-archive cloud verify`**, run inside a cloud VM (or by the setup script), checks the gate variables, reachability, and a synthetic write, read-back and delete test under a unique key, mirroring setup's storage test. It reports which step failed without printing credentials.

The bootstrap token changes a rule in the [privacy document](../../agent-archive-privacy.md): today the tool "does not request another token" beyond object credentials. `provision` asks for one only when the user opts into automated provisioning, keeps it in Keychain, never sends it to a cloud environment, and `cloud provision --forget-bootstrap` deletes it.

Short-lived R2 credentials do not remove the paste. Credentials stored in a vendor's environment settings must outlive the 7-day maximum, so those environments get a long-lived, bucket-scoped key. Short-lived credentials fit only where each launch supplies its own: Cursor launches through the API or SDK, CI jobs, and Claude Code self-hosted runners, whose wrapper script can mint per-session credentials.

### 9. After-the-fact pull (Phase 3)

`agent-archive import cursor-cloud` fetches finished Cloud Agent conversations with a Cursor API key stored in Keychain. Its output is a new adapter (`cursor-cloud-api`) with `source_kind: vendor_api`, so readers never mistake it for a native transcript. It maps the Cursor agent ID to `native_session_id` and deduplicates against pushed sessions only once the mapping between agent ID and conversation ID is verified. The v0 endpoint has no tool calls, and the v1 stream expires, so the import must run periodically from the Mac's collector. `claude --teleport` could feed the same pattern for Claude Code if push capture proves insufficient.

## Phases

| Phase | Scope | Exit criteria |
| --- | --- | --- |
| 0. Probes | Rerun the verified Claude probe method on Cursor Cloud Agents and Codex cloud: which hooks fire, the payload keys, whether a transcript exists at stop, egress to S3 and R2, and whether secrets reach hooks | Each unknown in the support matrix answered and recorded in [capture capabilities](../../reference/capture-capabilities.md) |
| 1. Claude Code | Linux build, cloud-mode configuration, `CaptureSession`, the `--cloud` hook, schema v2 (`execution`, `repo_key`, cloud `machine_id`), Claude adapter review, credential redaction, `cloud provision` (R2 storage, repository files, Claude paste block, GitHub secrets), `cloud verify`, CI documentation for `claude-code-action` | A Claude Code web session and a `claude-code-action` run each publish and read back from the Mac with `show`, including one subagent |
| 2. Cursor, retention and rotation | Cursor push (if the probe passes), the Mac cloud sweep, `cloud rotate`, S3 provisioning | A Cursor Cloud Agent session reads back; superseded cloud sources are swept |
| 3. Codex and pull | Codex cloud push if the probe passes, `codex-action`, the Cursor pull importer | Codex CI reads back; Cursor import reads back with `source_kind: vendor_api` |

## Acceptance criteria

- After `cloud provision`, each vendor without an API needs exactly one paste per credential lifetime, and a new session in that environment passes `cloud verify` with no further steps.
- A committed hook in a repository without `AGENT_ARCHIVE_CLOUD` does nothing on any machine, and a Mac with the local install captures each local session exactly once.
- A cloud session's final turn is in the archive after the session ends, verified by `show` on a Mac.
- A resumed or teleported session is not captured, and the diagnostic says why.
- A failed upload never delays the agent beyond the hook timeout, never blocks a stop, and is retried by the next stop in the same VM.
- The committed files, hook output and uploaded objects never contain the credential values.
- With the cloud sweep enabled, no unreferenced cloud source remains more than 24 hours past its supersession, and no cloud session remains past `RetentionDays`.
- Readers handle mixed schema v1 and v2 metadata.

## Open questions

1. Does Claude Code run `SessionEnd` in the cloud before reclaiming the VM, and with what time budget? Stop-based capture does not depend on it, but it would allow a final bookkeeping publish.
2. Can Claude Code's proxy-injected API credentials (Pro and Max) sign S3 or R2 requests? Only header credentials are documented; SigV4 needs the secret itself. If a future version supports it, credentials could leave the VM entirely.
3. Should cloud sessions share the Mac archive's retention period, or have their own?
4. Should `cloud provision` set up one shared cloud credential for all vendors (one token to rotate, several places to paste) or one per vendor (a leak is contained to one vendor)?
5. Is a per-session cloud upload key (for example a presigned URL issued by a small user-owned service) worth the extra infrastructure? It would remove long-lived credentials from the VM but breaks the "no hosted service" principle.
