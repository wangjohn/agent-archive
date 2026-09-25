# Security policy

agent-archive handles coding-agent transcripts, which can contain source
code, credentials, and personal data; it stores storage credentials in the
Keychain, edits your apps' settings files, and runs from launchd every 60
seconds. Security reports are welcome and taken seriously.

## Reporting a vulnerability

Report privately through GitHub:
**[Security → Report a vulnerability](https://github.com/wangjohn/agent-archive/security/advisories/new)**
(a private security advisory). Please don't open a public issue, discussion,
or pull request for a vulnerability.

Include what you found, the version (`agent-archive --version`, which for a
source build names the commit), how to reproduce it (a synthetic transcript
or payload is ideal — never send real session content or credentials), and
what an attacker gains.

If that link says private reporting isn't enabled (it is being turned on
for this repository), don't describe the problem anywhere public. Open an
issue with the
**[Security contact request](https://github.com/wangjohn/agent-archive/issues/new?template=security-contact.yml)**
template instead. It asks nothing about the problem, only confirms that you
have a report; the maintainer ([@wangjohn](https://github.com/wangjohn))
then opens a private draft security advisory, adds your GitHub account to
it, and you continue there.

## What to expect

agent-archive is maintained by one person, in spare time. Expect an
acknowledgement within a week and an assessment within two weeks. A
confirmed vulnerability is fixed in `main` first, then released, and
credited in the advisory and the [changelog](CHANGELOG.md) unless you
prefer otherwise. Please give us a reasonable time to fix it before
disclosing it; we'll agree on a date together.

## Supported versions

agent-archive is pre-release: only the latest `main` is supported. Once
versions are tagged, the latest release will be supported.

## In scope

- **Redaction or filter bypass:** a credential of a documented shape that
  survives the filter, content the filter should drop that reaches the bucket
  (hidden reasoning, injected instructions, binary content, unknown fields),
  or a way to make it read files outside what it should (for example through
  skill-folder symlinks). See [privacy](docs/security/privacy.md) for what
  the filter promises.
- **Credential leakage:** storage credentials reaching files, process
  arguments, logs, the bucket, or another process.
- **Hook injection:** a way for a transcript, a hook payload, a repository
  you clone, or another app's settings to make agent-archive run commands,
  write outside its data directory, or alter hooks it doesn't own.
- **Object-key traversal:** writing, reading, or deleting objects outside the
  configured prefix, or another session's objects.
- **Local state:** files created with permissions other users can read, or
  symlink tricks against the data directory, the LaunchAgent, or setup's
  rollback.
- **Supply chain:** the release workflow, install script, or dependencies.

## Not vulnerabilities

- **Best-effort redaction.** A secret with no recognizable name or shape, or
  one listed under "Known misses" in [privacy](docs/security/privacy.md), is
  a known limitation; improvements are welcome as ordinary issues or pull
  requests.
- **Anyone with bucket access can read the archive.** There is no
  client-side encryption by design; protect the bucket.
- **R2 privacy can't be verified.** R2's object credentials can't inspect
  public-bucket settings, so status reports `not_verified`.
- Problems that require an attacker who already controls your macOS account.
