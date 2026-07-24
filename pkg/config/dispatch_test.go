package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureDispatchToken_MintsAndPersists verifies that an empty token gets
// generated, set in memory, and appended to the config file so it survives
// restarts and rides into the VM.
func TestEnsureDispatchToken_MintsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "[github]\nowner = \"acme\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Config{}
	if err := c.EnsureDispatchToken(path, func() (string, error) { return "minted-token", nil }); err != nil {
		t.Fatalf("EnsureDispatchToken: %v", err)
	}
	if c.Dispatch.Token != "minted-token" {
		t.Fatalf("in-memory token = %q, want minted-token", c.Dispatch.Token)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, original) {
		t.Errorf("existing config content was clobbered; got:\n%s", got)
	}
	if !strings.Contains(got, `token = "minted-token"`) {
		t.Errorf("persisted config missing token line; got:\n%s", got)
	}

	// Reloading the persisted file must surface the same token.
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Dispatch.Token != "minted-token" {
		t.Fatalf("reloaded token = %q, want minted-token", reloaded.Dispatch.Token)
	}
}

// TestEnsureDispatchToken_NoopWhenSet verifies an existing token is left
// untouched and the config file is not modified.
func TestEnsureDispatchToken_NoopWhenSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	c := &Config{Dispatch: DispatchConfig{Token: "preset"}}
	called := false
	if err := c.EnsureDispatchToken(path, func() (string, error) { called = true; return "new", nil }); err != nil {
		t.Fatalf("EnsureDispatchToken: %v", err)
	}
	if called {
		t.Error("generator called despite token already set")
	}
	if c.Dispatch.Token != "preset" {
		t.Errorf("token = %q, want preset", c.Dispatch.Token)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("config file modified when token already set")
	}
}

// TestEnsureDispatchToken_EmptyPathKeepsInMemory verifies that with no durable
// path the token is still set in memory (better an ephemeral token than none),
// without error.
func TestEnsureDispatchToken_EmptyPathKeepsInMemory(t *testing.T) {
	c := &Config{}
	if err := c.EnsureDispatchToken("", func() (string, error) { return "ephemeral", nil }); err != nil {
		t.Fatalf("EnsureDispatchToken: %v", err)
	}
	if c.Dispatch.Token != "ephemeral" {
		t.Fatalf("token = %q, want ephemeral", c.Dispatch.Token)
	}
}
