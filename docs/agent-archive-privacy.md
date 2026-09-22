# Privacy

## Source filter version 5

Filter 5 is filter 4 plus what a parser needs to recognize a Claude Code
compaction. After `/compact` or auto-compaction, Claude Code writes a system
record with `subtype: "compact_boundary"`, then a user record marked
`isCompactSummary: true` (usually also `isVisibleInTranscriptOnly: true`)
whose text is a model-written summary of the conversation so far.

- **Retained from the boundary:** `type`, `subtype`, `uuid`, `parentUuid`,
  `logicalParentUuid` (the last record before the compaction), `sessionId`,
  `timestamp`, and `isSidechain`, and nothing else. The record is rebuilt from
  those typed values rather than passed through the ordinary filter, since a
  system record is otherwise hidden whole. An id is kept only when it looks
  like one (a string of at most 256 bytes with no whitespace, or null), the
  timestamp only when it parses, and `isSidechain` only as a boolean.
  Everything else — the boundary's text, `compactMetadata` (trigger and token
  count), `level`, `cwd`, `userType`, `version` — is dropped, and its key
  names are listed in the `unknown_field_omitted` gap. Only the exact
  `system`/`compact_boundary` shape is admitted, and only from Claude Code;
  every other system record is still omitted whole.
- **Retained on the summary:** `isCompactSummary` and
  `isVisibleInTranscriptOnly`, as booleans only (the rule filter 4 applies to
  `isMeta`; any other value under those names is omitted).
- **Kept, unlike an `isMeta` record:** the summary's text. It is model output
  describing the session, which is what a handoff to another agent needs. It
  passes every value rule below (injected-instruction stripping, credential
  redaction, the 64 KB cap) like any other message.

The shape is taken from Claude Code's behavior; no transcript on the
development machine has contained a compaction yet.

## Source filter version 4

Filter 4 is filter 3 plus one rule for harness-written records. Claude Code
marks user records it writes itself — an expanded skill or custom slash
command, the caveat it adds before local-command output — with
`isMeta: true`. Their text is instruction text the harness injected, the same
kind as a `<system-reminder>` block, not something the person wrote.

- **Retained:** the `isMeta` flag, only as a boolean (a string or object
  under that key is prose the allowlist never kept and is still omitted), and
  the record itself with its ids (`uuid`, `parentUuid`, `sessionId`, and the
  rest of the ordinary allowlist), so parent chains through the record
  survive and a parser can tell the record is not a human prompt.
- **Stripped:** the record's string content and every text block in it, at
  every depth, before any other filtering, with a `hidden_instruction_omitted`
  gap whose detail is `meta record text omitted`. A non-text block (for
  example a tool result) keeps its shape and identifiers (`tool_use_id`,
  `is_error`) but loses its own `text` and its nested content text, and is
  then left to the ordinary rules. A record whose `isMeta` is absent or false
  is untouched.

The typed slash command itself (`<command-name>/review-pr</command-name>`) and
local-command output (`<local-command-stdout>`) are not `isMeta` and are kept
as before; they are what the person did and what the command printed. Every
filter-3 rule below still applies.

## Source filter version 3

The source filter decides what leaves this machine. Filter 3 keeps the tool
evidence a reader needs and removes instruction text the harness injected into
a message. It does not relax any value rule: `blockedKeys`, `sensitiveValue`
redaction, the 64 KB string cap, the hidden role/channel rules, and the
exclusion of `encrypted_content`, `base_instructions`, `state`,
`thread_settings`, `rate_limits`, and `attachment` all still apply inside every
newly retained subtree.

Retained, in addition to filter 2:

- **Tool arguments.** Inside a tool-argument subtree — `input`, `arguments`,
  `tool_input`, and Codex's `payload.input` — every argument name is retained,
  because the names belong to the tool and no allowlist can anticipate them.
  This is what restores Edit `old_string`/`new_string`, Agent `prompt`, Skill
  `args`, Grep `pattern`, Bash `timeout`, and MCP tool arguments. Values are
  sanitized exactly as before, so a credential inside an Edit body is still
  redacted and an oversized argument is still capped.
- **Tool-result linkage and turn identity.** `tool_use_id`, `is_error`,
  `stop_reason`, `sessionId`, `requestId`, and `gitBranch`.
- **Token accounting, numbers only.** `usage` (Claude) and Codex's `info`,
  `total_token_usage`, `last_token_usage`, `turn_token_usage`,
  `thread_token_usage`, `last_agent_message`, `thread_id`, `root_turn_id`,
  `started_at_ms`, and `completed_at_ms`. Inside the four `*_token_usage`
  subtrees and `usage`, anything that is not a number is omitted.
- **Record types.** Codex `token_usage_record` and Cursor `turn_ended`.

`toolUseResult` is deliberately not retained: it duplicates the tool result
already kept in the message content.

Stripped:

- **Injected instruction blocks.** Claude Code wraps CLAUDE.md, hook output,
  and memory in `<system-reminder>…</system-reminder>` inside user content, and
  Codex writes AGENTS.md inside `<user_instructions>…</user_instructions>` and
  machine details inside `<environment_context>…</environment_context>`. Those
  blocks are removed from string content wherever they appear and a
  `hidden_instruction_omitted` gap is recorded; the rest of the message is
  kept. An opening tag whose block never closed drops everything after it.
  Untagged instruction text is not guessed at.

Omissions are now visible. A filtered transcript records one
`unknown_field_omitted` gap whose detail lists the distinct key names the
filter could not keep — names only, never values — sorted and capped at 64,
with a note when more were seen.

