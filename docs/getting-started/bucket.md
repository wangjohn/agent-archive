# Create a bucket

agent-archive stores sessions in a private bucket you own. Create it once;
every Mac you set up can share it. You need a Cloudflare or AWS account for
the bucket; there is no agent-archive account or hosted service. Cloudflare
R2 is the quickest to set up. `agent-archive setup` links here when you
choose "Show setup instructions" at its storage question.

## Cloudflare R2 (recommended)

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
The secret is kept in the macOS Keychain (on a build without a Keychain, in a
private credentials file: [where credentials are kept](../security/privacy.md#where-credentials-are-kept)).

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
