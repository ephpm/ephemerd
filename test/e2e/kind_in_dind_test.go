//go:build e2e && privileged

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/dind"
	"github.com/ephpm/ephemerd/pkg/networking"
)

// TestE2E_KindInDind_NetnsWiring reproduces the failure mode from CI where a
// privileged container created through ephemerd's fake Docker socket has its
// CNI veth attached to the wrong netns (or not at all), so processes inside
// only see lo and kubeadm fails with "unable to select an IP from lo".
//
// This is the focused, fast version: it uses an alpine container instead of
// kindest/node to exercise the same container-create + privileged + CNI-attach
// path without pulling a 1 GB systemd image. If this test fails, the dind
// network plumbing is broken regardless of the kindest/node specifics.
func TestE2E_KindInDind_NetnsWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping kind-in-dind e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	srv, _ := startDindForKindTest(t, "kind-netns")

	httpClient := dialDindUnix(srv.SocketPath())

	if err := dindPing(ctx, httpClient); err != nil {
		t.Fatalf("dind ping: %v", err)
	}

	// `kind` is the network kind itself creates via `docker network create`.
	if err := dindCreateNetwork(ctx, httpClient, "kind"); err != nil {
		t.Fatalf("create kind network: %v", err)
	}

	// Pull a small image we can exec into to inspect interfaces.
	if err := dindPullImage(ctx, httpClient, "docker.io/library/alpine:3.20"); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	cid, err := dindCreateContainer(ctx, httpClient, dindCreateOpts{
		Image:      "docker.io/library/alpine:3.20",
		Cmd:        []string{"sleep", "120"},
		Privileged: true,
		Network:    "kind",
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Logf("created container %s", cid[:12])

	defer func() {
		if rerr := dindRemoveContainer(context.Background(), httpClient, cid); rerr != nil {
			t.Logf("remove container: %v", rerr)
		}
	}()

	if err := dindStartContainer(ctx, httpClient, cid); err != nil {
		t.Fatalf("start container: %v", err)
	}
	t.Logf("started container %s", cid[:12])

	// Give CNI a moment — the veth should be wired before we exec.
	time.Sleep(500 * time.Millisecond)

	out, exit, err := dindExec(ctx, httpClient, cid, []string{"cat", "/proc/net/dev"})
	if err != nil {
		t.Fatalf("exec cat /proc/net/dev: %v", err)
	}
	if exit != 0 {
		t.Fatalf("cat /proc/net/dev exit=%d output=%q", exit, string(out))
	}
	t.Logf("/proc/net/dev:\n%s", string(out))

	ifaces := parseProcNetDev(out)
	t.Logf("interfaces inside container: %v", ifaces)

	hasNonLo := false
	for _, name := range ifaces {
		if name != "lo" {
			hasNonLo = true
			break
		}
	}
	if !hasNonLo {
		t.Fatalf("BUG REPRODUCED: container only sees loopback interface; CNI veth was not plumbed into netns. interfaces=%v", ifaces)
	}

	// Confirm the non-lo interface has a routable IPv4 address.
	out2, exit2, err := dindExec(ctx, httpClient, cid, []string{
		"sh", "-c",
		`for f in /sys/class/net/*; do iface=$(basename "$f"); [ "$iface" = "lo" ] && continue; ip -4 addr show "$iface" 2>/dev/null || ifconfig "$iface" 2>/dev/null; done`,
	})
	if err != nil {
		t.Fatalf("exec ip addr: %v", err)
	}
	t.Logf("non-loopback addrs (exit=%d):\n%s", exit2, string(out2))
	if !strings.Contains(string(out2), "inet ") {
		t.Fatalf("no inet address found on non-loopback interface; output=%q", string(out2))
	}

	// Verify the dind socket reports a non-loopback IP via docker inspect.
	// Before the fix, network_linux.go's setup() iterated result.Interfaces
	// in random map order and sometimes picked lo's 127.0.0.1, surfacing it
	// up to entry.IP / NetworkSettings.IPAddress.
	inspect, err := dindInspect(ctx, httpClient, cid)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	t.Logf("docker inspect reports IP=%q", inspect.NetworkSettings.IPAddress)
	if inspect.NetworkSettings.IPAddress == "" {
		t.Errorf("docker inspect returned empty NetworkSettings.IPAddress")
	}
	if strings.HasPrefix(inspect.NetworkSettings.IPAddress, "127.") {
		t.Errorf("docker inspect returned loopback IP %q (regression — should be from CNI bridge)",
			inspect.NetworkSettings.IPAddress)
	}
}

// TestE2E_KindInDind_KindestNode reproduces the CI failure mode by creating a
// kindest/node container through the fake Docker socket — exactly what `kind
// create cluster` does. The CI logs showed kubeadm failing with "unable to
// select an IP from lo" inside this image, suggesting the veth wasn't visible
// to processes inside the kindest/node systemd hierarchy.
//
// This test pulls a ~1 GB image. Set EPHEMERD_E2E_KIND_IMAGE to override the
// default image (e.g. to point at a locally cached tag).
func TestE2E_KindInDind_KindestNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping kindest/node e2e in short mode")
	}

	image := os.Getenv("EPHEMERD_E2E_KIND_IMAGE")
	if image == "" {
		// Pin to a kind v0.27 default tag with a digest. Without a digest,
		// containerd's resolver caches the manifest and we'd race against
		// upstream re-tags.
		image = "docker.io/kindest/node:v1.32.2"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	srv, _ := startDindForKindTest(t, "kind-node")

	httpClient := dialDindUnix(srv.SocketPath())

	if err := dindPing(ctx, httpClient); err != nil {
		t.Fatalf("dind ping: %v", err)
	}

	if err := dindCreateNetwork(ctx, httpClient, "kind"); err != nil {
		t.Fatalf("create kind network: %v", err)
	}

	t.Logf("pulling %s (this is large, ~1 GB)", image)
	if err := dindPullImage(ctx, httpClient, image); err != nil {
		t.Fatalf("pull %s: %v", image, err)
	}

	cid, err := dindCreateContainer(ctx, httpClient, dindCreateOpts{
		Image:      image,
		Privileged: true,
		Network:    "kind",
		Hostname:   "kind-control-plane",
		Tty:        true,
	})
	if err != nil {
		t.Fatalf("create kindest/node: %v", err)
	}
	t.Logf("created kindest/node %s", cid[:12])

	defer func() {
		if rerr := dindRemoveContainer(context.Background(), httpClient, cid); rerr != nil {
			t.Logf("remove kindest/node: %v", rerr)
		}
	}()

	if err := dindStartContainer(ctx, httpClient, cid); err != nil {
		t.Fatalf("start kindest/node: %v", err)
	}
	t.Logf("started kindest/node %s — waiting for systemd", cid[:12])

	// Give systemd ~30s to bring up its userspace; we then exec into the
	// container and check what kubeadm would see.
	time.Sleep(30 * time.Second)

	// Same check kubeadm does: list non-loopback v4 addresses on UP interfaces.
	out, exit, err := dindExec(ctx, httpClient, cid, []string{
		"sh", "-c",
		`ip -4 -o addr show 2>/dev/null | awk '$3=="inet" && $4!~/^127\./ {print $2, $4}'`,
	})
	if err != nil {
		t.Fatalf("exec ip addr: %v", err)
	}
	t.Logf("non-loopback v4 addrs inside kindest/node (exit=%d):\n%s", exit, string(out))

	if strings.TrimSpace(string(out)) == "" {
		// Repro hit. Dump more diagnostics.
		dump, _, _ := dindExec(ctx, httpClient, cid, []string{
			"sh", "-c",
			`echo "=== /proc/net/dev ==="; cat /proc/net/dev; echo "=== ip -a ==="; ip a 2>/dev/null; echo "=== /sys/class/net ==="; ls /sys/class/net`,
		})
		t.Logf("diagnostic dump:\n%s", string(dump))
		t.Fatalf("BUG REPRODUCED: kindest/node has no non-loopback IPv4 address; kubeadm would fail with 'unable to select an IP'")
	}

	// The actual CI repro indicator: kindest/node's entrypoint detects its
	// own IPv4 at boot via `getent ahostsv4 $(hostname)` and writes it into
	// /var/lib/kubelet/kubeadm-flags.env + manifests. If that detection
	// returns empty, kubeadm later fails with "unable to select an IP".
	// The detection output is the very first thing logged to stdout, so we
	// pull container logs and grep for the line.
	logs, err := dindContainerLogs(ctx, httpClient, cid)
	if err != nil {
		t.Fatalf("fetch container logs: %v", err)
	}
	if detected, line := findKindEntrypointIPv4(logs); detected == "" {
		t.Logf("container logs (first 4KB):\n%s", clip(logs, 4096))
		t.Fatalf("BUG REPRODUCED: kindest/node entrypoint detected an empty IPv4 address (line=%q); kubeadm init would fail with 'unable to select an IP from lo'", line)
	} else {
		t.Logf("kindest entrypoint detected IPv4 address: %s", detected)
		if strings.HasPrefix(detected, "127.") {
			t.Fatalf("BUG: kindest detected loopback IPv4 %q", detected)
		}
	}

	// Sanity: also verify the dind socket reports a non-loopback IP.
	inspect, err := dindInspect(ctx, httpClient, cid)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	t.Logf("docker inspect IP=%q", inspect.NetworkSettings.IPAddress)
	if strings.HasPrefix(inspect.NetworkSettings.IPAddress, "127.") {
		t.Errorf("docker inspect returned loopback IP %q", inspect.NetworkSettings.IPAddress)
	}
}

