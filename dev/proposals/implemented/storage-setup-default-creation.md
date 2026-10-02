# Storage setup with default bucket creation

Status: implemented. Builds on the R2 walkthrough and early permission checks in PR #286.

The storage wizard should ask users to choose Cloudflare R2 or Amazon S3, then guide them through creating a private archive bucket. Using an existing bucket remains an explicit secondary action before credentials are requested, on the creation summary, and during failure recovery. Existing installations and resumable drafts keep their storage by default.

This proposal changes interactive onboarding and its documentation. It does not change archive formats, configuration schemas, retention, noninteractive setup flags, or the credentials required for day-to-day archiving.

## Experience contract

The main storage menu contains exactly two numbered choices, always in this order: Cloudflare R2 and Amazon S3. Retain the current default-provider heuristic: use the saved provider when applicable; otherwise suggest S3 when AWS configuration indicates usage, and R2 otherwise. Detection changes the default, not the menu order or the selected action without confirmation.

After provider selection, creation is the default. Show a short introduction with Continue, Use an existing bucket, and Back. Keep existing-bucket access visible before the user creates a Cloudflare setup token or selects an AWS creation profile. Instructions live inside the provider flow rather than being a third numbered provider.

Use the existing suggested bucket naming scheme. On the normal path, do not ask users to accept a name and then separately decline location customization. Show the suggested name and location together on the creation summary. Customize opens those settings and returns to the same summary. No create request may occur until the user confirms the current settings.

For R2, location defaults to automatic placement. For S3, use the chosen profile's configured region when available. If none is available, ask for a region once before the summary; do not invent an automatic AWS location or silently choose a region. The summary names the AWS profile unless account identity has actually been obtained.

The initial screen of an already-configured installation remains its existing settings menu. If storage editing is requested, show the saved destination with Keep current storage as the default and Change storage as the secondary action. Keeping storage still runs the existing connection verification; it does not assert that stored settings are healthy. Capture-only and retention-only edits must not enter bucket creation.

## CLI wireframes

Fresh setup:

```text
Where should your archive live?

We'll create a private bucket in your account.

  1) Cloudflare R2
  2) Amazon S3

Choose [1]: _
```

Provider introduction:

```text
Set up Cloudflare R2

We'll create a private bucket and an archive key.
You'll provide a setup token once. It won't be saved.

[Enter] Continue
[e] Use an existing bucket
[b] Back

> _
```

AWS uses its own explanation: setup creates a bucket with Block Public Access using an AWS profile. It does not promise to create an IAM identity or a separate archive key.

R2 creation summary after account selection and permission lookup:

```text
Your archive storage

  Account       My Cloudflare account
  Bucket        agent-archive-07372f41
  Location      Automatic
  Archive key   Access to this bucket only

[Enter] Create
[c] Customize name or location
[e] Use an existing bucket
[b] Back

> _
```

R2 permission failure before creation:

```text
Cloudflare denied access to archive-key permissions.

Check this permission in Cloudflare:
  Account > Account API Tokens > Edit

Nothing has been created.

  1) Paste another token
  2) Retry after updating permissions

[e] Use an existing bucket
[b] Back

> _
```

Network failures instead explain connectivity and offer Retry or Paste another token. They must not claim a permission is missing. Show provider activation guidance only when the response supports it. Preserve the provider's useful error detail beneath the actionable explanation.

Reconfiguration:

```text
Current storage

  Cloudflare R2 / agent-archive

[Enter] Keep current storage
[c] Change storage

> _
```

## Prompt implementation

Add a small prompt helper in `internal/cli/prompt.go` that supports numbered primary options and named secondary actions. Keep `prompter.menu` unchanged for unrelated menus. The new helper takes an explicit default action and returns a key; it uses the existing heading, input, output, styling, and EOF behavior.

Render secondary actions as visible shortcuts, not numbered providers. Accept the advertised shortcuts, full action keys, primary option numbers, and Enter for the default. Reject ambiguous or invalid input without changing state. Reserve actions such as `e`, `c`, and `b` only at action prompts: secret and bucket-name inputs must not interpret those letters as navigation. Help is a named action that prints context and redisplays the same prompt; it never selects a storage path.

Action prompts run outside API-request signal handlers, preserving normal Ctrl-C behavior. The wireframes specify information and action hierarchy, not a requirement for a new terminal UI library, cursor navigation, browser automation, or automatic clipboard access.

## Storage routing

Refactor `promptStorage` in `internal/cli/setup.go` into a local routing loop with explicit destinations: provider selection, provider introduction, new bucket, existing bucket, and return to the caller. Avoid adding more recursive calls to `promptStorage` for Back or fallback.

Use a small internal outcome type for navigation, distinguishing Back to provider selection from Use existing bucket and Stop. Keep ordinary errors distinct from navigation outcomes. Provider helpers continue returning the current storage config, optional R2 key, and save-secret indicator at the outer boundary, so `advanceSetupDraft` and credential staging retain their contract.

