package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func responseError(status int, cause error) error {
	return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      cause,
	}}
}

func TestIsNotFoundTrustsOnlyTypedEvidence(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"NoSuchKey", &types.NoSuchKey{}, true},
		{"wrapped NoSuchKey", fmt.Errorf("operation GetObject: %w", &types.NoSuchKey{}), true},
		{"NotFound", &types.NotFound{}, true},
		{"HTTP 404", responseError(http.StatusNotFound, errors.New("api error")), true},
		{"bare smithy 404", &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusNotFound}}}, true},
		{"403 whose text says not found", responseError(http.StatusForbidden, errors.New("access denied: key not found in policy")), false},
		{"plain text not found", errors.New("dial tcp: lookup bucket.example: no such host (not found)"), false},
		{"plain text 404", errors.New("https response error StatusCode: 404, NoSuchKey"), false},
		{"404 NoSuchBucket", responseError(http.StatusNotFound, &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "The specified bucket does not exist"}), false},
		{"typed NoSuchBucket", &types.NoSuchBucket{}, false},
		{"500", responseError(http.StatusInternalServerError, errors.New("internal")), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isNotFound(c.err); got != c.want {
			t.Errorf("%s: isNotFound = %v, want %v", c.name, got, c.want)
		}
	}
}

// Through the real SDK: a 403 whose body says "not found" and a missing bucket
// are reported as the failures they are, while a missing key is ErrNotFound.
func TestS3GetReportsOnlyAMissingObjectAsNotFound(t *testing.T) {
	respond := func(status int, code, message string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(status)
			if _, err := fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message); err != nil {
				t.Error(err)
			}
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		missing bool
	}{
		{"missing key", respond(http.StatusNotFound, "NoSuchKey", "The specified key does not exist."), true},
		{"forbidden with not-found text", respond(http.StatusForbidden, "AccessDenied", "Resource not found or access denied"), false},
		{"missing bucket", respond(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist"), false},
	}
	for _, c := range cases {
		server := httptest.NewServer(c.handler)
		awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
		store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive", MaxGetBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Get(context.Background(), "sessions/x/metadata.json")
		server.Close()
		if got := errors.Is(err, ErrNotFound); got != c.missing {
			t.Errorf("%s: Get error = %v; reported as not found = %v, want %v", c.name, err, got, c.missing)
		}
		if err == nil {
			t.Errorf("%s: Get succeeded against an error response", c.name)
		}
	}
}
