package runnerbusy

import "testing"

// TestIsWorkerProcess pins the one rule that decides "a job is running
// here". It has to cover three different shapes of process identifier —
// Linux argv[0], Linux comm, Windows HCS ImageName — and it must NOT
// match the listener, which is alive for the whole life of an idle runner
// and would make every runner look permanently busy.
func TestIsWorkerProcess(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"linux argv0, official image", "/home/runner/bin/Runner.Worker", true},
		{"linux argv0, mounted runner", "/actions-runner/bin/Runner.Worker", true},
		{"linux comm", "Runner.Worker", true},
		{"windows hcs image name", "Runner.Worker.exe", true},
		{"windows full path", `C:\actions-runner\bin\Runner.Worker.exe`, true},
		{"case insensitive", "runner.worker.exe", true},
		{"surrounding whitespace", "  Runner.Worker\n", true},

		{"listener is not a worker", "/home/runner/bin/Runner.Listener", false},
		{"listener exe is not a worker", "Runner.Listener.exe", false},
		{"run.sh is not a worker", "/home/runner/run.sh", false},
		{"a job step is not a worker", "/usr/bin/bash", false},
		{"substring does not match", "Runner.WorkerHelper", false},
		{"prefix does not match", "My.Runner.Worker", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsWorkerProcess(tt.in); got != tt.want {
				t.Errorf("IsWorkerProcess(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestStateString pins the strings that end up in log lines and in the
// orphan-sweep metric's verdict label.
func TestStateString(t *testing.T) {
	tests := []struct {
		in   State
		want string
	}{
		{Unknown, "unknown"},
		{Idle, "idle"},
		{Busy, "busy"},
		{State(42), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestZeroValueIsUnknown pins the fail-safe default: a State nobody set
// must read as "could not determine", never as idle. Every probe returns
// the zero value on its error paths.
func TestZeroValueIsUnknown(t *testing.T) {
	var s State
	if s != Unknown {
		t.Fatalf("zero State = %v, want Unknown — an unset verdict must never read as idle", s)
	}
}
