# Adding a machine: pairing, per-machine keys, and revocation

> **Proposed.** Not implemented or scheduled. Prepared 2026-10-01 from a read of the code at commit 4de2d07 (no builds or tests were run). File references are to that commit; where they differ from the code, the code wins.

A person who has agent-archive working on one computer should be able to set up the next one in under a minute, without a trip to the Cloudflare dashboard, without retyping settings, and without copying a secret anywhere it can be read. This plan adds:

1. **`agent-archive machines add`** on a machine that is already set up. It writes an encrypted **pairing bundle** and prints a **pairing code**.
2. **`agent-archive setup --pair`** on the new machine. It takes the bundle and the code and sets the machine up with the same settings.
3. **`agent-archive machines`** and **`agent-archive machines revoke NAME`**. They list the machines that use the bucket and cut one off.
4. **Per-machine R2 keys.** Each machine gets its own key limited to the bucket, so one machine can be revoked without touching the others.

No Cloudflare API token with account-wide permissions is ever stored.

## Contents

- [Why](#why)
- [Decisions](#decisions)
- [User experience](#user-experience)
- [Part 1: what a pairing carries](#part-1-what-a-pairing-carries)
- [Part 2: the pairing bundle and code](#part-2-the-pairing-bundle-and-code)
- [Part 3: the machine list in the bucket](#part-3-the-machine-list-in-the-bucket)
- [Part 4: per-machine R2 keys](#part-4-per-machine-r2-keys)
- [Part 5: revocation](#part-5-revocation)
- [S3](#s3)
- [Security analysis](#security-analysis)
- [Compatibility](#compatibility)
- [Phases and pull requests](#phases-and-pull-requests)
- [Testing](#testing)
- [Documentation](#documentation)
- [Open questions and things to verify](#open-questions-and-things-to-verify)
- [Alternatives considered](#alternatives-considered)

## Why

Today a second machine has two options, and both have problems.

- **Run `agent-archive setup` again.** Every answer is entered by hand, and an R2 secret has to come from somewhere: a password manager at best, a chat message to yourself at worst.
- **Use the `setup --yes` command that first setup prints.** `printAnotherMachine` (`internal/cli/setup.go:975`) builds it from `anotherMachineCommand` (`setup.go:993`). It is a good start, but:
  - It is printed once, at the end of the first setup, and no command shows it again.
  - It drops settings. The folder inside the bucket has no `setup --yes` flag, so the command says to rerun interactive setup for it (`setup.go:984`). `retention_days`, `require_skill_use`, `skill_evidence`, `no_skills` and `handoff` are not carried at all.
  - Projects are carried as paths under `~` (`homeRelative`, `setup.go:1023`). A checkout at a different path on the new machine does not match. Handoff already solved this with `repo_key` (a hash of the normalized `origin` remote, `internal/archive/repo_key.go`), but setup does not use it.
  - The R2 key is the first machine's key, copied by hand. Every machine then shares one key, so a lost laptop cannot be cut off without replacing the key on every machine.

Copying the data directory is not an option: it carries `machine_id`, and two machines that believe they own the same sessions publish over each other ([multiple machines](../../docs/guides/multiple-machines.md)).

## Decisions

| # | Question | Decision |
| --- | --- | --- |
| D1 | Command names | `machines add` on the machine already set up; `setup --pair` on the new one; `machines` and `machines revoke NAME` anywhere. The old machine is managing machines and the new one is setting itself up, so each command is named for what it does. "Invite" was rejected: it reads as referring a friend. |
| D2 | How settings and the key travel | One encrypted pairing bundle, unlocked by a pairing code that agent-archive generates. The two travel separately: the bundle by any channel, the code typed by hand. |
| D3 | Pairing code | Six words from the EFF large wordlist (7,776 words, about 77.5 bits), generated with `crypto/rand`. Never chosen by the person. |
| D4 | Where R2 keys come from | In order: (1) a Cloudflare token already available without asking creates a fresh key; (2) a spare key; (3) ask for a token, offer to share the existing key, or cancel. |
| D5 | Storing a key-creating Cloudflare token | Never. Spare keys are created while a token is present anyway, and the token is read from the environment or a password manager reference when it is needed again. |
| D6 | The machine list | One object per machine under `<prefix>/machines/` in the bucket, readable from any machine. It is informational. What grants access is the set of keys in Cloudflare, so the list is checked against Cloudflare whenever a token is available. |
| D7 | Revocation | Deletes the machine's key and its spares through the API when a token is available. Otherwise it prints the dashboard link and the keys' names. Either way it records the revocation in the bucket. |
| D8 | Shared-key fallback | Allowed, and labeled as such everywhere: in `machines add`, in `machines`, and in `machines revoke`, which explains that revoking a machine on the shared key means replacing the key on every machine. |
| D9 | Project matching | By `repo_key` first, then by path under `~`. A project with no match is listed, never guessed. |
| D10 | Cryptography | Argon2id to derive the key from the code, XChaCha20-Poly1305 to encrypt. Both come from `golang.org/x/crypto`, from the same maintainers as the `golang.org/x/sys`, `x/term` and `x/text` already in `go.mod`. See [Open questions](#open-questions-and-things-to-verify) for a standard-library-only alternative. |

## User experience

### On the machine already set up

```text
$ agent-archive machines add
Name for the new machine: work-laptop

✓ Created a key for my-bucket only: "agent-archive my-bucket work-laptop 7f3a"
  (used a spare key; 1 spare left on this machine)

Pairing bundle (copy it to work-laptop any way you like):
  aa-pair1:eJyrVkrOz0vOz0kpTVayUlAqSi0uzcksSc1VslJKzs/NTVWqBQBk3Qeg...

Pairing code (type it on work-laptop; don't send it with the bundle):
  maple-orbit-tundra-velvet-crisp-harbor

On work-laptop, run:  agent-archive setup --pair
The bundle expires in 15 minutes.
```

- `--file PATH` writes the bundle to a file (mode 0600) instead of printing it, for AirDrop or a USB stick.
- `--expires DURATION` sets the expiry, from 5 minutes to 24 hours.
- `machines add --yes --name NAME` asks nothing. The code is still printed; nothing else can be done with it.

### On the new machine

```text
$ agent-archive setup --pair
Paste the pairing bundle: aa-pair1:eJyrVkrOz0vOz0kpTVayUlAqSi0u...
Pairing code: maple-orbit-tundra-velvet-crisp-harbor

✓ Paired with mac-studio. This machine is "work-laptop".

Storage   R2 my-bucket, folder agent-archive/        from mac-studio
Apps      Claude Code, Codex                          from mac-studio
Projects  ~/src/app        matched by repository
          ~/work/api       matched by repository
          docs-site        not found on this machine (skipped)
Retention 90 days · skill evidence: metadata          from mac-studio

Save? [Y/n]
✓ Storage check passed · key saved to Keychain · hooks installed · background collector running
```

- A plain `agent-archive setup` on a machine with no `config.json` asks first: "Set up from a machine you've already set up? (pairing)" or "Set up from scratch". Most people never type `--pair`.
- The review screen is the same one setup already shows (`internal/cli/setup_review.go`), filled in from the bundle. Every value can still be changed before saving.
- `--pair-file PATH` reads the bundle from a file, and `--pair-file -` from standard input.
- The code comes from the terminal, or from `AGENT_ARCHIVE_PAIRING_CODE` under `--yes`. Setup removes that variable once it has read it, as it does the R2 variables. The code is never a flag, so it never lands in shell history or `ps`.
- Each word is checked against the wordlist as it is typed, and a near miss is suggested ("did you mean *tundra*?"). A bundle that fails its outer checksum is reported as damaged before the code is asked for, so a bad paste and a wrong code give different messages.

### Anywhere

```text
$ agent-archive machines
NAME          PLATFORM       KEY                  PAIRED       LAST SEEN
mac-studio    macOS arm64    own key              2026-08-02   2 min ago     (this machine)
work-laptop   macOS arm64    own key              2026-10-01   just now
build-box     Linux x86-64   shared (mac-studio)  2026-09-14   3 days ago
old-mbp       macOS x86-64   revoked 2026-09-30, key deleted

Checked against Cloudflare: yes (token from CLOUDFLARE_API_TOKEN)

$ agent-archive machines revoke work-laptop
This deletes work-laptop's key ("agent-archive my-bucket work-laptop 7f3a") and its 0 spare keys.
work-laptop will stop uploading at once. Its sessions stay in the bucket.
Revoke? [y/N] y
✓ Key deleted · work-laptop marked revoked
```

## Part 1: what a pairing carries

### Carried

| Setting | Source | Notes |
| --- | --- | --- |
| Provider, bucket, prefix | `storage.Provider`, `Bucket`, `Prefix` | The prefix gap in today's `setup --yes` is closed (see [Phase 1](#phases-and-pull-requests)). |
| R2 account or endpoint | `storage.R2AccountID`, `R2Endpoint` | |
| AWS profile and region | `storage.AWSProfile`, `Region` | The profile name only; see [S3](#s3). |
| Apps | `harnesses` | Only apps found on the new machine are installed; the rest are listed and offered. |
| Projects | `archive.projects[]` (included only) | `repo_key`, a display name (the folder's base name), and the path under `~`. See [project matching](#project-matching). |
| Retention | `retention_days` | |
| Capture rules | `require_skill_use`, `skill_evidence`, `no_skills` | |
| Handoff preferences | `handoff.args`, `handoff.default_to` | |
| R2 key | Part 4 | The key for this machine, or the shared key, labeled as such. |
| Pairing details | | The new machine's name, a random `pairing_id`, the sender's `machine_id` and name, creation and expiry times. |

### Never carried

`machine_id`, `host_id`, `allow_network_home`, `R2CredentialRef`, `retired_credential_refs`, `declined_harnesses`, `imported_harnesses`, `hook_files`, `installed_executable`, `background_backend`, `destination_since`, `previous_destinations`, `storage_verified_at`, `bucket_privacy`, `paused`, local state, and anything from the AWS or proxy environment (paths and endpoints differ per machine). The new machine gets its own identity, and its own checks run fresh.

`excluded` projects are not carried: an exclusion is about a path on the old machine.

### Project matching

The bundle holds each project's `repo_key` (already computed by `internal/archive/repo_key.go`, never the remote URL itself), its display name, and its path under `~`.

On the new machine, setup collects candidate checkouts from:
1. the path under `~`, if it exists;
2. the projects the apps' history mentions (`backfill.KnownProject`, which setup's capture step already uses);
3. the current directory.

It computes `repo_key` for each candidate and matches on that. A match on the path alone, for a project with no `origin`, is labeled "matched by path". A `repo_key` that matches more than one checkout (two clones of one repository) asks which one, or includes both. Under `--yes` it includes every match and says so. A project with no match is listed as "not found on this machine (skipped)" and is never guessed. Each project's `activated_at` is the new machine's save time, as for any newly included project.

## Part 2: the pairing bundle and code

### Format

```text
aa-pair1:<base64url, no padding>

header (authenticated, not encrypted)
  version        1 byte   = 1
  kdf params     Argon2id time, memory, threads (so they can be raised later)
  salt           16 bytes
  nonce          24 bytes
  pairing_id     16 bytes
  expires_at     8 bytes  (Unix seconds)
ciphertext       XChaCha20-Poly1305(gzip(payload JSON)), header as associated data
checksum         4 bytes  CRC-32 of everything before it
```

- The CRC is for typos and truncated pastes only. It is not a security check; the AEAD tag is.
- The payload is compressed before encryption. Nothing in it is chosen by an attacker, so compression leaks nothing useful.
- A bundle with ten projects and an R2 key is about 1–1.5 KB of text. That is fine to paste or to send as a file, but too long to type. The bundle is meant to be copied; the code is the part that is typed.

### Key derivation and encryption

- The code is six words from the EFF large wordlist, generated with `crypto/rand`, about 77.5 bits. The wordlist is embedded in the binary (about 60 KB).
- The key is `Argon2id(code, salt, t=3, m=64 MiB, p=4)` → 32 bytes. These are the RFC 9106 second recommended parameters; they take well under a second on a laptop. The parameters are stored in the header so a later version can raise them.
- Encryption is XChaCha20-Poly1305 with a random 24-byte nonce, and the header as associated data, so the expiry and `pairing_id` cannot be changed without breaking the tag.
- The code is normalized before derivation: lower case, words joined by single hyphens, with spaces, hyphens or both accepted as separators.

### Expiry

`expires_at` defaults to 15 minutes. `setup --pair` refuses an expired bundle. That check only binds an honest agent-archive: anyone holding the bundle and the code can decrypt it with another program. **The encryption is what protects the key, not the expiry.** Expiry limits how long an honest mistake (a bundle left in a chat thread) stays usable. Unused pairings are also surfaced afterwards (see [unused pairings](#unused-pairings)).

### What is never written

- The pairing code is printed once and kept nowhere: not in `config.json`, the setup draft (`setup-draft.json`), the setup journal, logs, or `status` output.
- The bundle is printed or written to the `--file` path only. The new machine never saves it.
- The decrypted payload exists only in memory. Its R2 key follows the path an R2 key typed into setup already follows: the setup draft holds it until the save, as it does today for a new key (`docs/getting-started/setup.md`, "If saving fails after the storage check").

## Part 3: the machine list in the bucket

### Objects

```text
<prefix>/
  machines/
    <machine_id>.json            one per machine, written only by that machine
    <machine_id>.revoked.json    written by whichever machine revoked it
```

`<machine_id>.json`:

```json
{
  "schema_version": 1,
  "machine_id": "…",
  "name": "work-laptop",
  "platform": "darwin/arm64",
  "agent_archive_version": "0.2.0",
  "paired_at": "2026-10-01T17:02:11Z",
  "paired_from": "<machine_id of mac-studio>",
  "pairing_id": "…",
  "credential": { "kind": "r2_own", "access_key_id": "…" },
  "spare_access_key_ids": ["…"],
  "last_seen": "2026-10-01T17:05:00Z"
}
```

- `credential.kind` is `r2_own`, `r2_shared` (with the `machine_id` whose key it shares), or `aws_profile`.
- `access_key_id` is the R2 token's ID, which is also the R2 Access Key ID (`cloudflare.DeriveS3Credentials`, `internal/cloudflare/r2.go:129`). It is an identifier, not a secret; revocation needs it to call `DeleteToken`.
- Each machine writes only its own record. A revocation is a separate object, so two machines never write the same key and there is no read-modify-write race.
- The name is chosen in `machines add` (or by the first machine's setup, defaulting to the short host name). Names are lower case letters, digits and hyphens, at most 40 characters. A name already used by a machine that has not been revoked is refused. `machines rename NAME` changes this machine's own record only.
- `last_seen` is refreshed by the collector at most once a day, which is one small PUT per machine per day.
- The record holds no project paths, no sessions, nothing from a transcript. The machine name and platform are the only new personal data in the bucket; the privacy documentation says so.

### When records are written

- `setup` (any form) writes or refreshes this machine's record after a successful save.
- `setup --pair` writes it with `pairing_id` and `paired_from`. That record is the "paired" marker the old machine looks for.
- The collector refreshes `last_seen` at most daily, and writes the record if it is missing. That covers machines set up before this feature, which appear with their host name the first time a new binary's collector runs.
- `uninstall` writes nothing. It prints how to run `machines revoke` for this machine from another one, since uninstalling does not delete the key.

### Reading

`machines` lists `machines/` (a few objects; no pagination concern) and joins each record with its `.revoked.json`. When a Cloudflare token is available ([Part 4](#where-a-token-comes-from)), it also lists the account's tokens and flags:
- a record whose `access_key_id` has no token (deleted in the dashboard): shown as "key gone";
- a token whose name starts with `agent-archive <bucket> ` and that no record or spare claims: shown as "unknown key", with its name, so a stale or unexpected key is visible.

Without a token, `machines` says "Not checked against Cloudflare". The list is informational: any machine that can write to the bucket can edit it, and a compromised machine could hide itself or add entries. Only Cloudflare's view is authoritative, which is why the cross-check exists.

### Unused pairings

`machines add` records each pairing locally in `<data dir>/pairings/<pairing_id>.json` (mode 0600, no secret): name, key ID, key origin, expiry. After expiry, `status` and `machines` look for a `machines/*.json` record carrying that `pairing_id`.

- **Claimed:** the local record is deleted.
- **Not claimed, own key or spare:** `status` warns: "The pairing for work-laptop expired unused. Its key is still valid; revoke it with `agent-archive machines revoke work-laptop`." If a token is available without asking, `machines` offers to delete the key at once.
- **Not claimed, shared key:** `status` warns once that the shared key was in an unused bundle. Since the bundle needed the code to open, this is a notice, not an alarm.

An unused key is not deleted automatically, because deleting a key needs a Cloudflare token and none is stored ([D5](#decisions)). This is safe because the bundle is useless without the code.

## Part 4: per-machine R2 keys

### Spare keys

A spare key is an R2 key limited to the bucket, created ahead of time and kept in the credential store until `machines add` hands it out. Spares remove the Cloudflare trip from `machines add` without storing a token that can create other tokens.

- **When spares are created:**
  - during guided R2 setup (`internal/cli/setup_r2_create.go`), right after `mintKey`, while the token is still in memory;
  - during `machines add` or `machines revoke` whenever a token is available, refilling to the target count.
- **How many:** two by default. `machines add --spares N` (0 to 5) changes the target and saves it as `spare_keys` in `config.json`. `0` turns spares off.
- **Where:** the same credential store as the machine's own key (Keychain on macOS; `credentials/<reference>.json`, 0600, on Linux), under references listed in a new `spare_credential_refs` config field.
- **Names:** fixed by Cloudflare at creation, so they name the machine holding them: `agent-archive <bucket> spare <holder-name> <4 hex>`. The machine list maps each key ID to the machine actually using it, so a spare handed to work-laptop is shown as work-laptop's key even though its Cloudflare name still says "spare". `machines revoke` prints the Cloudflare name, since that is what the dashboard shows.
- **Risk:** a spare grants exactly what the holder's own key grants: read, write and list in one bucket. Stealing the holder's credential store gains an attacker nothing they would not get from the key already there. Revoking the holder also revokes its unused spares (they are listed in its record).

### Where a token comes from

A Cloudflare token with "Account API Tokens Write" is needed to create a key, delete a key, or list tokens. agent-archive looks in this order and stops at the first:

1. `CLOUDFLARE_API_TOKEN`, as wrangler uses. Removed from the environment once read, like the R2 variables.
2. A password manager reference saved in `config.json` as `cloudflare_token_command` (for example `op read op://Private/Cloudflare/agent-archive`). Run only from an interactive command, never by the collector, so its Touch ID or unlock prompt always has a person in front of it. The command's output is the token and is never logged; its stderr is shown only as "the command failed" plus its exit status, as the S3 `credential_process` handling does.
3. A prompt, with the same deep link and permission list guided setup prints (`printR2BootstrapInstructions`, `setup_r2_create.go:286`).

The token is held in memory for the one command and never written. The rule from guided setup still holds: object credentials never go to the management API, and the token never goes to the object store.

### `machines add` for R2

1. If a token is available without asking (1 or 2 above), create a fresh key named `agent-archive <bucket> <new-name> <4 hex>`, refill spares, and use the fresh key.
2. Otherwise, if a spare is left, use it and say how many remain.
3. Otherwise, ask:
   - "Paste a Cloudflare token to create a key for work-laptop" (and refill spares);
   - "Share this machine's key" (labeled: work-laptop cannot then be revoked on its own);
   - "Cancel".

`--yes` takes 1 or 2 when it can, and otherwise fails, naming `CLOUDFLARE_API_TOKEN` and `--share-key`. Sharing never happens without the person choosing it.

Key creation reuses the guided setup code path: `SelectPermissionGroup`, `BucketResource`, `CreateToken` and `DeriveS3Credentials` in `internal/cloudflare`, and the `checkKey` wait for a new key to start working (`setup_r2_create.go:708`). The key is verified before the bundle is written. If anything after `CreateToken` fails, the key is deleted, as guided setup does (`revoke`, `setup_r2_create.go:746`).

A machine set up with a key typed in by hand (not through guided setup) has no spares and may not know the account's token permissions. It works the same way: with a token it creates keys; without one it offers sharing.

## Part 5: revocation

`agent-archive machines revoke NAME`:

1. Looks up the record and confirms (`--yes` skips the question).
2. **Own key, token available:** `DeleteToken` for the key and each unused spare in the record. **No token:** prints each key's Cloudflare name and the dashboard link (Manage account → Account API tokens).
3. Writes `machines/<machine_id>.revoked.json`: who revoked it, when, and whether the keys were deleted (`keys_deleted: true|false`). A later `machines revoke NAME` with a token finishes the deletion and updates it.
4. **Shared key:** says that the machine shares mac-studio's key, so revoking it means replacing that key everywhere, and lists which machines use it. Replacing a shared key (`machines rotate-key`) is a follow-up, not part of this plan; until then the steps are printed.
5. **This machine:** allowed, with a warning that capture here will stop. It suggests `agent-archive uninstall`.

What revocation does not do, and says so:
- It does not delete the machine's sessions. Nobody sweeps their retention any more; [the uninstall guide's lifecycle rule](../../docs/getting-started/uninstall.md#a-lifecycle-rule-as-a-backstop) is the backstop, and the guide also explains how to delete them.
- It does not un-share anything the machine already downloaded.
- It does not touch the revoked machine itself. If that machine runs again, its uploads fail with an access error, and its `status` says the key was rejected.

## S3

S3 users already have the best credential story: a named profile, usually AWS SSO with short-lived credentials. The pairing bundle carries the profile name and region, never a credential.

- `setup --pair` checks that the profile exists on the new machine. If it does not, it prints `aws configure sso --profile <name>` (or `aws configure --profile <name>`) and stops before saving. Run `setup --pair` again after signing in; the same bundle works until it expires.
- The machine list records `credential.kind: aws_profile`. `machines revoke` writes the revocation record and says that access is controlled in AWS IAM, so there is no key for agent-archive to delete.
- S3 bundles are encrypted and use a code like R2 ones. They hold no secret, but they hold the bucket name and project repository hashes, and one flow is easier to explain than two. See [Open questions](#open-questions-and-things-to-verify).

## Security analysis

| Threat | Outcome |
| --- | --- |
| Someone gets the bundle (chat history, clipboard sync, a shared folder) | Useless without the code: about 77.5 bits behind Argon2id with 64 MiB. Offline guessing is out of reach. |
| Someone gets the code | Useless without the bundle. The code is never stored. |
| Someone gets both before the new machine uses them | They get a key limited to the bucket. With an own key or spare, `machines` shows the pairing unused (or used by an unexpected machine) and `machines revoke` cuts it off without touching other machines. With the shared key, the key must be replaced everywhere. |
| The old machine is compromised | The attacker gets that machine's key and its spares, all limited to the same bucket; no key-creating token is stored. Revoking the machine deletes all of them. |
| The new machine is compromised later | `machines revoke` deletes its key; other machines are unaffected (own key). |
| A machine edits the machine list (hides itself, adds entries) | Detected when `machines` runs with a token: every agent-archive key at Cloudflare must match a record. Without a token, the list says it was not checked. |
| A bundle is modified in transit | The AEAD tag fails, and setup refuses it. The expiry and `pairing_id` are authenticated too. |
| The bundle is used after it expires by a modified client | Possible; expiry binds honest clients only. The key is still visible as an unused pairing and can be revoked. |
| A Cloudflare token leaks from `CLOUDFLARE_API_TOKEN` or the password manager | Out of scope here. agent-archive never writes it, and the docs recommend an account-owned token with only the two permissions guided setup already lists. |

## Compatibility

- **`config.json`:** four optional fields: `machine_name`, `spare_keys` (target count), `spare_credential_refs`, and the optional `cloudflare_token_command`. `config.Load` (`internal/config/config.go:201`) does not reject unknown fields, so an older binary reads the file. An older binary's `setup` rewrites the file without them, which loses the spare references (the keys stay in the credential store and at Cloudflare). The docs say to upgrade every machine before using spares; `machines` reports spare keys at Cloudflare that no machine lists, so they can be found and deleted.
- **The bucket:** `machines/` is a new folder under the prefix. Retention, purge and listing only list `sessions/` and `listing/` (`internal/purge/purge.go:87`, `internal/reader/list_index.go:131`), so older binaries never see it. Retention never deletes it. `uninstall`'s "delete the archive" instructions and the bucket layout reference gain the folder.
- **Bucket permissions:** a key limited to the prefix ([bucket permissions](../../docs/security/bucket-permissions.md)) already covers `machines/`.
- **`setup --yes`:** unchanged for existing scripts. New flags are added only (Phase 1).
- **Metadata and schemas:** no change. Session metadata already records `machine_id`, which the machine list joins on.

## Phases and pull requests

### Phase 1: close the `setup --yes` gaps (no new concepts)

Useful on its own, and the base the pairing payload is applied through.

- **P1a.** Add `setup --yes` flags for what it cannot set today: `--prefix`, `--retention-days`, `--require-skill-use` / `--no-require-skill-use`. Teach `anotherMachineCommand` to emit them, plus `--skill-evidence` and `--no-skills`, and remove the "then run agent-archive setup there" fallback (`setup.go:984`).
- **P1b.** Match `--project` by repository: accept `--project-repo REPO_KEY` alongside paths, resolved as in [project matching](#project-matching), so the printed command works when checkouts are at different paths. Refactor matching into a function the pairing path reuses.

### Phase 2: pairing with the existing key

The biggest improvement to the experience. No dependency on new Cloudflare behavior.

- **P2a.** `internal/pairing`: the bundle format, wordlist, Argon2id and XChaCha20-Poly1305, normalization and typo suggestions. Pure functions with no filesystem or network access, like `internal/archive`. Adds `golang.org/x/crypto`.
- **P2b.** Machine records: `machines/` objects in `internal/storage` terms, writing this machine's record from setup and the collector, and `agent-archive machines` (list only, no Cloudflare check yet). `machines rename`.
- **P2c.** `machines add`, using the shared key for R2 (labeled) and the profile for S3. Local pending pairings and the unused-pairing warning in `status`.
- **P2d.** `setup --pair`, `--pair-file`, `AGENT_ARCHIVE_PAIRING_CODE`, and the "Set up from a machine you've already set up?" question at the start of a fresh interactive setup.

### Phase 3: live acceptance of guided R2 creation (gate)

Run every box of "Live acceptance: guided R2 creation" in `dev/contributing/testing.md` against a real Cloudflare account, plus the new items under [things to verify](#open-questions-and-things-to-verify). Nothing in Phase 4 ships before this passes. This is the open item already recorded in [the portable handoff plan](portable-handoff-and-onboarding.md#outcome-as-built).

### Phase 4: per-machine keys and revocation

- **P4a.** Token sources (`CLOUDFLARE_API_TOKEN`, `cloudflare_token_command`, prompt) as one function both commands use.
- **P4b.** Spare keys: created in guided setup and refilled by `machines add`; `spare_keys`, `spare_credential_refs`, `--spares`.
- **P4c.** `machines add` creates a fresh key when a token is available, falls back to spares, and asks before sharing.
- **P4d.** `machines revoke`, the `.revoked.json` record, and the Cloudflare cross-check in `machines`.

### Later, not in this plan

- `machines rotate-key` to replace a shared key on every machine that uses it.
- A pairing flow where the new machine shows a public key and the old machine encrypts to it (see [Alternatives](#alternatives-considered)).

## Testing

- **Pairing format:** round trip; wrong code; each header field tampered (AEAD failure); truncated and mistyped bundles (CRC failure, distinct message); expired bundle; future `version` refused with an upgrade message; code normalization (case, spaces, hyphens); wordlist suggestion for one-letter typos; fuzzing of the decoder.
- **Secrets never written:** after `machines add` and `setup --pair` in a temporary home, a grep over every file written (config, draft, journal, logs, pairings, `status --json`) finds neither the code, the bundle, the R2 secret (outside the credential store), nor a Cloudflare token. Same shape as the existing assertion for guided setup's bootstrap token.
- **Project matching:** same path; different path same remote; no remote with same path; two clones of one remote (interactive asks, `--yes` includes both); not found; a remote with credentials in its URL (the `repo_key` normalizer already strips them; assert nothing else carries them).
- **Machine records:** two machines in one `storagetest` store; revoke from a third; a record written by a newer schema version; a missing record backfilled by the collector; `last_seen` written at most daily.
- **Cloudflare:** against `internal/cloudflare/cloudflaretest`: key creation for a named machine, spare refill, `DeleteToken` for key and spares, the cross-check flagging an unknown key and a missing one, a token without "Account API Tokens Write" (each 403 names the permission), rollback when verification fails.
- **Compatibility:** an older-format `config.json` without the new fields; a bucket with `machines/` read by the current `list`, retention and purge (they ignore it).
- **End to end:** the sandbox recipe in `dev/contributing/testing.md` with two `AGENT_ARCHIVE_HOME`s and two `HOME`s against an in-memory or MinIO store: add, pair, list, revoke.

## Documentation

- `docs/guides/multiple-machines.md`: lead with `machines add` and `setup --pair`; keep `setup --yes` for scripts.
- `docs/getting-started/setup.md`: the pairing question, `--pair`, `--pair-file`, `AGENT_ARCHIVE_PAIRING_CODE`.
- `docs/reference/cli.md` (regenerated) and `help machines`.
- `docs/reference/configuration.md`: the new fields and environment variables.
- `docs/reference/bucket-layout.md`: the `machines/` folder.
- `docs/security/privacy.md`: what a pairing bundle holds, how it is protected, that the code and bundle are never stored, that the machine list stores names and platforms, and where spare keys are kept.
- `docs/security/bucket-permissions.md`: per-machine keys, and the token permissions `machines add` and `machines revoke` need.
- `docs/getting-started/uninstall.md`: revoke the machine's key from another machine; deleting the archive includes `machines/`.
- `README.md` quickstart line about a second machine.
- `CHANGELOG.md` per phase.

## Open questions and things to verify

**Design questions**

1. **Cryptography dependency.** `golang.org/x/crypto` for Argon2id and XChaCha20-Poly1305 (recommended), or the standard library only: PBKDF2-SHA256 (`crypto/pbkdf2`) with a high iteration count and AES-256-GCM. With a 77-bit generated code either is far beyond guessing; Argon2id is the better default against GPU guessing and costs one well-maintained dependency.
2. **S3 bundles without a code.** S3 bundles hold no secret. Skipping the code for S3 would make S3 pairing one paste, at the cost of the bucket name and repository hashes travelling in the clear and two flows to explain. This plan keeps one flow.
3. **Default spare count.** Two is enough for a laptop and a desktop added later. More means more dormant keys to track.
4. **`last_seen` from the record or from sessions.** A daily PUT is cheap and works for machines that capture nothing for days. Reading the newest session per `machine_id` writes nothing but needs a listing pass.
5. **Pairing a machine that is already set up.** Treat it as a reconfiguration (keep `machine_id`, show changes on the review screen), or refuse and point at `setup`. This plan proposes reconfiguration, matching what `setup` does today.

**To verify against Cloudflare (Phase 3)**

- That tokens created without `expires_on` never expire (already listed for guided setup).
- `DELETE /accounts/{account}/tokens/{id}` for account-owned tokens (`Client.DeleteToken` exists; confirm live).
- Listing account-owned tokens (`GET /accounts/{account}/tokens`), its pagination, and the permission it needs, for the cross-check.
- Whether a token with "Account API Tokens Write" can be limited to creating only tokens for R2, or for one bucket. If it can, storing such a token becomes worth revisiting, since it would remove the spare-key machinery; until then, [D5](#decisions) stands.
- The rate of token creation Cloudflare allows, for refilling spares.

## Alternatives considered

- **Copy the data directory or `config.json`.** Carries `machine_id`; two machines would claim the same sessions. Already warned against in [multiple machines](../../docs/guides/multiple-machines.md).
- **An unencrypted, secret-free profile with the key entered by hand.** Simpler, and still possible: `setup --yes` after Phase 1 is exactly that. Pairing exists to move the key without the person handling it.
- **A passphrase the person chooses.** People choose guessable passphrases, and the bundle can be attacked offline. A generated code is the same effort to type.
- **The new machine shows a public key; the old machine encrypts to it.** Nothing could be guessed offline at all. But the person types a roughly 50-character string into the old machine (or scans a QR code) in the opposite direction from the bundle. With a 77-bit generated code, the gain does not justify the friction. Kept as a later option.
- **A relay service (as Magic Wormhole uses) so only a short code is needed.** agent-archive has no server, and adding one is out of proportion.
- **Use the bucket as the mailbox for the bundle.** The new machine cannot read the bucket until it has a key, which is what the bundle delivers. A presigned URL could bridge that, but the URL is as long as the bundle and must be kept as private as one.
- **Store a key-creating Cloudflare token in the Keychain.** The smoothest experience, but the machine then holds a credential that can create tokens across the whole Cloudflare account. Spare keys give nearly the same experience with no such credential.
- **R2 temporary credentials.** Each machine would need a stored Cloudflare token to keep renewing them, which is the problem this plan avoids. Not verified against Cloudflare's current API.
- **A Cloudflare Worker that brokers bucket access per machine.** The cleanest per-machine control, but it replaces the S3 storage path and asks every user to deploy something.
