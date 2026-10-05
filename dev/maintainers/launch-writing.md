# Agent Archive demos and blog posts

Lead public writing with a concrete handoff between coding agents, then explain
searchable history and the storage model. Use this guide to record a demo and
shape blog posts once the demonstrated features are available in a published
release. Check commands against the version readers will install.

## Handoff demonstration

Use a disposable account and a synthetic repository containing no private
files. Record the tag, app versions, platform, and receiving checkout state.
Do not record credentials, bucket tokens, real transcripts, or private paths.

For a 45–60 second recording:

1. Start a new Claude Code task in the included demo project. Ask it to plan
   and begin a small, visible change, such as adding an archive search filter.
2. Show the task in progress. Say that you want to continue with another agent;
   do not claim a real rate limit unless one actually occurred.
3. Type `/handoff codex` using the setup-installed skill. Show Codex opening,
   reading the filtered context, inspecting current files, and continuing.
4. Show the resulting change or a passing project check. Explain that the
   conversation moved while both agents used the same checkout.
5. End on the repository link and tested release version.

Separately capture a session-search screen and a stats overview using synthetic
data, labeled as example data. Costs must be labeled estimates at list price.
The README includes a stats screenshot rendered from the synthetic shareable
report fixture, labeled as example data. A live handoff recording and session-search
screenshot remain to be captured and reviewed for sensitive content.

## Blog topics

**Continue a coding task when you switch agents.** Open with the demonstrated
Claude-to-Codex handoff. Explain what context is retained and how the receiving
agent checks current repository state. End with the smallest working install
and local-handoff flow for the published release.

**Keep coding-agent history in a bucket you own.** Explain searchable history,
multiple machines, capture scope, retention, and the storage model. Include the
best-effort redaction and no-client-side-encryption limits beside the benefit.

**Learn from your coding-agent sessions.** Show stats and the evaluation export
format. Separate observed usage from claims about quality or performance;
exports provide input to an evaluation system, not a completed benchmark.

Use the short description: “Continue coding-agent conversations across agents
and machines, with searchable history in your own S3 or R2 bucket.” Avoid
promising lossless runtime migration, encrypted archives, verified Linux Cursor
support, or performance comparisons without supporting measurements.
