package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestListBucketNamesFollowsEveryPage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>agent-archive-alex</Name></Bucket><Bucket><Name>photos</Name></Bucket></Buckets><ContinuationToken>next</ContinuationToken></ListAllMyBucketsResult>`)
			return
		}
		_, _ = fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>team-archive</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
	}))
	defer server.Close()
	names, err := ListBucketNames(context.Background(), diagnoseClient(server, nil))
	if want := []string{"agent-archive-alex", "photos", "team-archive"}; err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("names=%q err=%v, want %q", names, err, want)
	}
}

func TestListBucketNamesDeniedIsAccessDenied(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(s3Reply{status: http.StatusForbidden, code: "AccessDenied"})
	defer server.Close()
	_, err := ListBucketNames(context.Background(), diagnoseClient(server, nil))
	if Diagnose(err).Cause != CauseAccessDenied {
		t.Fatalf("err=%v, want access denied", err)
	}
}

func TestBucketRegionNamesTheRegion(t *testing.T) {
	t.Parallel()
	for location, want := range map[string]string{
		"":          "us-east-1",
		"EU":        "eu-west-1",
		"eu-west-2": "eu-west-2",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Trim(r.URL.Path, "/") != "team-archive" || !r.URL.Query().Has("location") {
				t.Errorf("unexpected request %s", r.URL)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">%s</LocationConstraint>`, location)
		}))
		region, err := BucketRegion(context.Background(), diagnoseClient(server, nil), "team-archive")
		server.Close()
		if err != nil || region != want {
			t.Errorf("location %q: region=%q err=%v, want %q", location, region, err, want)
		}
	}
}

func TestBucketRegionDeniedIsAccessDenied(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(s3Reply{status: http.StatusForbidden, code: "AccessDenied"})
	defer server.Close()
	region, err := BucketRegion(context.Background(), diagnoseClient(server, nil), "team-archive")
	if region != "" || Diagnose(err).Cause != CauseAccessDenied {
		t.Fatalf("region=%q err=%v, want access denied", region, err)
	}
}
