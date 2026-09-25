# Create a bucket

agent-archive stores sessions in a private bucket you own. Create it once;
every Mac you set up can share it. Cloudflare R2 is the quickest to set up.

## Cloudflare R2 (recommended)

1. In the [Cloudflare dashboard](https://dash.cloudflare.com), open **R2
   Object Storage** and create a bucket, for example `agent-archive`. (The
   first time, Cloudflare asks you to enable R2 and add a payment method;
   usage within the free tier is not charged.) Leave public access off.
2. On the R2 overview page, choose **Manage API tokens** → **Create API
   token**. Give it **Object Read & Write** permission, applied to **only
   that bucket**. Create it, then copy the **Access Key ID** and **Secret
   Access Key** (the secret is shown once).
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
