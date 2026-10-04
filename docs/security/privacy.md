# Privacy

agent-archive copies your coding-agent sessions to a bucket you own. This
page says what that copy contains, what is kept out of it, what the tool
changes on your machine, and where its protections stop. The per-version history
of the filter is in the [filter changelog](../../dev/specs/privacy-filter-changelog.md).

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
- **Where it runs.** Only on a machine you set it up on, only for projects you
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
  access only to machines you trust, and treat a handoff from a shared bucket
  like any other text you paste into an agent (see
  [handoff](../guides/handoff.md#what-the-receiving-agent-is-told)).
- **A repository can claim another's identity.** `handoff --latest` finds a
  session from another computer by the repository key (see
  [what is uploaded](#what-is-uploaded)), a hash of the `origin` remote in
  the directory's git configuration. Anyone who wrote a repository you clone
  controls that configuration, and anyone who can write to your prefix can
  put any key on a session (the key is also a hash of a guessable public
  URL), so a hostile repository can declare
  `origin = https://github.com/you/private` and make `--latest` in its
  directory choose your session of that repository, whose text a coding agent
  started there would then read. The key is a convenience for finding your
  own work, not authentication. What limits the damage:
  - A session that matched only by key never displaces one that matched by
    path, so this cannot turn a working `--latest` into something else.
  - `handoff` puts a key-only match to a check before downloading any of the
    session's source. On a terminal it names the session (this or another
    Mac, project, start time, first prompt, each cut short) and asks, default
    No. Where it cannot ask (a pipe, or an agent's shell) it refuses and
    prints only the machine (this or another Mac), the start time, and the
    command that selects the session by ID, worded for the person, with the
    ID only when it is 32 lowercase hexadecimal digits (otherwise "agent-archive
    list"). It prints no other text from the session or the archive there,
    since an agent reads it. The command carries the agent and `--worktree`
    you gave, not `--format`, `--output`, `--max-bytes`, or `--branch`.
  - That refusal is a speed bump, not a barrier. It stops a steered agent
    from using such a session by accident; an agent can still name the
    session ID itself, run the command it printed, or set
    `AGENT_ARCHIVE_NONINTERACTIVE=0`.
  - A path match, an explicit session ID, and the picker are never
    questioned, because nothing there is chosen by a key. A path match cannot
    be steered by a hostile repository, but a writer of the archive can forge
    one (a project ID is a hash of a path), which is the previous point again.
  This does not protect against someone who can write to your prefix, who can
  plant sessions outright (see the previous point), and nothing stops you
  from answering yes.
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
the metadata sidecar. The sidecar's optional `title` is a short, truncated
preview of the first filtered human prompt, and its optional `name` the
session's name as the filtered transcript holds it (the last Claude Code
session name, Cursor's chat name, or, for a Claude Code subagent, the
description its parent gave the task), cut the same way (for `list`); both
are filter-derived text stored in the bucket, not a separate redaction pass.

- **The filtered transcript**: your prompts; the agent's
  replies; tool calls with their arguments (Edit bodies, shell commands,
  search patterns, file paths) and tool results, each string capped at
  64 KB; working directories (which usually contain your username); Git
  branch names; model names; token counts; timestamps; the app's own session
  and message IDs; summaries the app wrote when compacting a conversation;
  the names the app gave the session, which pass the same redaction as
  your prompts: every Claude Code session name the transcript records (set
  automatically from your prompt or by `/rename`; renaming adds a name and
  does not remove the earlier ones) and Cursor's current chat name; for a
  Claude Code subagent, the description its parent gave the task ("find the
  retention tests"), read from the `agent-<id>.meta.json` file Claude Code
  writes beside the subagent's transcript, passed through the same
  redaction as your prompts and cut to 512 bytes (nothing else in that
  file, such as the path of a worktree, is uploaded); the pull requests a
  Claude Code session linked (their `owner/repo`, number, and GitHub link,
  and nothing of the pull requests' text); and final messages hooks
  reported.
- **Skill evidence**: fresh setup defaults to `metadata`: names and SHA-256
  hashes of filtered `SKILL.md` text, with no body. Choose `none` for no
  filesystem skill inventory or snapshots, or `body` to include up to 16 KB
  of filtered text. A configuration written before this setting existed
  retains `body` until you change it. The scanned folders include your
  **user-level** folders, whatever the project, plus the project's own:

  | App | User-level folders | Project folder |
  | --- | --- | --- |
  | Claude Code | `~/.claude/skills` | `<project>/.claude/skills` |
  | Codex | `~/.agents/skills`, `~/.codex/skills` | `<project>/.agents/skills` |
  | Cursor | `~/.cursor/skills` | `<project>/.cursor/skills` |

  Only each folder's immediate `<skill>/SKILL.md` files are read (up to 256
  per folder), never other files in them. In a user-level folder a
  symlinked skill is followed only when its resolved `SKILL.md` stays inside
  that selected skill root. A project's `SKILL.md` that resolves outside the
  project is skipped. Sessions imported with `backfill` carry no skill evidence.

  Change the mode in setup. The change limits subsequent uploads, including
  pending work rebuilt under the new mode, but does not remove copies already
  on disk or in the bucket. Older source objects can remain after replacement;
  [bucket layout](../reference/bucket-layout.md) describes their lifetime.
- **Metadata**: the machine ID of the machine that captured it (random, made at
  setup), a project ID (a hash of the project's path, not the path itself),
  a repository key (below), the app and its version, the app's own session ID, capture times and the
  session's last record time, counts (including how many distinct files were
  edited, never which), models, skills used, the names of the ten most-called
  tools (MCP tool names included) with their call counts, the MCP servers
  called and how often, token counts (in total and per model), the git work
  the session's tool calls confirmed (commit SHAs, branch names, `owner/repo`,
  pull request numbers and links; never commit messages, pull request text,
  or commands), the last Git branch the transcript recorded, the pull
  requests a Claude Code session linked (`owner/repo`, number, and GitHub
  link), and the
  capture gaps the filter recorded (the names of omitted fields, never their
  values).
- **Repository key** (`repo_key`, in the metadata): when a project is a git
  repository with an `origin` remote, a hash of that remote's normalized
  address (host, owner, and repository name, with any username or token,
  scheme, port, and `.git` removed), so the same repository on another
  computer can be recognized. Only the hash is stored: never the address, and
  never a credential that was part of it. The hash is not secret from someone
  who already knows the address. Anyone with read access to your bucket can
  hash a repository URL they are curious about and check whether it appears,
  which reveals that you worked in that repository, and for a public
  repository the address is guessable. That is why only the hash is stored,
  and why bucket read access should stay limited to you. A project that is
  not a git repository, has no `origin`, or whose remote is a local path has
  no key. Two details of the matching: the repository name keeps its case, so
  `Acme/Widget` and `acme/widget` get different keys even on a host that
  treats them as one repository (a missed match, never a wrong one), and a
  remote's port is ignored, so two repositories with the same name on
  different ports of one host share a key. Only `origin` is read, as written
  in your git configuration: a remote that is a `url.insteadOf` shorthand is
  not expanded and may not match the full address used elsewhere. The key can
  be out of date: one recorded when the session started is never looked up
  again; one derived later stays if the remote is removed or git cannot be
  run; a changed remote replaces it only at the session's next content
  publish or metadata refresh; and a finished session never updates. The key
  is also read from the directory you run `handoff --latest` in, and is a
  convenience, not proof of identity: see the
  [threat model](#threat-model) for what a repository that lies about its
  `origin` can do and what `handoff` does about it.
- **Replay marker** (`replay`, in the metadata): only for a session a
  replay tool ran with `AGENT_ARCHIVE_REPLAY` set in its environment, the
  tool's run identifier (letters, digits, `.`, `_`, `:`, `-`; any other
  value marks the session without being recorded). Nothing else is read
  from the environment.
- **Commit** (`git_head`, in the metadata): for a session whose working
  directory is in a git repository, the full name of the commit checked out
  when the session started, whether the working tree then had uncommitted
  changes (a yes or no; which files, and what changed, are never read into
  the archive), and the commit checked out at the latest stop hook, each
  with the time a hook saw it. agent-archive's hooks run `git rev-parse` and
  `git status` in that directory to find them, with a short timeout. No
  branch, remote address, or path is taken from git. Anyone who can read
  your bucket and the repository can match a session to the commit it
  started from. A session that was not in a repository, whose hooks could
  not run git in time, has no observation. Sessions that started before this
  was recorded, or that
  `backfill` imported, have no starting commit; later live stops can record
  the last commit. Nothing infers a commit later.
- **Hook observations**: for each hook event, its name, the app's turn and
  message IDs, the model and model settings the hook reported, and, for a
  stop hook, the agent's final message (filtered like the transcript).

agent-archive adds nothing else about your machine: no hostname, username, or
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
  carry typed input into a browser or terminal (see [filter rules](#filter-rules)).
- **Cursor context payloads**: code chunks, file states, diffs, console logs,
  and other context Cursor attaches to a message.
- **Claude Code's `toolUseResult`**, which duplicates the tool result already
  kept.
- **Your credentials.** On macOS, R2 secrets are in the Keychain; S3
  credentials stay in your AWS profile. Neither appears in files, arguments,
  logs, or the bucket. On Linux, which has no Keychain, an R2 secret is in a
  private file (not encrypted) in the data directory, never in arguments,
  logs, or the bucket. See [Where credentials are kept](#where-credentials-are-kept).

Recognizable secrets inside kept text are replaced with `[REDACTED]` (see
[value-level redaction](../../dev/specs/privacy-filter.md#value-level-redaction)). Each omission and
redaction is recorded as a capture gap on the session, so a reader knows
something was removed.

## After a filter upgrade

For the current configured bucket, `agent-archive purge plan` inventories
unreferenced source objects and separately lists sessions whose current source
still uses an older filter. It writes a private, expiring plan under the local
data directory. `agent-archive purge plan --mode old-filter --before-filter 14`
narrows deletion candidates to unreferenced sources made by older filter
versions; replace `14` with the version you are upgrading to. Review the
printed bucket, prefix, keys, sizes, and digest. Pause **every** machine uploading
to the prefix, then run `agent-archive purge apply PLAN` within five minutes
and enter the digest prefix, or pass `--yes` for a noninteractive run. The
command rereads all metadata before each source deletion and writes a report
next to the plan; retry that plan before expiry after a partial failure.
This coordination is not atomic against an external writer. In a versioned
bucket, an administrator must also remove noncurrent versions and delete
markers. A current old-filter source cannot be deleted without deleting the
session unless its original can be safely refiltered and republished first.
Missing transcripts and retired destinations cannot be refiltered
automatically.

A new filter version (see the [filter changelog](../../dev/specs/privacy-filter-changelog.md))
applies to what is uploaded from then on. It does not clean what is already
in the bucket:

- **Sessions whose transcript is still on the capturing machine** are refiltered
  and republished automatically on that machine's next passes. The copy made
  with the old filter becomes a privacy-sensitive predecessor. After the new
  publication passes metadata and source read-back verification, retention
  removes the old copy after the 24-hour reader grace interval. A failed
  verification or deletion leaves it for retry on the next pass.
- **Sessions whose transcript is still there but no longer holds everything
  that was archived** (the app truncated or compacted it) are not replaced
  by the shorter transcript: the archived copy itself is filtered again
  with the new filter and republished, and the session is recorded as
  having a rewritten transcript. The old copy then stays as the previous
  source, with the same verified cleanup rule. If the app later restores the
  full transcript and the new filter writes some of its records differently
  from the old one (a new redaction label, say), the collector cannot tell
  the restored file from a rewritten one: the archived copy is kept, and
  records added to that transcript afterwards are not archived.
- **Sessions whose transcript is gone** (deleted, or on a machine that no longer
  runs agent-archive) are never refiltered: their current copy stays as the
  old filter made it until the session expires.
- **Sessions in an earlier destination** (after you changed storage) are
  not touched at all.

`show` prints a session's filter version (`filter_version` in `show --json`). To remove the older copies now,
delete from the bucket the source objects no session's metadata points at.
Pause every machine that uploads to the bucket first, so no publication is in
flight: a new source is uploaded before the metadata that points at it.
This needs the [AWS CLI](https://aws.amazon.com/cli/) and `jq`, and
credentials that can list, read, and delete under the prefix. Keep **every**
uploading machine paused until the plan has been applied. Every uploading
installation must report a successful pause; this local recipe cannot establish
that remote writers have stopped. Run the following blocks
in the **same bash or zsh shell**; a plan expires after five minutes and can
only be applied once. Keep the printed plan directory for recovery with
`purge_resume` if an attempt fails or expires. External
writers can still race a shell recipe, so these commands cannot provide an
atomic deletion against concurrent writes.

First, list what would be deleted:

<!-- purge-recipe:list (scripts/test_purge_recipe.py runs the three blocks below) -->
```sh
purge_dir=                    # clear any previous valid plan before attempting pause
purge_pause_ok=
if agent-archive pause; then   # must succeed on every uploading installation
  purge_pause_ok=yes
else
  echo "Pause failed; no cleanup plan is available. Inspect the lock holder and retry pause." >&2
fi

bucket=my-archive-bucket       # your bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty
# export AWS_PROFILE=...       # a profile that can list, read, and delete
# For R2: export AWS_ENDPOINT_URL=https://<ACCOUNT_ID>.r2.cloudflarestorage.com

# Identity contains no credentials. Use the same explicit endpoint/profile/region
# in a fresh shell. Do not put credentials in endpoint URLs.
purge_identity() {
  printf '%s\n' "${AWS_ENDPOINT_URL:-}" "${AWS_ENDPOINT_URL_S3:-}" \
    "${AWS_PROFILE:-}" "${AWS_DEFAULT_PROFILE:-}" "${AWS_REGION:-}" "${AWS_DEFAULT_REGION:-}" \
    "${AWS_IGNORE_CONFIGURED_ENDPOINT_URLS:-}" "${AWS_USE_FIPS_ENDPOINT:-}" "${AWS_USE_DUALSTACK_ENDPOINT:-}"
  config_path=${AWS_CONFIG_FILE:-$HOME/.aws/config}
  printf '%s\n' "$config_path"
  if [ -e "$config_path" ]; then
    shasum -a 256 "$config_path" || return 1
  else
    printf '%s\n' absent
  fi
}
purge_seal() {
  (cd "$purge_dir" && shasum -a 256 manifest.json > manifest.sha256) || return 1
  chmod 400 "$purge_dir/manifest.json" "$purge_dir/manifest.sha256" || return 1
}
purge_seal_attempt() {
  (cd "$purge_dir" && shasum -a 256 bucket prefix mode created keys targets metas tombstones > attempt.sha256) || return 1
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    (cd "$purge_dir" && shasum -a 256 "meta.$n" >> attempt.sha256) || return 1
  done < "$purge_dir/metas"
}
# Commit the authoritative progress payload and its checksum with one rename.
# Text logs remain useful diagnostics; the capsule preserves crash uncertainty.
purge_progress_write() {
  [ ! -d "$purge_dir/progress.json" ] && [ ! -L "$purge_dir/progress.json" ] || return 1
  jq -n --arg inflight "$1" --rawfile removed "$purge_dir/removed" \
    --rawfile tombstones "$purge_dir/tombstones" \
    '{removed:($removed|split("\n")[:-1]),tombstones:($tombstones|split("\n")[:-1]),inflight:$inflight}' \
    > "$purge_dir/progress.payload" || return 1
  digest=$(shasum -a 256 "$purge_dir/progress.payload") || return 1
  digest=${digest%% *}
  jq -n --arg digest "$digest" --rawfile payload "$purge_dir/progress.payload" \
    '{digest:$digest,payload:$payload}' > "$purge_dir/progress.next" || return 1
  chmod 400 "$purge_dir/progress.next" || return 1
  mv "$purge_dir/progress.next" "$purge_dir/progress.json"
}
purge_progress_check() {
  jq -ejr '.payload | select(type == "string")' "$1/progress.json" > "$purge_dir/progress.check" || return 1
  digest=$(shasum -a 256 "$purge_dir/progress.check") || return 1
  digest=${digest%% *}
  [ "$digest" = "$(jq -er '.digest' "$1/progress.json")" ] || return 1
  jq -e --slurpfile manifest "$purge_dir/manifest.json" '
    . as $state | $manifest[0].targets as $targets |
    (.removed|type)=="array" and (.tombstones|type)=="array" and
    (.inflight|type)=="string" and
    all(.removed[],.tombstones[]; . as $key | $targets|index($key)) and
    (.inflight == "" or (.inflight as $key | $targets|index($key)))
    ' "$purge_dir/progress.check" >/dev/null || return 1
  # A crash can append a successful inflight deletion before the capsule rename.
  jq -r '.removed[]' "$purge_dir/progress.check" > "$purge_dir/progress.expected" || return 1
  if ! cmp -s "$purge_dir/progress.expected" "$1/removed"; then
    jq -er '.inflight | select(length > 0)' "$purge_dir/progress.check" >> "$purge_dir/progress.expected" || return 1
    cmp -s "$purge_dir/progress.expected" "$1/removed" || return 1
  fi
  jq -r '.tombstones[]' "$purge_dir/progress.check" > "$purge_dir/progress.expected" || return 1
  cmp -s "$purge_dir/progress.expected" "$1/tombstones"
}
purge_manifest() {
  jq -n --arg bucket "$bucket" --arg prefix "$prefix" --arg mode "$mode" \
    --arg selector "$selector" --arg identity "$(purge_identity)" \
    --argjson created "$(cat "$purge_dir/created")" \
    --rawfile keys "$purge_dir/keys" --rawfile targets "$purge_dir/targets" \
    --rawfile metas "$purge_dir/metas" --rawfile selected "$purge_dir/selected" --slurpfile snapshots "$purge_dir/snapshots" \
    '{format:1,bucket:$bucket,prefix:$prefix,mode:$mode,selector:$selector,
      identity:$identity,created:$created,keys:($keys|split("\n")[:-1]),
      targets:($targets|split("\n")[:-1]),metas:($metas|split("\n")[:-1]),
      selected_sessions:($selected|split("\n")[:-1]),snapshots:$snapshots}' > "$purge_dir/manifest.json" || return 1
  purge_seal
}
purge_check_manifest() {
  (cd "$1" && shasum -a 256 -c manifest.sha256 >/dev/null) &&
    jq -e 'type == "object" and .format == 1 and
      (.created|type)=="number" and (.keys|type)=="array" and
      (.targets|type)=="array" and (.metas|type)=="array" and
      (.snapshots|type)=="array" and (.metas|length)==(.snapshots|length) and
      (.keys as $k | all(.targets[]; . as $t | $k|index($t)))' \
      "$1/manifest.json" >/dev/null
}
purge_dir=                         # never inherit a previous plan
purge_prepare() {
  mode=$1; selector=${2:-}
  [ "${purge_pause_ok:-}" = yes ] || { echo "Pause must succeed before preparing a plan." >&2; return 1; }
  purge_dir=                       # a failed new attempt cannot expose an old plan
  case "$prefix" in ''|*/) ;; *) echo "Prefix must be empty or end in /." >&2; return 1 ;; esac
  case "$mode" in
    unreferenced|all) ;;
    old) case "$selector" in ''|*[!0-9]*) echo "Invalid filter version." >&2; return 1 ;; esac ;;
    machine) [ -n "$selector" ] || { echo "Missing machine ID." >&2; return 1; } ;;
    *) echo "Invalid purge mode." >&2; return 1 ;;
  esac
  case "${AWS_ENDPOINT_URL:-}${AWS_ENDPOINT_URL_S3:-}" in
    *'@'*|*'?'*|*'#'*) echo "Endpoint must not contain credentials, query, or fragment." >&2; return 1 ;;
  esac
  purge_identity >/dev/null || return 1
  umask 077
  purge_dir=$(mktemp -d "${TMPDIR:-/tmp}/agent-archive-purge.XXXXXXXX") || return 1
  printf '%s\n' "$bucket" > "$purge_dir/bucket" || return 1
  printf '%s\n' "$prefix" > "$purge_dir/prefix" || return 1
  printf '%s\n' "$mode" > "$purge_dir/mode" || return 1
  # Fetch a complete listing before inspecting or deleting any object.
  scope="${prefix}sessions/"; [ "$mode" = all ] && scope=$prefix
  if ! aws s3api list-objects-v2 --bucket "$bucket" --prefix "$scope" \
      --query 'Contents[].Key' --output json </dev/null > "$purge_dir/list.json"; then
    echo "Listing failed; nothing deleted." >&2; return 1
  fi
  if ! jq -r --arg p "$scope" '
      if . == null then empty
      elif type == "array" and all(.[]; type == "string" and startswith($p) and
          (explode | all(.[]; . >= 32))) then .[]
      else error("incomplete listing") end
    ' "$purge_dir/list.json" > "$purge_dir/unsorted"; then
    echo "Listing invalid; nothing deleted." >&2; return 1
  fi
  LC_ALL=C sort -u "$purge_dir/unsorted" > "$purge_dir/keys" || return 1
  : > "$purge_dir/metas" || return 1
  : > "$purge_dir/metadata.keys" || return 1
  while IFS= read -r key; do
    case "$key" in "${prefix}sessions/"*/metadata.json)
      printf '%s\n' "$key" >> "$purge_dir/metadata.keys" || return 1 ;;
    esac
  done < "$purge_dir/keys"
  # Full-prefix mode deliberately removes every scoped key, even damaged metadata.
  # Only selective modes read metadata to establish source ownership.
  [ "$mode" = all ] || cp "$purge_dir/metadata.keys" "$purge_dir/metas" || return 1
  : > "$purge_dir/current" || return 1
  : > "$purge_dir/selected" || return 1
  : > "$purge_dir/snapshots" || return 1
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    if ! aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/meta.$n"; then
      echo "Cannot read $meta; nothing deleted." >&2; return 1
    fi
    jq -Rs . "$purge_dir/meta.$n" >> "$purge_dir/snapshots" || return 1
    # Require the source in this session, a decimal filter version, and a machine ID.
    if ! jq -ser --arg dir "${meta%metadata.json}" --arg p "$prefix" '
        if length == 1 and (.[0] | type) == "object" then .[0]
        else error("metadata must contain one object") end |
        .source_bundle.key as $s | .filter_version as $v | .machine_id as $m |
        if ($s | type) == "string" and ($s | startswith(($dir | ltrimstr($p)))) and
           ($s | ltrimstr(($dir | ltrimstr($p))) |
             test("^source\\.[0-9a-f]{64}\\.jsonl\\.gz$")) and
           ($s | explode | all(.[]; . >= 32)) and
           ($v | tostring | test("^[0-9]+$")) and
           ($m | type) == "string" and ($m | length > 0) then
          [$s, ($v | tostring), $m] | @tsv
        else error("invalid metadata") end
      ' "$purge_dir/meta.$n" > "$purge_dir/fields.$n"; then
      echo "Invalid $meta; nothing deleted." >&2; return 1
    fi
    IFS="$(printf '\t')" read -r source version owner < "$purge_dir/fields.$n" || return 1
    if ! grep -Fxq -- "$prefix$source" "$purge_dir/keys"; then
      echo "Current source missing for $meta; nothing deleted." >&2; return 1
    fi
    printf '%s%s\n' "$prefix" "$source" >> "$purge_dir/current" || return 1
    if { [ "$mode" = old ] && [ "$version" -lt "$selector" ]; } ||
       { [ "$mode" = machine ] && [ "$owner" = "$selector" ]; }; then
      printf '%s\n' "$meta" >> "$purge_dir/selected" || return 1
    fi
  done < "$purge_dir/metas"
  LC_ALL=C sort -u "$purge_dir/current" -o "$purge_dir/current" || return 1
  : > "$purge_dir/targets" || return 1
  if [ "$mode" = all ]; then
    cat "$purge_dir/metadata.keys" > "$purge_dir/targets" || return 1
    while IFS= read -r key; do
      if ! grep -Fxq -- "$key" "$purge_dir/metadata.keys"; then
        printf '%s\n' "$key" >> "$purge_dir/targets" || return 1
      fi
    done < "$purge_dir/keys"
  elif [ "$mode" = unreferenced ]; then
    jq -r --arg p "$prefix" '
      if . == null then empty else .[] |
        select((ltrimstr($p) |
          test("^sessions/[^/]+/[^/]+/source\\.[0-9a-f]{64}\\.jsonl\\.gz$"))) end
      ' "$purge_dir/list.json" > "$purge_dir/sources" || return 1
    LC_ALL=C sort -u "$purge_dir/sources" -o "$purge_dir/sources" || return 1
    LC_ALL=C comm -23 "$purge_dir/sources" "$purge_dir/current" > "$purge_dir/targets" || return 1
  else
    while IFS= read -r meta; do
      printf '%s\n' "$meta" >> "$purge_dir/targets" || return 1
      while IFS= read -r key; do
        case "$key" in "${meta%metadata.json}"*)
          [ "$key" = "$meta" ] || printf '%s\n' "$key" >> "$purge_dir/targets" || return 1 ;;
        esac
      done < "$purge_dir/keys"
    done < "$purge_dir/selected"
  fi
  date +%s > "$purge_dir/created" || return 1
  : > "$purge_dir/tombstones" || return 1
  purge_manifest || return 1
  purge_seal_attempt || return 1
  : > "$purge_dir/VALID" || return 1 # written only after every check succeeds
  echo "Plan $purge_dir: $(wc -l < "$purge_dir/targets" | tr -d ' ') exact keys."
  cat "$purge_dir/targets"
}
purge_apply() {
  [ "${purge_pause_ok:-}" = yes ] || { echo "Pause must succeed before deleting." >&2; return 1; }
  if [ -z "${purge_dir:-}" ] || [ ! -f "$purge_dir/VALID" ]; then
    echo "No valid plan; nothing deleted." >&2; return 1
  fi
  rm "$purge_dir/VALID" || return 1 # a plan can be attempted only once
  (cd "$purge_dir" && shasum -a 256 -c attempt.sha256 >/dev/null) || return 1
  purge_check_manifest "$purge_dir" || { echo "Invalid manifest; nothing deleted." >&2; return 1; }
  [ "$(jq -r .identity "$purge_dir/manifest.json")" = "$(purge_identity)" ] || {
    echo "Destination identity changed; nothing deleted." >&2; return 1;
  }
  if [ "$(cat "$purge_dir/bucket")" != "$bucket" ] ||
     [ "$(cat "$purge_dir/prefix")" != "$prefix" ] ||
     [ "$(cat "$purge_dir/mode")" != "$mode" ]; then
    echo "Bucket, prefix, or mode changed; nothing deleted." >&2; return 1
  fi
  now=$(date +%s); created=$(cat "$purge_dir/created")
  if [ "$now" -lt "$created" ] || [ $((now - created)) -gt 300 ]; then
    echo "Plan expired; nothing deleted." >&2; return 1
  fi
  scope="${prefix}sessions/"; [ "$mode" = all ] && scope=$prefix
  if ! aws s3api list-objects-v2 --bucket "$bucket" --prefix "$scope" \
      --query 'Contents[].Key' --output json </dev/null > "$purge_dir/recheck.json" ||
     ! jq -r --arg p "$scope" '
       if . == null then empty
       elif type == "array" and all(.[]; type == "string" and startswith($p) and
           (explode | all(.[]; . >= 32))) then .[]
       else error("incomplete listing") end
     ' "$purge_dir/recheck.json" > "$purge_dir/recheck.unsorted" ||
     ! LC_ALL=C sort -u "$purge_dir/recheck.unsorted" > "$purge_dir/recheck.keys" ||
     ! cmp -s "$purge_dir/keys" "$purge_dir/recheck.keys"; then
    echo "Listing changed or failed; nothing deleted. Make a new plan." >&2; return 1
  fi
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    if ! aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/recheck.meta" ||
       ! cmp -s "$purge_dir/meta.$n" "$purge_dir/recheck.meta"; then
      echo "Metadata changed or failed at $meta; nothing deleted. Make a new plan." >&2
      return 1
    fi
  done < "$purge_dir/metas"
  : > "$purge_dir/removed" || return 1
  : > "$purge_dir/errors" || return 1
  : > "$purge_dir/pending" || return 1
  cp "$purge_dir/targets" "$purge_dir/pending" || return 1
  purge_progress_write "" || return 1
  while IFS= read -r key; do
    # Write ahead: if interrupted after remote deletion, listing resolves uncertainty.
    printf '%s\n' "$key" > "$purge_dir/inflight" || return 1
    purge_progress_write "$key" || return 1
    if ! aws s3 rm "s3://$bucket/$key" </dev/null 2> "$purge_dir/error.next"; then
      cat "$purge_dir/error.next" >> "$purge_dir/errors" || return 1
      cat "$purge_dir/error.next" >&2
      purge_progress_write "" || return 1
      rm "$purge_dir/inflight" || return 1
      echo "Delete failed at $key. Already removed:" >&2
      cat "$purge_dir/removed" >&2
      echo "Not confirmed removed (including failed key):" >&2
      cat "$purge_dir/pending" >&2
      return 1
    fi
    if ! printf '%s\n' "$key" >> "$purge_dir/removed" ||
       ! sed '1d' "$purge_dir/pending" > "$purge_dir/next" ||
       ! mv "$purge_dir/next" "$purge_dir/pending"; then
      echo "Progress log failed after removing $key; stopped. Inspect the bucket." >&2
      return 1
    fi
    purge_progress_write "" || return 1
    rm "$purge_dir/inflight" || return 1
  done < "$purge_dir/targets"
  echo "Deleted $(wc -l < "$purge_dir/removed" | tr -d ' ') keys."
}
purge_resume() {
  original=$1
  [ "${purge_pause_ok:-}" = yes ] || { echo "Pause must succeed before recovery." >&2; return 1; }
  purge_dir=
  purge_check_manifest "$original" || { echo "Unreadable or corrupt manifest; nothing deleted." >&2; return 1; }
  [ "$(jq -r .bucket "$original/manifest.json")" = "$bucket" ] &&
    [ "$(jq -r .prefix "$original/manifest.json")" = "$prefix" ] &&
    [ "$(jq -r .identity "$original/manifest.json")" = "$(purge_identity)" ] || {
      echo "Destination changed; nothing deleted." >&2; return 1;
    }
  case "${AWS_ENDPOINT_URL:-}${AWS_ENDPOINT_URL_S3:-}" in
    *'@'*|*'?'*|*'#'*) echo "Endpoint must not contain credentials, query, or fragment." >&2; return 1 ;;
  esac
  purge_identity >/dev/null || return 1
  umask 077
  purge_dir=$(mktemp -d "${TMPDIR:-/tmp}/agent-archive-purge.XXXXXXXX") || return 1
  cp "$original/manifest.json" "$purge_dir/manifest.json" &&
    cp "$original/manifest.sha256" "$purge_dir/manifest.sha256" || return 1
  mode=$(jq -r .mode "$purge_dir/manifest.json")
  selector=$(jq -r .selector "$purge_dir/manifest.json")
  printf '%s\n' "$bucket" > "$purge_dir/bucket" || return 1
  printf '%s\n' "$prefix" > "$purge_dir/prefix" || return 1
  printf '%s\n' "$mode" > "$purge_dir/mode" || return 1
  scope="${prefix}sessions/"; [ "$mode" = all ] && scope=$prefix
  aws s3api list-objects-v2 --bucket "$bucket" --prefix "$scope" \
    --query 'Contents[].Key' --output json </dev/null > "$purge_dir/list.json" || return 1
  jq -r --arg p "$scope" '
    if . == null then empty
    elif type == "array" and all(.[]; type == "string" and startswith($p) and
      (explode|all(.[]; . >= 32))) then .[] else error("incomplete listing") end
    ' "$purge_dir/list.json" > "$purge_dir/unsorted" || return 1
  LC_ALL=C sort -u "$purge_dir/unsorted" > "$purge_dir/keys" || return 1
  jq -r '.keys[]' "$purge_dir/manifest.json" > "$purge_dir/original.keys" || return 1
  LC_ALL=C comm -13 "$purge_dir/original.keys" "$purge_dir/keys" > "$purge_dir/new.keys" || return 1
  [ ! -s "$purge_dir/new.keys" ] || { echo "New objects conflict with recovery; nothing deleted." >&2; return 1; }
  : > "$purge_dir/metas" || return 1
  : > "$purge_dir/targets" || return 1
  : > "$purge_dir/tombstones" || return 1
  if [ -e "$original/progress.json" ]; then
    purge_progress_check "$original" || { echo "Corrupt progress; nothing deleted." >&2; return 1; }
    jq -r '.tombstones[],.removed[],(.inflight|select(length>0))' \
      "$purge_dir/progress.check" > "$purge_dir/tombstones" || return 1
  elif [ -e "$original/removed" ] || [ -e "$original/pending" ] || [ -e "$original/inflight" ]; then
    echo "Missing progress capsule; nothing deleted." >&2; return 1
  fi
  jq -r '.metas[]' "$purge_dir/manifest.json" > "$purge_dir/original.metas" || return 1
  n=0; i=0
  while IFS= read -r meta; do
    i=$((i + 1))
    if grep -Fxq -- "$meta" "$purge_dir/keys"; then
      if grep -Fxq -- "$meta" "$purge_dir/tombstones"; then
        echo "Metadata reappeared or has an interrupted delete: $meta; nothing deleted." >&2; return 1
      fi
      n=$((n + 1))
      aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/meta.$n" || return 1
      jq -jr --argjson i "$((i - 1))" '.snapshots[$i]' "$purge_dir/manifest.json" > "$purge_dir/expected" || return 1
      cmp -s "$purge_dir/expected" "$purge_dir/meta.$n" || {
        echo "Metadata changed at $meta; nothing deleted." >&2; return 1;
      }
      printf '%s\n' "$meta" >> "$purge_dir/metas" || return 1
    elif ! jq -e --arg m "$meta" '.targets|index($m)' "$purge_dir/manifest.json" >/dev/null; then
      echo "Unselected metadata disappeared; nothing deleted." >&2; return 1
    else
      # An observed absence is lineage history even without a local delete record.
      printf '%s\n' "$meta" >> "$purge_dir/tombstones" || return 1
    fi
  done < "$purge_dir/original.metas"
  # Full-prefix plans do not snapshot metadata bodies, but retain deletion history.
  if [ "$mode" = all ]; then
    while IFS= read -r key; do
      case "$key" in "${prefix}sessions/"*/metadata.json)
        if grep -Fxq -- "$key" "$purge_dir/tombstones" &&
           grep -Fxq -- "$key" "$purge_dir/keys"; then
          echo "Metadata reappeared or has an interrupted delete: $key; nothing deleted." >&2; return 1
        elif ! grep -Fxq -- "$key" "$purge_dir/keys"; then
          printf '%s\n' "$key" >> "$purge_dir/tombstones" || return 1
        fi ;;
      esac
    done < "$purge_dir/original.keys"
  fi
  # Preserve original metadata-first order; never discover new ownership.
  jq -r '.targets[]' "$purge_dir/manifest.json" > "$purge_dir/original.targets" || return 1
  while IFS= read -r key; do
    if grep -Fxq -- "$key" "$purge_dir/keys"; then
      printf '%s\n' "$key" >> "$purge_dir/targets" || return 1
    fi
  done < "$purge_dir/original.targets"
  # Preserve inherited deletion history even if this recovery plan is never applied.
  : > "$purge_dir/removed" || return 1
  purge_progress_write "" || return 1
  date +%s > "$purge_dir/created" || return 1
  purge_seal_attempt || return 1
  : > "$purge_dir/VALID" || return 1
  echo "Recovery plan $purge_dir: review these remaining original keys, then purge_apply within five minutes:"
  cat "$purge_dir/targets"
}
purge_prepare unreferenced
```

Review the printed exact keys. The manifest needs the macOS `shasum` utility as well as AWS CLI and `jq`.
A failed listing or any unreadable or malformed
metadata leaves no valid plan and must be fixed before trying again. Then,
within five minutes and in the same shell, delete:

<!-- purge-recipe:delete -->
```sh
purge_apply
```

To remove sessions whose current copy predates a filter version (here 10)
as well, delete each one whole, metadata first:

Run the preparation block above first in the same shell. The following makes
a new plan. Review every printed key, then run the `purge_apply` block above
within five minutes in that shell.

<!-- purge-recipe:old-sessions -->
```sh
purge_prepare old 10
```

After a failed, interrupted, or expired attempt, keep every writer paused.
In a fresh bash or zsh shell, load the helper definitions above (omit the final
`purge_prepare unreferenced` line), restore the same bucket, prefix, endpoint,
profile and region (and unchanged AWS configuration), then run `purge_resume /absolute/path/to/retained-plan`.
Review its printed remaining original keys and run `purge_apply` again within
five minutes. Use the newest printed recovery directory for each subsequent retry, keeping
its earlier directories until cleanup succeeds. Recovery uses the immutable
manifest and a complete bucket listing, so a remote delete interrupted before local logging does not lose source keys.
Changed or reappearing metadata, new objects, and corrupt or missing attempt progress abort recovery. An uncertain
metadata deletion that still exists also aborts; inspect that conflict first.
The recipes cannot guarantee atomicity against external writers. The private
manifest retains metadata snapshots and object names, never transcript content
or credentials. After successful cleanup, remove the retained plan directories;
deleting them earlier removes resumability.

When you are done, run `agent-archive resume` on every machine you paused.

A session deleted this way is gone from `list`; if its transcript is still
on the capturing machine and changes again, that machine publishes it anew. On an S3
bucket with versioning turned on, a delete only hides the object: remove
the noncurrent versions too, or add a lifecycle rule that expires them.

## What an agent can read through the skill

Setup also installs an [`agent-archive` skill](../guides/agent-skills.md) in
each app, so that a coding agent can pull in a past session when you ask. It
reads through the same commands you run: `handoff`, `list`, `show`, and
`status`. That is the filtered content the [archive holds](#what-is-uploaded)
(a session found on this machine is filtered the same way before it is printed),
cut to roughly 120 KB a session (`handoff`'s bound is best effort), and
nothing broader: no bucket credentials, no raw transcript files, no files of
your projects. `status` adds your setup's summary: the storage destination,
the included project folders, and the state of capture. `list` shows titles
(each is the session's name, else its first prompt) across all your
projects. Three things follow.

- **It is shown to that agent's provider.** A pulled-in session becomes part
  of the receiving agent's conversation, so Claude Code, Codex, or Cursor (and
  whoever they send prompts to) see what another agent's session held,
  including any secret the filter missed. Ask for a session only in an agent
  you would show it to.
- **It is untrusted text.** The filter removes credentials and injected
  instruction blocks, not hostile wording, and a session's text may have come
  from a web page or a file the original agent read. The skill tells the
  agent to treat what it prints as data: never to follow an instruction in it,
  or run a command because it suggests one, and to open only the one file a
  trimmed handoff names. That is guidance to a model, not a guarantee. The
  backstop is the agent's own permission prompt: in Claude Code, `handoff
  --to`, `setup`, and `purge` are not pre-approved and ask you first (unless
  you allowed them, or run the agent without approvals); in Codex and Cursor,
  their approvals and sandbox decide, and Cursor's Run Everything mode asks
  nothing.
- **The skill is not a barrier.** The agent runs as you, so it can read
  anything you can whatever the skill says; the skill only names what it
  should run. `agent-archive setup --no-skills` stops offering agents the
  skill; it does not stop an agent you have given a shell from running
  `agent-archive`.

## What changes on your machine

The `hooks` entry of each included app's settings file, one background job (a
LaunchAgent on macOS; on Linux a systemd user timer and service in
`~/.config/systemd/user`), two
agent skill files per app location (`agent-archive` and `/handoff`, marked so
uninstall removes only setup's), local state private to your account
(transcripts are read in place, not copied), and, for R2, one Keychain item on
macOS or a credentials file on Linux ([below](#where-credentials-are-kept)).
When it reads a Cursor chat database it works from a temporary copy, which
holds every Cursor chat, including those of projects you did not include, and
is deleted when the read ends (an abandoned one by the next read): on macOS in your per-user temporary
folder, on Linux in `~/.cache/agent-archive/cursor-snapshots` (or under
`XDG_CACHE_HOME`; mode 0700, never `/tmp`, and tagged so backup tools that
honor `CACHEDIR.TAG` skip it). The full list, and what uninstall
removes, is in [setup](../getting-started/setup.md#what-setup-changes-on-your-machine).

## Where credentials are kept

What follows is how the code stores an R2 secret on each platform.

- **macOS:** the login Keychain, under the service `agent-archive`. The
  secret is never written to a file.
- **Linux, which has no Keychain:** the secret is stored **on disk**, in a file per credential, `<data directory>/credentials/<reference>.json`,
  created with mode 0600 in a folder with mode 0700 (the data directory is
  `~/.local/share/agent-archive` or `AGENT_ARCHIVE_HOME`). It is written to a
  temporary file that is created 0600 and renamed into place, so it is never
  readable by others, even briefly. agent-archive **refuses to read** the
  file, and to save into the folder, when the file or the folder is
  accessible by group or others, is a symbolic link, is owned by another
  user, or is not a regular file or folder; the error names the path and the
  `chmod` that fixes it. Root, and anyone who can read your files as you
  (a backup, another process of yours), can still read the file: it is
  protected from other accounts, not encrypted. The file is not uploaded,
  is not in `status` or error output, and is removed by
  `uninstall --delete-local-data`.
- **Better on Linux: an S3 profile.** S3 credentials stay in your AWS
  shared credentials or SSO configuration, which can be short-lived or
  role-based, and agent-archive stores no secret of its own. We recommend an
  S3 profile on Linux over the credentials file wherever you can use one.
- **Containers and services: environment variables.** Where no credentials
  file exists for the reference, agent-archive reads the R2 key from
  `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY`
  (the variables `setup --yes` reads), which suits a container's
  configuration or a service's `EnvironmentFile`. The fallback applies to a
  process that has those variables: a scheduled collector does not inherit
  an interactive shell's variables, so `setup` never counts an exported key
  as stored; it saves the key to the credentials file, where the collector
  finds it. The fallback is read only:
  agent-archive never writes or deletes an environment credential. A
  credentials file that exists but is refused for its permissions is an
  error; it is never skipped in favor of the environment.

## Filter rules

Every retained string, at every depth, passes injected-instruction
stripping, credential redaction, and a 64 KB cap. In short:

- **Tool arguments** keep their names, but values are dropped for typed or
  submitted text (a browser tool filling a form, a terminal's stdin), for a
  value labelled as a password, PIN, card number, or similar, and for any
  argument whose name says it is a credential (`api_key`, `password`,
  `token`, `Authorization`, …).
- **Credentials in text** are replaced with `[REDACTED]`, keeping the name
  so you can see what was there: assignments by name (`DB_PASSWORD=…`,
  `"api_key": …`, `--token …`), known token shapes (AWS, GitHub, Slack,
  Stripe, Google, OpenAI, Anthropic, and more), private keys, JWTs,
  passwords in URLs and on command lines, `.netrc` and `.pgpass` entries,
  and wallet seed phrases. Filter 15 also redacts `aa-pair1:` machine pairing
  bundles, including short or truncated payloads, in every retained string.
  An ordinary-word pairing code has no reliable recognizable shape; avoid
  pasting it into a transcript.
- **JSON inside strings** is decoded and filtered as JSON.

Each redaction or omission is recorded as a capture gap, so a session says
what was taken out. The exact rules, with every pattern and its edge cases,
are in the [filter specification](../../dev/specs/privacy-filter.md).

## Bucket privacy evidence

Setup tests object access and inspects native bucket public-access controls separately. Successful uploads do not prove a bucket is private. Inspection uses existing credentials, is read-only, and has a five-second total deadline. Failure or missing inspection permission does not require administrator credentials.

For AWS S3, all four bucket-level Block Public Access flags must be observed enabled before the tool reports `verified_private`. Otherwise it checks policy status and the bucket ACL for public configuration. A public policy or ACL yields `public_or_risky`; incomplete or denied checks yield `not_verified`. Account-level controls might further restrict access, so `public_or_risky` identifies configuration that needs review, not proof of anonymous object access. A private bucket policy or ACL alone is insufficient because object ACLs and access points can expose data.

For R2, setup stores S3-compatible object credentials, not a Cloudflare management API token. These cannot inspect managed/custom public domains. A manually configured R2 bucket therefore remains `not_verified` and links the public-bucket settings instructions. The tool does not send object credentials to the management API. When setup creates the bucket itself, it can inspect public access with the temporary bootstrap token ([below](#guided-r2-bucket-creation)).

### Guided R2 bucket creation

This is experimental, and offered only when `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1`
is set. When setup creates an R2 bucket for you, you paste a Cloudflare API token with
two permissions (Workers R2 Storage Write, and Account API Tokens Write). That
**bootstrap token** can create buckets, and create and revoke API tokens, in
your Cloudflare account, so it is far more powerful than the key the archive
uses. Setup treats it accordingly:

- It is used only against Cloudflare's management API, in memory, for the
  seconds setup takes. It is never written to the setup draft, the setup
  journal, the configuration, the Keychain, a log, or diagnostics, never put in
  the environment of a program setup starts, and never sent to the collector or
  the bucket. Setup drops it as soon as the key is stored, or when the flow
  fails or stops before that. (Go cannot guarantee that no copy lingers in
  process memory until the process exits.)
  A `CLOUDFLARE_API_TOKEN` you set yourself is read once and then removed from
  setup's own environment, so a program setup starts does not inherit it (if
  that fails, setup says so). It stays in your shell, as you set it.
- The key it stores is a separate token that can read, write, and list objects
  in the one new bucket. The two are never used for each other's endpoint: the
  archive's key never goes to the management API.
- You can delete the bootstrap token in the dashboard as soon as setup ends.
- Setup uses the token to change only what it just created: the new bucket
  and a token for it (which it revokes again if the key fails its check or
  can't be stored). It sets no lifecycle rule.
- If Ctrl-C stops setup while it creates and checks the key, setup revokes
  that token before it exits (a second Ctrl-C during the revoke is answered
  with "still revoking"). A Ctrl-C after the key exists, while setup reads the
  bucket's public-access settings and stores the key, is not caught: it leaves
  that key's token in your account, and its name was printed when it was
  created, so you can revoke it in the dashboard. The bootstrap token stays in
  memory until the key is stored.
- Once the key is stored, the bootstrap token is gone, so setup can no longer
  revoke the key's token. If setup then ends without using the bucket (a
  failed storage check, a cancelled review, an error), it prints the bucket's
  name and the token's name, once, and how to remove them in the dashboard.

**What "checked at setup" means.** With the bootstrap token, setup reads two
things about the new bucket once: whether its public `r2.dev` URL is on, and
whether it has custom domains. It prints what it found, such as "r2.dev public
access: off (checked at setup)". That is a snapshot from the moment of
creation. Setup saves this check as bucket privacy evidence. If both reads
succeed and show `r2.dev` off with no enabled custom domains, the review reports the
bucket private **at setup**. This does not cover signed URLs, access granted
later in the dashboard, applications that proxy reads, or copies of what you
archive. If either read is refused, the review keeps privacy unknown; a read
that failed never hides what the other found. If the `r2.dev` URL is on, or a
custom domain serves the bucket, setup stops and asks what now: check again
(after you turn it off in the dashboard), choose another storage option, or
continue anyway; Enter chooses another storage option. Choosing another
revokes the key's token, which was made and checked but not stored, and says
the empty bucket is left in your account. Turning public access off is up to
you. The collector cannot repeat the management API reads with the bucket's
object key. Its next privacy refresh returns to `not_verified`; without a
refresh, the saved observation becomes stale after 24 hours.

Inspection covers native bucket public access. It does not assess applications that proxy authorized reads, shared signed URLs, or copies of archived data.

The result includes fixed diagnostic codes, scope, check time, and a storage-configuration fingerprint. Setup records it in the resumable setup draft and commits it to the active configuration only when setup is confirmed. Resuming a draft checks storage again; guided R2 privacy retains its original setup-time observation because the bootstrap token is gone. Status reads the local result, never a remote API. After 24 hours, a clock rollback, or a storage configuration change, status reports privacy as unverified. The scheduled background collector refreshes evidence when the saved result is missing, is for another storage configuration, or is more than 12 hours old. It can refresh S3 privacy through the existing credentials; for R2 it returns to `not_verified` without a management token. A paused install is never inspected. Provider errors and credentials are not included in the report.

References used for the implementation:

- [AWS Block Public Access semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html)
- [R2 S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)
- [R2 public bucket settings](https://developers.cloudflare.com/r2/buckets/public-buckets/)

Synthetic tests cover allowed, denied, incomplete, public-policy, public-ACL, R2-unavailable, expired, and changed-configuration results. Actual AWS inspection and R2 dashboard verification remain live acceptance checks.

## Informational machine records

Setup and the collector write a small record under `machines/<machine_id>.json`
in your bucket. New personal data is the name you choose and the operating
system/architecture. The default name is `unnamed-` plus four characters of a
random machine ID, never your hostname. Records also carry application version,
nonsecret credential identifiers and locally committed provenance when present,
and a heartbeat updated at most daily. They contain no project paths, sessions,
transcript content, credential secrets, pairing codes or bundles. A heartbeat
does not reveal current activity; paused machines need not send one.

All bucket writers can forge these records. Listing does not contact management
APIs or a password manager, and records do not authorize revocation or establish
exclusive key ownership. Uninstall leaves remote records and access unchanged;
remove access at your storage provider, and include `machines/` when deleting
the entire archive. Local registration retry state is removed by
`uninstall --delete-local-data`.

Explicit experimental provider verification reads metadata only. Management
API tokens remain in memory, are removed from the process environment before
requests, and are never written to config, setup drafts, journals, credential
stores or output. Interactive token commands have bounded stdout and discarded
stderr; child environments exclude token, secret, object and pairing credential
variables. Ordinary machine listing and collection never acquire a management
token. Provider inventory can be restricted to creator-owned keys, so missing
metadata never proves that access was removed.

#### Dedicated key issuance draft

The experimental issuance ledger under `issued/` is mode 0600 and contains immutable
recipient, issuer and slot IDs, destination binding, provider key ID/name, opaque
credential references, labels, timestamps and lifecycle/cleanup outcomes. It contains
no management token, object secret, pairing code or encrypted bundle. Unused spare
object credentials remain in the configured credential store; the config's
`spare_credential_refs` is advisory and cannot grant eligibility. Default target two,
configurable zero through five. Spares can outlive the issuer's main key.

Creation/reservation/delivery intents are journaled before external effects. Lost API
responses leave explicit cleanup work; provider inventory cannot recover the token's
one-time value. Ambiguous exposure is never returned to the spare pool. Removing the
issuer-local delivered secret retains lineage, because an issuer could have copied
any secret it created. Bucket claims remain informational and cannot establish
ownership or authorize deletion. The management token is acquired for one explicit
command, never saved, never sent to storage, and discarded afterward. Live provider
acceptance and revocation integration are still pending for this draft.

## Encrypted shared-key pairing beta

A pairing bundle carries the destination, app/capture and retention settings,
repository hashes and portable scope paths, handoff arguments, and, for R2, the
explicitly shared object credential. S3 carries only its local profile name and
settings. Argon2id and XChaCha20-Poly1305 protect the bundle with a generated
six-word code; deliver the two pieces separately. Interactive source delivery
requires terminal input and output so a redirected file cannot retain the code.
Alternate-screen clearing cannot protect against recording or screen sharing.

The receiver keeps the decrypted payload in memory and stages R2 secrets only
in the credential store. Neither side writes the code or bundle to config,
drafts, journals, registration state or the secret-free `issued/` ledger. Only
an explicitly requested source `--file` saves an encrypted bundle; delete it
when no longer needed. Clipboard cleanup checks for the exact bundle before
clearing; clipboard history can retain it. The ledger retains delivery intent,
expiry, credential references and informational claim observations, and is
removed by `uninstall --delete-local-data`.

Pairing refuses inside coding agents. Pasted bundles are redacted before upload,
but ordinary-word codes cannot be reliably recognized. If either piece may
have been seen, create new pairing pieces; if both may have been seen, replace
the shared R2 credential on every machine using it. Expiry and local cancellation
do not revoke bucket access. This beta has no independent per-machine revocation.

Experimental revocation progress contains immutable IDs, destination metadata,
requester/time, the bounded explicitly unverified requested name or immutable ID,
and per-key pending/confirmed/failed-or-unknown outcomes. Request metadata never
authorizes deletion. Local
journals and distinct bucket operation objects never contain credential values.
Operator binding files must come from independent local/out-of-band evidence;
bucket claims do not authorize deletion. Own-key checkpoints store only opaque
references and slot IDs, keeping old local access until replacement commits.
