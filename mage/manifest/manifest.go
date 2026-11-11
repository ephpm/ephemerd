// Package manifest holds the mage targets that build, sign and verify the
// signed runner manifest the self-healing runner subsystem consumes
// (docs/arch/self-healing-runner.md).
//
//	mage manifest:build    dist/runner-manifest.json from mage/download's pinned table
//	mage manifest:sign     dist/runner-manifest.json.minisig with RUNNER_MANIFEST_MINISIGN_KEY
//	mage manifest:verify   the pair against the public key compiled into ephemerd
//	mage manifest:keygen   a fresh minisign keypair (rotation)
//
// The manifest's version and per-platform SHA-256 values come from ONE
// place, mage/download/download.go (RunnerVersion, RunnerFloor,
// pinnedSHA256): the same table `mage download` refuses to embed an
// archive without. Nothing here re-types a hash.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/mage/download"
	"github.com/ephpm/ephemerd/pkg/runnerversion"
)

// Environment knobs. Every one has a default so `mage manifest:build` with
// no environment produces the stable manifest for the pinned runner.
const (
	// EnvOut is the manifest path. Default dist/runner-manifest.json.
	EnvOut = "RUNNER_MANIFEST_OUT"
	// EnvFloor overrides download.RunnerFloor for one build (an emergency
	// floor bump without a code change). Must be <= RunnerVersion.
	EnvFloor = "RUNNER_MANIFEST_FLOOR"
	// EnvChannel is "stable" (default) or "canary".
	EnvChannel = "RUNNER_MANIFEST_CHANNEL"
	// EnvRollback = "true" marks a deliberate downgrade.
	EnvRollback = "RUNNER_MANIFEST_ROLLBACK"
	// EnvPublishedAt pins published_at (RFC 3339) for a reproducible
	// build; default now.
	EnvPublishedAt = "RUNNER_MANIFEST_PUBLISHED_AT"
	// EnvSecretKey is the minisign secret key: the whole .key file or just
	// its base64 line. This is the GitHub Actions secret
	// RUNNER_MANIFEST_MINISIGN_KEY on ephpm/ephemerd.
	EnvSecretKey = "RUNNER_MANIFEST_MINISIGN_KEY"
	// EnvSecretKeyFile is a path to the .key file, for local signing.
	EnvSecretKeyFile = "RUNNER_MANIFEST_MINISIGN_KEY_FILE"
	// EnvPublicKeyFile makes verify use this .pub instead of the embedded
	// key (checking a rotated key before it is compiled in).
	EnvPublicKeyFile = "RUNNER_MANIFEST_PUBLIC_KEY_FILE"
	// EnvKeygenDir is where keygen writes runner-manifest.pub and
	// runner-manifest.key. Default: the current directory. The .key file
	// MUST NOT be committed.
	EnvKeygenDir = "RUNNER_MANIFEST_KEYGEN_DIR"
)

const defaultOut = "dist/runner-manifest.json"

func outPath() string {
	if v := os.Getenv(EnvOut); v != "" {
		return v
	}
	return defaultOut
}

// Build writes runner-manifest.json from the pinned runner table.
func Build() error {
	m, err := Compose(os.Getenv(EnvFloor), os.Getenv(EnvChannel), os.Getenv(EnvRollback) == "true", os.Getenv(EnvPublishedAt))
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Round-trip through the daemon's own parser: what we publish must be
	// what nodes accept.
	if _, err := runnerversion.ParseManifest(data); err != nil {
		return fmt.Errorf("composed manifest does not validate: %w", err)
	}
	out := outPath()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("  Wrote %s (version %s, floor %s, channel %s, %d platforms)\n", out, m.Version, m.Floor, m.Channel, len(m.Platforms))
	return nil
}

