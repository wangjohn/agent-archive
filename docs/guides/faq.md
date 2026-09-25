# FAQ

**Is there a hosted service or an account?**
No. agent-archive talks only to the bucket you configure. There is no
telemetry.

**What exactly is uploaded?**
Prompts, assistant text, tool inputs and outputs, commands, file paths,
working directories (which usually contain your username), Git branch names,
model names, and token counts, after a best-effort filter that drops hidden
reasoning, injected instructions, images, and unknown fields, and redacts
recognizable credentials. There is no client-side encryption. See
[privacy](../security/privacy.md).

**Can it capture sessions I had before installing it?**
Yes, with [`agent-archive backfill`](backfill.md). Setup alone captures only
new sessions.

**Does it capture every project on my Mac?**
No. Only projects you include in setup (or that a backfill adds, which it
tells you first). Sessions run from your home directory or a temporary
directory are skipped by default.

**What does it cost?**
Your provider's storage and request charges. Sources are gzip-compressed
filtered transcripts (tool results are capped at 64 KB per string), and a
session is uploaded again only when it changed; retention deletes old
snapshots and expired sessions. R2 has no egress fees; S3 charges for data
you read back with `list`, `show`, and `handoff` from outside AWS. Watch the
bucket's metrics for your own usage.

**How do I stop it for a while?**
`agent-archive pause`, then `agent-archive resume`. Pause persists across
restarts.

**How do I delete everything?**
`agent-archive uninstall --delete-local-data` removes the hooks, the
LaunchAgent, agent-archive's local files, and stored R2 credentials. It never
touches the bucket: delete `sessions/` under your prefix yourself. See
[uninstall](../getting-started/uninstall.md).

**How long are sessions kept?**
90 days by default, counted from each session's last capture; setup lets you
change it. Imported sessions count from the day of the import.

**Can I use it on several Macs?**
Yes; see [multiple Macs](multiple-macs.md).

**Which app versions work?**
See the tested versions in the [README](../../README.md#supported-platforms)
and [capture capabilities](../reference/capture-capabilities.md). A version
agent-archive hasn't seen is reported as `unverified` rather than assumed to
work.

**Is my R2 bucket verified private?**
No. R2's S3-compatible credentials can't inspect public-bucket settings, so
status reports R2 privacy as `not_verified`. For S3, setup and the collector
inspect Block Public Access, the bucket policy, and the ACL read-only.

**Why didn't a session show up?**
Check `agent-archive status` first; [troubleshooting](troubleshooting.md)
explains each line. Common causes: the project isn't included, the app's
hooks weren't approved (Codex: `/hooks`), the session resumed one that began
before setup, or collection is paused.
