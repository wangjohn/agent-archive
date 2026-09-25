# Privacy

agent-archive copies your coding-agent sessions to a bucket you own. This
page says what that copy contains, what is kept out of it, what the tool
changes on your Mac, and where its protections stop. The per-version history
of the filter is in the [filter changelog](filter-changelog.md).

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
  carry typed input into a browser or terminal (see the deny list below).
- **Cursor context payloads**: code chunks, file states, diffs, console logs,
  and other context Cursor attaches to a message.
- **Claude Code's `toolUseResult`**, which duplicates the tool result already
  kept.
- **Your credentials.** R2 secrets are in the macOS Keychain; S3 credentials
  stay in your AWS profile. Neither appears in files, arguments, logs, or
  the bucket.

Recognizable secrets inside kept text are replaced with `[REDACTED]` (see
[value-level redaction](#value-level-redaction)). Each omission and
redaction is recorded as a capture gap on the session, so a reader knows
something was removed.

## After a filter upgrade

A new filter version (see the [filter changelog](filter-changelog.md))
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
stripping, value-level redaction, and the 64 KB cap. Tool-argument subtrees
(`input`, `arguments`, `tool_input`) keep every argument name, because the
names belong to the tool, subject to the deny list below.

### Tool-argument deny list

Retaining tool arguments wholesale has two exceptions, applied at every depth
of a tool-argument subtree. In both cases the argument's key name is recorded
in a `sensitive_or_hidden_field_omitted` gap (`omitted tool argument keys: …`,
sorted, capped at 64, names only) and the value is never retained.

- **Typed or submitted text.** An argument named `text`, `value`, `values`,
  `keys`, or `chars` is dropped when the tool's name (`name` or `tool_name`
  beside the argument subtree), split into words at `_`, `-`, `.`, and
  camelCase, holds one of `type`, `typing`, `fill`, `form`, `input`,
  `keyboard`, `key`, `keys`, `press`, `select`, `paste`, `autofill`,
  `credential`, `verification`, `otp`, `password`, `computer`, or `stdin`
  (filter 11; filter 10 matched a few whole names and missed every
  `fill_form` tool). So Playwright's `browser_type`, `browser_fill_form`, and
  `browser_select_option`, chrome-devtools' `fill` and `fill_form`, a
  computer-use tool, and Codex's `write_stdin` all lose what they typed.
  `Edit`, `Write`, and other tools keep their `text`/`value` arguments.
- **A value labelled as a secret** (filter 11). For every tool, `text`,
  `value`, `values`, `keys`, or `chars` is dropped when a key beside it
  (`name`, `label`, `element`, `placeholder`, `field`, `selector`, `id`,
  `key`, `type`, `autocomplete`, `aria_label`, `for`) says the value is a
  password, passcode, PIN, one-time code, secret, token, card number, CVV,
  security or verification code, SSN, or IBAN: a form field
  `{"name": "Password", "value": …}`, an input `{"type": "password", …}`,
  or an environment entry `{"name": "DB_PASSWORD", "value": …}`; or when
  such a label (not `type` or `autocomplete`) names a credential as an
  argument key would (below), as a HAR header or cookie does
  (`{"name": "Authorization", "value": …}`). A two-string array whose first
  string names a credential (`["X-Api-Key", …]`, a header list) loses its
  second.
- **Credential-named arguments.** For every tool, an argument whose key
  names a credential is dropped. The key is split into words the same way,
  and it names a credential when its words hold a term of the credential
  vocabulary, the one list the value-level patterns below use too
  (`api key`, `access key`, `private key`, `secret(s)`, `password(s)`,
  `passwd`, `pass`, `passphrase`, `token`, `authorization`, `bearer`,
  `credential(s)`, `creds`, `cookie(s)`, `auth`, `auth key`, `account key`,
  `shared access key`, `subscription key`, `access key id`, `seed phrase`,
  `recovery phrase`, and a few more; `pwd` only after
  another word). So `X-Api-Key`, `passwd`, `pass`, `private_key`, `auth`,
  `secrets`, and `password_confirmation` are dropped, while budgets such as
  `max_tokens` (a plural is another word) are kept. An
  argument object whose members were all dropped is pruned with them.

A string that holds a JSON object or array (Codex's
`function_call.arguments` and `custom_tool_call.input`, a tool result an MCP
server returned as JSON text, a Cursor result stored as a string) is decoded
and filtered as the object it is (filter 11), with the rules of an argument
subtree: every key name kept, the deny list applied, every string redacted,
binary blocks dropped, and JSON strings inside it decoded in turn. A string
the filter changes is stored re-encoded (compact, keys sorted); one it
leaves alone keeps its bytes. Filter 10 saw such strings only as text, so a
Codex `browser_type` call kept the password it typed. An array of strings
that is a command line (`["mysql", "-pS3cret"]`) has its secret values
redacted by position, like the command-line shapes below.

### Value-level redaction

Every retained string, at every depth, passes these patterns. A match is
replaced with `[REDACTED]` and a `sensitive_content_redacted` gap is recorded.

- Credential assignments (filter 9). The name ends in a term of the
  credential vocabulary (filter 11: `api_key`, `access_key`, `private_key`,
  `private_key_id`, `encryption_key`, `signing_key`, `master_key`,
  `account_key`, `shared_access_key`, `shared_access_signature`, `auth_key`,
  `subscription_key`, `secret(s)`, `password(s)`, `passwd`, `passphrase`,
  `pgpass`, `token`, `authorization`, `bearer`, `credential(s)`,
  `cookie(s)`, `dockerconfigjson`; filter 12: `access_key_id`,
  `seed_phrase`, `recovery_phrase`), with `_`, `-`, `.`, a space, or nothing
  between the parts of a two-word term (`API Key: …`), in any case,
  with anything glued on before it; or in `pass` or `creds` alone or after a separator
  or a camelCase boundary (`DB_PASS`, `redis.pass`, `dbPass`, but not
  `bypass`); or in `pwd` or npm's `_auth` after a separator (`MYSQL_PWD`,
  `DB_PWD`, `:_auth`), since a bare `PWD` or `OLDPWD` is the shell's working
  directory. The trigger may be followed by `key` or `access_key` (`Secret
  Key` too), then `base`, then a number, then `value`. So `DB_PASSWORD`, `AWS_SECRET_ACCESS_KEY`,
  `OPENAI_API_KEY`, `SECRET_KEY_BASE`, `DB_PASSWORD_1`, `accessToken`,
  `PGPASSWORD`, `spring.datasource.password`, and `x-api-key` all match. The
  name may be quoted (`"…"`, `'…'`, or escaped inside a string, `\"…\"`);
  the separator is `=`, `:`, `:=`, `=>`, a full-width colon or equals sign,
  or a tab; a
  `--name value` (or `-name value`) command-line flag counts too. A quoted
  value is taken up to its closing quote (plus anything glued on after it,
  as a shell reads it; filter 10). An unquoted value runs to the end of its
  line (filter 11; filter 10 stopped at whitespace, `,`, `;`, or a quote, so
  `password: correct horse battery staple` kept three words), stopping
  earlier only at the closing quote of a string the assignment sits in, at
  whitespace followed by shell punctuation, a comment, a flag, or another
  `name=` assignment, at `, ` or `; `, and in a URL query at the next `&`.
  A Cookie header's value runs to the end of the line. A value that is only
  `true`, `false`, `null`, `nil`, `none`, or `undefined` is not a secret and
  is kept. The value is replaced and the rest is kept:
  `DB_PASSWORD=[REDACTED]`, `"password": "[REDACTED]"`. An HTTP scheme
  before the value stays: `Authorization: Bearer [REDACTED]`. A single token
  in brackets or braces is a value too (`password=[hunter2]`,
  `token={abc123}`), with anything glued on after it
  (`password=[REDACTED]realsecret` loses `realsecret`, and a glued bracket
  group goes whole). A value may begin with `=` unless whitespace follows
  it; the extra `=` signs stay (`PASSWORD==[REDACTED]`).
