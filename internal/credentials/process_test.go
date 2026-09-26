package credentials

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
)

func TestCredentialProcessFailedFindsTheSDKsErrorWhereverItIsWrapped(t *testing.T) {
	t.Parallel()
	process := &processcreds.ProviderError{Err: errors.New("error in credential_process: exit status 1")}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"direct":     {process, true},
		"wrapped":    {fmt.Errorf("setup test upload: operation error S3: PutObject, failed to retrieve credentials: %w", process), true},
		"other":      {errors.New("AccessDenied"), false},
		"nil":        {nil, false},
		"same words": {errors.New("process provider error: error in credential_process"), false},
	} {
		if got := CredentialProcessFailed(tc.err); got != tc.want {
			t.Errorf("%s: CredentialProcessFailed = %v, want %v", name, got, tc.want)
		}
	}
}
