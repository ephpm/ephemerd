package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyFile_Basic(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	dst := filepath.Join(t.TempDir(), "dst.txt")

	want := "hello world\n"
	if err := os.WriteFile(src, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("copied content = %q, want %q", got, want)
	}
}

func TestCopyFile_OverwritesExisting(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	dst := filepath.Join(t.TempDir(), "dst.txt")

	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old-content-larger"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("dst = %q, want %q (truncated)", got, "new")
	}
}

func TestCopyFile_MissingSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst.txt")

	err := copyFile(filepath.Join(t.TempDir(), "does-not-exist"), dst)
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestCopyFile_BinaryContent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.bin")
	dst := filepath.Join(t.TempDir(), "dst.bin")

	want := []byte{0x00, 0x01, 0xff, 0xfe, 0x42, 0x00}
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != string(want) {
		t.Errorf("binary content mismatch")
	}
}

func TestCreateDefaultConfig_NewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	if err := createDefaultConfig(path); err != nil {
		t.Fatalf("createDefaultConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	for _, want := range []string{"[github]", "[runner]", "[log]", "max_concurrent"} {
		if !strings.Contains(content, want) {
			t.Errorf("config missing %q\n%s", want, content)
		}
	}
}

func TestCreateDefaultConfig_DoesNotOverwriteExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	existing := `[forgejo]
instance_url = "https://codeberg.org"
`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := createDefaultConfig(path); err != nil {
		t.Fatalf("createDefaultConfig: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != existing {
		t.Errorf("existing config was overwritten:\ngot:  %s\nwant: %s", got, existing)
	}
}
