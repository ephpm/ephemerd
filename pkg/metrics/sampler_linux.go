//go:build linux

package metrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/containerd/containerd/api/types"
	cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
)

// DefaultContainerdNamespace is the namespace ephemerd's runtime uses for
// every job container — kept in sync with pkg/runtime. The Linux sampler
// applies this to its Sample context so the containerd Task.Metrics RPC
// resolves the right task without the caller having to thread a
// namespaced ctx through the gRPC stream handler.
const DefaultContainerdNamespace = "ephemerd"

// taskMetricsReader is the minimal subset of containerd.Task we need.
// containerd.Task already satisfies this — declaring it as an interface
// lets tests inject a fake without dragging in the full containerd
// client surface.
type taskMetricsReader interface {
	Metrics(ctx context.Context) (*types.Metric, error)
}

// LinuxSampler reads cgroupv2 stats for a running container by way of
// containerd's Task.Metrics RPC, plus optional netns-scoped network
// counters via netlink. The configured CPU + memory limits are constants
// for the container's lifetime and are baked in at construction.
type LinuxSampler struct {
	reader    taskMetricsReader
	namespace string // containerd namespace, applied to Sample's ctx
	cpuLimit  uint64
	memLimit  uint64
	netnsPath string // optional; empty means skip network sampling
	netReader networkReader
	log       *slog.Logger
	// netLogged keeps the once-per-container warning bound when the netns
	// reader keeps failing — see Sample.
	netLogged atomic.Bool
}

// networkReader abstracts the per-netns network-stats source so tests
// can inject canned counters without setting up a real netns.
type networkReader interface {
	ReadNetwork(netnsPath string) (rxBytes, txBytes uint64, err error)
}

// NewLinuxSampler builds a Sampler over a containerd Task. cpuLimit is in
// cores (0 = unlimited); memLimitBytes is in bytes (0 = unlimited). When
// netnsPath is non-empty, the sampler also reads per-namespace network
// counters via netlink on every Sample. Empty netnsPath disables network
// sampling (network bytes report as 0).
func NewLinuxSampler(reader taskMetricsReader, cpuLimit, memLimitBytes uint64) *LinuxSampler {
	return &LinuxSampler{
		reader:    reader,
		namespace: DefaultContainerdNamespace,
		cpuLimit:  cpuLimit,
		memLimit:  memLimitBytes,
		netReader: procNetDevReader{},
	}
}

// WithNamespace overrides the containerd namespace applied to Sample's
// context. Only callers that put containers in a non-default namespace
// need this.
func (s *LinuxSampler) WithNamespace(ns string) *LinuxSampler {
	s.namespace = ns
	return s
}

// WithLogger attaches a logger for non-fatal sampler diagnostics
// (currently: netns lookup failures, reported once per container).
func (s *LinuxSampler) WithLogger(log *slog.Logger) *LinuxSampler {
	s.log = log
	return s
}

// WithNetwork enables per-netns network sampling against the given
// namespace path (e.g. /var/run/netns/cni-xyz, /proc/<pid>/ns/net).
// Returns the sampler for chaining.
func (s *LinuxSampler) WithNetwork(netnsPath string) *LinuxSampler {
	s.netnsPath = netnsPath
	return s
}

// withNetworkReader is used by tests to swap the network reader.
func (s *LinuxSampler) withNetworkReader(r networkReader) *LinuxSampler {
	s.netReader = r
	return s
}

