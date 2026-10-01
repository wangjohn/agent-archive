package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestPairingAgentPresenceRefusesBeforeEveryEffect(t *testing.T) {
	t.Parallel()
	commands := [][]string{{"machines", "add", "--yes", "--name", "laptop", "--share-key"}, {"setup", "--pair", "--yes"}, {"setup", "-pair-file=-", "--yes"}, {"setup", "-pair=true"}}
	for _, marker := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CURSOR_AGENT"} {
		for _, command := range commands {
			env := Env{LookupEnv: func(key string) (string, bool) { return "", key == marker || key == envNonInteractive }, Home: func() (string, error) { t.Fatal("home read"); return "", nil }, UserHomeDir: func() (string, error) { t.Fatal("user home read"); return "", nil }, Credentials: func() (credentials.CredentialStore, error) { t.Fatal("credentials opened"); return nil, nil }, OpenStore: func(config.Config) (storage.ObjectStore, error) { t.Fatal("storage opened"); return nil, nil }, UnsetEnv: func(string) error { t.Fatal("environment changed"); return nil }}
			var out, errOut bytes.Buffer
			if code := Run(command, strings.NewReader("secret"), &out, &errOut, env); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "separate terminal") {
				t.Fatalf("%v %s: %d %s %s", command, marker, code, &out, &errOut)
			}
		}
	}
}

func TestPairingRejectsOversizeAndChecksumBeforeCodeAndHome(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"aa-pair1:bad", strings.Repeat("x", pairing.MaxBundle+3)} {
		env := Env{LookupEnv: noEnv, PairingCode: func() (string, error) { t.Fatal("asked for code"); return "", nil }, Home: func() (string, error) { t.Fatal("read home"); return "", nil }}
		var out bytes.Buffer
		if Run([]string{"setup", "--pair-file", "-"}, strings.NewReader(input), &out, &out, env) != 1 {
			t.Fatal("accepted malformed input")
		}
	}
}

func pairingSourceFixture(t *testing.T) (Env, string, *storagetest.MemoryStore) {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	store := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	env.PairingRepoRoot = func(context.Context, string) (string, error) { return "", errors.New("no repository") }
	project := filepath.Join(userHome, "src", "app")
	must(t, os.MkdirAll(project, 0700))
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "studio", Storage: credentials.Config{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Harnesses: []string{"codex"}, RetentionDays: 90, SkillEvidence: config.SkillEvidenceMetadata, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true}}}}
	must(t, config.Save(home, cfg))
	return env, home, store
}

func TestPairingSourcePrecheckAndDeliveryIntentBoundary(t *testing.T) {
	// Sequential KDF work bounds test memory.
	env, home, _ := pairingSourceFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("secret backend message") }
	var out bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &out, env) != 1 {
		t.Fatal("failed source accepted")
	}
	ledgers, err := readPairingLedgers(home)
	must(t, err)
	if len(ledgers) != 0 || strings.Contains(out.String(), "secret backend") {
		t.Fatal("precheck created state or leaked error")
	}
	env, home, _ = pairingSourceFixture(t)
	path := filepath.Join(t.TempDir(), "existing")
	must(t, os.WriteFile(path, []byte("preserve"), 0600))
	out.Reset()
	if Run([]string{"machines", "add", "--yes", "--name", "laptop", "--file", path}, strings.NewReader(""), &out, &out, env) != 1 {
		t.Fatal("existing file overwritten")
	}
	ledgers, err = readPairingLedgers(home)
	must(t, err)
	if len(ledgers) != 1 || ledgers[0].State != pairingDeliveryIntent {
		t.Fatalf("%+v", ledgers)
	}
	data, err := os.ReadFile(path)
	must(t, err)
	if string(data) != "preserve" {
		t.Fatal("file changed")
	}
	if !strings.Contains(strings.Join(pairingWarnings(home, env.now()), " "), "uncertain") {
		t.Fatal("uncertainty hidden")
	}
}

