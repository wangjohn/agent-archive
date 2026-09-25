package capture

import "github.com/wangjohn/agent-archive/internal/credentials"

// credentialsTestConfig is a syntactically valid storage destination for
// tests that never actually touch storage (they use OpenStore above, or
// test hook logic that writes only local files).
func credentialsTestConfig() credentials.Config {
	return credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}
}
