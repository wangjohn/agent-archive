# Set up capture

Before enabling capture, read [what leaves your Mac](../security/privacy.md#what-is-uploaded): filtering and credential redaction are best effort, there is no client-side encryption, and visible user-level `SKILL.md` text may be uploaded even when you include only one project. Setup captures new sessions in explicitly included projects; [backfill](../guides/backfill.md) imports older sessions only when you choose it.

```sh
agent-archive setup
```

Setup has three steps: choose apps and projects, connect storage, then review
and start. You need a private Cloudflare R2 or Amazon S3 bucket: an existing
one ([create one](bucket.md); [bucket permissions](../security/bucket-permissions.md)
has the least access it needs), or, for S3, one setup creates for you in
your own AWS account. Run it from inside a project you want
archived, so setup can offer it, or from anywhere: setup then offers the
projects your apps have sessions in. Choose "Show setup instructions" at the
storage prompt for a link to the [bucket guide](bucket.md). Setup asks questions, so it needs a terminal: without one it
stops before asking anything and changes nothing. Its prompt sequence is not
a scripting API; to script it, use [`setup --yes`](#set-up-without-questions).

## Before the first question

Setup first checks what starting to archive will need, and prints one line
for each check: a ✓, or a ✗ with the problem and how to fix it.

- **Hook files.** The file of each app setup found, or that your saved
  settings or an unfinished setup include, must be one setup can add its
  hooks to: plain JSON, without comments, trailing commas, or duplicate
  keys. A problem names the file, line, and column, such as
  `~/.claude/settings.json:2:3`. A file that doesn't exist yet is fine;
  setup creates it.
- **Background job.** `launchctl` must say whether the collector's job is
  already loaded.
- **Keychain.** When your saved settings or an unfinished setup store in
  R2, the macOS Keychain, where the R2 key is kept, must open.

A ✗ stops setup before it asks anything: nothing is changed, and an
unfinished setup is kept. Fix what is marked, then run `agent-archive setup`
again. To leave out an app whose file you don't want to change, choose the
apps with [`setup --yes --apps`](#set-up-without-questions). `setup --yes`
makes the same checks, for the apps it would include, and checks the
Keychain when it stores in R2.

Setup captures only **new** sessions in the projects you include. To import
conversations already on this Mac, run [`agent-archive
backfill`](../guides/backfill.md) afterwards.

## 1. Apps and projects

The first time, when you run setup inside a Git project and it finds apps,
it asks one question for both: "Archive Codex and Claude Code sessions in
~/src/web-app?" Accept to continue: the apps and that project are chosen, and
retention is the default 90 days. If your apps have sessions in other
projects, setup says how many; the review at the end repeats that "Edit a
setting" adds projects, drops apps, or changes retention. Decline to answer
the questions below instead. A repository that is your home folder or holds
it (a dotfiles checkout, say), or that is one of the temporary folders
backfill skips (`/tmp`, `/private/tmp`, `/var/folders`, `$TMPDIR`) or holds
one, is too broad to archive on one Enter: setup says so in one line, offers
no project, pre-selects none below either, and asks for the projects. A
repository inside a temporary folder, such as `/tmp/x`, is an ordinary project.
While it reads your apps' history for the count of other projects, setup
shows "Looking for your other projects...".

Otherwise setup offers the apps it finds together: "Include Codex and Claude
Code?" Accept to continue, or decline to choose apps individually. If no apps are
found, it opens the individual choices immediately. On reconfiguration, it
lists the apps included and not included, then asks "Change which apps are
included? [y/N]".

Setup then lists projects to archive: the ones Claude Code and Codex
sessions on this Mac ran in, most recent first, with how many sessions each
has and when one was last used. Enter their numbers (`1 3`, or a range such
as `2-4`), `a` for all of them, or type a project path; a blank line
finishes. With no history to offer, it asks for paths.

If you run setup inside a Git project, that project heads the list, marked
✓ as already included, so a blank line archives just it. Here a number
switches a project in or out: enter `1` to leave the current project out.

Include each project explicitly; nothing outside an included project is
captured. If you finish with no project included, setup asks again.

## 2. Storage

Setup suggests S3 when your shell sets `AWS_PROFILE` or your AWS settings
already have a profile with credentials, and R2 otherwise.

- **R2:** enter the account ID, then the bucket, then credentials. Pasting
  the bucket's URL from the Cloudflare dashboard,
  `https://<account-id>.r2.cloudflarestorage.com/<bucket>`, gives both the
  account and the bucket, so the bucket isn't asked for. Any other S3 API
  endpoint (such as an EU jurisdiction's) works too. Secret input is hidden
  on a terminal and stored in the macOS Keychain.
- **S3:** choose an existing AWS profile, then the bucket. Setup offers
  the profiles in your AWS settings. The profiles come
  from `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` when your shell
  sets them. The suggested profile is `AWS_PROFILE` when your shell sets it,
  else `default`, or the only profile, when it has credentials. A profile
  whose settings name no credentials (no access keys, `credential_process`,
  SSO, login session, or role it can assume) is marked "no credentials
  configured", as is a profile the AWS SDK cannot load. Profile discovery
  only checks which settings are present: it never runs
  `credential_process`, signs in, or prints or saves a secret.

  With the profile chosen, setup lists the buckets it can see
  (`s3:ListAllMyBuckets`) and offers them by number, suggesting the saved
  bucket or else the first named `agent-archive…`; you can also type a
  name. It then reads the bucket's own region (`s3:GetBucketLocation`) and
  uses it, so a bucket in another region than the profile's works. When
  the profile may not list buckets or read the location, setup says why in
  one line and asks instead: for the bucket name, and for the region unless
  the profile names one. A typed region must look like one, such as
  `us-east-1`. These lookups use the profile's credentials only inside the
  AWS SDK; setup never prints or saves them.

  **Create a new S3 bucket.** Choose "Amazon S3: create a new private bucket
  for me" at the storage question (or type `new` where setup says the
  profile can see no buckets). After the profile, setup asks for the region
  (the profile's, unless you type another) and a name, suggesting
  `agent-archive-` and eight random characters, since bucket names are
  shared by everyone on AWS. A suggested name that turns out to be in use
  is replaced once by another random one; a name you typed is asked for
  again, and after two in-use answers in a row setup also offers to pick an
  existing bucket instead. Regions outside the standard AWS partition (China,
  GovCloud) are not supported here; pick an existing bucket for those.
  Setup then creates the bucket, turns on all four Block Public Access
  settings, and reads them back, showing "Checked: Block Public Access is
  on". It sets no lifecycle rule and no bucket policy, and it never creates
  IAM users or access keys.

  Creating a bucket needs `s3:CreateBucket` and
  `s3:PutBucketPublicAccessBlock`, which the [runtime
  policy](../security/bucket-permissions.md) deliberately does not grant, so
  use a profile that has them for this step. If the profile is refused,
  setup says which permissions are missing (an organization policy can also
  forbid creation) and goes on to pick an existing bucket. If S3 gives no
  clear answer to the request, setup says the bucket may exist and to check
  the S3 console; it deletes nothing in that case. If the bucket was created
  but Block Public Access could not be turned on, setup does not use it: it
  offers to try again, to delete the empty bucket (after you type its name),
  or to stop. When it succeeds, setup prints the runtime
  policy for the new bucket and recommends attaching it to a separate IAM
  identity and choosing that profile for storage, because the profile that
  created the bucket is what setup saves and it is usually far broader than
  archiving needs. Guided creation is interactive only; `setup --yes` still
  takes an existing bucket.

Setup checks the connection in two steps. First it lists at most one object
under `.setup-test/`, which writes nothing, so a wrong account ID, key, or
profile fails within moments with an explanation. Then it writes one
temporary synthetic object (`.setup-test/<random>.json`), reads it back, and
deletes it. That proves the credentials work; it does not prove the bucket is private. Setup then
inspects the bucket's public-access settings read-only (S3 only; see
[privacy](../security/privacy.md#bucket-privacy-evidence)). R2 keys cannot
read those settings, so for R2 the review reminds you to check that public
access is disabled in the Cloudflare dashboard.

Never pass secrets as command arguments; secret input fails rather than
falling back to visible keystrokes.

## 3. Review and start

The review lists what setup will save, then a checklist of what it found:

```
Step 3 of 3 · Ready to start

  Apps       Codex 0.121.0 · Claude Code 2.1.90
  Projects   ~/src/web-app
  Skills     metadata  User skill roots outside selected projects may be scanned
  Storage    s3://team-archive/agent-archive/  us-east-1 · profile work
  Keep for   90 days

  ✓ Storage connected       write, read, list, delete
  ✓ Bucket is private       all public access blocked
  ✓ Hook files are valid    ~/.codex/hooks.json, ~/.claude/settings.json
  ! Codex needs one step    approve the hooks with /hooks after setup
```

The summary shows the apps with their versions ("version not detected"
when setup could not read one), the projects, the storage address with its
region and profile (or R2 account), and the deletion period. The session
scope shows when it is not the default (every new session). An app you left
out is listed as skipped: setup does not offer it again, but you can add it
back under "Apps and projects" in a later `agent-archive setup`. The
deletion period is 90 days by default; older sessions are deleted from the
bucket automatically. When you reconfigure, each changed value is marked `*`
with its old value beneath it.

The Skills row controls filesystem skill evidence. Fresh setup uses
`metadata` (names and filtered hashes, no body). Choose Edit a setting to
select `none` or `body`. User-level skill folders may be scanned outside
your included projects. If an older configuration did not record a mode,
setup displays `body (kept from previous setup)` and lets you change it.
Changing the mode affects future uploads; it does not remove earlier local
or bucket copies.

In the checklist, ✓ is fine, ! needs you, and ✗ needs fixing first. While
any row is ✗, setup does not offer to start: fix what it names, then choose
"Check again", which checks storage and the hook files again.


- **Storage connected**: the storage check wrote, read, listed and deleted a
  test file.
- **Bucket is private**: the bucket blocks all public access. A public
  bucket is marked ✗ with a link on fixing it; a bucket whose settings could
  not be read is marked !. R2 keys cannot read public-access settings, so
  for R2 the review reminds you to check public access in the Cloudflare
  dashboard.
- **Hook files are valid**: setup can edit each app's hook file.
- **Codex needs one step**: Codex asks you to approve new hooks. After
  setup, run `/hooks` in Codex and approve them.

Any warnings about the change follow, such as a shorter retention period or
hooks moving to another file.

At "Start archiving?", enter the number for "Edit a setting" to adjust apps, projects, session scope, retention, storage,
the folder inside the bucket, or the AWS region. The folder inside the
bucket (the prefix) is `agent-archive/` unless you change it. Retention is
a whole number of days from 1 to 36,500; there is no "keep forever" (36,500
days is about a century). Storage changes are checked
again before starting. If the connection test fails, setup says why and
how to fix it; see [when setup's storage check
fails](../guides/troubleshooting.md#when-setups-storage-check-fails).

Run setup from the `agent-archive` you will keep using. Setup refuses a
binary in a temporary folder, including the one `go run` builds and deletes
when it exits, since every hook and the collector would run a path that is
gone; build one with `go build` or use the installer.

Setup saves non-secret choices after each completed step. If it is
interrupted, run it again to continue or start over. After a plain
`agent-archive uninstall`, setup sets up again with your saved answers as
the defaults. Reconfiguration lets you
edit capture, storage, or retention separately (its headings then drop
"Step n of 3"), or choose "Nothing, exit" to leave without changing
anything, and keeps the machine identity, existing project activation
times, paused state, and unrelated hooks.

## Set up without questions

For a second Mac, or any scripted setup, pass the answers as flags with
`--yes`. Setup then asks nothing, runs the same
[checks](#before-the-first-question) and storage check, and saves;
if an answer is missing or the check fails, it says so and changes nothing.
A bucket that allows public access stops it the same way, marked ✗ with a
link on fixing it, as it blocks the review of interactive setup; a bucket
whose public-access settings could not be read is only marked !.
Every missing or wrong answer is listed at once, one per line with the flag
that fixes it, before any check runs.

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
- For S3, `--region` defaults to the profile's region, and must look like a
  region, such as `us-east-1`.
- `--apps` defaults to the apps already set up, else those found on this Mac.
  It can add apps but never removes one: it must name every app already set
  up, and to remove an app (and its hooks) you run `agent-archive setup`.
- `--project` adds to the projects already set up; repeat it for several.
- `--skill-evidence none|metadata|body` sets the skill evidence mode. A fresh
  setup defaults to `metadata`; an older configuration without the field
  retains `body` until changed.
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
  `AWS_CA_BUNDLE`, the `AWS_ENDPOINT_URL` overrides, the proxy
  variables (a proxy URL with a password is left out), and aws-vault's and
  1Password's settings for where credentials live, when set, and your
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
[testing](../../dev/contributing/testing.md)).

## After setup

After "Configuration saved.", setup says, with one line per app, what to do next:

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

Once it has said how to check capture, setup offers to import the past
sessions of the projects you chose, when they have some on this Mac that
aren't in the archive yet:

```text
Looking for past sessions in these projects… 214 found.
Import the 214 past sessions from these projects? [Y/n]
```

Yes runs the same import as `agent-archive backfill --project DIR` for each
chosen project, with the same checks, and uploads the sessions; it ends with
the import's ID, and `agent-archive backfill undo ID` removes them again (see
[backfill](../guides/backfill.md)). If the import stops or fails, setup
stays done and prints the `agent-archive backfill` command that finishes
it. No changes
nothing; you can run `agent-archive backfill` any time. Setup skips the offer
while capture is paused, when there is nothing to import, and with `--yes`,
which asks nothing and mentions `agent-archive backfill` in its next steps
instead.

Setup's storage check and installed hooks establish configuration, not a captured session. After `agent-archive sync` or the next background pass, check that the app's Capture row says **archived, verified**, then confirm the session appears in `agent-archive list` and `agent-archive show SESSION_ID`. An overall `Ready` state alone does not establish that this app published a new session and had it read back. For a short route through the check, see [first successful capture](../README.md#first-successful-capture).

Setup's last line, after the import offer, is the command that sets up another Mac with the same
storage, apps and projects ([without questions](#set-up-without-questions)),
ready to copy:

```text
To set up another Mac with this storage, run there:
  agent-archive setup --yes --provider s3 --bucket BUCKET --aws-profile PROFILE --region us-east-1 --apps codex,claude --project ~/code/app
```

Projects in your home folder are written from `~`. For R2 the command never
carries the key: set `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and
`AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` on the other Mac first. `--yes` has
no option for the folder inside the bucket, so when you changed it, setup
adds a line saying to set it there with `agent-archive setup`.
