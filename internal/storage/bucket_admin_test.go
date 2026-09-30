package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

// bucketCall is one request the fake S3 server received.
type bucketCall struct {
	method string
	path   string
	query  string
	body   string
}

// bucketRecorder answers each request with reply and records every request, so a
// test can say what was and was not sent.
type bucketRecorder struct {
	mu    sync.Mutex
	calls []bucketCall
	reply func(bucketCall, http.ResponseWriter)
}

func (f *bucketRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := bucketCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	f.reply(call, w)
}

func (f *bucketRecorder) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, call := range f.calls {
		out = append(out, call.method+" "+call.path+"?"+call.query)
	}
	return out
}

func replyS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>synthetic</Message><RequestId>r</RequestId></Error>`, code)
}

// adminIn is a BucketAdmin whose client is in region and talks to server.
func adminIn(server *httptest.Server, region string) *BucketAdmin {
	cfg := aws.Config{Region: region, Credentials: awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")}
	return NewBucketAdmin(NewClient(cfg, server.URL, true, 1))
}

func TestCreateBucketInUSEast1SendsNoLocationConstraintAfterCheckingTheNameIsFree(t *testing.T) {
	t.Parallel()
	fake := &bucketRecorder{reply: func(c bucketCall, w http.ResponseWriter) {
		if c.method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := adminIn(server, "us-east-1").CreateBucket(context.Background(), "agent-archive-1", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"HEAD /agent-archive-1/?", "PUT /agent-archive-1/?"}
	if got := fake.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests %q, want %q", got, want)
	}
	if body := fake.calls[1].body; strings.Contains(body, "LocationConstraint") {
		t.Fatalf("us-east-1 must not send a location constraint (S3 refuses one), got %q", body)
	}
}

func TestCreateBucketOutsideUSEast1SendsTheLocationConstraint(t *testing.T) {
	t.Parallel()
	fake := &bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }}
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := adminIn(server, "eu-west-2").CreateBucket(context.Background(), "agent-archive-1", "eu-west-2"); err != nil {
		t.Fatal(err)
	}
	if got := fake.methods(); len(got) != 1 || got[0] != "PUT /agent-archive-1/?" {
		t.Fatalf("requests %q, want one PUT (other regions report an owned bucket themselves)", got)
	}
	if body := fake.calls[0].body; !strings.Contains(body, "<LocationConstraint>eu-west-2</LocationConstraint>") {
		t.Fatalf("body %q lacks the location constraint", body)
	}
}

func TestCreateBucketNameInUseIsTakenAndChangesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		region string
		reply  func(bucketCall, http.ResponseWriter)
		puts   int
	}{
		{"us-east-1 name owned by this account", "us-east-1", func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }, 0},
		{"us-east-1 name owned by another account", "us-east-1", func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }, 0},
		{"us-east-1 name in another region", "us-east-1", func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusMovedPermanently) }, 0},
		{"BucketAlreadyExists", "eu-west-1", func(_ bucketCall, w http.ResponseWriter) { replyS3Error(w, http.StatusConflict, "BucketAlreadyExists") }, 1},
		{"BucketAlreadyOwnedByYou", "eu-west-1", func(_ bucketCall, w http.ResponseWriter) {
			replyS3Error(w, http.StatusConflict, "BucketAlreadyOwnedByYou")
		}, 1},
		{"OperationAborted", "eu-west-1", func(_ bucketCall, w http.ResponseWriter) { replyS3Error(w, http.StatusConflict, "OperationAborted") }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &bucketRecorder{reply: tc.reply}
			server := httptest.NewServer(fake)
			defer server.Close()
			err := adminIn(server, tc.region).CreateBucket(context.Background(), "taken-name", tc.region)
			if !errors.Is(err, ErrBucketNameTaken) {
				t.Fatalf("err = %v, want ErrBucketNameTaken", err)
			}
			puts := 0
			for _, call := range fake.calls {
				if call.method == http.MethodPut {
					puts++
				}
			}
			if puts != tc.puts {
				t.Fatalf("%d PUTs, want %d: %q", puts, tc.puts, fake.methods())
			}
		})
	}
}

func TestCreateBucketDeniedIsAccessDeniedNotTaken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(&bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { replyS3Error(w, http.StatusForbidden, "AccessDenied") }})
	defer server.Close()
	err := adminIn(server, "eu-west-1").CreateBucket(context.Background(), "agent-archive-1", "eu-west-1")
	if err == nil || errors.Is(err, ErrBucketNameTaken) || Diagnose(err).Cause != CauseAccessDenied {
		t.Fatalf("err = %v, want access denied that is not a taken name", err)
	}
}

func TestBlockPublicAccessTurnsAllFourSettingsOn(t *testing.T) {
	t.Parallel()
	fake := &bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }}
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := adminIn(server, "us-east-1").BlockPublicAccess(context.Background(), "agent-archive-1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0].method != http.MethodPut || fake.calls[0].path != "/agent-archive-1/" || !strings.Contains(fake.calls[0].query, "publicAccessBlock") {
		t.Fatalf("requests %q, want one PUT ?publicAccessBlock", fake.methods())
	}
	for _, flag := range []string{"BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"} {
		if !strings.Contains(fake.calls[0].body, "<"+flag+">true</"+flag+">") {
			t.Errorf("body %q does not set %s to true", fake.calls[0].body, flag)
		}
	}
}

func TestBlockPublicAccessDeniedIsAccessDenied(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(&bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { replyS3Error(w, http.StatusForbidden, "AccessDenied") }})
	defer server.Close()
	err := adminIn(server, "us-east-1").BlockPublicAccess(context.Background(), "agent-archive-1")
	if Diagnose(err).Cause != CauseAccessDenied {
		t.Fatalf("err = %v, want access denied", err)
	}
}

func TestDeleteBucketDeletesOnlyThatBucket(t *testing.T) {
	t.Parallel()
	fake := &bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }}
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := adminIn(server, "us-east-1").DeleteBucket(context.Background(), "agent-archive-1"); err != nil {
		t.Fatal(err)
	}
	if got := fake.methods(); len(got) != 1 || got[0] != "DELETE /agent-archive-1/?" {
		t.Fatalf("requests %q, want one DELETE of the bucket", got)
	}
}

func TestBucketAdminInspectPrivacyReadsBlockPublicAccessBack(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(&bucketRecorder{reply: func(c bucketCall, w http.ResponseWriter) {
		if c.method != http.MethodGet || !strings.Contains(c.query, "publicAccessBlock") {
			t.Errorf("unexpected request %s %s?%s", c.method, c.path, c.query)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	}})
	defer server.Close()
	report := adminIn(server, "us-east-1").InspectPrivacy(context.Background(), "agent-archive-1")
	if report.State != "verified_private" {
		t.Fatalf("report %+v, want verified_private", report)
	}
}
