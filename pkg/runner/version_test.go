package runner

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersionString(t *testing.T) {
	cases := map[string][]int{
		"2.338.2":  {2, 338, 2},
		"v2.338.2": {2, 338, 2},
		" 2.1.0 ":  {2, 1, 0},
		"":         nil,
		"latest":   nil,
		"2.x.1":    nil,
	}
	for in, want := range cases {
		got := ParseVersionString(in)
		if len(got) != len(want) {
			t.Errorf("ParseVersionString(%q) = %v want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ParseVersionString(%q) = %v want %v", in, got, want)
			}
		}
	}
	if CompareVersions([]int{2, 338, 2}, []int{2, 337, 0}) <= 0 || CompareVersions([]int{2, 338}, []int{2, 338, 0}) != 0 {
		t.Fatal("CompareVersions")
	}
	if ParseVersion("actions-runner-win-x64-2.338.2.zip")[1] != 338 {
		t.Fatal("ParseVersion")
	}
}

func TestExtractArchiveTarAndZip(t *testing.T) {
	// tar.gz
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "bin/run.sh", Mode: 0o755, Size: 2, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractArchive(bytes.NewReader(tgz.Bytes()), "x.tar.gz", dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "bin", "run.sh")); err != nil || string(b) != "hi" {
		t.Fatalf("tar extract: %v %q", err, b)
	}

	// zip
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, err := zw.Create("run.cmd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("@echo")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dest = t.TempDir()
	if err := ExtractArchive(bytes.NewReader(zb.Bytes()), "x.zip", dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "run.cmd")); err != nil || string(b) != "@echo" {
		t.Fatalf("zip extract: %v %q", err, b)
	}
}

func TestEmbeddedArchiveMatchesFindTarball(t *testing.T) {
	// A fresh checkout embeds only .gitkeep (the seam must error, not hand
	// back something that is not a runner); after `mage download` the real
	// archive is there and the seam must return the same file findTarball
	// selects, with a parseable version.
	m := New(t.TempDir(), nil)
	name, f, err := m.EmbeddedArchive()
	if err != nil {
		if _, ftErr := m.findTarball(); ftErr == nil {
			t.Fatalf("EmbeddedArchive errored but findTarball did not: %v", err)
		}
		return
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	want, err := m.findTarball()
	if err != nil {
		t.Fatal(err)
	}
	if "embed/"+name != want {
		t.Fatalf("EmbeddedArchive = %s, findTarball = %s", name, want)
	}
	if ParseVersion(name) == nil {
		t.Fatalf("embedded archive name %q has no version", name)
	}
}
