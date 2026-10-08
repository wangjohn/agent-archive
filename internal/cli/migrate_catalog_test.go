package cli

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type qualifiedCLIStore struct{ *storagetest.MemoryStore }

func (*qualifiedCLIStore) CatalogAtomicQualification() error { return nil }

func privateCatalogFromLegacy(t *testing.T, source *storagetest.MemoryStore) *catalog.Store {
	t.Helper()
	raw := &qualifiedCLIStore{storagetest.NewMemoryStore()}
	objects, err := source.List(t.Context(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	w, err := catalog.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	var metadata []storage.Object
	for _, obj := range objects {
		bytes, err := source.Get(t.Context(), obj.Key)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(obj.Key, "/metadata.json") {
			metadata = append(metadata, obj)
			continue
		}
		if err = raw.Put(t.Context(), obj.Key, bytes); err != nil {
			t.Fatal(err)
		}
	}
	for _, obj := range metadata {
		bytes, err := source.Get(t.Context(), obj.Key)
		if err != nil {
			t.Fatal(err)
		}
		var summary archive.Metadata
		if err = json.Unmarshal(bytes, &summary); err != nil {
			t.Fatal(err)
		}
		ref, err := w.PutImmutable(t.Context(), catalog.KindMetadata, bytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Commit(t.Context(), catalog.CatalogMutation{ID: "private/" + storage.SHA256Hex([]byte(obj.Key)), SessionKey: obj.Key, Next: &catalog.CatalogEntry{Metadata: ref, Summary: summary}}); err != nil {
			t.Fatal(err)
		}
	}
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), owner, "private fixture credential proof"); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func TestRealCLICatalogCommandsEqualPrivateExhaustiveOracle(t *testing.T) {
	a := newScopedArchive(t)
	a.add(t, "alpha001", "Résumé query", a.label, linked(212))
	a.add(t, "alpha002", "Child detail", a.label, subagentOf("alpha001"))
	a.add(t, "beta0001", "Unrelated", "other")
	remote := privateCatalogFromLegacy(t, a.mem)
	measured := storagetest.NewMeasuredStore(remote, 0)
	for _, args := range [][]string{{"list", "--all-projects", "--json", "--limit", "2"}, {"list", "--all-projects", "résumé"}, {"show", "alpha"}, {"show", "#212"}, {"stats", "--json"}} {
		var wantOut, wantErr, gotOut, gotErr bytes.Buffer
		oracleEnv := a.env
		oracleEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return a.mem, nil }
		wantCode := Run(args, bytes.NewReader(nil), &wantOut, &wantErr, oracleEnv)
		catalogEnv := a.env
		catalogEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
		gotCode := Run(args, bytes.NewReader(nil), &gotOut, &gotErr, catalogEnv)
		if gotCode != wantCode || gotOut.String() != wantOut.String() || gotErr.String() != wantErr.String() {
			t.Fatalf("%v catalog(%d,%q,%q) oracle(%d,%q,%q)", args, gotCode, gotOut.String(), gotErr.String(), wantCode, wantOut.String(), wantErr.String())
		}
	}
	if measured.Metrics().Lists != 0 {
		t.Fatalf("catalog canonical LIST: %+v", measured.Metrics())
	}
}

func TestMigrateHelpAndNoForceBypass(t *testing.T) {
	var out, stderr bytes.Buffer
	env := Env{Home: func() (string, error) { t.Fatal("help opened user home"); return "", nil }}
	if code := Run([]string{"migrate", "--help"}, nil, &out, &stderr, env); code != 0 {
		t.Fatal(code, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	forceEnv := Env{Home: func() (string, error) { return t.TempDir(), nil }}
	if code := Run([]string{"migrate", "--format", "catalog-v4", "--prefix", "new", "--force"}, nil, &out, &stderr, forceEnv); code != 2 {
		t.Fatal(code, stderr.String())
	}
	if !reflect.DeepEqual(commandHelp["migrate"][:6], "Usage:") {
		t.Fatal("missing help")
	}
}
