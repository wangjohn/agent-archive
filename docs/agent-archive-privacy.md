# Privacy

What leaves this machine for a session is its metadata sidecar and its
filtered source bundle, `sessions/<harness>/<id>/source.<sha256>.jsonl.gz`:
gzip of newline-delimited JSON with a header line, one line per retained
native record, then text transcripts and supplemental evidence (source schema
2). The line format changes how retained evidence is packaged, not what is
retained: every line holds only what the source filter below kept.

## Source filter version 10

Filter 10 changes how a Cursor plain-text transcript is split into role
sections. Adapter version 0.10.0 goes with it; JSONL and composer output is
unchanged.

- **Role headers only at column 0.** Filter 9 treated any line whose trimmed
  text began with `user:`, `system:`, or another role name as a role header.
  An indented YAML key in tool output (`    user: …` in a docker-compose
  file) therefore started a new section, and `  system: linux` hid
  everything after it as if it were a system prompt. A header is now only a
  role name and a colon at the start of the line (in any case), followed by
  a space or the end of the line. An indented role word is content: it is
  retained, sanitized like the rest of its section, and never hides or
  reveals anything.

## Source filter version 9

Filter 9 closes four ways content left the machine that the filter was meant
to stop. Adapter version 0.9.0 goes with it.

- **Credential assignments by any common name.** Filter 8 required the
  trigger word to stand alone, and since `_` is part of a word that missed
  every snake_case or SCREAMING_CASE name (`DB_PASSWORD=`,
  `AWS_SECRET_ACCESS_KEY=`, `OPENAI_API_KEY=`, `GITHUB_TOKEN=`); a quote
  between the name and the separator missed every JSON key
  (`"password": "…"`). A credential name is now any name ending in a trigger
  word, optionally followed by `key` or `access_key`, `base`, and a number,
  in any case and with anything glued on before it (`db_password`,
  `accessToken`, `PGPASSWORD`, `SECRET_KEY_BASE`, `DB_PASSWORD_1`,
  `spring.datasource.password`), quoted or not; the separator is `=`, `:`,
  `:=`, or `=>`, and `--name value` command-line flags are covered too. Only
  the value is replaced now, and a quoted value keeps its quotes:
  `DB_PASSWORD=[REDACTED]`, `"password": "[REDACTED]"`,
  `Authorization: Bearer [REDACTED]`. Filter 8 replaced the name as well. See
  [Value-level redaction](#value-level-redaction) for the full rule and its
  false positives.
- **Images, documents, and other binary blocks are dropped.** A pasted
  screenshot or PDF, or an image a tool read, arrives as a content block
  whose data is base64. Its key names (`type`, `source`, `data`) were all on
  the allowlist, so filter 8 kept it. Now a block of type `image`,
  `document`, `input_image`, `input_file`, `input_audio`, or `image_url`, a
  `file` block with inline data, and any block whose `source` has type
  `base64` are dropped whole, at any depth (a tool result keeps its
  `tool_use_id`), with a `binary_content_omitted` gap naming the kind. A
  base64 `data:` URL inside a string keeps its media type and loses its
  payload (`data:image/png;base64,[OMITTED]`), with the same gap. `data` is
  no longer an allowed key anywhere: nothing the filter keeps needs it.
- **Structured Cursor tool results are sanitized.** A Cursor tool result or
  error that is an object, not a string, was encoded to one string before
  the sanitizer saw it, so the key rules never ran on it. It is now sanitized
  first, the way a tool call's arguments are (every key name kept,
  `blockedKeys` and the credential-named deny list applied, every string
  redacted, binary blocks dropped), then encoded. Keys dropped by the deny
  list are named in `sensitive_or_hidden_field_omitted` as
  `omitted tool result keys: …`. An empty result stays `{}`; one the
  sanitizer leaves nothing of is dropped and named as `tool.result` (or
  `tool.error`, `toolResult.result`) in `unknown_field_omitted`.
