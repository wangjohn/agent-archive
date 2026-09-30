# Privacy

agent-archive copies your coding-agent sessions to a bucket you own. This
page says what that copy contains, what is kept out of it, what the tool
changes on your Mac, and where its protections stop. The per-version history
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
- **A repository can claim another's identity.** `handoff --latest` finds a
  session from another computer by the repository key (see
  [what is uploaded](#what-is-uploaded)), a hash of the `origin` remote in
  the directory's git configuration. Anyone who wrote a repository you clone
  controls that configuration, and anyone who can write to your prefix can
  put any key on a session, so a hostile repository can declare
  `origin = https://github.com/you/private` and make `--latest` in its
  directory choose your session of that repository, whose text a coding agent
  started there would then read. The key is a convenience for finding your
  own work, not authentication. What limits the damage: the match is
  recognized as one made by repository and not by path, and `handoff` then
  names the session on stderr (machine, project, branch, start time, first
  prompt) and, on a terminal, asks before printing or launching, default No;
  where it cannot ask (a pipe, or an agent's shell) it refuses, shows only
  the machine, project, and start time, and prints the command that selects
  the session by ID. A path match, an explicit session ID, and the picker are
  never questioned, because nothing there is chosen by a key. This does not
  protect against someone who can write to your prefix, who can plant
  sessions outright (see the previous point), and nothing stops you from
  answering yes.
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
preview of the first filtered human prompt (for `list`); it is still
filter-derived text stored in the bucket, not a separate redaction pass.

- **The filtered transcript**: your prompts; the agent's
  replies; tool calls with their arguments (Edit bodies, shell commands,
  search patterns, file paths) and tool results, each string capped at
  64 KB; working directories (which usually contain your username); Git
  branch names; model names; token counts; timestamps; the app's own session
  and message IDs; summaries the app wrote when compacting a conversation;
  and final messages hooks reported.
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
- **Metadata**: the machine ID of the Mac that captured it (random, made at
  setup), a project ID (a hash of the project's path, not the path itself),
  a repository key (below), the app and its version, the app's own session ID, capture times and the
  session's last record time, counts (including how many distinct files were
  edited, never which), models, skills used, the names of the ten most-called
  tools (MCP tool names included) with their call counts, the MCP servers
  called and how often, token counts (in total and per model), the git work
  the session's tool calls confirmed (commit SHAs, branch names, `owner/repo`,
  pull request numbers and links; never commit messages, pull request text,
  or commands), and the
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
  carry typed input into a browser or terminal (see [filter rules](#filter-rules)).
- **Cursor context payloads**: code chunks, file states, diffs, console logs,
  and other context Cursor attaches to a message.
- **Claude Code's `toolUseResult`**, which duplicates the tool result already
  kept.
- **Your credentials.** On macOS, R2 secrets are in the Keychain; S3
  credentials stay in your AWS profile. Neither appears in files, arguments,
  logs, or the bucket. See [Where credentials are kept](#where-credentials-are-kept)
  for a build with no Keychain.

Recognizable secrets inside kept text are replaced with `[REDACTED]` (see
[value-level redaction](../../dev/specs/privacy-filter.md#value-level-redaction)). Each omission and
redaction is recorded as a capture gap on the session, so a reader knows
something was removed.

## After a filter upgrade

For the current configured bucket, `agent-archive purge plan` inventories
unreferenced source objects and separately lists sessions whose current source
still uses an older filter. It writes a private, expiring plan under the local
data directory. `agent-archive purge plan --mode old-filter --before-filter 12`
narrows deletion candidates to unreferenced sources made by older filter
versions; replace `12` with the version you are upgrading to. Review the
printed bucket, prefix, keys, sizes, and digest. Pause **every** Mac uploading
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

- **Sessions whose transcript is still on the capturing Mac** are refiltered
  and republished automatically on that Mac's next passes. The copy made
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
- **Sessions whose transcript is gone** (deleted, or on a Mac that no longer
  runs agent-archive) are never refiltered: their current copy stays as the
  old filter made it until the session expires.
- **Sessions in an earlier destination** (after you changed storage) are
  not touched at all.

`show` prints a session's filter version (`filter_version` in `show --json`). To remove the older copies now,
delete from the bucket the source objects no session's metadata points at.
Pause every Mac that uploads to the bucket first, so no publication is in
flight: a new source is uploaded before the metadata that points at it.
This needs the [AWS CLI](https://aws.amazon.com/cli/) and `jq`, and
credentials that can list, read, and delete under the prefix. Keep **every**
uploading Mac paused until the plan has been applied. Run the following blocks
in the **same bash or zsh shell**; a plan expires after five minutes and can
only be applied once. If anything fails, start again with a new plan. External
writers can still race a shell recipe, so these commands cannot provide an
atomic deletion against concurrent writes.

First, list what would be deleted:

<!-- purge-recipe:list (scripts/test_purge_recipe.py runs the three blocks below) -->
```sh
agent-archive pause            # on every Mac that uploads to this bucket

bucket=my-archive-bucket       # your bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty
# export AWS_PROFILE=...       # a profile that can list, read, and delete
# For R2: export AWS_ENDPOINT_URL=https://<ACCOUNT_ID>.r2.cloudflarestorage.com

purge_dir=                         # never inherit a previous plan
purge_prepare() {
  mode=$1; selector=${2:-}
  purge_dir=                       # a failed new attempt cannot expose an old plan
  case "$prefix" in ''|*/) ;; *) echo "Prefix must be empty or end in /." >&2; return 1 ;; esac
  case "$mode" in
    unreferenced|all) ;;
    old) case "$selector" in ''|*[!0-9]*) echo "Invalid filter version." >&2; return 1 ;; esac ;;
    machine) [ -n "$selector" ] || { echo "Missing machine ID." >&2; return 1; } ;;
    *) echo "Invalid purge mode." >&2; return 1 ;;
  esac
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
  while IFS= read -r key; do
    case "$key" in "${prefix}sessions/"*/metadata.json)
      printf '%s\n' "$key" >> "$purge_dir/metas" || return 1 ;;
    esac
  done < "$purge_dir/keys"
  : > "$purge_dir/current" || return 1
  : > "$purge_dir/selected" || return 1
  n=0
  while IFS= read -r meta; do
    n=$((n + 1))
    if ! aws s3 cp "s3://$bucket/$meta" - </dev/null > "$purge_dir/meta.$n"; then
      echo "Cannot read $meta; nothing deleted." >&2; return 1
    fi
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
    cp "$purge_dir/keys" "$purge_dir/targets" || return 1
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
  : > "$purge_dir/VALID" || return 1 # written only after every check succeeds
  echo "Plan $purge_dir: $(wc -l < "$purge_dir/targets" | tr -d ' ') exact keys."
  cat "$purge_dir/targets"
}
purge_apply() {
  if [ -z "${purge_dir:-}" ] || [ ! -f "$purge_dir/VALID" ]; then
    echo "No valid plan; nothing deleted." >&2; return 1
  fi
  rm "$purge_dir/VALID" || return 1 # a plan can be attempted only once
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
  : > "$purge_dir/removed"
  cp "$purge_dir/targets" "$purge_dir/pending" || return 1
  while IFS= read -r key; do
    if ! aws s3 rm "s3://$bucket/$key" </dev/null; then
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
  done < "$purge_dir/targets"
  echo "Deleted $(wc -l < "$purge_dir/removed" | tr -d ' ') keys."
}
purge_prepare unreferenced
```

Review the printed exact keys. A failed listing or any unreadable or malformed
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

When you are done, run `agent-archive resume` on every Mac you paused.

A session deleted this way is gone from `list`; if its transcript is still
on the capturing Mac and changes again, that Mac publishes it anew. On an S3
bucket with versioning turned on, a delete only hides the object: remove
the noncurrent versions too, or add a lifecycle rule that expires them.

## What changes on your Mac

The `hooks` entry of each included app's settings file, one LaunchAgent,
local state private to your account (transcripts are read in place, not
copied), and, for R2, one Keychain item (a credentials file on a build
without a Keychain: [below](#where-credentials-are-kept)). The full list, and what uninstall
removes, is in [setup](../getting-started/setup.md#what-setup-changes-on-your-mac).

## Where credentials are kept

What follows is how the code stores an R2 secret; it is not a statement about
which platforms are supported.

- **macOS build:** the login Keychain, under the service `agent-archive`. The
  secret is never written to a file.
- **A build for another platform (Linux), which has no Keychain:** the secret
  is stored **on disk**, in a file per credential, `<data directory>/credentials/<reference>.json`,
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
  role-based, and agent-archive stores no secret of its own. Prefer it to the
  credentials file where you can.
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
  and wallet seed phrases.
- **JSON inside strings** is decoded and filtered as JSON.

Each redaction or omission is recorded as a capture gap, so a session says
what was taken out. The exact rules, with every pattern and its edge cases,
are in the [filter specification](../../dev/specs/privacy-filter.md).

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