// --- shared setup helpers ---

// startDindForKindTest builds the per-test CNI manager + dind server on top of
// the package-shared containerd. Cleanup runs via t.Cleanup.
func startDindForKindTest(t *testing.T, name string) (*dind.Server, *networking.Manager) {
	t.Helper()

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI plugins: %v", err)
	}

	netDataDir := filepath.Join(sharedDataDir, "net-"+name)
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatalf("creating net data dir: %v", err)
	}

	netMgr, err := networking.New(networking.Config{
		DataDir:   netDataDir,
		CNIBinDir: cm.Dir(),
		Log:       sharedLog,
	})
	if err != nil {
		t.Fatalf("init networking: %v", err)
	}
	t.Cleanup(netMgr.Cleanup)

	if err := netMgr.InstallFirewallRules(); err != nil {
		sharedLog.Warn("install firewall rules", "error", err)
	}

	dindData := filepath.Join(sharedDataDir, "dind-"+name)
	if err := os.MkdirAll(dindData, 0o755); err != nil {
		t.Fatalf("creating dind data dir: %v", err)
	}

	srv, err := dind.New(dind.Config{
		JobID:   name,
		DataDir: dindData,
		Client:  sharedCtrd.Client(),
		Network: netMgr,
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("dind.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("dind.Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	return srv, netMgr
}

// --- HTTP-over-unix-socket client for the fake docker daemon ---

func dialDindUnix(sockPath string) *http.Client {
	return &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
}

func dindPing(ctx context.Context, c *http.Client) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ping status=%d body=%s", resp.StatusCode, body)
	}
	return nil
}

