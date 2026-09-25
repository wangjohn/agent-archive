package capture

import "github.com/wangjohn/agent-archive/internal/credentials"

// credentialsTestConfig is a syntactically valid storage destination for
// the configurations these tests save. The hook never touches storage; it
// writes only local files.
func credentialsTestConfig() credentials.Config {
	return credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}
}