func TestPairingTwoHomesS3CommitIdentityAndSecretFreeState(t *testing.T) {
	source, sourceHome, store := pairingSourceFixture(t)
	var sourceOut, errOut bytes.Buffer
	if code := Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &sourceOut, &errOut, source); code != 0 {
		t.Fatalf("source %d %s", code, &errOut)
	}
	lines := strings.Split(sourceOut.String(), "\n")
	var bundle, code string
	for _, line := range lines {
		if strings.HasPrefix(line, "aa-pair1:") {
			bundle = line
		}
		if value, ok := strings.CutPrefix(line, "Pairing code (deliver separately): "); ok {
			code = value
		}
	}
	if bundle == "" || code == "" {
		t.Fatal("scripted delivery missing pieces")
	}
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), source.now())
	project := filepath.Join(userHome, "src", "app")
	must(t, os.MkdirAll(project, 0700))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
	present := true
	env.LookupEnv = func(k string) (string, bool) { return code, k == "AGENT_ARCHIVE_PAIRING_CODE" && present }
	env.UnsetEnv = func(k string) error {
		if k != "AGENT_ARCHIVE_PAIRING_CODE" {
			t.Fatal(k)
		}
		present = false
		return nil
	}
	existing := config.Config{MachineID: strings.Repeat("b", 32), Storage: credentials.Config{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}}
	must(t, config.Save(home, existing))
	var out bytes.Buffer
	if exit := Run([]string{"setup", "--pair-file", "-", "--yes", "--project", project}, strings.NewReader(bundle), &out, &out, env); exit != 0 {
		t.Fatalf("receiver %d %s", exit, &out)
	}
	cfg, found, err := config.Load(home)
	must(t, err)
	if !found || cfg.MachineID != existing.MachineID || cfg.MachineName != "laptop" || cfg.MachineAssignment == nil || present || !cfg.Archive.Enabled {
		t.Fatalf("bad receiver %+v", cfg)
	}
	if !strings.Contains(out.String(), "Paired") {
		t.Fatal("missing postcommit success")
	}
	for _, dir := range []string{home, sourceHome} {
		must(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				return nil
			}
			data, e := os.ReadFile(path)
			if e == nil && (bytes.Contains(data, []byte(bundle)) || bytes.Contains(data, []byte(code))) {
				t.Errorf("secret delivery persisted in %s", path)
			}
			return e
		}))
	}
	var listing bytes.Buffer
	if Run([]string{"machines", "--json"}, nil, &listing, &listing, source) != 0 {
		t.Fatalf("%s", &listing)
	}
	ledgers, err := readPairingLedgers(sourceHome)
	must(t, err)
	if len(ledgers) != 1 || ledgers[0].State != pairingClaimObserved || ledgers[0].ObservedMachineID != cfg.MachineID {
		t.Fatalf("claim %+v listing%s", ledgers, &listing)
	}
	env.Now = func() time.Time { return source.now().Add(48 * time.Hour) }
	must(t, store.Delete(context.Background(), "machines/"+cfg.MachineID+".json"))
	must(t, publishMachineAfterSetup(home, env))
	records := machines.List(context.Background(), store)
	if len(records.Records) != 1 || records.Records[0].PairingID != cfg.MachineAssignment.PairingID {
		t.Fatal("publication could not reconstruct after expiry")
	}
}

func TestPairingClipboardClearsOnlyUnchangedAndCodeUsesAlternateScreen(t *testing.T) {
	t.Parallel()
	var wrote []byte
	env := Env{Clipboard: func(b []byte) error { wrote = append([]byte(nil), b...); return nil }, PairingClipboardRead: func() ([]byte, error) { return []byte("new user contents"), nil }}
	env.clearPairClipboard("bundle")
	if wrote != nil {
		t.Fatal("new contents overwritten")
	}
	env.PairingClipboardRead = func() ([]byte, error) { return []byte("bundle"), nil }
	env.clearPairClipboard("bundle")
	if len(wrote) != 0 {
		t.Fatal("unchanged contents retained")
	}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	must(t, showPairingCode(p, "synthetic-secret", Env{Interrupts: noInterrupts}))
	s := out.String()
	start, end := strings.Index(s, "\x1b[?1049h"), strings.Index(s, "\x1b[?1049l")
	pos := strings.Index(s, "synthetic secret")
	if start < 0 || pos < start || end < pos {
		t.Fatalf("code outside alternate screen %q", s)
	}
}

func TestPairingRelocatedSubtreeAndExcludedAncestorStayRestricted(t *testing.T) {
	t.Parallel()
	sourceHome, receiverHome := t.TempDir(), t.TempDir()
	sourceRepo := filepath.Join(sourceHome, "original")
	destRepo := filepath.Join(receiverHome, "moved")
	must(t, os.MkdirAll(filepath.Join(sourceRepo, "packages", "one"), 0700))
	must(t, os.MkdirAll(filepath.Join(destRepo, "packages", "one"), 0700))
	env := testEnv(t, t.TempDir(), time.Now())
	env.PairingRepoRoot = func(context.Context, string) (string, error) { return sourceRepo, nil }
	env.repoKeyContext = func(context.Context, string) string { return "repo-0123456789abcdef" }
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: filepath.Join(sourceRepo, "packages", "one"), Included: true}, {Root: sourceRepo, Included: false}}}}
	inc, exc, err := exportPairingScope(context.Background(), cfg, sourceHome, env)
	must(t, err)
	if len(inc) != 1 || inc[0].RepoPath != "packages/one" || len(exc) != 1 || exc[0].Path != "." {
		t.Fatalf("%+v %+v", inc, exc)
	}
	env.WorkingDir = func() (string, error) { return destRepo, nil }
	env.PairingRepoRoot = func(context.Context, string) (string, error) { return destRepo, nil }
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	scopes, err := pairScope(p, pairing.Payload{Inclusions: inc}, config.Config{}, receiverHome, env, true)
	must(t, err)
	if len(scopes) != 1 || scopes[0].Root != local.CanonicalPath(filepath.Join(destRepo, "packages", "one")) {
		t.Fatalf("broadened subtree %+v %s", scopes, &out)
	}
	scopes, err = pairScope(p, pairing.Payload{Inclusions: inc, Exclusions: exc}, config.Config{}, receiverHome, env, true)
	must(t, err)
	for _, scope := range scopes {
		if scope.Included && !slicesContainsExcluded(scopes, scope.Root) {
			t.Fatalf("excluded ancestor broadened %+v", scopes)
		}
	}
	env.PairingRepoRoot = func(context.Context, string) (string, error) { return "", errors.New("unavailable") }
	inc, _, err = exportPairingScope(context.Background(), cfg, sourceHome, env)
	must(t, err)
	if inc[0].RepoKey != "" {
		t.Fatal("unknown repository exported key-only portability")
	}
	outside := t.TempDir()
	must(t, os.Symlink(outside, filepath.Join(receiverHome, "escape")))
	if _, err = resolvePortablePath(receiverHome, "escape/subtree"); err == nil {
		t.Fatal("symlink escape accepted")
	}
}

