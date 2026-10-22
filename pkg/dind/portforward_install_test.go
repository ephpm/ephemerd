package dind

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestInstallPortForwards_NoIPIsAnError pins the fix for ephpm run
// 39690226239: a postgres sibling whose CNI setup failed started with no IP,
// its -p binding was skipped without a word, and the job waited minutes on a
// port that never opened. A binding that cannot be honoured must now be an
// error the start handler turns into a failed `docker start`.
func TestInstallPortForwards_NoIPIsAnError(t *testing.T) {
	s := &Server{
		runnerNetNS: "/proc/self/ns/net",
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	entry := &containerEntry{
		ID: "27bdc8173467b088e1a75928c0c067a9",
		PortBindings: map[string][]portBinding{
			"5432/tcp": {{HostIP: "127.0.0.1", HostPort: "26241"}},
		},
	}

	err := s.installPortForwards(entry)
	if err == nil {
		t.Fatal("installPortForwards with no container IP returned nil; want an error")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:26241") {
		t.Errorf("error %q does not name the binding it could not publish", err)
	}
	if len(entry.PortForwards) != 0 {
		t.Errorf("got %d forwards installed, want 0", len(entry.PortForwards))
	}
}

// TestInstallPortForwards_NothingToPublish confirms the no-op cases stay
// no-ops: no bindings, a binding with no host port (docker picks one), and a
// server that was never told the runner's netns (Windows siblings).
func TestInstallPortForwards_NothingToPublish(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name  string
		netns string
		ports map[string][]portBinding
	}{
		{"no bindings", "/proc/self/ns/net", nil},
		{"no host port", "/proc/self/ns/net", map[string][]portBinding{"80/tcp": {{HostIP: "127.0.0.1"}}}},
		{"udp only", "/proc/self/ns/net", map[string][]portBinding{"53/udp": {{HostIP: "127.0.0.1", HostPort: "5353"}}}},
		{"no runner netns", "", map[string][]portBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8080"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{runnerNetNS: tc.netns, log: log}
			entry := &containerEntry{ID: "0123456789abcdef", PortBindings: tc.ports}
			if err := s.installPortForwards(entry); err != nil {
				t.Fatalf("installPortForwards: %v", err)
			}
		})
	}
}
