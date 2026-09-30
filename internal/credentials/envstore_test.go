package credentials

import (
	"context"
	"errors"
	"testing"
)

func envOf(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// The variable names are the ones setup --yes reads (internal/cli pins that
// they agree), and the ones cloud mode's own names must not be confused with.
func TestEnvStoreNames(t *testing.T) {
	if EnvR2AccessKeyID != "AGENT_ARCHIVE_R2_ACCESS_KEY_ID" || EnvR2SecretAccessKey != "AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY" {
		t.Fatalf("names = %s, %s", EnvR2AccessKeyID, EnvR2SecretAccessKey)
	}
}

func TestEnvStoreLoadsTheKeyFromTheEnvironment(t *testing.T) {
	store := NewEnvStore(envOf(map[string]string{EnvR2AccessKeyID: " " + testKeyID + "\n", EnvR2SecretAccessKey: testSecret + " "}))
	// The environment names one key, so any reference reads it.
	for _, ref := range []string{testRef, "anything", "env"} {
		got, err := store.Load(context.Background(), ref)
		if err != nil || got != testCredentials {
			t.Errorf("Load(%q) = %+v, %v", ref, got, err)
		}
	}
	if _, err := store.Load(context.Background(), ""); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("Load with no reference = %v", err)
	}
}

func TestEnvStoreMissingVariablesAreAMissingCredential(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"none":         {},
		"id only":      {EnvR2AccessKeyID: testKeyID},
		"secret only":  {EnvR2SecretAccessKey: testSecret},
		"empty":        {EnvR2AccessKeyID: "", EnvR2SecretAccessKey: ""},
		"blank secret": {EnvR2AccessKeyID: testKeyID, EnvR2SecretAccessKey: "  \n"},
		"cloud names":  {"AGENT_ARCHIVE_ACCESS_KEY_ID": testKeyID, "AGENT_ARCHIVE_SECRET_ACCESS_KEY": testSecret},
		"aws names":    {"AWS_ACCESS_KEY_ID": testKeyID, "AWS_SECRET_ACCESS_KEY": testSecret},
	} {
		_, err := NewEnvStore(envOf(values)).Load(context.Background(), testRef)
		if !errors.Is(err, ErrEnvironmentCredentialMissing) || !errors.Is(err, ErrMissingCredential) {
			t.Errorf("%s: Load = %v, want a missing credential", name, err)
		}
		assertNoSecret(t, name, err)
	}
}

func TestEnvStoreIsReadOnly(t *testing.T) {
	calls := 0
	store := NewEnvStore(func(name string) (string, bool) {
		calls++
		return testKeyID, true
	})
	ctx := context.Background()
	for name, err := range map[string]error{
		"Save":   store.Save(ctx, testRef, testCredentials),
		"Delete": store.Delete(ctx, testRef),
	} {
		if !errors.Is(err, ErrEnvironmentReadOnly) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s = %v, want the read-only error", name, err)
		}
		assertNoSecret(t, name, err)
	}
	if calls != 0 {
		t.Errorf("Save and Delete read the environment %d times", calls)
	}
	if got := ErrEnvironmentReadOnly.Error(); got != "environment credentials are read-only" {
		t.Errorf("read-only message = %q", got)
	}
}

func TestEnvStoreDefaultsToTheProcessEnvironment(t *testing.T) {
	t.Setenv(EnvR2AccessKeyID, testKeyID)
	t.Setenv(EnvR2SecretAccessKey, testSecret)
	got, err := NewEnvStore(nil).Load(context.Background(), testRef)
	if err != nil || got != testCredentials {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}