Extract the manual R2 branch from `promptStorage` into an existing-bucket helper. Reuse account-ID/bucket-URL parsing, hidden secret entry, credential-store readability checks, and prefix defaults. Keep existing S3 profile/bucket discovery through `promptS3Location` and `promptS3ExistingBucket`.

Only preserve a saved credential reference when its destination remains compatible. Changing provider, R2 endpoint/account, or bucket must not silently carry credentials or privacy evidence from the previous destination. Back retains safe in-memory settings when useful, but it never commits a new destination.

For a fresh setup, selecting a provider enters its creation introduction. For saved storage, Keep returns the existing config for verification, while Change enters provider selection. A draft that already has staged storage proceeds through the existing resume and verification flow rather than creating another bucket. A draft stopped before credentials were staged does not acquire new recovery guarantees from this change.

## Cloudflare R2 changes

Reuse `r2Creator.connect` for account selection, permission lookup, retry, and token replacement. Complete it before generating the settings summary. Successful permission lookup is not proof that token creation will succeed; do not display a blanket permissions-verified claim.

Split `askBucket` into settings initialization and a customization editor. Initialize a suggested name once per new creation attempt; re-rendering the summary must not generate a different name. Customize can change the name, jurisdiction, and location hint using existing validators. If an automatically suggested name collides, retain the existing bounded regeneration behavior; explicitly customized collisions return to name editing and summary confirmation.

Replace the confirmation sentinel and yes/no prompt with a summary action handled outside `attempt`'s signal handler. Keep creation, key minting, activation retries, storage round-trip testing, and public-access inspection inside their existing operational code. Editing settings resets confirmation before any subsequent creation.

Add Use an existing bucket to the connection-failure and creation-recovery paths. Before transferring to manual R2 setup, discard the bootstrap client and reset account/group state. Do not pass the management token into the manual credential path or save it. Token replacement must also continue discarding the old client and must not reread a rejected environment token.

After a bucket exists, recovery distinguishes bucket creation from archive-key creation. Retry reuses the new bucket. Changing to manual setup can offer that exact bucket as the destination, but asks for valid S3 credentials; it does not treat the bootstrap token as an archive key. If the user chooses another account or bucket, report what the abandoned creation left behind. Never adopt an unrelated existing bucket merely because its name collided.

Retain key revocation when verification or staging fails, explicit orphan-token names on ambiguous responses, and empty-bucket reporting. No automatic bucket deletion is added to R2 fallback. Keep privacy reporting truthful when inspection is unavailable or public access is detected.

## Amazon S3 changes

Keep AWS profile selection before creation settings. Separate region/name preparation from mutations in `createS3Bucket` and `createNamedBucket`: current `createNamedBucket` asks for a name and creates immediately, so routing changes alone cannot implement the summary contract.

Show the chosen profile, region, suggested name, and Block Public Access plan before Create. Customize permits name, region, and creation-profile changes. Changing a profile recalculates its region default only when the user has not explicitly selected a region, and rebuilds the admin client before creation. Validate names and supported regions before confirmation.

Continue creating the bucket, enabling all four Block Public Access flags, and reading those settings back. Preserve `secureNewBucket`'s cleanup/recovery behavior when securing the bucket fails. Recovery offers retry, profile replacement where applicable, and an explicit existing-bucket action rather than silently switching to the existing-bucket questionnaire.

Reuse `offerCreatedBucket` and the run's created-bucket records so Back and retry do not create duplicate buckets. Retain the least-privilege policy advice and the post-creation archive-profile decision: AWS setup does not create a narrower identity, and the creation profile may be broader than archiving requires. Present Use the selected profile as the default and Choose another archive profile as a secondary action, with a concise explanation of the difference.

Do not add IAM creation or policy simulation as part of this UX change. AWS creation authorization cannot be promised from merely loading a profile. Handle denied creation without leaving users stuck and keep existing-bucket access available before attempting creation.

## Compatibility and persisted state

No config or setup-draft format migration is planned. Provider/mode choices and proposed new-bucket settings stay in memory until existing staging and draft-save boundaries are reached. Bootstrap tokens remain memory-only. Document that restarting after an unstaged partial creation can require manual recovery; do not imply full crash recovery.

Keep `setup --yes` and its flags unchanged. Interactive numeric choices necessarily change: 1 and 2 select providers, and the old creation-option numbers disappear. Update screen fixtures and documented examples rather than treating old numbers as stable.

Continue recognizing explicit creation aliases `r2-create` and `s3-new` as shortcuts to their respective creation routes. Add explicit `r2-existing` and `s3-existing` shortcuts for scripted interactive input and advanced users. Bare `r2` and `s3` now select the provider's default route; call out that behavioral change. Keep `help` as an unnumbered action. Avoid ambiguous prefix matching between provider and mode aliases.