func slicesContainsExcluded(projects []archive.ProjectActivation, root string) bool {
	for _, p := range projects {
		if !p.Included && p.Root == root {
			return true
		}
	}
	return false
}

func TestPairingR2StageFailureRetryReusesCredentialAndKeepsPriorConfig(t *testing.T) {
	now := time.Now().UTC()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, userHome, kc, now)
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	p := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Storage: pairing.Storage{Provider: "r2", Bucket: "synthetic", R2Account: strings.Repeat("a", 32), Region: "auto"}, AccessKeyID: "ACCESS", SecretAccessKey: "R2_SENTINEL_SECRET", Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(p, code)
	must(t, err)
	env.LookupEnv = func(k string) (string, bool) { return code, k == "AGENT_ARCHIVE_PAIRING_CODE" }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("failure") }
	var out bytes.Buffer
	args := []string{"setup", "--pair-file", "-", "--yes", "--project", project}
	if Run(args, strings.NewReader(bundle), &out, &out, env) != 1 {
		t.Fatal("storage failure committed")
	}
	_, found, err := config.Load(home)
	must(t, err)
	if found {
		t.Fatal("prior configuration changed")
	}
	if len(kc.items) != 1 {
		t.Fatalf("staged %d credentials", len(kc.items))
	}
	must(t, filepath.WalkDir(home, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		data, e := os.ReadFile(path)
		if e == nil && (bytes.Contains(data, []byte(p.SecretAccessKey)) || bytes.Contains(data, []byte(bundle)) || bytes.Contains(data, []byte(code))) {
			t.Fatalf("secret written %s", path)
		}
		return e
	}))
	store := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	out.Reset()
	if exit := Run(args, strings.NewReader(bundle), &out, &out, env); exit != 0 {
		t.Fatalf("retry %d %s", exit, &out)
	}
	if len(kc.items) != 1 {
		t.Fatal("duplicate staged credential")
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	if cfg.Storage.R2CredentialRef != "pairing-"+p.PairingID || cfg.MachineAssignment == nil || cfg.MachineAssignment.SharedWith != p.IssuerID {
		t.Fatalf("%+v", cfg)
	}
}

func TestPairingReceiverMissingProfileAndChangedDestinationDoNotSave(t *testing.T) {
	now := time.Now().UTC()
	p := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(p, code)
	must(t, err)
	for _, existing := range []bool{false, true} {
		home := t.TempDir()
		env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
		env.LookupEnv = func(k string) (string, bool) { return code, k == "AGENT_ARCHIVE_PAIRING_CODE" }
		var before []byte
		if existing {
			cfg := config.Config{MachineID: strings.Repeat("f", 32), Storage: credentials.Config{Provider: "s3", Bucket: "prior", AWSProfile: "local", Region: "us-east-1"}}
			must(t, config.Save(home, cfg))
			before, err = os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
		}
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
			t.Fatal("storage opened before consent/profile")
			return nil, nil
		}
		var out bytes.Buffer
		if Run([]string{"setup", "--pair-file", "-", "--yes"}, strings.NewReader(bundle), &out, &out, env) != 1 {
			t.Fatal("unsafe receiver accepted")
		}
		if existing {
			after, e := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, e)
			if !bytes.Equal(before, after) || !strings.Contains(out.String(), "destination differs") {
				t.Fatalf("config changed or consent missing %s", &out)
			}
		} else {
			if _, e := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(e) || !strings.Contains(out.String(), "aws configure --profile archive") {
				t.Fatalf("saved missing profile %v %s", e, &out)
			}
		}
		if _, e := os.Stat(draftPath(home)); !os.IsNotExist(e) {
			t.Fatal("refusal saved draft")
		}
	}
}
