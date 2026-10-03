package config

import "testing"

func TestSparePolicyDefaultAndBounds(t *testing.T) {
	t.Parallel()
	if (Config{}).SpareTarget() != 2 {
		t.Fatal("wrong default")
	}
	for n := -1; n <= 6; n++ {
		cfg := Config{SpareKeys: &n}
		home := t.TempDir()
		err := Save(home, cfg)
		if n < 0 || n > 5 {
			if err == nil {
				t.Fatal("invalid target accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		loaded, _, err := Load(home)
		if err != nil || loaded.SpareKeys == nil || loaded.SpareTarget() != n {
			t.Fatal("target did not round trip")
		}
	}
}