When a previously saved R2 credential cannot be read, offer replacement through the existing-bucket path. Do not create a new bucket to repair missing credentials. Existing privacy evidence must retain its current destination-binding and freshness rules.

## Experimental R2 rollout

The repo's [live R2 acceptance checklist](../../contributing/testing.md#live-acceptance-guided-r2-creation) currently requires the experimental gate until its checks are complete. This proposal does not claim those checks have passed.

During implementation, both providers remain visible but R2 creation remains gated. With the gate off, the R2 introduction explicitly offers existing-bucket connection and setup instructions; it must not promise automatic creation or accept Enter as a hidden opt-in. Tests cover both gate states. With the gate on, the target R2 creation flow is available.

The final default-creation UX for R2 ships broadly only after the existing live acceptance requirements are satisfied and evidence is recorded. Remove the gate and update instructions together in a separate, evidence-backed release change. AWS creation and the two-provider menu can ship independently of that promotion.

## Implementation sequence

| Step | Work | Completion criterion |
| --- | --- | --- |
| 1 | Add action-prompt helper and provider/mode routing; extract manual R2 helper; preserve saved-storage behavior. | Two providers, explicit existing routes, Back and help work without mutations. |
| 2 | Implement R2 settings summary, customization, and existing-bucket fallback around current creator. | Token check precedes settings; Create is the only mutation entry; retry and staging invariants pass. |
| 3 | Split S3 settings from creation; add summary/customization and explicit failure recovery. | No bucket request before confirmation; security checks and created-bucket reuse remain intact. |
| 4 | Update setup introduction, provider instructions, docs, changelog, screen goldens, and compatibility examples. | Every displayed route and shortcut matches the implemented behavior. |
| 5 | Run regression/CI checks and isolated acceptance; record R2 gate results separately. | Evidence distinguishes mock-tested UX from live provider acceptance. |

Implement the cohesive interactive change in one follow-up PR, with the steps as reviewable commits. Keep gate removal separate. PR #286 remains the prerequisite for the clearer token instructions and early permission checks; if it has not merged, branch from that PR and use a stacked base until it merges, then retarget to main. Do not duplicate its diff in an independent PR.

## Verification plan

Test the prompt helper's primary numbers, secondary shortcuts, defaults, invalid/ambiguous input, help, EOF, and Ctrl-C behavior. Verify that navigation shortcuts are not intercepted at secret or name inputs.

Drive full fresh R2 and S3 setup through injected environments. Assert two providers, stable menu order, correct default selection, no default name/location questionnaires, and creation only after summary confirmation. Test Customize loops with stable suggested names and edited settings; verify the actual provider request matches the confirmed summary.

Test existing-bucket selection before credentials, from the summary, and after failures. Assert no unintended creation calls, no reused management credentials, correct config/credential/privacy reset on destination changes, and explicit leftover-resource reporting after partial creation.

Cover R2 rejected environment-token replacement, failed permission lookup before bucket questions, retries on the same created bucket, activation delay, key verification failure, key revocation, credential staging failure, public-access failure, ambiguous API responses, and interrupts. Preserve the rule that API signal handlers are inactive while waiting at prompts.

Cover S3 missing credentials, missing profile region, unsupported region, denied creation, name collisions, securing/read-back failure, profile changes, runtime-profile selection, previously created bucket reuse, and cleanup. Test that a failed creation never silently connects another bucket.

Cover reconfiguration, capture-only edits, missing saved credentials, draft resume with staged storage, and unchanged noninteractive setup flags. Update existing numbered-menu expectations, prompt tests, and setup screen goldens; review every changed screen rather than only regenerating snapshots.

Run focused setup/provider tests and doclink/reference checks first, then the repository's required race suite, vet, lint, and shared checks before merging. All automated provider tests use mocks and injected homes/credentials. Live acceptance uses the repo's disposable-environment recipe and scratch provider resources, never the developer's real archive or scheduler.

## Scope limits and review decisions

The proposed defaults are: preserve the current provider suggestion heuristic; use explicit existing-bucket shortcuts; require an S3 region only when the profile provides none; avoid automatic resource deletion on navigation; and retain the experimental R2 gate pending acceptance.

This work does not add Cloudflare OAuth, automatic browser operation, IAM provisioning, account billing setup, bucket listings with broader permissions, new durable resource journals, or background cleanup. Those are separate designs. The review should settle the proposed defaults and the visible action wording before implementation begins.

## Implementation record

Implemented in one change: shared secondary-action prompts, two provider routes, R2 and S3 creation summaries, customization, explicit fallback, and installed-storage reuse. S3 settings are held separately from creation requests. Existing draft and configuration schemas and noninteractive flags are unchanged. Live provider acceptance remains outstanding; the R2 experimental gate remains enabled only by its environment flag.
