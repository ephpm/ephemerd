package main

import (
	"strings"
	"testing"
)

const fixture = `package download

const (
	RunnerVersion = "2.335.1"
	CNIVersion    = "1.6.2"
)

var pinnedSHA256 = map[string]string{
	// GitHub Actions runner (actions/runner v2.335.1)
	"actions-runner-linux-x64-2.335.1.tar.gz":   "4ef2f25285f0ae4477f1fe1e346db76d2f3ebf03824e2ddd1973a2819bf6c8cf",
	"actions-runner-linux-arm64-2.335.1.tar.gz": "6d1e85bfd1a506a8b17c1f1b9b57dba458ffed90898799aaa9f599520b0d9207",
	"actions-runner-win-x64-2.335.1.zip":        "eb65c95277af42bcf3778a799c41359d224ba2a67b4de26b7cea1729b09c803d",
	"actions-runner-osx-x64-2.335.1.tar.gz":     "b2fe57b2ae5b0bc1605f9fc0723c07eedf06167321d3478ce0440f15e5b0a010",
	"actions-runner-osx-arm64-2.335.1.tar.gz":   "e1a9bc7a3661e06fa0b129d15c2064fe65dc81a431001d8958a9db1409b73769",

	// CNI plugins (containernetworking/plugins v1.6.2)
	"cni-plugins-linux-amd64-v1.6.2.tgz": "b8e811578fb66023f90d2e238d80cec3bdfca4b44049af74c374d4fae0f9c090",
}
`

func testRelease(version string) *release {
	rel := &release{TagName: "v" + version}
	for i, p := range runnerPlatforms {
		rel.Assets = append(rel.Assets, asset{
			Name:   assetName(p.stem, p.ext, version),
			Digest: "sha256:" + strings.Repeat(string(rune('a'+i)), 64),
		})
	}
	// An unpinned platform must be ignored, not required.
	rel.Assets = append(rel.Assets, asset{Name: "actions-runner-linux-arm-" + version + ".tar.gz", Digest: "sha256:" + strings.Repeat("f", 64)})
	return rel
}

func TestRewriteDownloadGo(t *testing.T) {
	pins, err := resolvePins(testRelease("2.337.0"), "2.337.0")
	if err != nil {
		t.Fatal(err)
	}
	out, err := rewriteDownloadGo([]byte(fixture), "2.335.1", "2.337.0", pins)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		`RunnerVersion = "2.337.0"`,
		`// GitHub Actions runner (actions/runner v2.337.0)`,
		`"actions-runner-linux-x64-2.337.0.tar.gz":   "` + strings.Repeat("a", 64) + `",`,
		`"actions-runner-win-x64-2.337.0.zip":        "` + strings.Repeat("c", 64) + `",`,
		`"actions-runner-osx-arm64-2.337.0.tar.gz":   "` + strings.Repeat("e", 64) + `",`,
		`"cni-plugins-linux-amd64-v1.6.2.tgz": "b8e8`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten file lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "2.335.1") {
		t.Errorf("old version survived:\n%s", got)
	}
}

func TestRewriteDownloadGoRefusesUnknownShape(t *testing.T) {
	pins, err := resolvePins(testRelease("2.337.0"), "2.337.0")
	if err != nil {
		t.Fatal(err)
	}
	// A sixth reference to the old version that runnerbump does not know.
	src := fixture + "\n// see 2.335.1\n"
	if _, err := rewriteDownloadGo([]byte(src), "2.335.1", "2.337.0", pins); err == nil {
		t.Fatal("expected an error for a leftover old-version reference")
	}
	// A pin site that has gone missing.
	src = strings.Replace(fixture, `"actions-runner-osx-x64-2.335.1.tar.gz"`, `"renamed.tar.gz"`, 1)
	if _, err := rewriteDownloadGo([]byte(src), "2.335.1", "2.337.0", pins); err == nil {
		t.Fatal("expected an error for a missing pin site")
	}
}

func TestResolvePinsMissingAsset(t *testing.T) {
	rel := testRelease("2.338.0")
	rel.Assets = rel.Assets[1:] // drop linux-x64
	if _, err := resolvePins(rel, "2.338.0"); err == nil || !strings.Contains(err.Error(), "actions-runner-linux-x64-2.338.0.tar.gz") {
		t.Fatalf("want missing-asset error naming linux-x64, got %v", err)
	}
}

func TestResolvePinsNotesMismatch(t *testing.T) {
	rel := testRelease("2.338.0")
	rel.Body = "<!-- BEGIN SHA linux-x64 -->" + strings.Repeat("0", 64) + "<!-- END SHA linux-x64 -->"
	if _, err := resolvePins(rel, "2.338.0"); err == nil {
		t.Fatal("want an error when the release notes disagree with the API digest")
	}
	rel.Body = "<!-- BEGIN SHA linux-x64 -->" + strings.Repeat("A", 64) + "<!-- END SHA linux-x64 -->"
	if _, err := resolvePins(rel, "2.338.0"); err != nil {
		t.Fatalf("matching (case-insensitive) notes SHA should pass: %v", err)
	}
}

func TestRewriteDockerfile(t *testing.T) {
	src := "FROM x\n\nARG RUNNER_VERSION=2.333.1\nARG CNI_VERSION=1.6.2\n"
	out, err := rewriteDockerfile([]byte(src), "2.338.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM x\n\nARG RUNNER_VERSION=2.338.0\nARG CNI_VERSION=1.6.2\n"; string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if _, err := rewriteDockerfile([]byte("FROM x\n"), "2.338.0"); err == nil {
		t.Fatal("expected an error when the ARG is absent")
	}
}

func TestSemverLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.337.0", "2.338.0", true},
		{"2.337.0", "2.337.0", false},
		{"2.338.0", "2.337.0", false},
		{"2.99.0", "2.100.0", true}, // numeric, not lexical
		{"2.337.9", "3.0.0", true},
	} {
		got, err := semverLess(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("semverLess(%s, %s) = %v, %v; want %v", c.a, c.b, got, err, c.want)
		}
	}
	if _, err := semverLess("2.337", "2.338.0"); err == nil {
		t.Error("expected an error for a two-part version")
	}
}
