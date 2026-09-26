package credentials

import (
	"errors"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
)

// CredentialProcessFailed reports whether err came from running an AWS
// profile's credential_process: the program failed, timed out, or printed
// something that is not credentials. Its message is never recorded, since
// the SDK's includes whatever the program printed, which can be
// credentials; callers say what failed in words of their own.
func CredentialProcessFailed(err error) bool {
	var process *processcreds.ProviderError
	return errors.As(err, &process)
}