func dindCreateNetwork(ctx context.Context, c *http.Client, name string) error {
	body := map[string]any{
		"Name":   name,
		"Driver": "bridge",
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/networks/create", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil // already exists is fine
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("network create status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

func dindPullImage(ctx context.Context, c *http.Client, ref string) error {
	repo, tag, found := strings.Cut(ref, ":")
	if !found {
		tag = "latest"
	}
	u := fmt.Sprintf("http://docker/images/create?fromImage=%s&tag=%s", repo, tag)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("image pull status=%d body=%s", resp.StatusCode, b)
	}
	// Drain progress stream — pull is incomplete until the body is consumed.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("draining pull progress: %w", err)
	}
	return nil
}

type dindCreateOpts struct {
	Image      string
	Cmd        []string
	Hostname   string
	Privileged bool
	Network    string
	Tty        bool
	Env        []string
}

func dindCreateContainer(ctx context.Context, c *http.Client, opts dindCreateOpts) (string, error) {
	body := map[string]any{
		"Image": opts.Image,
		"Cmd":   opts.Cmd,
		"Tty":   opts.Tty,
		"Env":   opts.Env,
		"HostConfig": map[string]any{
			"Privileged":  opts.Privileged,
			"NetworkMode": opts.Network,
			"SecurityOpt": []string{"seccomp=unconfined"},
		},
	}
	if opts.Hostname != "" {
		body["Hostname"] = opts.Hostname
	}
	if opts.Network != "" {
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				opts.Network: map[string]any{},
			},
		}
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/containers/create", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("container create status=%d body=%s", resp.StatusCode, b)
	}
	var result struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding create response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("empty container id in response")
	}
	return result.ID, nil
}

