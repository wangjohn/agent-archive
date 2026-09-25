# Bucket permissions

agent-archive needs to write, read, list, and delete objects under one prefix
of one bucket. Give it credentials that can do that and nothing else. Every
key it touches is under the prefix you enter in setup (the "folder inside the
bucket"): session objects under `<prefix>/sessions/`, and a short-lived
connection-test object under `<prefix>/.setup-test/`. See
[bucket layout](../reference/bucket-layout.md).

Keep the bucket private. agent-archive never makes objects public, never
sets ACLs, and never writes outside the prefix.

## Amazon S3

Setup uses a named AWS profile (`~/.aws/config`), never environment keys.
Attach a policy like this to the profile's user or role, with your bucket
and prefix in place of `my-archive-bucket` and `agent-archive`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ArchiveObjects",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::my-archive-bucket/agent-archive/*"
    },
    {
      "Sid": "ListArchivePrefix",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-archive-bucket",
      "Condition": {"StringLike": {"s3:prefix": ["agent-archive/*"]}}
    },
    {
      "Sid": "InspectBucketPrivacyReadOnly",
      "Effect": "Allow",
      "Action": ["s3:GetBucketPublicAccessBlock", "s3:GetBucketPolicyStatus", "s3:GetBucketAcl"],
      "Resource": "arn:aws:s3:::my-archive-bucket"
    }
  ]
}
```

What each statement is for:

- **`ArchiveObjects`** covers every read, upload, and delete, all under the
  prefix.
- **`ListArchivePrefix`** lets `list` enumerate sessions and retention find
  old sources. The condition limits listing to keys under the prefix, so
  these credentials can't learn the names of anything else in the bucket.
- **Telling "missing" from "denied".** Before a session's first upload,
  and in `show`, agent-archive reads a key that may not exist yet and must
  learn that it is missing. S3 answers a read of a missing key with 404
  only when the caller may list the bucket, and otherwise with 403. A
  `GetObject` request carries no `s3:prefix`, so under this policy S3 may
  well answer 403. agent-archive handles either answer: when a read is
  refused with 403, it asks one `ListObjectsV2` with the key itself as the
  prefix (which the condition allows) and treats the key as missing only if
  that listing succeeds without it. Any other refusal stays an error.
  Setup's connection test checks this end to end: it reads its deleted test
  object back and fails, pointing here, unless that read comes back "not
  found".

> **Not yet tested on AWS.** agent-archive's handling of both answers is
> tested against simulated S3 responses, and its end-to-end runs used a
> local S3-compatible server with full access. This policy itself has not
> yet been applied to a real AWS bucket. If setup's connection test fails
> with "a missing object must read as not found", tell us in an issue. You
> can drop the `Condition` from `ListArchivePrefix` as a workaround; the
> credentials can then list every key name in the bucket (names only, not
> contents).

The third statement is optional. Without it, setup and `status` report the
bucket's privacy as `not_verified` instead of checking it; nothing else
changes. With it, agent-archive reports `verified_private` only when all four
Block Public Access flags are on (see
[privacy](privacy.md#bucket-privacy-evidence)).

A bucket that uses customer-managed KMS keys also needs `kms:Encrypt`,
`kms:Decrypt`, and `kms:GenerateDataKey` on that key. The actual error is
reported rather than asking for broad permissions up front.

## Cloudflare R2

Create an R2 API token with **Object Read & Write** permission, scoped to the
one bucket, and give setup its access key ID and secret access key (stored in
the macOS Keychain under the service `agent-archive`, never in files). R2's
S3-compatible credentials can't read the bucket's public-access settings, so
R2 privacy is always `not_verified`: check in the Cloudflare dashboard that
the bucket has no public `r2.dev` URL or custom domain.

## One key per Mac

Use separate credentials on each Mac where you can, so revoking one Mac
doesn't affect the others. Macs can share a prefix; see
[multiple Macs](../guides/multiple-macs.md).
