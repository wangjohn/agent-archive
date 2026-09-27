package cli

import (
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// storageCheckError is what setup returns when the storage check failed and
// setup stops. The diagnosis was already printed, so its message only says
// what became of the answers; the check's own error stays reachable through
// Unwrap.
type storageCheckError struct {
	err error
	// outcome is what became of the answers, such as "your answers are
	// kept".
	outcome string
}

func (e *storageCheckError) Error() string {
	return "the storage check failed; " + e.outcome
}

func (e *storageCheckError) Unwrap() error { return e.err }

// storageDiagnosis is storage.Diagnose for the configured storage, with the
// <profile> in its fix filled in.
//
// R2 has no regions to choose from ("auto" is the only one), so a failure
// the provider blames on the region, such as AuthorizationHeaderMalformed,
// comes from the account in the endpoint there, and is described so.
//
// The check reads back the test file it just wrote, so ErrChecksumMismatch
// and ErrNotFound here mean that file, not an object in general. That
// wording lives here, not in storage.Diagnose, whose other callers' reads
// share those sentinels.
func storageDiagnosis(cfg credentials.Config, err error) storage.Diagnosis {
	d := storage.Diagnose(err)
	if d.Cause == storage.CauseOther && errors.Is(err, storage.ErrChecksumMismatch) {
		// The provider answered both calls, so this is neither access nor
		// the network: something between them changed the object.
		d = storage.Diagnosis{
			Cause:       storage.CauseOther,
			Explanation: "The test file read back from the bucket didn't match what was written.",
			Fix:         "Check for a proxy, or a bucket rule, that changes stored objects, then try again.",
		}
	}
	if d.Cause == storage.CauseOther && errors.Is(err, storage.ErrNotFound) {
		d = storage.Diagnosis{
			Cause:       storage.CauseOther,
			Explanation: "The test file wasn't in the bucket when it was read back, just after it was written.",
			Fix:         "Check the bucket's settings (a lifecycle rule or replication that removes new objects), then try again.",
		}
	}
	if cfg.Provider == credentials.ProviderR2 && d.Cause == storage.CauseWrongRegion {
		d = storage.Diagnosis{
			Cause:       d.Cause,
			Explanation: "Cloudflare R2 didn't accept the request for this account.",
			Fix:         "Check the R2 account ID, or paste the bucket's URL from the R2 dashboard, then try again.",
		}
	}
	profile := cfg.AWSProfile
	if profile == "" {
		profile = "default"
	}
	d.Fix = strings.ReplaceAll(d.Fix, "<profile>", profile)
	return d
}

// storageFailureHeadline is the diagnosis's first line: what failed, in a
// few words.
func storageFailureHeadline(cfg credentials.Config, d storage.Diagnosis) string {
	r2 := cfg.Provider == credentials.ProviderR2
	if d.Cause == storage.CauseNoCredentials && r2 {
		return "Can't sign in to Cloudflare R2."
	}
	if d.Cause == storage.CauseNoCredentials {
		return "Can't sign in to AWS."
	}
	if d.Cause == storage.CauseAccessDenied {
		return "Access denied."
	}
	if d.Cause == storage.CauseNoSuchBucket {
		return "Bucket " + cfg.Bucket + " wasn't found."
	}
	if d.Cause == storage.CauseWrongRegion && r2 {
		return "Cloudflare R2 refused the request."
	}
	if d.Cause == storage.CauseWrongRegion {
		return "The bucket is in another region."
	}
	if d.Cause == storage.CauseNetwork {
		return "No connection to the storage provider."
	}
	return "The storage check failed."
}

// storageFixLabel names the failure menu's first choice, after the answer
// the diagnosis points at. It asks for the region alone, or else the storage
// questions again, with the answers given as defaults.
func storageFixLabel(cfg credentials.Config, d storage.Diagnosis) string {
	r2 := cfg.Provider == credentials.ProviderR2
	if d.Cause == storage.CauseNoCredentials && r2 {
		return "Enter the R2 access key again"
	}
	if d.Cause == storage.CauseNoCredentials {
		return "Pick another profile"
	}
	if d.Cause == storage.CauseNoSuchBucket {
		return "Change the bucket name"
	}
	if d.Cause == storage.CauseWrongRegion && !r2 {
		return "Change the region"
	}
	return "Change storage settings"
}

// printStorageFailure prints why the storage check failed, once: a
// headline after a red ✗, the cause, and the fix, with any command in it in the command
// color. The check's own error, which can run to several hundred characters
// of SDK text, is printed only when verbose; otherwise a dim line says how
// to see it (details is that command). Even with verbose, an error from a
// profile's credential_process is withheld: the SDK's message quotes
// whatever that program printed, which can be credentials.
func printStorageFailure(p *prompter, cfg credentials.Config, err error, verbose bool, details string) storage.Diagnosis {
	d := storageDiagnosis(cfg, err)
	s := p.style
	terminal.Println(p.out, "")
	terminal.Println(p.out, s.hang("  "+s.failMark()+" ", storageFailureHeadline(cfg, d)))
	terminal.Println(p.out, s.hang("    ", d.Explanation))
	terminal.Println(p.out, "")
	terminal.Println(p.out, s.hang("    Fix: ", paintCommands(s, d.Fix)))
	if verbose && credentials.CredentialProcessFailed(err) {
		terminal.Println(p.out, s.dim(s.hang("    Details: ", "not shown, since the credential_process error quotes the program's output, which can hold credentials; run the command yourself to see it")))
	} else if verbose {
		terminal.Println(p.out, s.dim(s.hang("    Details: ", err.Error())))
	} else {
		terminal.Println(p.out, "    "+s.dim("Details:")+" "+s.cmd(details))
	}
	terminal.Println(p.out, "")
	return d
}

// paintCommands drops the backquotes around each command in a diagnosis
// sentence and paints the command instead.
func paintCommands(s textStyle, sentence string) string {
	parts := strings.Split(sentence, "`")
	for i := 1; i < len(parts); i += 2 {
		parts[i] = s.cmd(parts[i])
	}
	return strings.Join(parts, "")
}

// lookUpBucketRegion asks S3 for the region of cfg's bucket, with cfg's
// profile, and says which it is. It returns "" when the lookup fails.
func lookUpBucketRegion(p *prompter, env Env, cfg credentials.Config) string {
	finder, err := env.awsBuckets(cfg.AWSProfile, cfg.Region)
	if err != nil {
		return ""
	}
	region, err := bucketRegion(finder, cfg.Bucket)
	if err != nil {
		return ""
	}
	terminal.Printf(p.out, "Bucket %s is in %s.\n", cfg.Bucket, region)
	return region
}

// reopenStorage returns draft set to ask the storage questions again after a
// check that failed as d says, with its answers as the defaults. The answer
// d blames is one those questions would keep without asking, so it is
// replaced, cleared or marked to be asked: after a wrong-region failure the
// S3 region becomes the bucket's own when S3 named it, and the region that
// failed is recorded as FailedRegion, to be asked for again rather than
// checked again; after R2 refused the access key, the key is cleared. The other answers are all asked again.
func reopenStorage(draft setupDraft, d storage.Diagnosis) setupDraft {
	draft.Step = 1
	s := &draft.Config.Storage
	if d.Cause == storage.CauseNoCredentials && s.Provider == credentials.ProviderR2 {
		// A key staged by this draft stays in StagedRefs for cleanup.
		s.R2CredentialRef = ""
	}
	if d.Cause == storage.CauseWrongRegion && s.Provider == credentials.ProviderS3 {
		draft.FailedRegion = s.Region
		if d.Region != "" {
			s.Region = d.Region
		}
	}
	return draft
}

// askFailedRegion asks for the bucket region again after a storage check
// failed for region, saying why first.
func askFailedRegion(p *prompter, region string) (string, error) {
	terminal.Printf(p.out, "The last storage check failed with region %s.\n", region)
	return promptRegion(p, "Bucket region", region)
}