Skill snapshots embedded in supplemental evidence keep their inventory entry
(name, sha256 of the whole original file, scope) but the archived body is
capped at 16 KB, with `original_bytes` recording the real size and `truncated`
marking the cut. Moving bodies to content-addressed objects is deferred.

### Tool-argument deny list

Retaining tool arguments wholesale has two exceptions, applied at every depth
of a tool-argument subtree. In both cases the argument's key name is recorded
in a `sensitive_or_hidden_field_omitted` gap (`omitted tool argument keys: …`,
sorted, capped at 64, names only) and the value is never retained.

- **Typed or submitted text.** An argument named `text`, `value`, or `values`
  is dropped when the tool's name (`name` or `tool_name` beside the argument
  subtree, compared case-insensitively) is `type`, `form_input`, `computer`,
  `key`, `enter_verification_code`, or `autofill_credential`; ends with `_`
  followed by one of those (an MCP tool such as `mcp__browser__computer`); or
  ends with `_type`, `_input`, or `_fill`. These tools send their text outward
  into a browser field, a terminal, or a device, and a login form's contents
  are exactly what a transcript must not keep. `Edit`, `Write`, and other tools
  keep their `text`/`value` arguments.
- **Credential-named arguments.** For every tool, an argument whose lowercase
  key contains `password`, `secret`, `token`, `credential`, `api_key`,
  `apikey`, `cookie`, or `authorization` is dropped. This is a substring rule,
  broader than `blockedKeys`, and it knowingly catches budgets such as
  `max_tokens`. An argument object whose members were all dropped is pruned
  with them.

Codex `custom_tool_call.input` and `function_call.arguments` are JSON encoded
as one string, so the deny list cannot see their keys; the string still passes
every value-level redaction below.

### Value-level redaction

Every retained string, at every depth, passes these patterns. A match is
replaced with `[REDACTED]` and a `sensitive_content_redacted` gap is recorded.

- Assignments of `api_key`, `access_key`, `secret`, `password`,
  `authorization`, `bearer`, or `token` to a value (`name=value`, `name: value`,
  `Authorization: Bearer …`), AWS access key IDs (`AKIA…`), and Anthropic/OpenAI
  style `sk-` keys.
- PEM private key blocks: `-----BEGIN … PRIVATE KEY-----` through the next
  `-----END … -----`, or to the end of the string when the END line is missing.
  Certificates and public keys are not redacted.
- JWTs: three base64url segments, the first beginning with `eyJ`.
- URL userinfo: in `scheme://user:pass@host` (or `scheme://user@host`) the
  userinfo is replaced and the scheme and host are kept.
- GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`) and
  Slack tokens (`xox[baprs]-`).

Known false positives. The assignment pattern cannot tell a credential from
code: `token = parse(x)` and `password: required` are redacted, and
`token := parse(x)` loses its left-hand side. This is accepted rather than
narrowed, because the cost of a missed credential is higher than the cost of a
redacted identifier in an archived transcript; a reader sees the
`sensitive_content_redacted` gap and can consult the original source if it
still exists. Words that merely contain a trigger (`tokens`, `secretary`,
`password_policy`) do not match, because the pattern requires a whole word
followed by `=` or `:`.

Redaction is best effort in both directions: a legitimate value that looks like
a credential is redacted, and a tool argument that happens to contain one of
the instruction tags above loses that span. Both are recorded as gaps.

# Bucket privacy evidence

Setup tests object access and inspects native bucket public-access controls separately. Successful uploads do not prove a bucket is private. Inspection uses existing credentials, is read-only, and has a five-second total deadline. Failure or missing inspection permission does not require administrator credentials.

For AWS S3, all four bucket-level Block Public Access flags must be observed enabled before the tool reports `verified_private`. Otherwise it checks policy status and the bucket ACL for public configuration. A public policy or ACL yields `public_or_risky`; incomplete or denied checks yield `not_verified`. Account-level controls might further restrict access, so `public_or_risky` identifies configuration that needs review, not proof of anonymous object access. A private bucket policy or ACL alone is insufficient because object ACLs and access points can expose data.

For R2, setup stores S3-compatible object credentials, not a Cloudflare management API token. These cannot inspect managed/custom public domains. R2 therefore remains `not_verified` and links the public-bucket settings instructions. The tool does not request another token or send object credentials to the management API.

Inspection covers native bucket public access. It does not assess applications that proxy authorized reads, shared signed URLs, or copies of archived data.

The result includes fixed diagnostic codes, scope, check time, and a storage-configuration fingerprint. Setup records it in the resumable setup draft as soon as the storage connection succeeds and commits it to the active configuration only when setup is confirmed; resuming a draft inspects again. Status reads the local result, never a remote API. After 24 hours, a clock rollback, or a storage configuration change, status reports privacy as unverified. The scheduled background collector refreshes the evidence with the same read-only inspection whenever the saved result is missing, is for another storage configuration, or is more than 12 hours old, so an active install stays verified without rerunning setup; a paused install is never inspected, and running setup also refreshes it. Provider errors and credentials are not included in the report.

References used for the implementation:

- [AWS Block Public Access semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html)
- [R2 S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)
- [R2 public bucket settings](https://developers.cloudflare.com/r2/buckets/public-buckets/)

Synthetic tests cover allowed, denied, incomplete, public-policy, public-ACL, R2-unavailable, expired, and changed-configuration results. Actual AWS inspection and R2 dashboard verification remain live acceptance checks.
