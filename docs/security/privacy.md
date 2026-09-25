# Privacy

agent-archive copies your coding-agent sessions to a bucket you own. This
page says what that copy contains, what is kept out of it, what the tool
changes on your Mac, and where its protections stop. The per-version history
of the filter is in the [filter changelog](filter-changelog.md).

## Threat model

- **Who can read the archive.** Anyone who can read your bucket: you, anyone
  you share credentials or the bucket with, and your storage provider.
  There is no client-side encryption; objects are protected by your
  provider's access control and its encryption at rest. Keep the bucket
  private and the credentials narrow ([bucket permissions](bucket-permissions.md)).
- **What the filter is for.** Keeping out of the archive what the archive
  doesn't need (hidden reasoning, injected instructions, images, unknown
  fields) and what is most dangerous to keep (recognizable credentials).
  It is an allowlist: a field the filter doesn't know is dropped and named,
  not passed through.
- **What it is not.** It is not data-loss prevention. Redaction is best
  effort and pattern-based: a secret with no recognizable name or shape, a
  customer's personal data, or proprietary source code in a tool result is
  archived as it appears. Treat the archive as being as sensitive as your
  terminal history and your repositories.
- **Where it runs.** Only on a Mac you set it up on, only for projects you
  include, and only for sessions that start after a project is included
  (or that you import with `backfill`, after reviewing the plan). Nothing is
  sent anywhere but your bucket: there is no hosted service and no
  telemetry. The one thing uploaded from outside your projects is the
  [skill evidence](#what-is-uploaded) from your user-level skill folders.
- **Who can change the archive.** Anyone who can write to your prefix
  controls what `list`, `show`, and `handoff` return. A handoff is a prompt
  for a coding agent, so a planted or altered session is text another agent
  will read: readers check each source's SHA-256 against its metadata,
  which catches corruption but not someone who can write both. Give write
  access only to Macs you trust, and treat a handoff from a shared bucket
  like any other text you paste into an agent (see
  [handoff](../guides/handoff.md#what-the-receiving-agent-is-told)).
- **The recorded agent runs as you.** The coding agent whose session is
  being archived runs with your account's permissions. It can read and
  change agent-archive's local state and configuration, the apps' hook
  files, and your AWS profiles, and it can run `agent-archive` itself
  (`pause`, `uninstall`). So the archive is a convenience record of what
  the agent reported, not a tamper-proof audit log of what it did; and
  anything the agent read (a file in a cloned repository, a web page) is in
  the transcript as it was, including text written to mislead the next
  agent that reads it.

Security problems, including a way around the filter, are reported as
[SECURITY.md](../../SECURITY.md) describes.

## What is uploaded

For each captured session, two objects (see
[bucket layout](../reference/bucket-layout.md)): the source bundle, which
holds the filtered transcript, skill evidence, and hook observations, and
the metadata sidecar.

- **The filtered transcript**: your prompts; the agent's
  replies; tool calls with their arguments (Edit bodies, shell commands,
  search patterns, file paths) and tool results, each string capped at
  64 KB; working directories (which usually contain your username); Git
  branch names; model names; token counts; timestamps; the app's own session
  and message IDs; summaries the app wrote when compacting a conversation;
  and final messages hooks reported.
- **Skill evidence**: for every hook-captured session, the name, SHA-256,
  and filtered body (the first 16 KB) of each `SKILL.md` installed in that
  app's skill folders, and which folder it came from. These are your
  **user-level** folders, whatever the project, plus the project's own:

  | App | User-level folders | Project folder |
  | --- | --- | --- |
  | Claude Code | `~/.claude/skills` | `<project>/.claude/skills` |
  | Codex | `~/.agents/skills`, `~/.codex/skills` | `<project>/.agents/skills` |
  | Cursor | `~/.cursor/skills` | `<project>/.cursor/skills` |

  Only each folder's immediate `<skill>/SKILL.md` files are read (up to 256
  per folder), never other files in them. In a user-level folder a
  symlinked skill is followed wherever it points, as long as the file it
  reaches is named `SKILL.md`; a project's `SKILL.md` that resolves outside
  the project is skipped. There is no setting to turn this off: keep
  a skill you don't want uploaded out of these folders. Sessions imported
  with `backfill` carry no skill evidence.
- **Metadata**: the machine ID of the Mac that captured it (random, made at
  setup), a project ID (a hash of the project's path, not the path itself),
  the app and its version, the app's own session ID, capture times, counts,
  models, skills used, and the capture gaps the filter recorded (the names
  of omitted fields, never their values).
- **Hook observations**: for each hook event, its name, the app's turn and
  message IDs, the model and model settings the hook reported, and, for a
  stop hook, the agent's final message (filtered like the transcript).

agent-archive adds nothing else about your Mac: no hostname, username, or
IP address, beyond what already appears in the transcript (a working
directory usually contains your username). Your storage provider sees each
connection, as it would for any upload.

Feedback you attach with `agent-archive feedback` is filtered the same way
and uploaded with the session.

## What is never uploaded

- **Hidden reasoning** (thinking blocks, reasoning items) and
  **system/developer instructions**.
- **Injected instructions**: `<system-reminder>`, `<user_instructions>`,
  `<environment_context>`, and `<recommended_plugins>` blocks, and the text of
  Claude Code's `isMeta` records (expanded skills and slash commands).
- **Images, documents, audio, and other binary content**, including base64
  `data:` URLs inside text.
- **Fields the filter doesn't know**, at any depth, and whole record types it
  doesn't know. Their names are listed in a gap.
- **Credential-named fields** (`password`, `token`, `api_key`, `cookie`,
  `authorization`, and similar keys), dropped whole; and tool arguments that
  carry typed input into a browser or terminal (see the deny list below).
- **Cursor context payloads**: code chunks, file states, diffs, console logs,
  and other context Cursor attaches to a message.
- **Claude Code's `toolUseResult`**, which duplicates the tool result already
  kept.
- **Your credentials.** R2 secrets are in the macOS Keychain; S3 credentials
  stay in your AWS profile. Neither appears in files, arguments, logs, or
  the bucket.

Recognizable secrets inside kept text are replaced with `[REDACTED]` (see
[value-level redaction](#value-level-redaction)). Each omission and
redaction is recorded as a capture gap on the session, so a reader knows
something was removed.

## After a filter upgrade

A new filter version (see the [filter changelog](filter-changelog.md))
applies to what is uploaded from then on. It does not clean what is already
in the bucket:

- **Sessions whose transcript is still on the capturing Mac** are refiltered
  and republished automatically on that Mac's next passes. The copy made
  with the old filter becomes the session's previous source, and retention
  always keeps a session's immediate predecessor, so for a session that
  doesn't change again the old copy stays until the whole session expires
  (90 days after its last capture by default).
- **Sessions whose transcript is gone** (deleted, or on a Mac that no longer
  runs agent-archive) are never refiltered: their current copy stays as the
  old filter made it until the session expires.
- **Sessions in an earlier destination** (after you changed storage) are
  not touched at all.

`show` prints a session's `filter_version`. To remove the older copies now,
delete from the bucket the source objects no session's metadata points at.
Pause every Mac that uploads to the bucket first, so no publication is in
flight: a new source is uploaded before the metadata that points at it.
This needs the [AWS CLI](https://aws.amazon.com/cli/) and `jq`, and
credentials that can list, read, and delete under the prefix:

First, list what would be deleted:

<!-- purge-recipe:list (scripts/test_purge_recipe.py runs the three blocks below) -->
```sh
agent-archive pause            # on every Mac that uploads to this bucket

bucket=my-archive-bucket       # your bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty
# export AWS_PROFILE=...       # a profile that can list, read, and delete
# For R2: export AWS_ENDPOINT_URL=https://<ACCOUNT_ID>.r2.cloudflarestorage.com

# Every key under sessions/, then the source each metadata.json points at.
# Inside a loop, each aws reads from </dev/null so it can't swallow the list.
aws s3api list-objects-v2 --bucket "$bucket" --prefix "${prefix}sessions/" \
  --query 'Contents[].Key' --output text | tr '\t' '\n' | grep -v '^None$' | sort > keys.txt
: > failed.txt
grep '/metadata\.json$' keys.txt | while read -r meta; do
  aws s3 cp "s3://$bucket/$meta" - </dev/null | jq -er --arg p "$prefix" '.source_bundle.key | strings | $p + .' ||
    echo "$meta" >> failed.txt
done | sort -u > current.txt

# Sources no metadata points at: superseded copies, and leftovers of
# interrupted deletions.
grep '/source\.[0-9a-f]*\.jsonl\.gz$' keys.txt | comm -23 - current.txt > unreferenced.txt
wc -l unreferenced.txt failed.txt   # sources to delete; metadata that could not be read
```

Review `unreferenced.txt`. `failed.txt` must be empty: a metadata object
that could not be read would make every source beside it look unreferenced,
so the next block refuses to delete anything until the listing is run again
without failures. Then delete:

<!-- purge-recipe:delete -->
```sh
if [ -s failed.txt ]; then
  echo "Some metadata could not be read (failed.txt); nothing deleted." >&2
else
  while read -r key; do aws s3 rm "s3://$bucket/$key" </dev/null; done < unreferenced.txt
fi
```

To remove sessions whose current copy predates a filter version (here 10)
as well, delete each one whole, metadata first:

<!-- purge-recipe:old-sessions -->
```sh
grep '/metadata\.json$' keys.txt | while read -r meta; do
  version=$(aws s3 cp "s3://$bucket/$meta" - </dev/null | jq -r '.filter_version | strings')
  case "$version" in
    '' | *[!0-9]*) echo "could not read $meta; skipped" >&2 ;;
    *) if [ "$version" -lt 10 ]; then
         aws s3 rm "s3://$bucket/$meta" </dev/null &&
           aws s3 rm "s3://$bucket/${meta%metadata.json}" --recursive </dev/null
       fi ;;
  esac
done
```

When you are done, run `agent-archive resume` on every Mac you paused.

A session deleted this way is gone from `list`; if its transcript is still
on the capturing Mac and changes again, that Mac publishes it anew. On an S3
bucket with versioning turned on, a delete only hides the object: remove
the noncurrent versions too, or add a lifecycle rule that expires them.

## What changes on your Mac

- The hook files of the apps you include: `~/.claude/settings.json` (or
  `$CLAUDE_CONFIG_DIR/settings.json`), `~/.codex/hooks.json` (or
  `$CODEX_HOME/hooks.json`), and `~/.cursor/hooks.json`. Only the `hooks`
  entry is changed; uninstall restores the rest byte for byte.
- `~/Library/LaunchAgents/com.agent-archive.collector.plist`, which runs the
  collector every 60 seconds.
- Local state in `~/.local/share/agent-archive` (or `AGENT_ARCHIVE_HOME`),
  private to your account: see [local state](../reference/local-state.md).
  It holds registrations, frozen uploads, and caches; transcripts are read in
  place, not copied, except a Cursor database copy that exists only while a
  read of it is in progress.
- A Keychain item (service `agent-archive`) for R2 credentials.

`agent-archive uninstall` removes the hooks and the LaunchAgent;
`--delete-local-data` also removes the local state and Keychain item. Neither
touches the bucket.

## Filter rules

Every retained string, at every depth, passes injected-instruction
stripping, value-level redaction, and the 64 KB cap. Tool-argument subtrees
(`input`, `arguments`, `tool_input`) keep every argument name, because the
names belong to the tool, subject to the deny list below.

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
  flag counts too. The value, quoted up to its closing quote (plus anything
  glued on after it, as a shell reads it; filter 10) or unquoted up to
  whitespace, `,`, `;`, or a quote, is replaced and the rest is kept:
  `DB_PASSWORD=[REDACTED]`, `"password": "[REDACTED]"`. An HTTP scheme
  before the value stays: `Authorization: Bearer [REDACTED]`. A single token
  in brackets or braces is a value too (`password=[hunter2]`,
  `token={abc123}`), with anything glued on after it
  (`password=[REDACTED]realsecret` loses `realsecret`, and a glued bracket
  group goes whole). A value may begin with `=` unless whitespace follows
  it; the extra `=` signs stay (`PASSWORD==[REDACTED]`).
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
(`GOOGLE_APPLICATION_CREDENTIALS=/path/key.json`), a one-item list
(`password: [required]`, taken for a bracketed value), and the word after a
flag in prose (`pass --token flag`). This is accepted rather
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
- For the same reason, an unquoted value that begins with `[` or `{` but is
  not a single closed token is kept whole: `password=[Kx9!q2Lm`,
  `password={secret`, `password=[admin:hunter2]`, `password=[a b]realsecret`.
  About 2% of random passwords drawn from the full symbol set begin with a
  bracket or brace.
- A project skill's `SKILL.md` that is a hard link to another file cannot be
  told apart from a real file. A cloned repository cannot create one (git
  does not store hard links); it needs local write access to the project.
- A name that does not end in a trigger word (`AWS_ACCESS_KEY_ID`,
  `DATABASE_URL`, `DSN`, `CONNECTION_STRING`) is not redacted by this
  pattern; its value is redacted only if it has a recognizable shape
  (`AKIA…`/`ASIA…`, URL userinfo).
- A credential in prose (`the password is hunter2`) or on the line after
  its YAML key is not recognized.
- Text glued after a closing quote is taken with the value (filter 10) only
  up to a closing `]`, `}`, or `)`, which usually closes the structure
  around the value (`{"password":"abc"}`, `f(PASSWORD="abc")`) and must stay.
  So in the rare `PASSWORD="abc")realsecret`, `realsecret` is kept.
- A Cursor plain-text transcript has no structure beyond its role headers,
  so a line in tool output that itself starts at column 0 with `user:`,
  `assistant:`, `tool:`, or a hidden role (`system:`, `thinking:`, …) reads
  as a header, exactly as Cursor's own format would: it starts a section
  (a Person turn in the handoff) or hides what follows. Filter 10 stopped
  treating indented role words this way; a column-0 one cannot be told
  apart. Cursor's JSONL transcripts and database chats are not affected.

Redaction is best effort in both directions: a legitimate value that looks like
a credential is redacted, and a tool argument that happens to contain one of
the instruction tags above loses that span. Both are recorded as gaps.

## Bucket privacy evidence

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
