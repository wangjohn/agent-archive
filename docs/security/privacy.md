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
- **Skill evidence**: for every hook-captured session, the name, SHA-256
  (of the whole file with its credentials redacted, from filter 12; of the
  original bytes before), and filtered body (the first 16 KB) of each `SKILL.md` installed in that
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
  carry typed input into a browser or terminal (see [filter rules](#filter-rules)).
- **Cursor context payloads**: code chunks, file states, diffs, console logs,
  and other context Cursor attaches to a message.
- **Claude Code's `toolUseResult`**, which duplicates the tool result already
  kept.
- **Your credentials.** R2 secrets are in the macOS Keychain; S3 credentials
  stay in your AWS profile. Neither appears in files, arguments, logs, or
  the bucket.

Recognizable secrets inside kept text are replaced with `[REDACTED]` (see
[value-level redaction](../../dev/specs/privacy-filter.md#value-level-redaction)). Each omission and
redaction is recorded as a capture gap on the session, so a reader knows
something was removed.

## After a filter upgrade

A new filter version (see the [filter changelog](../../dev/specs/privacy-filter-changelog.md))
applies to what is uploaded from then on. It does not clean what is already
in the bucket:

- **Sessions whose transcript is still on the capturing Mac** are refiltered
  and republished automatically on that Mac's next passes. The copy made
  with the old filter becomes the session's previous source, and retention
  always keeps a session's immediate predecessor, so for a session that
  doesn't change again the old copy stays until the whole session expires
  (90 days after its last capture by default).
- **Sessions whose transcript is still there but no longer holds everything
  that was archived** (the app truncated or compacted it) are not replaced
  by the shorter transcript: the archived copy itself is filtered again
  with the new filter and republished, and the session is recorded as
  having a rewritten transcript. The old copy then stays as the previous
  source, as above. If the app later restores the full transcript and the
  new filter writes some of its records differently from the old one (a
  new redaction label, say), the collector cannot tell the restored file
  from a rewritten one: the archived copy is kept, and records added to
  that transcript afterwards are not archived.
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

The `hooks` entry of each included app's settings file, one LaunchAgent,
local state private to your account (transcripts are read in place, not
copied), and, for R2, one Keychain item. The full list, and what uninstall
removes, is in [setup](../getting-started/setup.md#what-setup-changes-on-your-mac).

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
