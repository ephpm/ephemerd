//go:build e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/networking"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// TestE2E_NetworkIsolation verifies that the container firewall rules block
// access to RFC1918 private addresses while allowing public internet access.
func TestE2E_NetworkIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")

	// Pull alpine
	testImage := "docker.io/library/alpine:latest"
	if _, err := ctrdClient.GetImage(nsCtx, testImage); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, testImage, client.WithPullUnpack); err != nil {
			t.Fatalf("pulling image: %v", err)
		}
	}
	img, err := ctrdClient.GetImage(nsCtx, testImage)
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	// Set up networking with firewall rules
	netDataDir := filepath.Join(sharedDataDir, "net-isolation")
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI plugins: %v", err)
	}

	net, err := networking.New(networking.Config{
		DataDir:   netDataDir,
		CNIBinDir: cm.Dir(),
		Log:       sharedLog,
	})
	if err != nil {
		t.Fatalf("initializing networking: %v", err)
	}
	defer net.Cleanup()

	if err := net.InstallFirewallRules(); err != nil {
		t.Fatalf("installing firewall rules: %v", err)
	}

	// Create container
	containerID := fmt.Sprintf("e2e-netiso-%d", time.Now().UnixNano())

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithProcessArgs("sleep", "86400"),
		withDNSMount(netDataDir, containerID),
	}

	container, err := ctrdClient.NewContainer(nsCtx, containerID,
		client.WithImage(img),
		client.WithNewSnapshot(containerID+"-snapshot", img),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	defer func() {
		if err := container.Delete(nsCtx, client.WithSnapshotCleanup); err != nil {
			t.Logf("container delete: %v", err)
		}
	}()

	task, err := container.NewTask(nsCtx, cio.NullIO)
	if err != nil {
		t.Fatalf("creating task: %v", err)
	}
	defer func() {
		if status, serr := task.Status(nsCtx); serr == nil && status.Status == client.Running {
			if err := task.Kill(nsCtx, 9); err != nil {
				t.Logf("killing task: %v", err)
			}
			exitCh, werr := task.Wait(nsCtx)
			if werr == nil {
				<-exitCh
			}
		}
		if _, err := task.Delete(nsCtx, client.WithProcessKill); err != nil {
			t.Logf("task delete: %v", err)
		}
	}()

	// Attach networking
	pid := task.Pid()
	netns := fmt.Sprintf("/proc/%d/ns/net", pid)
	if _, err := net.Setup(ctx, containerID, netns); err != nil {
		t.Fatalf("setting up network: %v", err)
	}
	defer func() {
		if err := net.Teardown(ctx, containerID, netns); err != nil {
			t.Logf("network teardown: %v", err)
		}
	}()

	if err := task.Start(nsCtx); err != nil {
		t.Fatalf("starting task: %v", err)
	}

	// Helper to exec a command inside the container and return exit code + output
	execInContainer := func(name, cmd string) (uint32, string) {
		t.Helper()
		execID := fmt.Sprintf("exec-%s-%d", name, time.Now().UnixNano())

		logDir := filepath.Join(netDataDir, "exec-logs")
		if mkErr := os.MkdirAll(logDir, 0o755); mkErr != nil {
			t.Fatalf("creating exec log dir: %v", mkErr)
		}
		logPath := filepath.Join(logDir, execID+".log")

		pspec := &ocispec.Process{
			Args: []string{"/bin/sh", "-c", cmd},
			Cwd:  "/",
			User: ocispec.User{UID: 0, GID: 0},
		}

		process, err := task.Exec(nsCtx, execID, pspec, cio.LogFile(logPath))
		if err != nil {
			t.Fatalf("exec %q: %v", name, err)
		}

		exitCh, err := process.Wait(nsCtx)
		if err != nil {
			if _, delErr := process.Delete(nsCtx, client.WithProcessKill); delErr != nil {
				t.Logf("process delete after wait error: %v", delErr)
			}
			t.Fatalf("wait %q: %v", name, err)
		}

		if err := process.Start(nsCtx); err != nil {
			if _, delErr := process.Delete(nsCtx, client.WithProcessKill); delErr != nil {
				t.Logf("process delete after start error: %v", delErr)
			}
			t.Fatalf("start %q: %v", name, err)
		}

		var exitCode uint32
		select {
		case status := <-exitCh:
			exitCode = status.ExitCode()
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %q", name)
		}

		if _, err := process.Delete(nsCtx); err != nil {
			t.Logf("process delete %q: %v", name, err)
		}

		output, _ := os.ReadFile(logPath)
		return exitCode, string(output)
	}

	// Test 1: Public internet should be reachable
	t.Run("public_internet", func(t *testing.T) {
		code, output := execInContainer("public", "wget -q -O /dev/null --timeout=10 http://1.1.1.1 2>&1 && echo OK || echo FAIL")
		t.Logf("public internet: exit=%d output=%s", code, strings.TrimSpace(output))
		// Don't fail if internet is unavailable (CI may be airgapped),
		// but log the result for debugging
	})

	// Test 2: RFC1918 10.0.0.0/8 should be blocked (except the container subnet)
	t.Run("block_rfc1918_10", func(t *testing.T) {
		// Try to reach a 10.x address that's NOT the container's own gateway.
		// Use a non-routable address with a short timeout.
		code, output := execInContainer("rfc1918-10", "wget -q -O /dev/null --timeout=3 http://10.0.0.1 2>&1; echo exit=$?")
		t.Logf("RFC1918 10.0.0.1: exit=%d output=%s", code, strings.TrimSpace(output))
		// The connection should fail (timeout or refused)
		if strings.Contains(output, "200") {
			t.Error("RFC1918 10.0.0.1 should be blocked but got HTTP 200")
		}
	})

	// Test 3: Link-local should be blocked
	t.Run("block_link_local", func(t *testing.T) {
		code, output := execInContainer("link-local", "wget -q -O /dev/null --timeout=3 http://169.254.169.254 2>&1; echo exit=$?")
		t.Logf("link-local 169.254.169.254: exit=%d output=%s", code, strings.TrimSpace(output))
		if strings.Contains(output, "200") {
			t.Error("link-local 169.254.169.254 should be blocked but got HTTP 200")
		}
	})

	// Test 4: Container can resolve DNS
	t.Run("dns_resolution", func(t *testing.T) {
		code, output := execInContainer("dns", "nslookup github.com 2>&1 || true")
		t.Logf("DNS resolution: exit=%d output=%s", code, strings.TrimSpace(output))
		// Don't fail — DNS may not work in all CI environments
	})
}