- YAML values on the lines below their key (filter 11): a block scalar
  (`password: |` or `>-` and its indented lines) and an indented scalar on
  the next line (`password:` then `  S3cret`) become one `[REDACTED]` line.
  A key whose indented lines are a mapping or a list is a structure, and its
  members are checked on their own.
- Structures and entries (filter 11): every string value inside an object
  or array whose name is a credential (`"secret": {"value": …}`,
  `"passwords": [ … ]`, in JSON, Python, JavaScript, or JSON escaped inside
  a string, across lines) is redacted, keeping keys, numbers, and the value
  of a descriptive key (`type`, `kind`, `name`, `description`, `provider`,
  `scheme`, `alg`, `format`, `encoding`); and the sibling `value:` of a YAML
  entry whose `name:` (or `key:`) is a credential (`- name: DB_PASSWORD`
  then `value: …`, a Kubernetes `env` list), or the `"value"` after a
  credential `"name"` on one line (`{"name": "Authorization", "value":
  …}`).
- Files shown with line numbers (filter 11): the Claude Code Read tool,
  `cat -n`, and `grep -n` number each line. The YAML, entry, and private
  key rules read a line after its number.
- Command lines whose secret is a flag's value only for particular programs
  (filter 11): `curl -u user:secret` (and `--user`, `-U`, `--proxy-user`;
  the user name stays), `mysql -psecret`, `sshpass -p`, `docker|podman|helm
  … login -p`, `az login -p`, `redis-cli -a`, `mongosh -p`, `sqlcmd -P`,
  `ldapsearch -w`, `zip -P`, `7z -p`, `ssh-keygen -N`, `keytool
  -storepass`, `openssl -k` and `pass:secret`, macOS `security … -p`/`-w`,
  `gh secret set --body`, `aws configure set NAME value`, `npm|yarn|git
  config set NAME value`, fish `set -gx NAME value`, `setenv NAME value`,
  and PowerShell `ConvertTo-SecureString "…"`. `ssh -p 22` and `mkdir -p`
  are left alone.
