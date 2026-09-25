# Set up capture

```sh
agent-archive setup
```

Setup has three steps: choose apps and projects, connect storage, then review
and start. You need an existing private Cloudflare R2 or Amazon S3 bucket
([create one](bucket.md); [bucket permissions](../security/bucket-permissions.md)
has the least access it needs). Run it from inside a project you want
archived, so setup can offer it, or from anywhere: setup then offers the
projects your apps have sessions in. Type `help` at the storage prompt for
provider instructions. Setup asks questions, so it needs a terminal: without one it
stops before asking anything and changes nothing. Its prompt sequence is not
a scripting API; to script it, use [`setup --yes`](#set-up-without-questions).

Setup captures only **new** sessions in the projects you include. To import
conversations already on this Mac, run [`agent-archive
backfill`](../guides/backfill.md) afterwards.

## 1. Apps and projects

Setup offers the apps it finds together: "Include Codex and Claude Code?"
Accept to continue, or decline to choose apps individually. If no apps are
found, it opens the individual choices immediately. On reconfiguration, it
lists the apps included and not included, then asks "Change which apps are
included? [y/N]".

If setup finds the current Git project, it shows its full path and asks
"Archive sessions in this project?" Accept, then answer "Add another
project? [y/N]" to add more, or decline to choose projects yourself.

To choose projects, setup lists the ones Claude Code and Codex sessions on
this Mac ran in, most recent first. Enter their numbers (`1 3`, or a range
such as `2-4`), or type a project path; a blank line finishes. With no
history to offer, it asks for paths. Include each project explicitly; nothing
outside an included project is captured.

## 2. Storage

- **R2:** enter the account ID, then the bucket, then credentials. Pasting
  the bucket's URL from the Cloudflare dashboard,
  `https://<account-id>.r2.cloudflarestorage.com/<bucket>`, gives both the
  account and the bucket, so the bucket isn't asked for. Any other S3 API
  endpoint (such as an EU jurisdiction's) works too. Secret input is hidden
  on a terminal and stored in the macOS Keychain.
- **S3:** enter the bucket and choose an existing AWS profile. Setup offers
  the profiles in your AWS settings and uses the profile's region when it has
  one; it asks for a region only when one is missing. The profiles come
  from `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` when your shell
  sets them.

Setup checks the connection with one temporary synthetic object
(`.setup-test/<random>.json`), which it deletes again. That proves the
credentials work; it does not prove the bucket is private. Setup then
inspects the bucket's public-access settings read-only (S3 only; see
[privacy](../security/privacy.md#bucket-privacy-evidence)). R2 keys cannot
read those settings, so for R2 the review reminds you to check that public
access is disabled in the Cloudflare dashboard.

Never pass secrets as command arguments; secret input fails rather than
falling back to visible keystrokes.

## 3. Review and start

The summary shows the apps, projects, destination, and automatic deletion
period, and the session scope when it is not the default (every new
session). An app you left out is listed as skipped: setup does not offer it
again, but you can add it back under "Apps and projects" in a later
`agent-archive setup`. The deletion period is 90 days by default; older
sessions are deleted from the bucket automatically.

At "Start archiving?", enter the number for "Edit a setting" to adjust apps, projects, session scope, retention, storage,
the folder inside the bucket, or the AWS region. The folder inside the
bucket (the prefix) is `agent-archive/` unless you change it. Retention is
a whole number of days from 1 to 36,500; there is no "keep forever" (36,500
days is about a century). Storage changes are checked
again before starting. If the connection test fails, choose "Edit settings"
or "Retry" after restoring access.

Filtering is best effort, so archived text can still contain sensitive
information. Read [what leaves your Mac](../security/privacy.md) before
enabling.

Run setup from the `agent-archive` you will keep using. Setup refuses a
binary in a temporary folder, including the one `go run` builds and deletes
when it exits, since every hook and the collector would run a path that is
gone; build one with `go build` or use the installer.

Setup saves non-secret choices after each completed step. If it is
interrupted, run it again to continue or start over. After a plain
`agent-archive uninstall`, setup sets up again with your saved answers as
the defaults. Reconfiguration lets you
edit capture, storage, or retention separately, and keeps the machine
identity, existing project activation times, paused state, and unrelated
hooks.

## Set up without questions

For a second Mac, or any scripted setup, pass the answers as flags with
`--yes`. Setup then asks nothing, runs the same storage check, and saves;
if an answer is missing or the check fails, it says so and changes nothing.

```sh
export AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY=...   # or pipe it on standard input
agent-archive setup --yes --provider r2 --r2-account ACCOUNT_ID --bucket BUCKET \
  --r2-access-key-id KEY_ID --project ~/code/app --project ~/code/api \
  --apps codex,claude

agent-archive setup --yes --provider s3 --bucket BUCKET --aws-profile PROFILE \
  --project ~/code/app
```

- The R2 secret access key comes from `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY`,
  or else from standard input. It is never a flag. The access key ID can
  also come from `AGENT_ARCHIVE_R2_ACCESS_KEY_ID`. Setup removes both
  variables once read, so no command it runs inherits them. Without a new
  key, the one already stored is kept.
- If saving fails after the storage check, a new R2 key stays with the
  unfinished setup; run `agent-archive setup` to finish or discard it.
- `--r2-account` also takes the bucket URL, which names the bucket too.
- For S3, `--region` defaults to the profile's region.
- `--apps` defaults to the apps already set up, else those found on this Mac.
  It can add apps but never removes one: it must name every app already set
  up, and to remove an app (and its hooks) you run `agent-archive setup`.
- `--project` adds to the projects already set up; repeat it for several.
- Without storage flags, the storage already set up is kept, so
  `agent-archive setup --yes --project DIR` just adds a project.

[CLI reference](../reference/cli.md#agent-archive-setup) lists every flag.

## What setup changes on your Mac

- **Hooks**, where each app reads them: Claude Code's
  `$CLAUDE_CONFIG_DIR/settings.json` and Codex's `$CODEX_HOME/hooks.json`
  when those variables are set in the shell you run setup from, otherwise
  `~/.claude/settings.json` and `~/.codex/hooks.json`; Cursor's is always
  `~/.cursor/hooks.json`. The paths are recorded (`hook_files` in the
  [configuration](../reference/configuration.md)), so `status` and
  `uninstall` find them from any shell. Only the `hooks` entry of each file
  is rewritten (and Cursor's `version`, when missing): every other setting
  keeps its exact text, key order, and numbers, and uninstall restores a file
  setup only added hooks to byte for byte. A file that removing the hooks
  leaves empty (`{}`, or Cursor's `version` alone), as when setup created
  it, is deleted. A file that is a symlink, as dotfile managers such
  as stow or chezmoi create, is updated at its target and the link is kept.
  Setup refuses a file it cannot edit safely and says where the problem is:
  anything that is not plain JSON (a comment, a trailing comma, a byte-order
  mark), or a key that appears twice inside `hooks`.
- **A LaunchAgent**, `~/Library/LaunchAgents/com.agent-archive.collector.plist`,
  which runs the collector every 60 seconds. launchd gives it none of your
  shell's environment, so for S3 the plist also carries what setup's storage
  check ran with: `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`,
  `AWS_CA_BUNDLE`, the `AWS_ENDPOINT_URL` overrides and the proxy
  variables when set (a proxy URL with a password is left out), and your
  shell's `PATH` (without directories every account can write to), which a profile's `credential_process`
  (`aws-vault`, `op`, `granted`) is found through. Never your shell's AWS
  keys or tokens. If that command is not a program on your `PATH` (an alias
  or shell function), the review warns that background uploads will fail.
  Run setup again after moving your AWS files or the helper; `status` warns
  when they no longer match
  ([configuration](../reference/configuration.md#environment-variables)).
- **Local state** in `~/.local/share/agent-archive` (or `AGENT_ARCHIVE_HOME`;
  see [local state](../reference/local-state.md)), private to your account.
  It holds registrations, frozen uploads, and caches; transcripts are read in
  place, not copied, except a Cursor database copy that exists only while a
  read of it is in progress.
- **A Keychain item** (service `agent-archive`) for R2 credentials. S3
  credentials stay in your AWS profile.

`agent-archive uninstall` removes the hooks and the LaunchAgent;
`--delete-local-data` also removes the local state and the Keychain item.
Neither touches the bucket ([uninstall](uninstall.md)).

Only the account's own default installation, in `~/.local/share/agent-archive`
under the home directory macOS records for your account, uses the launchd
label `com.agent-archive.collector`. Any other data directory, whether set
with `AGENT_ARCHIVE_HOME` or moved by a sandbox that overrides `HOME`, gets a
label of its own (`com.agent-archive.collector.<hash>`), and its hooks carry
the directory in their command, since apps run hooks without your shell's
environment. That directory is also how each installation recognizes its own
hooks: setup replaces and uninstall removes only handlers whose command runs
with this installation's data directory, and leaves another installation's
alone. If an app's hook file already holds another installation's hooks,
setup names that installation before its first question and installs
nothing beside it (two installations would each capture every session): if
you choose that app, setup stops right after the apps and projects step,
before asking about storage, and keeps your answers. `status` reports it too;
uninstall that installation first, or give the new one its own `HOME` (or
`CLAUDE_CONFIG_DIR` and `CODEX_HOME`) so the apps' hook files are separate
too. Before stopping a job, setup and uninstall check that launchd loaded it
from this installation's own plist; a job loaded from any other plist is left
running and reported. These keep a second or test installation from stopping
or replacing the default one. They do not stop it from loading its own job
into your real launchd: stub `launchctl` in tests (see
[testing](../contributing/testing.md)).

## After setup

Setup ends with one line per app on what to do next:

- **Codex:** run `/hooks` and approve the archive hooks, then start a new
  session. Codex doesn't run hooks it hasn't approved, and this is the most
  common reason nothing is captured.
- **Claude Code:** nothing to approve; start a new session.
- **Cursor:** nothing to approve; start a new Agent chat.

Sessions already open are not captured: capture needs a provable fresh
start, so only a new session in an included project counts. In Codex and
Claude Code, `/clear` also starts one; in Cursor, only a new chat does. Setup finishes without waiting for it. Check
progress with:

```sh
agent-archive status
agent-archive status --json
```

What each status line means is in
[troubleshooting](../guides/troubleshooting.md#reading-status).
