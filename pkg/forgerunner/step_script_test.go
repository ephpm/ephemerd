package forgerunner

import (
	"strings"
	"testing"
)

// --- resolveShell tests ---

func TestResolveShell_KnownShells(t *testing.T) {
	tests := []struct {
		shell, wantBin, wantExt string
		wantArgsContains        string
	}{
		{"bash", "bash", ".sh", "pipefail"},
		{"BASH", "bash", ".sh", "pipefail"}, // case insensitive lookup, but bash branch returns literal "bash"
		{"sh", "sh", ".sh", "-e"},
		// pwsh/powershell branch returns shell verbatim (preserves case)
		// because docker-mode setups sometimes need the original casing.
		{"pwsh", "pwsh", ".ps1", "-NoProfile"},
		{"powershell", "powershell", ".ps1", "-ExecutionPolicy"},
		{"cmd", "cmd", ".cmd", "/C"},
		{"python", "python", ".py", ""},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			bin, args, ext := resolveShell(tt.shell)
			if bin != tt.wantBin {
				t.Errorf("bin = %q, want %q", bin, tt.wantBin)
			}
			if ext != tt.wantExt {
				t.Errorf("ext = %q, want %q", ext, tt.wantExt)
			}
			if tt.wantArgsContains != "" {
				joined := strings.Join(args, " ")
				if !strings.Contains(joined, tt.wantArgsContains) {
					t.Errorf("args %q missing %q", joined, tt.wantArgsContains)
				}
			}
		})
	}
}

func TestResolveShell_CustomShell(t *testing.T) {
	bin, args, ext := resolveShell("zsh")
	if bin != "zsh" {
		t.Errorf("bin = %q, want zsh", bin)
	}
	if ext != ".sh" {
		t.Errorf("ext = %q, want .sh", ext)
	}
	if len(args) != 0 {
		t.Errorf("custom shell args should be empty, got %v", args)
	}
}

func TestResolveShell_DefaultEmpty(t *testing.T) {
	// Empty shell triggers defaultShell() which is platform-specific.
	bin, _, ext := resolveShell("")
	if bin == "" {
		t.Error("default bin should not be empty")
	}
	if ext == "" {
		t.Error("default ext should not be empty")
	}
}

// --- processOutput tests ---

// TestProcessOutput_PassesThroughPlainLines verifies non-command lines are
// added verbatim.
func TestProcessOutput_PassesThroughPlainLines(t *testing.T) {
	input := "hello\nworld\n"
	rep := NewLogReporter(nil, 0, nil)

	processOutput(strings.NewReader(input), rep, nil)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.total != 2 {
		t.Errorf("line count = %d, want 2", rep.total)
	}
	if len(rep.rows) != 2 {
		t.Fatalf("row count = %d, want 2", len(rep.rows))
	}
	if rep.rows[0].Content != "hello" {
		t.Errorf("rows[0] = %q, want hello", rep.rows[0].Content)
	}
	if rep.rows[1].Content != "world" {
		t.Errorf("rows[1] = %q, want world", rep.rows[1].Content)
	}
}

func TestProcessOutput_AnnotationCommands(t *testing.T) {
	input := "::error::oops\n::warning::heads up\n::notice::fyi\n::debug::trace\n"
	rep := NewLogReporter(nil, 0, nil)

	processOutput(strings.NewReader(input), rep, nil)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.total != 4 {
		t.Fatalf("line count = %d, want 4", rep.total)
	}

	wantPrefixes := []string{"[error]", "[warning]", "[notice]", "[debug]"}
	for i, row := range rep.rows {
		if !strings.HasPrefix(row.Content, wantPrefixes[i]) {
			t.Errorf("rows[%d] = %q, want prefix %q", i, row.Content, wantPrefixes[i])
		}
	}
}

func TestProcessOutput_GroupAndEndgroup(t *testing.T) {
	input := "::group::My Group\nline 1\n::endgroup::\n"
	rep := NewLogReporter(nil, 0, nil)

	processOutput(strings.NewReader(input), rep, nil)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.total != 3 {
		t.Fatalf("line count = %d, want 3", rep.total)
	}
	if rep.rows[0].Content != "##[group]My Group" {
		t.Errorf("rows[0] = %q, want ##[group]My Group", rep.rows[0].Content)
	}
	if rep.rows[1].Content != "line 1" {
		t.Errorf("rows[1] = %q, want line 1", rep.rows[1].Content)
	}
	if rep.rows[2].Content != "##[endgroup]" {
		t.Errorf("rows[2] = %q, want ##[endgroup]", rep.rows[2].Content)
	}
}

func TestProcessOutput_AddMaskRegistersWithMasker(t *testing.T) {
	input := "::add-mask::topsecret\nuser said: topsecret\n"
	masker := NewSecretMasker(nil)
	// Pass masker to BOTH the reporter (so AddLine redacts) and to
	// processOutput (so add-mask registers the secret with the same
	// masker that AddLine consults). This mirrors executor.Run's wiring.
	rep := NewLogReporter(nil, 0, masker)

	processOutput(strings.NewReader(input), rep, masker)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	// add-mask is consumed (does not generate a row); only the second
	// line is logged. The masker should redact the secret.
	if rep.total != 1 {
		t.Fatalf("line count = %d, want 1 (add-mask should not emit)", rep.total)
	}
	got := rep.rows[0].Content
	if strings.Contains(got, "topsecret") {
		t.Errorf("masker did not redact: got %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("expected redaction marker in %q", got)
	}
}

func TestProcessOutput_LegacyCommandsIgnored(t *testing.T) {
	// Legacy file-based commands should be silently dropped.
	input := "::set-output::name=foo::value=bar\n::set-env::name=X::value=Y\n::add-path::extra\nnormal line\n"
	rep := NewLogReporter(nil, 0, nil)

	processOutput(strings.NewReader(input), rep, nil)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.total != 1 {
		t.Fatalf("line count = %d, want 1 (only the normal line)", rep.total)
	}
	if rep.rows[0].Content != "normal line" {
		t.Errorf("rows[0] = %q, want normal line", rep.rows[0].Content)
	}
}

func TestProcessOutput_UnknownCommandPassesThrough(t *testing.T) {
	input := "::made-up-command::value\n"
	rep := NewLogReporter(nil, 0, nil)

	processOutput(strings.NewReader(input), rep, nil)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.total != 1 {
		t.Fatalf("line count = %d, want 1", rep.total)
	}
	// Unknown command — the original line gets logged verbatim.
	if rep.rows[0].Content != "::made-up-command::value" {
		t.Errorf("rows[0] = %q, want verbatim original line", rep.rows[0].Content)
	}
}

func TestProcessOutput_EmptyInput(t *testing.T) {
	rep := NewLogReporter(nil, 0, nil)
	processOutput(strings.NewReader(""), rep, nil)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.total != 0 {
		t.Errorf("line count = %d, want 0", rep.total)
	}
}
