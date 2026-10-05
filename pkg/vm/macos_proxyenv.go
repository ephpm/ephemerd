package vm

import (
	"regexp"
	"strings"

	"github.com/ephpm/ephemerd/pkg/proxies"
)

// Cache-proxy env for macOS VM jobs.
//
// Container jobs get the cache-proxy env (GOPROXY, RUSTUP_DIST_SERVER,
// GHREL_PROXY, ...) from the container runtime. A macOS VM job has no
// container: the GitHub runner is started over SSH inside a fresh macOS VM, and
// until this existed nothing put that env anywhere the runner could see it. So
// every crate, module, toolchain and release asset a macOS job fetched went
// straight to the internet. On this fleet's Mac — which runs almost nothing but
// macOS VM jobs — the caches stayed at 0 bytes after the proxies had been fixed
// to advertise a reachable address, and the node kept pulling ~130 GB/day.
//
// The env reaches the job through the runner's own .env file, which the runner
// loads into every job's environment at startup. It is staged in two steps so
// that the reachability check runs where it matters — inside the guest:
//
//  1. macOSProxyEnvScript runs as its own bounded SSH command BEFORE the runner
//     starts. It writes the env to a staging file only if every proxy address
//     answers from inside the VM, and reports which way it went.
//  2. The runner setup script appends the staged file to the runner's .env,
//     if it exists, right before starting the runner.
//
// All-or-nothing on purpose. Go falls back to direct on an unreachable GOPROXY
// ("|direct"), but cargo and rustup do not — a dead RUSTUP_DIST_SERVER fails the
// build. A missing proxy costs bandwidth; a black-holed one costs a build.

// macOSProxyEnvStage is where the verified env is staged inside the guest. A
// per-job VM boots from a fresh clone, so nothing stale can be sitting there.
const macOSProxyEnvStage = "/tmp/ephemerd-runner.env"

// safeHostPort limits what may be interpolated into the probe loop. The values
// come from ephemerd's own config, but they are pasted into a shell script, so
// anything that is not plainly a host:port is refused rather than quoted.
var safeHostPort = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]+$`)

// heredocDelim terminates the quoted heredoc that carries the env. A quoted
// delimiter disables all expansion, so values are written byte-for-byte.
const heredocDelim = "EPHEMERD_PROXY_ENV"

// macOSProxyEnvScript returns the guest shell script that stages env at stage
// (macOSProxyEnvStage in production), or "" when there is nothing to stage. It
// prints exactly one status line: "staged <n>" or "unreachable <host:port>".
func macOSProxyEnvScript(env []string, stage string) string {
	var lines []string
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if !ok || k == "" || strings.ContainsAny(e, "\r\n") || e == heredocDelim {
			continue
		}
		lines = append(lines, e)
	}
	if len(lines) == 0 {
		return ""
	}

	// Every URL-looking element must yield a probeable address. EnvHostPorts
	// silently skips what it cannot parse, which would let an unparseable URL
	// be staged without ever being checked — so check element by element.
	var probes []string
	seen := map[string]bool{}
	for _, l := range lines {
		_, v, _ := strings.Cut(l, "=")
		for _, el := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '|' }) {
			if !strings.Contains(el, "://") {
				continue // "direct", "off", "sparse": not an address
			}
			hps := proxies.EnvHostPorts([]string{"X=" + el})
			if len(hps) != 1 || !safeHostPort.MatchString(hps[0]) {
				// Cannot verify it, so cannot advertise it — or anything else.
				return ""
			}
			if !seen[hps[0]] {
				seen[hps[0]] = true
				probes = append(probes, "'"+hps[0]+"'")
			}
		}
	}

	var b strings.Builder
	b.WriteString("STAGE='" + stage + "'\n")
	b.WriteString(`rm -f "$STAGE" "$STAGE.new"` + "\n")
	b.WriteString(`cat > "$STAGE.new" <<'` + heredocDelim + "'\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString(heredocDelim + "\n")
	if len(probes) > 0 {
		b.WriteString("for hp in " + strings.Join(probes, " ") + "; do\n")
		// Any HTTP answer at all proves the address is reachable; only a
		// connection failure or timeout makes curl exit non-zero without -f.
		b.WriteString(`  if ! curl -s -o /dev/null --connect-timeout 3 --max-time 5 "http://$hp/"; then` + "\n")
		b.WriteString(`    rm -f "$STAGE.new"; echo "unreachable $hp"; exit 0` + "\n")
		b.WriteString("  fi\n")
		b.WriteString("done\n")
	}
	b.WriteString(`mv "$STAGE.new" "$STAGE"` + "\n")
	b.WriteString(`echo "staged $(wc -l < "$STAGE" | tr -d ' ')"` + "\n")
	return b.String()
}

// macOSProxyEnvStaged interprets the script's status line: true only when the
// env was staged.
func macOSProxyEnvStaged(out string) bool {
	return strings.HasPrefix(strings.TrimSpace(out), "staged ")
}
