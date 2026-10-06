# Bucket permissions

agent-archive needs to write, read, list, and delete objects under one prefix
of one bucket. Give it credentials that can do that and nothing else. Every
key it touches is under the prefix you enter in setup (the "folder inside the
bucket"): session objects under `<prefix>/sessions/`, and a short-lived
connection-test object under `<prefix>/.setup-test/`, and listing hints under
`<prefix>/listing/`. See
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

### Creating a bucket (setup time only)

The policy above is what runs day to day, and it deliberately cannot create
buckets or change Block Public Access. If you let `agent-archive setup`
[create the bucket](../getting-started/bucket.md#let-setup-create-it), setup
uses the profile you chose, once, with these extra permissions:

| Permission | Used for |
| --- | --- |
| `s3:CreateBucket` | Creating the bucket. |
| `s3:PutBucketPublicAccessBlock` | Turning on all four Block Public Access settings. |
| `s3:DeleteBucket` | Deleting the still-empty bucket only if you ask to after Block Public Access could not be turned on, and confirm by typing its name. |
| `s3:GetBucketPublicAccessBlock` | Reading the settings back (also in the runtime policy). |

Setup also asks STS who the profile is (`sts:GetCallerIdentity`, which needs
no permission) when S3 answers a check that a bucket name is free with
"forbidden", so that a refused key is reported as one and not as a taken
name. An organization policy (a service control policy or a permissions
boundary) can deny the actions above even to a profile whose own policy
allows them; setup's message says so when creation is refused.

Guided creation works in the standard AWS partition only; for a region in
China, GovCloud or an isolated partition, create the bucket yourself and pick
it. Two runs that choose the same name at the same moment can both be told the
name is free; the second is then refused, or, in us-east-1, shares the
bucket. That is why setup never deletes a bucket on one keypress: you type
its name first, and S3 refuses to delete a bucket that holds objects.

Setup does not set a bucket policy, ACLs, a lifecycle rule, or default
encryption (S3 already encrypts new buckets with SSE-S3), and it never
creates IAM users or access keys. It does not keep a separate credential for
creation: it asks which profile archiving should use, defaulting to the one
that created the bucket, so attach the policy above to a separate identity
and choose that profile when you can.

Setup sets no lifecycle rule on purpose. A rule expires objects one by one
by their own age, while agent-archive's retention deletes a whole session
together and never a source snapshot a live metadata file still points to,
and a rule set to today's retention would go on deleting after you raise it
in setup. If you want one as a backstop, see [a lifecycle rule as a
backstop](../getting-started/uninstall.md#a-lifecycle-rule-as-a-backstop).

## Cloudflare R2

Create an R2 API token with **Object Read & Write** permission, scoped to the
one bucket, and give setup its access key ID and secret access key (stored in
the macOS Keychain under the service `agent-archive`, never in files; on Linux,
which has no Keychain, in a private credentials file in the data directory that
is not encrypted, so prefer an S3 profile there). R2's
S3-compatible credentials can't read the bucket's public-access settings, so
R2 privacy is always `not_verified`: check in the Cloudflare dashboard that
the bucket has no public `r2.dev` URL or custom domain.

### If setup creates the bucket

For guided creation, choose **Cloudflare R2**, then **Continue**.
Setup makes this key itself. It
creates an account API token whose one policy grants **Workers R2 Storage
Bucket Item Write** on that one bucket: it can read, write, and list objects
there, and it can't manage the bucket or reach any other bucket. The token has no expiry, since an expired key would silently
stop capture; revoke it in the dashboard to end access. It is named
`agent-archive <bucket> <random>`. It is not limited to the archive's key
prefix: R2 scopes these tokens to a bucket, so use a bucket that holds only
the archive.

To do that, setup needs a bootstrap token **at setup time only**, with
**Workers R2 Storage Write** (create the bucket) and **Account API Tokens
Write** (create the bucket's key and, if that key fails its check or can't be
stored, revoke it). It is a much broader credential than the archive's key,
so it is never saved; see [privacy](privacy.md#guided-r2-bucket-creation).

These are API permission names. In the custom account token form under
**Manage account → Account API tokens**, choose **Account → Workers R2
Storage → Edit** and **Account → Account API Tokens → Edit**. The R2-specific
token form only offers R2 permission presets; it cannot grant the token
management permission. For a setup without this broader credential, create
the bucket by hand and use an **Object Read & Write** token limited to that
bucket; see [R2 setup](../getting-started/bucket.md#cloudflare-r2-recommended).

## One key per machine

Use separate credentials on each machine where you can, so revoking one machine
doesn't affect the others. Machines can share a prefix; see
[multiple machines](../guides/multiple-machines.md).