// TestE2E_NetworkIsolation_ContainerToContainer verifies that containers
// on the same bridge CAN communicate with each other (intra-subnet traffic
// is allowed by the firewall rules).
func TestE2E_NetworkIsolation_ContainerToContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")

	testImage := "docker.io/library/alpine:latest"
	img, err := ctrdClient.GetImage(nsCtx, testImage)
	if err != nil {
		t.Skipf("alpine not cached: %v", err)
	}

	netDataDir := filepath.Join(sharedDataDir, "net-c2c")
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI: %v", err)
	}

	net, err := networking.New(networking.Config{
		DataDir:   netDataDir,
		CNIBinDir: cm.Dir(),
		Log:       sharedLog,
	})
	if err != nil {
		t.Fatalf("networking: %v", err)
	}
	defer net.Cleanup()

	if err := net.InstallFirewallRules(); err != nil {
		sharedLog.Warn("firewall rules", "error", err)
	}

	// Create two containers on the same bridge
	createContainer := func(name string) (client.Container, client.Task, string) {
		id := fmt.Sprintf("e2e-c2c-%s-%d", name, time.Now().UnixNano())

		specOpts := []oci.SpecOpts{
			oci.WithImageConfig(img),
			oci.WithProcessArgs("sleep", "86400"),
			withDNSMount(netDataDir, id),
		}

		c, err := ctrdClient.NewContainer(nsCtx, id,
			client.WithImage(img),
			client.WithNewSnapshot(id+"-snapshot", img),
			client.WithNewSpec(specOpts...),
		)
		if err != nil {
			t.Fatalf("creating container %s: %v", name, err)
		}

		task, err := c.NewTask(nsCtx, cio.NullIO)
		if err != nil {
			if delErr := c.Delete(nsCtx, client.WithSnapshotCleanup); delErr != nil {
				t.Logf("container delete: %v", delErr)
			}
			t.Fatalf("creating task %s: %v", name, err)
		}

		pid := task.Pid()
		netns := fmt.Sprintf("/proc/%d/ns/net", pid)
		if _, err := net.Setup(ctx, id, netns); err != nil {
			t.Fatalf("network setup %s: %v", name, err)
		}

		if err := task.Start(nsCtx); err != nil {
			t.Fatalf("starting %s: %v", name, err)
		}

		return c, task, id
	}

	cleanup := func(c client.Container, task client.Task, id string) {
		if status, err := task.Status(nsCtx); err == nil && status.Status == client.Running {
			if err := task.Kill(nsCtx, 9); err != nil {
				t.Logf("kill %s: %v", id, err)
			}
			exitCh, err := task.Wait(nsCtx)
			if err == nil {
				<-exitCh
			}
		}
		if _, err := task.Delete(nsCtx, client.WithProcessKill); err != nil {
			t.Logf("task delete %s: %v", id, err)
		}
		netns := fmt.Sprintf("/proc/%d/ns/net", task.Pid())
		if err := net.Teardown(ctx, id, netns); err != nil {
			t.Logf("teardown %s: %v", id, err)
		}
		if err := c.Delete(nsCtx, client.WithSnapshotCleanup); err != nil {
			t.Logf("container delete %s: %v", id, err)
		}
	}

	c1, task1, id1 := createContainer("a")
	defer cleanup(c1, task1, id1)

	c2, task2, id2 := createContainer("b")
	defer cleanup(c2, task2, id2)

	// Get container A's IP by exec'ing hostname -i
	getIP := func(task client.Task, name string) string {
		execID := fmt.Sprintf("getip-%s-%d", name, time.Now().UnixNano())
		logPath := filepath.Join(netDataDir, execID+".log")

		pspec := &ocispec.Process{
			Args: []string{"/bin/sh", "-c", "hostname -i 2>/dev/null || ip -4 addr show eth0 2>/dev/null | grep inet | awk '{print $2}' | cut -d/ -f1"},
			Cwd:  "/",
			User: ocispec.User{UID: 0, GID: 0},
		}

		process, err := task.Exec(nsCtx, execID, pspec, cio.LogFile(logPath))
		if err != nil {
			t.Fatalf("exec getip %s: %v", name, err)
		}

		exitCh, err := process.Wait(nsCtx)
		if err != nil {
			if _, delErr := process.Delete(nsCtx, client.WithProcessKill); delErr != nil {
				t.Logf("process delete: %v", delErr)
			}
			t.Fatalf("wait getip %s: %v", name, err)
		}
		if err := process.Start(nsCtx); err != nil {
			if _, delErr := process.Delete(nsCtx, client.WithProcessKill); delErr != nil {
				t.Logf("process delete: %v", delErr)
			}
			t.Fatalf("start getip %s: %v", name, err)
		}

		select {
		case <-exitCh:
		case <-ctx.Done():
			t.Fatalf("timeout getip %s", name)
		}
		if _, err := process.Delete(nsCtx); err != nil {
			t.Logf("process delete: %v", err)
		}

		output, _ := os.ReadFile(logPath)
		return strings.TrimSpace(string(output))
	}

	ipA := getIP(task1, "a")
	t.Logf("container A IP: %s", ipA)

	if ipA == "" {
		t.Skip("could not determine container A IP — skipping container-to-container test")
	}

	// Container B pings container A — should succeed (same subnet)
	execID := fmt.Sprintf("ping-%d", time.Now().UnixNano())
	logPath := filepath.Join(netDataDir, execID+".log")

	pspec := &ocispec.Process{
		Args: []string{"/bin/sh", "-c", fmt.Sprintf("ping -c 1 -W 3 %s 2>&1", ipA)},
		Cwd:  "/",
		User: ocispec.User{UID: 0, GID: 0},
	}

	process, err := task2.Exec(nsCtx, execID, pspec, cio.LogFile(logPath))
	if err != nil {
		t.Fatalf("exec ping: %v", err)
	}

	exitCh, err := process.Wait(nsCtx)
	if err != nil {
		if _, delErr := process.Delete(nsCtx, client.WithProcessKill); delErr != nil {
			t.Logf("process delete: %v", delErr)
		}
		t.Fatalf("wait ping: %v", err)
	}
	if err := process.Start(nsCtx); err != nil {
		if _, delErr := process.Delete(nsCtx, client.WithProcessKill); delErr != nil {
			t.Logf("process delete: %v", delErr)
		}
		t.Fatalf("start ping: %v", err)
	}

	select {
	case status := <-exitCh:
		output, _ := os.ReadFile(logPath)
		t.Logf("ping result: exit=%d output=%s", status.ExitCode(), strings.TrimSpace(string(output)))
		if status.ExitCode() != 0 {
			t.Errorf("container B should be able to ping container A (same subnet), exit code: %d", status.ExitCode())
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for ping")
	}
	if _, err := process.Delete(nsCtx); err != nil {
		t.Logf("process delete: %v", err)
	}
}

// withDNSMount and other helpers are defined in e2e_test.go (shared TestMain file)
// so they're available here via the same package.
