package storage

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestConfiguredCatalogNamespaceBindsActualDestination(t *testing.T) {
	cfg := credentials.Config{Provider: "s3", Bucket: "archive", Prefix: "scope", Region: "us-east-1"}
	client := NewClient(aws.Config{Region: "us-east-1"}, "https://synthetic.invalid", true, 1)
	same := NewClient(aws.Config{Region: "us-east-1"}, "https://synthetic.invalid", true, 1)
	first := configuredCatalogNamespace(cfg, client)
	if first == "" || first != configuredCatalogNamespace(cfg, same) {
		t.Fatal("same immutable destination lacks stable namespace")
	}
	for _, change := range []func(*credentials.Config){
		func(c *credentials.Config) { c.Bucket = "other" },
		func(c *credentials.Config) { c.Prefix = "other" },
	} {
		changed := cfg
		change(&changed)
		if first == configuredCatalogNamespace(changed, client) {
			t.Fatal("configured destination changed without rebinding summaries")
		}
	}
	other := NewClient(aws.Config{Region: "us-east-1"}, "https://other.invalid", true, 1)
	if first == configuredCatalogNamespace(cfg, other) {
		t.Fatal("actual SDK endpoint not bound")
	}
	unconfigured, err := NewS3Store(S3StoreOptions{Client: client, Bucket: cfg.Bucket, Prefix: cfg.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.CatalogNamespace() != "" {
		t.Fatal("arbitrary client asserted configured identity")
	}
	cfg.AWSProfile = "private-profile-reference"
	if strings.Contains(configuredCatalogNamespace(cfg, client), cfg.AWSProfile) {
		t.Fatal("credentials reference retained")
	}
}
