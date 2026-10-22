package config

import "testing"

func TestActionsCacheConfig(t *testing.T) {
	var off ActionsCacheConfig
	if err := off.validate(); err != nil {
		t.Fatalf("disabled block failed validation: %v", err)
	}
	if off.ResolvedPort() != DefaultActionsCachePort || off.MaxBytesPerRepo() != 100<<30 || off.MaxEntryBytes() != 10<<30 {
		t.Fatal("defaults are wrong")
	}
	if err := (ActionsCacheConfig{Enabled: true}).validate(); err == nil {
		t.Fatal("enabled without a registry passed validation")
	}
	if err := (ActionsCacheConfig{Enabled: true, Registry: "cache.lan:5001"}).validate(); err == nil {
		t.Fatal("a scheme-less registry passed validation")
	}
	if err := (ActionsCacheConfig{Enabled: true, Registry: "http://cache.lan:5001"}).validate(); err != nil {
		t.Fatalf("valid block failed: %v", err)
	}
	t.Setenv("EPHEMERD_TEST_AC_PASS", "hunter2")
	c := ActionsCacheConfig{Enabled: true, Registry: "http://c:5001", Password: "file", PasswordEnv: "EPHEMERD_TEST_AC_PASS"}
	if c.ResolvedPassword() != "hunter2" {
		t.Fatal("password_env did not win over password")
	}
	if err := (ActionsCacheConfig{Enabled: true, Registry: "http://c:5001", PasswordEnv: "EPHEMERD_TEST_AC_UNSET"}).validate(); err == nil {
		t.Fatal("password_env naming an unset variable passed validation")
	}
	if (ActionsCacheConfig{MaxSizeGBPerRepo: -1}).MaxBytesPerRepo() != 0 {
		t.Fatal("negative size cap should disable eviction")
	}
}