- **Skill snapshots stay inside their skill root.** A skill's `SKILL.md` is
  now resolved through symlinks before it is read, and the resolved file is
  what is read; it must be a regular file. A user-level skill's file must lie
  inside the skill root or be itself named `SKILL.md` (a linked skill, not an
  arbitrary file under a skill's name), so a skill directory linked into a
  skills checkout elsewhere (`~/.claude/skills/x -> ~/src/skills/x`) keeps
  working. A project-level skill's file must be named `SKILL.md` and lie
  inside the project: a repository controls its skill root as much as its
  links (`.claude/skills -> ..`), so being inside that root proves nothing.
  A cloned repository can therefore ship neither
  `.claude/skills/x/SKILL.md -> ~/.aws/credentials`, nor `-> ../../../.env`,
  nor a linked skill root with `x/SKILL.md -> ../master.key`, to have that
  file archived; a skills directory the repository shares between harnesses
  keeps working. When a session runs from
  the home directory, its project skill directory is the user's own; it is
  observed once, under the user scope. Any other entry counts as
  uninspected in the root's inventory.
- **Truncation keeps characters whole.** The 64 KB string cap and the 16 KB
  skill-snapshot cap cut on a UTF-8 character boundary; filter 8 could split
  a multi-byte character and leave invalid UTF-8.

Every filter-8 rule below still applies.

## Source filter version 8

Filter 8 adds one source format, `cursor-composer`: a Cursor chat read from
Cursor's own database (`state.vscdb`) rather than from a hook's transcript.
JSONL and text output are byte-identical to filter 7. A chat is a
`composerData` value and one row per message; the filter builds records from
them through an allowlist and then passes every record through the same
sanitizer as a JSONL record.

- **Kept:** one session record (the chat's ID and creation time) and, per
  message, its role (user or assistant), ID, creation time, request ID, start
  and completion times, model name, input and output token counts (an
  all-zero count is left out), text, and its tool call: the tool's name and
  call ID, its arguments, and its result or, for a failed call, its error
  (as text, whatever its shape) with `is_error`. Tool arguments pass the same
  deny list and redaction as in a JSONL transcript.
- **Dropped by design:** every context payload Cursor attaches to a message
  (`codebaseContextChunks`, `attachedCodeChunks`, `originalFileStates`,
  `diffHistories`, `images`, `consoleLogs`, `recentlyViewedFiles`, and the
  like), named in a `cursor_context_omitted` gap; the model's reasoning
  (`thinking`), as `hidden_instruction_omitted`; and the chat's settings and
  bookkeeping (`lastUpdatedAt`, `modelConfig`, `usageData`,
  `workspaceIdentifier`, `blobEncryptionKey`, UI state). Every other key not
  kept, including a tool call's `toolCallBinary`, `userDecision`, and
  `additionalData`, and a result recorded beside an error, is named in
  `unknown_field_omitted` with its level: `chat.`, `message.`, `tool.`
  (`toolFormerData`), `toolResult.` (`toolResults` entries), `model.`,
  `tokens.`, or `record.` (a key the shared sanitizer omitted from a record
  this filter built). Names only, never values.
- **Tool arguments.** Arguments come from `rawArgs` when it decodes to a
  non-empty object, else from `params`. A string argument that is itself
  JSON (an object or array), at any depth, is dropped rather than kept as
  text the argument rules never saw, and the nearest argument name is given
  in `cursor_tool_argument_omitted`; objects and arrays it leaves empty are
  removed. When neither source yields arguments, a source that did not
  decode is named in the same gap under a separate detail, so Cursor's own
  `rawArgs` and `params` are never confused with a tool's argument names. An
  empty argument object is dropped silently.
- **Fail closed.** Only the format versions seen in a real database are read:
  chat `_v` 18 and message `_v` 3. Any other version, older or newer, refuses
  the whole chat as an unsafe format. So do malformed JSON and a message list
  that does not match the chat's headers. Older chats kept their messages
  inline in `conversation` and are refused by version; inline messages in a
  chat that is read are not read either, and add a
  `cursor_inline_conversation_omitted` gap.
- **Counted, not guessed.** A message with no row, or a row belonging to
  another message, adds `cursor_bubble_missing` (and, for the second,
  `cursor_bubble_id_mismatch`); content kept in blobs, which are never read,
  adds `cursor_blob_content_unavailable`; an unknown or inconsistent message
  type adds `cursor_message_type_unknown`. Their details give counts out of
  all the chat's messages, never an ID.
- **Output stops early rather than skipping.** Output stops at the first
  message whose row is missing, belongs to another message, or disagrees with
  its header about its type, and at the first message still in flight: one
  the chat lists in `generatingBubbleIds`; one whose tool call's status is
  anything but `completed`, `error`, or `cancelled` (or `canceled`); or, as a
  backstop, the chat's last message when the chat's `status` is anything but
  `completed`, `none`, `aborted`, `cancelled` (or `canceled`), `error`, or
  empty. That message and everything after it wait for a later pass, counted
  in `cursor_incomplete_tail_omitted`. A completion time is not required:
  most finished replies have none. A chat with no kept message has no
  records at all.
- **Rewrites.** Stopping early keeps a chat's records append-only while it
  grows, but Cursor can still rewrite a message it has finished (late token
  counts, an edited prompt, a checkpoint restore). The filter then produces
  different records; the collector, not the filter, handles a rewritten
  Cursor database chat: it publishes the chat as it now is in place of the
  earlier snapshot (which retention then removes as superseded) and keeps
  one `cursor_chat_rewritten` gap, which names no content, counting the
  rewrites.

Every filter-7 rule below still applies.

## Source filter version 7

Filter 7 changes only how a Cursor plain-text transcript is filtered; JSONL
output is byte-identical to filter 6. Older Cursor versions give the hook a
plain-text transcript made of role sections: a `user:`, `assistant:`, or
`tool:` line and the continuation lines under it.

- **Sanitized per section.** Each visible section is sanitized on its own, so
  injected-instruction stripping, credential redaction, and the 64 KB string
  cap apply per message, as they do to a JSONL record. Filter 6 sanitized the
  whole transcript as one string, so any transcript over 64 KB was cut to its
  first 64 KB. A section over 64 KB is now truncated alone, with a
  `content_truncated` gap, and every other section is kept.
- **Structure kept.** Sections stay in their original order, each line as it
  was, so the retained text is read back by the same role prefixes. Hidden
  sections (`system:`, `developer:`, `thinking:`, `analysis:`) and their
  continuation lines are still omitted.
- **Gaps recorded once.** Each gap is recorded once per transcript. Filter 6
  added a `hidden_instruction_omitted` gap for every hidden section.
- **Size.** The transcript is bounded by the record size limit (64 MiB, the
  collector's transcript cap) instead of 2 MB. Over the limit it is a
  `record_size_limit` capture gap rather than a refusal on every pass.

Every filter-6 rule below still applies.

## Source filter version 6

Filter 6 is filter 5 plus two narrow retentions and one more injected block.

- **Retained:** Claude Code's `origin`, reduced to `{kind: <string>}`, and
  `promptSource`, only as a string. A person's prompt carries
  `origin.kind: "human"`; a background-task completion carries
  `"task-notification"` and `promptSource: "system"`; only that known kind is
  treated as harness-written, and any other kind stays a prompt. Every other member of
  `origin` is omitted and its name reported in the `unknown_field_omitted`
  gap; an `origin` that is not an object, or has no string `kind`, and a
  `promptSource` that is not a string, are omitted whole. The kept strings
  still pass value redaction.
- **Stripped:** Codex's `<recommended_plugins>…</recommended_plugins>` block,
  a catalog of uninstalled plugins the harness prepends to the first user
  message, is removed like `<environment_context>`, with the same
  `hidden_instruction_omitted` gap.

Every filter-5 rule below still applies.

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
  like one (a string of at most 256 bytes with no whitespace and no tag
  brackets, which the credential redaction below would leave unchanged, or
  null), the timestamp only when it parses, and `isSidechain` only as a
  boolean.
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

- Credential assignments (filter 9). The name ends in `api_key`,
  `access_key`, `private_key`, `encryption_key`, `signing_key`,
  `master_key`, `secret`, `password`, `passwd`, `passphrase`, `token`,
  `authorization`, `bearer`, or `credential(s)` (`_`, `-`, `.`, or nothing
  between the parts of a two-word trigger), in any case, with anything glued
  on before it; or in `pwd` or npm's `_auth` after a separator (`MYSQL_PWD`,
  `DB_PWD`, `:_auth`), since a bare `PWD` or `OLDPWD` is the shell's working
  directory. The trigger may be followed by `key` or `access_key`, then
  `base`, then a number. So `DB_PASSWORD`, `AWS_SECRET_ACCESS_KEY`,
  `OPENAI_API_KEY`, `SECRET_KEY_BASE`, `DB_PASSWORD_1`, `accessToken`,
  `PGPASSWORD`, `spring.datasource.password`, and `x-api-key` all match. The
  name may be quoted (`"…"`, `'…'`, or escaped inside a string, `\"…\"`);
  the separator is `=`, `:`, `:=`, or `=>`; a `--name value` command-line
  flag counts too. The value, quoted up to its closing quote or unquoted up
  to whitespace, `,`, `;`, or a quote, is replaced and the rest is kept:
  `DB_PASSWORD=[REDACTED]`, `"password": "[REDACTED]"`. An HTTP scheme
  before the value stays: `Authorization: Bearer [REDACTED]`. A single token
  in brackets or braces is a value too (`password=[hunter2]`,
  `token={abc123}`), with anything glued on after it
  (`password=[REDACTED]realsecret` loses `realsecret`). A value may begin
  with `=` (`PASSWORD==abc`) unless whitespace follows it.
- AWS access key IDs (`AKIA…`, and `ASIA…` for temporary STS credentials)
  and Anthropic/OpenAI style `sk-` keys.
- PEM private key blocks: `-----BEGIN … PRIVATE KEY-----` through the next
  `-----END … -----`, or to the end of the string when the END line is missing.
  Certificates and public keys are not redacted.
- JWTs: three base64url segments, the first beginning with `eyJ`.
- URL userinfo: in `scheme://user:pass@host` (or `scheme://user@host`) the
  userinfo is replaced and the scheme and host are kept.
- GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`) and
  Slack tokens (`xox[baprs]-`).

Known false positives. The assignment pattern cannot tell a credential from
code or a path: `token = parse(x)`, `nextToken := lexer.Next()`,
`credentials := loadCreds()`, and `password: required` have their
right-hand side redacted; so do a saved directory in a `*_PWD` variable
(`ORIG_PWD=$(pwd)`), a path to a credentials file
(`GOOGLE_APPLICATION_CREDENTIALS=/path/key.json`), and the word after a flag
in prose (`pass --token flag`). This is accepted rather
than narrowed, because the cost of a missed credential is higher than the
cost of a redacted identifier in an archived transcript; a reader sees the
`sensitive_content_redacted` gap and can consult the original source if it
still exists. Names with anything after the trigger word other than the
suffix above (`tokens`, `max_tokens`, `token_count`, `secretary`,
`password_policy`, `TOKEN_URL`, `SECRET_NAME`, `--password-stdin`) do not
match, nor do `PWD`, `OLDPWD`, `auth`, comparisons (`token == nil`), or a
name with no value.

Known misses.

- An unquoted value stops at a quote, so a quote inside an unquoted
  password leaves the rest of the password.
- In JSON escaped more than once inside a string (`\\\"password\\\":…`),
  the value ends at the first escaped quote of any depth, so the tail of a
  value after an escaped quote inside it (`\\\"ab\\\\\\\"cd\\\"`: `cd`) is
  kept. JSON escaped once (`\"password\":\"ab\\\"cd\"`) is handled.
- A value that is an object or array holding whitespace, a comma, a colon,
  or a quote (`"credentials": {"type": …}`, `password: [required, min 8]`)
  is a structure and is not replaced as text: in parsed records its members
  are checked by name, but in free text a secret inside it is caught only by
  its own name or shape.
- A project skill's `SKILL.md` that is a hard link to another file cannot be
  told apart from a real file. A cloned repository cannot create one (git
  does not store hard links); it needs local write access to the project.
- A name that does not end in a trigger word (`AWS_ACCESS_KEY_ID`,
  `DATABASE_URL`, `DSN`, `CONNECTION_STRING`) is not redacted by this
  pattern; its value is redacted only if it has a recognizable shape
  (`AKIA…`/`ASIA…`, URL userinfo).
- A credential in prose (`the password is hunter2`) or on the line after
  its YAML key is not recognized.

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
