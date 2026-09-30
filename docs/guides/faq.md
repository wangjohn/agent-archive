# FAQ

**Is there a hosted service or an account?**
There is no agent-archive account, hosted service, or telemetry. You do need a Cloudflare or AWS account to provide the private bucket and credentials that agent-archive uses for storage. See [create a bucket](../getting-started/bucket.md).

**What exactly is uploaded?**
Prompts, assistant text, tool inputs and outputs, commands, file paths,
working directories (which usually contain your username), Git branch names,
model names, and token counts, after a best-effort filter that drops hidden
reasoning, injected instructions, images, and unknown fields, and redacts
recognizable credentials. Each session also carries the filtered text of
the skills installed in the app's user-level skill folders (such as
`~/.claude/skills`), whatever the project. There is no client-side
encryption. See [privacy](../security/privacy.md#what-is-uploaded).

**Can it capture sessions I had before installing it?**
Yes, with [`agent-archive backfill`](backfill.md). Setup alone captures only
new sessions in explicitly included projects. If setup offers to import past sessions, you can decline and run `backfill` later.

**Does it capture every project on my Mac?**
No. Only projects you include in setup (or that a backfill adds, which it
tells you first). Sessions run from your home directory or a temporary
directory are skipped by default.

**What does it cost?**
Your provider's storage, request, retrieval, and transfer charges, as
applicable. Check the current [Cloudflare R2 pricing](https://developers.cloudflare.com/r2/pricing/)
or [Amazon S3 pricing](https://aws.amazon.com/s3/pricing/) for the storage
class and region you choose. Sources are gzip-compressed filtered
transcripts (tool results are capped at 64 KB per string), and a session is
uploaded again only when it changed; retention deletes old snapshots and
expired sessions. There is no typical figure to quote, since it depends on
how long and how many your sessions are. To measure yours, `aws s3 ls
s3://<bucket>/<prefix>sessions/ --recursive --summarize` prints the object
count and total size (divide by the sessions `agent-archive list` counts),
and your provider's billing page shows request counts.

**How do I stop it for a while?**
`agent-archive pause`, then `agent-archive resume`. Pause persists across
restarts.

**How do I delete everything?**
`agent-archive uninstall --delete-local-data` removes the hooks, the
LaunchAgent, agent-archive's local files, and stored R2 credentials; then
remove the binary. It never touches the bucket, and no Mac deletes another
Mac's sessions: see
[delete the archive](../getting-started/uninstall.md#delete-the-archive-in-the-bucket)
for the commands, and for a lifecycle rule that cleans up after a Mac you
retire.

**How long are sessions kept?**
90 days by default, counted from each session's last capture; setup lets you
set 1 to 36,500 days. There is no "keep forever". Imported sessions count
from the day of the import. After a
[filter upgrade](../security/privacy.md#after-a-filter-upgrade), older
copies can stay until the session expires.

**Can I use it on several Macs?**
Yes; see [multiple Macs](multiple-macs.md). If you move to a new Mac with
Migration Assistant, or restore from Time Machine, read
[that section](multiple-macs.md#migration-assistant-and-time-machine)
first: the copy keeps the old Mac's identity.

**Can my coding agent see my archive?**
Only through the `agent-archive` skill setup installs (`agent-archive setup --no-skills` turns it off), and then only what `handoff`, `list`, and `show` print: the same filtered content you would see running them, cut to a size bound. The agent treats it as data, not instructions, and Claude Code asks before it first uses the skill and before it runs the commands. See [agent skills](agent-skills.md) and [privacy](../security/privacy.md#what-an-agent-can-read-through-the-skill).

**Does it run on Linux or Windows?**
No. Capture runs on macOS only: it relies on launchd and the Keychain.
Linux builds exist so the tests run in CI; they cannot capture.

**What happens when an app changes its transcript format?**
Nothing is guessed. A field the filter doesn't know is dropped and named in
a capture gap, a record type it doesn't know is dropped whole, and a source
the parser can't read gets parser status `failed` rather than wrong counts.
`status` reports a new app version as `unverified` until one of its sessions
is published and read back. If sessions from a new version look wrong, file
a capture gap issue; a fix comes as a new adapter or filter version, and
sessions whose transcripts are still on your Mac are refiltered then.

**Which app versions work?**
See the [tested app versions](../reference/capture-capabilities.md#tested-app-versions). A version
agent-archive hasn't seen is reported as `unverified` rather than assumed to
work.

**Is my R2 bucket verified private?**
No. R2's S3-compatible credentials can't inspect public-bucket settings, so
status reports R2 privacy as `not_verified`. For S3, setup and the collector
inspect Block Public Access, the bucket policy, and the ACL read-only.

**Why didn't a session show up?**
Check `agent-archive status` first. A connected bucket or `Ready` state does not by itself prove this app captured and read back a session. Follow the app-specific steps for [Claude Code](troubleshooting.md#no-claude-code-session), [Codex](troubleshooting.md#no-codex-session), or [Cursor](troubleshooting.md#no-cursor-session).
