# Create a bucket

agent-archive stores sessions in a private bucket you own. Create it once;
every Mac you set up can share it. You need a Cloudflare or AWS account for
the bucket; there is no agent-archive account or hosted service. Cloudflare
R2 is the quickest to set up. `agent-archive setup` can create an R2 bucket
for you (below), and links here when you choose "Show setup instructions" at
its storage question.

## Cloudflare R2 (recommended)

### Let setup create it (experimental)

`agent-archive setup` can create the bucket for you. This is **experimental**:
it has not yet been run against every kind of Cloudflare account, so the
option is hidden unless you turn it on by setting
`AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1` in the shell that runs setup. Then, at
the storage question, choose **Cloudflare R2: create a new bucket for me**. You
make one Cloudflare API token by hand, once; setup does the rest. R2 must
already be enabled on your Cloudflare account (Cloudflare may ask for a
payment method; see [current R2
pricing](https://developers.cloudflare.com/r2/pricing/)).

1. In the [Cloudflare dashboard](https://dash.cloudflare.com), open **Manage
   account → Account API tokens → Create Token** ([Cloudflare's
   steps](https://developers.cloudflare.com/fundamentals/api/get-started/account-owned-tokens/))
   and create an account token with two permissions on your account:
   **Workers R2 Storage Write** (creates the bucket) and **Account API Tokens
   Write** (creates the bucket's own key). Account members can grant only
   permissions they hold themselves, so if setup is refused here, ask an
   account administrator to create the token.
2. Run `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1 agent-archive setup`, choose
   **Cloudflare R2: create a new bucket for me**, and paste the token when
   asked (it is hidden). If your shell sets `CLOUDFLARE_API_TOKEN` and
   `CLOUDFLARE_ACCOUNT_ID`, as Cloudflare's own tools expect, setup uses them
   and doesn't ask.
3. Setup asks for a bucket name (suggesting `agent-archive-` and six random
   characters) and lets you pick a data location if you care. It then creates
   a new bucket (Cloudflare buckets have no public access by default), creates
   a second token that can read, write, and list objects in **that one bucket
   only**, checks that it works, and keeps that second key in the macOS
   Keychain like any R2 key.

The token you pasted is used during setup and then dropped: it is never
saved, in the Keychain or anywhere else, and the collector never sees it. You
can delete it in the dashboard afterwards. If setup stops partway, it names
the token it was creating, so you can revoke a leftover one; it also says
when it leaves an empty bucket behind, and if it can't store the new key it
revokes that token itself. What setup does with the token, and what it reports
about public access, is in
[privacy](../security/privacy.md#guided-r2-bucket-creation).

Setup does not set any lifecycle rule on the bucket. To have Cloudflare delete
old objects whatever happens to your Macs, add one yourself: see [a lifecycle
rule as a backstop](uninstall.md#a-lifecycle-rule-as-a-backstop).

Setup creates only a bucket you have not used: it never changes an existing
one, and it never reuses an existing name. To use a bucket you already have,
or if you prefer to do it by hand, follow the manual steps below.

### Create it by hand

1. In the [Cloudflare dashboard](https://dash.cloudflare.com), open
   **Storage & databases → R2 → Overview** ([current Cloudflare
   steps](https://developers.cloudflare.com/r2/get-started/)), enable an R2
   subscription if prompted, and create a bucket, for example
   `agent-archive`. Cloudflare may require a checkout flow. Check [current
   R2 pricing and included
   usage](https://developers.cloudflare.com/r2/pricing/) before enabling it;
   charges depend on storage class and usage. Leave public access off.
2. Under **Account Details** on the R2 overview page, choose **Manage** next
   to **API Tokens**, then create an account or user API token. Give it
   **Object Read & Write** permission, scoped to **only that bucket**. Create
   it, then copy the **Access Key ID** and **Secret Access Key** (the secret
   is shown once). See Cloudflare's current [R2 token instructions](https://developers.cloudflare.com/r2/api/tokens/)
   if the dashboard wording changes.
3. Copy your **Account ID** from the R2 overview page, or the bucket's S3 API
   URL from its **Settings** (`https://<account-id>.r2.cloudflarestorage.com/<bucket>`).

Then run `agent-archive setup`, choose `r2`, and paste the Account ID (then
the bucket name) or the bucket's URL (which names both), and the two keys.
The secret is kept in the macOS Keychain.

## Amazon S3

1. Create a bucket in the S3 console. Keep **Block all public access** on
   (the default).
2. Create an IAM user (or role) with the
   [least-privilege policy](../security/bucket-permissions.md#amazon-s3) for
   that bucket, and an access key for it.
3. Save the key as an AWS profile: `aws configure --profile agent-archive`.

Then run `agent-archive setup`, choose `s3`, and pick that profile.

## One key per Mac

Creating a separate token or access key for each Mac lets you revoke one
without touching the others. See [multiple Macs](../guides/multiple-macs.md).