func dindStartContainer(ctx context.Context, c *http.Client, id string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/containers/"+id+"/start", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("container start status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

func dindRemoveContainer(ctx context.Context, c *http.Client, id string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://docker/containers/"+id+"?force=1&v=1", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("container remove status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

func dindContainerLogs(ctx context.Context, c *http.Client, id string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+id+"/logs?stdout=1&stderr=1", nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("logs status=%d body=%s", resp.StatusCode, b)
	}
	return io.ReadAll(resp.Body)
}

// findKindEntrypointIPv4 pulls the IPv4 address out of the kindest/node
// entrypoint's startup log line: "INFO: detected IPv4 address: <ip>".
// Returns ("", "") if no line is found, or (ip, full-line) when matched.
// Returns ("", line) when the line exists but is empty — that's the bug.
func findKindEntrypointIPv4(logs []byte) (ipv4, fullLine string) {
	const marker = "detected IPv4 address:"
	for _, line := range strings.Split(string(logs), "\n") {
		idx := strings.Index(line, marker)
		if idx < 0 {
			continue
		}
		val := strings.TrimSpace(line[idx+len(marker):])
		val = strings.TrimRight(val, "\r")
		return val, line
	}
	return "", ""
}

func clip(data []byte, n int) string {
	if len(data) <= n {
		return string(data)
	}
	return string(data[:n]) + "...[truncated]"
}

type dindInspectResult struct {
	NetworkSettings struct {
		IPAddress string                  `json:"IPAddress"`
		Networks  map[string]struct {
			IPAddress string `json:"IPAddress"`
			Gateway   string `json:"Gateway"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func dindInspect(ctx context.Context, c *http.Client, id string) (dindInspectResult, error) {
	var out dindInspectResult
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+id+"/json", nil)
	resp, err := c.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("inspect status=%d body=%s", resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decoding inspect: %w", err)
	}
	return out, nil
}

// dindExec runs a command in a running container via the non-hijacked exec
// path (synchronous, output buffered). Returns combined output and exit code.
func dindExec(ctx context.Context, c *http.Client, cid string, cmd []string) ([]byte, int, error) {
	createBody := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          cmd,
	}
	data, _ := json.Marshal(createBody)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/containers/"+cid+"/exec", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("exec create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("exec create status=%d body=%s", resp.StatusCode, b)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, 0, fmt.Errorf("decoding exec create: %w", err)
	}

	startBody := map[string]any{"Detach": false, "Tty": false}
	startData, _ := json.Marshal(startBody)
	startReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/exec/"+created.ID+"/start", bytes.NewReader(startData))
	startReq.Header.Set("Content-Type", "application/json")
	startResp, err := c.Do(startReq)
	if err != nil {
		return nil, 0, fmt.Errorf("exec start: %w", err)
	}
	defer startResp.Body.Close()
	if startResp.StatusCode >= 400 {
		b, _ := io.ReadAll(startResp.Body)
		return nil, 0, fmt.Errorf("exec start status=%d body=%s", startResp.StatusCode, b)
	}

	out, err := io.ReadAll(startResp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("reading exec output: %w", err)
	}

	// Get exit code from /exec/{id}/json
	inspReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/exec/"+created.ID+"/json", nil)
	inspResp, err := c.Do(inspReq)
	if err != nil {
		return out, 0, fmt.Errorf("exec inspect: %w", err)
	}
	defer inspResp.Body.Close()
	var info struct {
		ExitCode int `json:"ExitCode"`
	}
	_ = json.NewDecoder(inspResp.Body).Decode(&info)
	return out, info.ExitCode, nil
}

// parseProcNetDev returns the list of interface names in /proc/net/dev output.
// First two lines are headers, remaining lines start with the iface name + ":".
func parseProcNetDev(data []byte) []string {
	var ifs []string
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if i < 2 { // skip header rows
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		ifs = append(ifs, strings.TrimSpace(name))
	}
	return ifs
}
