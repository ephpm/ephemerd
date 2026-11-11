package runnerversion

import (
	"errors"
	"strings"
	"testing"
)

func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest(manifestJSON(t, validManifest("2.338.2", "2.338.0", zeroSHA)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "2.338.2" || m.Channel != ChannelStable || len(m.Platforms) != 3 {
		t.Fatalf("unexpected manifest %+v", m)
	}
	a, err := m.Artifact("linux-x64")
	if err != nil || a.SHA256 != zeroSHA {
		t.Fatalf("artifact: %v %+v", err, a)
	}
	if _, err := m.Artifact("linux-arm64"); err == nil {
		t.Fatal("expected missing platform error")
	}
	if !m.BelowFloor([]int{2, 337, 0}) || m.BelowFloor([]int{2, 338, 0}) {
		t.Fatal("BelowFloor wrong")
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]func(m *Manifest){
		"schema":           func(m *Manifest) { m.SchemaVersion = 2 },
		"version":          func(m *Manifest) { m.Version = "2.338" },
		"floor":            func(m *Manifest) { m.Floor = "latest" },
		"floor above":      func(m *Manifest) { m.Floor = "2.339.0" },
		"channel":          func(m *Manifest) { m.Channel = "nightly" },
		"published_at":     func(m *Manifest) { m.PublishedAt = zeroTime() },
		"no platforms":     func(m *Manifest) { m.Platforms = nil },
		"bad platform key": func(m *Manifest) { m.Platforms["linux-amd64"] = m.Platforms["linux-x64"] },
		"bad sha": func(m *Manifest) {
			p := m.Platforms["linux-x64"]
			p.SHA256 = strings.Repeat("A", 64) // upper-case hex is not accepted
			m.Platforms["linux-x64"] = p
		},
		"http url": func(m *Manifest) {
			p := m.Platforms["linux-x64"]
			p.URL = "http://example.com/x/" + ArchiveFilename("linux-x64", m.Version)
			m.Platforms["linux-x64"] = p
		},
		"filename mismatch": func(m *Manifest) {
			p := m.Platforms["linux-x64"]
			p.URL = "https://example.com/x/actions-runner-linux-x64-2.338.1.tar.gz"
			m.Platforms["linux-x64"] = p
		},
		"win ext": func(m *Manifest) {
			p := m.Platforms["win-x64"]
			p.URL = "https://example.com/x/actions-runner-win-x64-2.338.2.tar.gz"
			m.Platforms["win-x64"] = p
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest("2.338.2", "2.338.0", zeroSHA)
			mutate(m)
			_, err := ParseManifest(manifestJSON(t, m))
			if !errors.Is(err, ErrManifestInvalid) {
				t.Fatalf("expected ErrManifestInvalid, got %v", err)
			}
		})
	}
	if _, err := ParseManifest([]byte("{not json")); !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("expected ErrManifestInvalid for bad json, got %v", err)
	}
}

func TestPlatformKey(t *testing.T) {
	cases := map[[2]string]string{
		{"linux", "amd64"}:   "linux-x64",
		{"linux", "arm64"}:   "linux-arm64",
		{"windows", "amd64"}: "win-x64",
		{"darwin", "arm64"}:  "osx-arm64",
		{"darwin", "amd64"}:  "osx-x64",
	}
	for in, want := range cases {
		if got := PlatformKey(in[0], in[1]); got != want {
			t.Errorf("PlatformKey(%s,%s) = %s want %s", in[0], in[1], got, want)
		}
	}
	if ArchiveFilename("win-x64", "2.338.2") != "actions-runner-win-x64-2.338.2.zip" {
		t.Fatal("win filename")
	}
	if ArchiveFilename("osx-arm64", "2.338.2") != "actions-runner-osx-arm64-2.338.2.tar.gz" {
		t.Fatal("osx filename")
	}
}