- `.netrc` passwords (`machine … login … password secret`, or a `password
  secret` line), XML elements and attributes (`<password>…</password>`,
  `<add key="ApiKey" value="…"/>`), and URL query parameters whose names are
  not credential words (`?key=`, `&sig=`, `X-Amz-Signature=`) (filter 11).
- AWS access key IDs (`AKIA…`, and `ASIA…` for temporary STS credentials),
  Anthropic/OpenAI style `sk-` keys, and, since filter 11, the prefixed
  tokens of Stripe (`sk_live_`, `rk_live_`, `whsec_`), GitLab (`glpat-` and
  its siblings), Google (`AIza…`, `GOCSPX-`, `ya29.`), Hugging Face (`hf_`),
  npm (`npm_`), PyPI, SendGrid, Shopify, DigitalOcean, HashiCorp Vault,
  Databricks, Linear, Grafana, Postman, New Relic, Sentry, Atlassian, Figma,
  Doppler, age, Mailgun, Telegram bots, and Azure AD client secrets, and
  since filter 12 Groq (`gsk_`) and xAI (`xai-`) keys; Slack,
  Discord, and Teams incoming-webhook URLs (the path after the host); and a
  `Bearer` token outside a header.
- PEM and PGP private key blocks: `-----BEGIN … PRIVATE KEY-----` (or
  `PRIVATE KEY BLOCK-----`) through the next `-----END … -----` when
  everything between is key body (each line possibly numbered, marked as a
  diff, quote, or comment, or quoted as a string in source code), or holds a
  base64 run of 48 characters or more (and no other BEGIN line); when the END
  line is missing (a key cut off by a truncated record), through the base64
  lines that follow the BEGIN line, a line with a 48-character base64 run
  counting as one (filter 11; filter 10 took everything to the end of the
  string, so source code that names the BEGIN line lost the rest of the
  file). When the BEGIN line is missing (a key read in two parts, or a tool
  result in chunks), the base64 lines right above an END line that starts
  its line are redacted with it. Certificates and public keys are not
  redacted.
- A PEM block encoded in base64 whole (kubeconfig `client-key-data`), from
  its `LS0tLS1CRUdJTi` prefix, certificates included (filter 12).
- Docker `config.json` `"auth"` and `"identitytoken"` values shown as text
  (filter 12).
