package config

import "testing"

func TestPublicationCompositionScopePersistsAndDerivesStorage(t *testing.T) {
	t.Parallel()
	home := durableTestHome(t)
	if err := Save(home, Config{MachineID: "synthetic", GenerationProtection: true, CodexHistoryProtection: true}); err != nil {
		t.Fatal(err)
	}
	var retained PublicationCompositionGuard
	if err := WithPublicationComposition(home, func(g PublicationCompositionGuard) error {
		retained = g
		cfg, found, err := Load(home)
		if err != nil || !found || cfg.SchemaVersion != 8 || !cfg.DurableStorageProtection || !cfg.PublicationCompositionProtection || !cfg.GenerationProtection || !cfg.CodexHistoryProtection {
			t.Fatalf("floor not durably composed: %+v %v", cfg, err)
		}
		storage, err := g.Storage(home)
		if err != nil {
			return err
		}
		return storage.CheckHome(home)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Storage(home); err == nil {
		t.Fatal("expired composition accepted")
	}
	if err := WithDurableStorage(home, func(g DurableStorageGuard) error { return g.CheckHome(home) }); err != nil {
		t.Fatal(err)
	}
	if err := Save(home, Config{MachineID: "rollback"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(home)
	if err != nil || cfg.SchemaVersion != 8 || !cfg.PublicationCompositionProtection || !cfg.DurableStorageProtection {
		t.Fatalf("lower writer lost sticky floor: %+v %v", cfg, err)
	}
}
