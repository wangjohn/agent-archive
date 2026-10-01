package config

import "testing"

func TestPauseGenerationPersistsAndChangesOnlyAtTransitions(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Config{MachineID: "machine"}); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(home)
	if err != nil || !found || cfg.PauseGeneration != "" {
		t.Fatalf("legacy config=%+v found=%t error=%v", cfg, found, err)
	}
	previous := ""
	for _, paused := range []bool{true, true, false, false, true} {
		before := cfg.Paused
		cfg, err = SetPaused(home, paused)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PauseGeneration == "" || (before != paused && previous == cfg.PauseGeneration) || (before == paused && previous != cfg.PauseGeneration) {
			t.Fatalf("transition %t -> %t generation=%q previous=%q", before, paused, cfg.PauseGeneration, previous)
		}
		saved, found, err := Load(home)
		if err != nil || !found || saved.PauseGeneration != cfg.PauseGeneration || saved.Paused != paused || saved.MachineID != "machine" {
			t.Fatalf("saved config=%+v found=%t error=%v", saved, found, err)
		}
		previous = cfg.PauseGeneration
	}
}