- `.pgpass` lines (`host:port:database:user:password`): the password, to
  the end of its line, when the port is 5432, 6432, or `*` (a file read by
  the Claude Code Read tool), and with any port in a string that mentions
  `.pgpass` or `PGPASSFILE` (`cat ~/.pgpass`, a heredoc into it) (filter
  12).
- Wallet seed phrases: 12 to 24 words of three to eight letters after a
  name holding `mnemonic` (`MNEMONIC="…"`, `--mnemonic "…"`, a
  comma-separated list or an array), or exactly 12, 15, 18, 21, or 24 after
  one holding `seed` (`SEED=…`, `wallet seed:`) (filter 12).
  `SEED_PHRASE=` and `Secret Recovery Phrase:` are credential names above.
- JWTs: three base64url segments, the first beginning with `eyJ`.
- URL userinfo: in `scheme://user:pass@host` (or `scheme://user@host`) the
  userinfo is replaced and the scheme and host are kept. The userinfo ends
  at the last `@` of the authority, and a password holding `/`, `?`, or `#`
  runs to the `@` a host follows (filter 11), unless the part after the
  colon is a port.
- GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`) and
  Slack tokens (`xox[abposre]-`, `xapp-`).

Known false positives. The assignment pattern cannot tell a credential from
code or a path: `token = parse(x)`, `nextToken := lexer.Next()`,
`credentials := loadCreds()`, and `password: required` have their
right-hand side redacted; so do a saved directory in a `*_PWD` variable
(`ORIG_PWD=$(pwd)`), a path to a credentials file
(`GOOGLE_APPLICATION_CREDENTIALS=/path/key.json`), a one-item list
(`password: [required]`, taken for a bracketed value), and the word after a
flag in prose (`pass --token flag`). Every scalar under a YAML key named
like a credential is redacted, so references to secrets lose their names
too: Docker Compose `secrets:` then `- db_password`, or `file:
./db_password.txt` under it. Between a private key's BEGIN and END lines,
any text with a 48-character base64 run is taken as the key, so code
between two constants that name the armor lines is redacted when it holds
a long hash. This is accepted rather
than narrowed, because the cost of a missed credential is higher than the
cost of a redacted identifier in an archived transcript; a reader sees the
`sensitive_content_redacted` gap and can consult the original source if it
still exists. Names with anything after the trigger word other than the
suffix above (`tokens`, `max_tokens`, `token_count`, `secretary`,
`password_policy`, `TOKEN_URL`, `SECRET_NAME`, `--password-stdin`) do not
match, nor do `PWD`, `OLDPWD`, `auth`, comparisons (`token == nil`), or a
name with no value.

Known misses.

- An unquoted value runs to the end of its line (filter 11), so an
  environment prefix before a command loses the command too:
  `TOKEN=abc npm test` becomes `TOKEN=[REDACTED]`. This errs toward the
  secret. A space-separated `-p value` with no program the filter knows
  (`tool -p secret`) is not recognized.
- In JSON escaped more than once inside a string (`\\\"password\\\":…`),
  the value ends at the first escaped quote of any depth, so the tail of a
  value after an escaped quote inside it (`\\\"ab\\\\\\\"cd\\\"`: `cd`) is
  kept. JSON escaped once (`\"password\":\"ab\\\"cd\"`) is handled.
- A value that is an object or array holding whitespace, a comma, a colon,
  or a quote (`"credentials": {"type": …}`, `password: [required, min 8]`)
  is a structure and is not replaced as text. Its string values are
  redacted (above), but an unquoted value inside it (a YAML flow mapping
  `{user: me, pass: x}` aside from its own credential names, a number such
  as a PIN) is caught only by its own name or shape. A YAML mapping or
  sequence written over the lines below a credential key (`secrets:` then
  `github: …`) is redacted value by value (filter 11).
- For the same reason, an unquoted value that begins with `[` or `{` but is
  not a single closed token is kept whole: `password=[Kx9!q2Lm`,
  `password={secret`, `password=[admin:hunter2]`, `password=[a b]realsecret`.
  About 2% of random passwords drawn from the full symbol set begin with a
  bracket or brace.