// Sample reads the latest cgroupv2 metrics. cgroup v1 is intentionally not
// supported — every kernel ephemerd ships runs cgroupv2.
func (s *LinuxSampler) Sample(ctx context.Context) (ContainerStats, error) {
	if s.reader == nil {
		return ContainerStats{}, errors.New("nil metrics reader")
	}
	if s.namespace != "" {
		ctx = namespaces.WithNamespace(ctx, s.namespace)
	}
	raw, err := s.reader.Metrics(ctx)
	if err != nil {
		return ContainerStats{}, fmt.Errorf("reading task metrics: %w", err)
	}
	if raw == nil || raw.Data == nil {
		return ContainerStats{}, errors.New("empty metrics payload")
	}

	decoded, err := typeurl.UnmarshalAny(raw.Data)
	if err != nil {
		return ContainerStats{}, fmt.Errorf("unmarshaling metrics payload (typeurl=%q): %w", raw.Data.GetTypeUrl(), err)
	}
	stats, ok := decoded.(*cgroupsv2.Metrics)
	if !ok {
		return ContainerStats{}, fmt.Errorf("unexpected metrics type %T (typeurl=%q); cgroupv1 not supported", decoded, raw.Data.GetTypeUrl())
	}

	out := ContainerStats{
		CPULimit:         s.cpuLimit,
		MemoryLimitBytes: s.memLimit,
	}
	if stats.CPU != nil {
		out.CPUUsageNanos = stats.CPU.UsageUsec * 1000
	}
	if stats.Memory != nil {
		out.MemoryBytes = stats.Memory.Usage
		out.MemoryAnonBytes = stats.Memory.Anon
		// cgroupv2 reports "max" as math.MaxUint64 when unlimited. If the
		// caller didn't pass an explicit cap, surface the kernel-reported
		// one when present.
		if stats.Memory.UsageLimit != 0 && stats.Memory.UsageLimit != math.MaxUint64 && out.MemoryLimitBytes == 0 {
			out.MemoryLimitBytes = stats.Memory.UsageLimit
		}
	}

	if s.netnsPath != "" && s.netReader != nil {
		rx, tx, err := s.netReader.ReadNetwork(s.netnsPath)
		if err != nil {
			// Don't fail the whole sample — CPU/memory are still useful
			// when a transient netns lookup fails (container teardown
			// race, missing CAP_SYS_ADMIN, etc.). Leave network at 0;
			// the counter Add will be a no-op. Log once per container so
			// a misconfigured netns path is visible without spamming.
			if s.log != nil && s.netLogged.CompareAndSwap(false, true) {
				s.log.Warn("network sampling failed; rx/tx will report 0 for this container",
					"netns_path", s.netnsPath, "error", err)
			}
			return out, nil //nolint:nilerr // network sample failure is non-fatal; CPU/memory still flow
		}
		// Log first successful read at INFO so operators can see network
		// sampling is wired correctly without grepping for absence.
		if s.log != nil && s.netLogged.CompareAndSwap(false, true) {
			s.log.Info("network sampling active", "netns_path", s.netnsPath, "rx_bytes", rx, "tx_bytes", tx)
		}
		out.NetworkRxBytes = rx
		out.NetworkTxBytes = tx
	}
	return out, nil
}

// procNetDevReader reads per-link statistics by parsing
// /proc/<pid>/net/dev — the kernel exposes one row per interface in the
// netns owning that pid, including rx_bytes (column 1) and tx_bytes
// (column 9). Switched to this from netlink after observing
// "protocol not supported" on the embedded virt kernel — netlink
// (NETLINK_ROUTE) isn't compiled into every ephemerd-supported kernel,
// but /proc is universal.
type procNetDevReader struct{}

// netnsPidRE extracts the pid from a /proc/<pid>/ns/net path. We use the
// pid to read /proc/<pid>/net/dev, which exposes the netns-scoped
// interface table.
var netnsPidRE = regexp.MustCompile(`^/proc/(\d+)/ns/net$`)

func (procNetDevReader) ReadNetwork(netnsPath string) (uint64, uint64, error) {
	m := netnsPidRE.FindStringSubmatch(netnsPath)
	if len(m) != 2 {
		return 0, 0, fmt.Errorf("netns path %q does not match /proc/<pid>/ns/net", netnsPath)
	}
	procPath := "/proc/" + m[1] + "/net/dev"
	f, err := os.Open(procPath) //nolint:gosec // procPath is composed from a validated pid
	if err != nil {
		return 0, 0, fmt.Errorf("opening %s: %w", procPath, err)
	}
	defer func() { _ = f.Close() }()

	var rx, tx uint64
	scanner := bufio.NewScanner(f)
	// First two lines are headers ("Inter-|...", "face |...") — skip them.
	for i := 0; i < 2 && scanner.Scan(); i++ {
	}
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colon])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		// /proc/net/dev row format (one whitespace-separated number per
		// field): rx: bytes packets errs drop fifo frame compressed
		// multicast; tx: bytes packets errs drop fifo colls carrier
		// compressed. So bytes are fields[0] (rx) and fields[8] (tx).
		if len(fields) < 16 {
			continue
		}
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			rx += v
		}
		if v, err := strconv.ParseUint(fields[8], 10, 64); err == nil {
			tx += v
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading %s: %w", procPath, err)
	}
	return rx, tx, nil
}