// Compose builds the manifest value. floor/channel/publishedAt empty =
// defaults (download.RunnerFloor, stable, now).
func Compose(floor, channel string, rollback bool, publishedAt string) (*runnerversion.Manifest, error) {
	if floor == "" {
		floor = download.RunnerFloor
	}
	if channel == "" {
		channel = string(runnerversion.ChannelStable)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if publishedAt != "" {
		t, err := time.Parse(time.RFC3339, publishedAt)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvPublishedAt, err)
		}
		at = t.UTC()
	}
	arts := download.RunnerArtifacts()
	if len(arts) == 0 {
		return nil, errors.New("no pinned runner archives for " + download.RunnerVersion + " in mage/download")
	}
	m := &runnerversion.Manifest{
		SchemaVersion: runnerversion.SchemaVersion,
		Version:       download.RunnerVersion,
		Floor:         floor,
		Channel:       runnerversion.Channel(channel),
		Rollback:      rollback,
		PublishedAt:   at,
		Platforms:     map[string]runnerversion.PlatformArtifact{},
	}
	for _, a := range arts {
		m.Platforms[a.Platform] = runnerversion.PlatformArtifact{URL: a.URL, SHA256: a.SHA256}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Sign writes <out>.minisig next to the manifest, signed with the secret
// key from RUNNER_MANIFEST_MINISIGN_KEY (or _FILE).
func Sign() error {
	sk, err := loadSecretKey()
	if err != nil {
		return err
	}
	out := outPath()
	data, err := os.ReadFile(out)
	if err != nil {
		return fmt.Errorf("reading manifest (run mage manifest:build first): %w", err)
	}
	// minisign's own trusted-comment convention, tab-separated.
	tc := fmt.Sprintf("timestamp:%d\tfile:%s\thashed", time.Now().Unix(), filepath.Base(out))
	sig := sk.Sign(data, tc)
	// Prove the signature verifies under the key we just used before
	// writing it; a mismatch here is a bug, not a config problem.
	if err := sk.Public().Verify(data, sig); err != nil {
		return fmt.Errorf("self-check of fresh signature failed: %w", err)
	}
	if err := os.WriteFile(out+".minisig", []byte(sig), 0o644); err != nil {
		return err
	}
	fmt.Printf("  Wrote %s.minisig (key id %X)\n", out, sk.KeyID)
	return nil
}

// Verify checks the manifest + .minisig against the public key compiled
// into ephemerd (or RUNNER_MANIFEST_PUBLIC_KEY_FILE). This is the CI gate
// that proves the secret in the repository matches the key nodes trust.
func Verify() error {
	var pk *runnerversion.PublicKey
	var err error
	if f := os.Getenv(EnvPublicKeyFile); f != "" {
		text, rerr := os.ReadFile(f)
		if rerr != nil {
			return rerr
		}
		pk, err = runnerversion.ParsePublicKey(string(text))
	} else {
		pk, err = runnerversion.EmbeddedPublicKey()
	}
	if err != nil {
		return fmt.Errorf("public key: %w", err)
	}
	out := outPath()
	data, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(out + ".minisig")
	if err != nil {
		return err
	}
	m, err := pk.VerifyAndParse(data, string(sig))
	if err != nil {
		return fmt.Errorf("%s does not verify under key %X: %w", out, pk.KeyID, err)
	}
	fmt.Printf("  OK: %s verifies under key %X (version %s, floor %s, channel %s, published %s)\n",
		out, pk.KeyID, m.Version, m.Floor, m.Channel, m.PublishedAt.Format(time.RFC3339))
	return nil
}

// Keygen writes a fresh minisign keypair: runner-manifest.pub (commit it
// to pkg/runnerversion/keys/ to rotate the trust root) and
// runner-manifest.key (store it ONLY as the RUNNER_MANIFEST_MINISIGN_KEY
// secret; never commit it). Refuses to overwrite existing files.
func Keygen() error {
	dir := os.Getenv(EnvKeygenDir)
	if dir == "" {
		dir = "."
	}
	pubPath := filepath.Join(dir, "runner-manifest.pub")
	keyPath := filepath.Join(dir, "runner-manifest.key")
	for _, p := range []string{pubPath, keyPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists; refusing to overwrite a signing key", p)
		}
	}
	pk, sk, err := runnerversion.GenerateKeypair(nil)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	id := strings.ToUpper(strconv.FormatUint(keyIDUint(pk.KeyID), 16))
	if err := os.WriteFile(pubPath, []byte(pk.EncodeFile("ephemerd runner-manifest signing key "+id+" (minisign)")), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, []byte(sk.Encode("ephemerd runner-manifest SECRET key "+id+" — never commit; GitHub secret RUNNER_MANIFEST_MINISIGN_KEY")), 0o600); err != nil {
		return err
	}
	fmt.Printf("  Wrote %s (public, commit as pkg/runnerversion/keys/runner-manifest.pub)\n", pubPath)
	fmt.Printf("  Wrote %s (SECRET: add as the %s repository secret, then delete)\n", keyPath, EnvSecretKey)
	return nil
}

func keyIDUint(id [8]byte) uint64 {
	var v uint64
	for _, b := range id {
		v = v<<8 | uint64(b)
	}
	return v
}

func loadSecretKey() (*runnerversion.SecretKey, error) {
	text := os.Getenv(EnvSecretKey)
	if text == "" {
		f := os.Getenv(EnvSecretKeyFile)
		if f == "" {
			return nil, fmt.Errorf("set %s (key contents) or %s (path)", EnvSecretKey, EnvSecretKeyFile)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	sk, err := runnerversion.ParseSecretKey(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvSecretKey, err)
	}
	return sk, nil
}