- A project skill's `SKILL.md` that is a hard link to another file cannot be
  told apart from a real file. A cloned repository cannot create one (git
  does not store hard links); it needs local write access to the project.
- A name that does not end in a trigger word (`DATABASE_URL`, `DSN`,
  `CONNECTION_STRING`) is not redacted by this pattern; its value is
  redacted only if it has a recognizable shape (`AKIA…`/`ASIA…`, URL
  userinfo).
- Access key IDs are redacted (`*_ACCESS_KEY_ID`, filter 12), although an
  ID is not a secret on its own: AWS's were already redacted by their
  `AKIA…` shape and dropped as tool arguments, and an R2 access key ID is
  the ID of the Cloudflare API token behind it. Other key IDs
  (`KMS_KEY_ID`, `private_key_id` aside) are kept.
- **A `.pgpass` line on an unusual port, shown without the file's name
  in the same string, keeps its password.** The Claude Code Read tool
  shows a file's lines with its path in the call, a separate record, so
  `db:15432:app:alice:secret` read that way is kept: five colon-separated
  fields alone are too common a shape (compiler diagnostics, log lines) to
  redact. Lines on port 5432, 6432, or `*` are redacted wherever they
  appear.
- A seed phrase is recognized only after a `mnemonic` or seed name, on one
  line: a numbered list (`1. legal 2. winner …`), a YAML block sequence
  (`mnemonic:` then `- legal` lines), a phrase in another language, or one
  with words outside three to eight letters is kept.
- A credential in prose (`the password is hunter2`), a Markdown table row
  (`| password | hunter2 |`), a name spelled with full-width letters or a
  zero-width space, or a leetspeak name (`p4ssword=`) is not recognized;
  nor is a secret piped into a command (`echo … | docker login
  --password-stdin`, `sudo -S`), a form label in another language (`Mot de
  passe`). A URL-encoded assignment (`password%3D…`, `token%3A…`) is
  recognized (filter 11), but not one encoded twice (`password%253D…`) or
  in another encoding (base64, HTML entities). A YAML value on the lines
  below its key is recognized (filter 11); when its first line reads as a
  mapping entry (`correct horse: battery`), each value is redacted and the
  keys are kept.
- The middle part of a private key split into three or more strings (no
  BEGIN or END line in it) has no marker and is kept, as is a key in a
  format with no armor lines (PuTTY `.ppk`, a JWK's `"d"`, a raw base64
  file) unless its name or shape is recognized.
- Text glued after a closing quote is taken with the value (filter 10) only
  up to a closing `]`, `}`, or `)`, which usually closes the structure
  around the value (`{"password":"abc"}`, `f(PASSWORD="abc")`) and must stay.
  So in the rare `PASSWORD="abc")realsecret`, `realsecret` is kept.
- A Cursor plain-text transcript has no structure beyond its role headers,
  and its exact format is not pinned by a fixture in this repository. Since
  filter 11 a header is a role and a colon at column 0, written in the case
  of the transcript's first header: lower case (`user:`) or capitalized
  (`User:`), never both in one transcript. A line in the other case is
  content, so in a lower-case transcript capitalized prose such as
  `Analysis: …` hides nothing, and in a capitalized one a YAML `user:` line
  in tool output starts no Person turn. When the transcript separates its
  sections with blank lines, a `user:`, `assistant:`, or `tool:` line that
  does not follow a blank line is content too. What remains: a column-0
  line in the transcript's own header case still starts a section in a
  transcript without blank lines between sections, and after a blank line
  in any transcript; and a column-0 hidden role in that case (`system:`,
  `thinking:`, …) always hides what follows, so nothing that might be a
  hidden section is kept (the `hidden_instruction_omitted` gap counts the
  sections and lines hidden). A transcript whose first line is not a header
  in either case (`USER:`, or text before the first header) is refused as
  a capture gap. Cursor's JSONL transcripts and database chats are not
  affected.

Redaction is best effort in both directions: a legitimate value that looks like
a credential is redacted, and a tool argument that happens to contain one of
the instruction tags above loses that span. Both are recorded as gaps.

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
