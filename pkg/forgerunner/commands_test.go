package forgerunner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		line    string
		want    *Command
		isCmd   bool
	}{
		{
			line:  "::set-output name=foo::bar",
			want:  &Command{Name: "set-output", Params: map[string]string{"name": "foo"}, Value: "bar"},
			isCmd: true,
		},
		{
			line:  "::error file=main.go,line=10::something broke",
			want:  &Command{Name: "error", Params: map[string]string{"file": "main.go", "line": "10"}, Value: "something broke"},
			isCmd: true,
		},
		{
			line:  "::add-mask::my-secret",
			want:  &Command{Name: "add-mask", Params: map[string]string{}, Value: "my-secret"},
			isCmd: true,
		},
		{
			line:  "::group::Build",
			want:  &Command{Name: "group", Params: map[string]string{}, Value: "Build"},
			isCmd: true,
		},
		{
			line:  "::endgroup::",
			want:  &Command{Name: "endgroup", Params: map[string]string{}, Value: ""},
			isCmd: true,
		},
		{
			line:  "just a regular log line",
			isCmd: false,
		},
		{
			line:  ":: not a command",
			isCmd: false,
		},
		{
			line:  "",
			isCmd: false,
		},
	}

	for _, tt := range tests {
		cmd, ok := ParseCommand(tt.line)
		if ok != tt.isCmd {
			t.Errorf("ParseCommand(%q): isCmd = %v, want %v", tt.line, ok, tt.isCmd)
			continue
		}
		if !ok {
			continue
		}
		if cmd.Name != tt.want.Name {
			t.Errorf("ParseCommand(%q): Name = %q, want %q", tt.line, cmd.Name, tt.want.Name)
		}
		if cmd.Value != tt.want.Value {
			t.Errorf("ParseCommand(%q): Value = %q, want %q", tt.line, cmd.Value, tt.want.Value)
		}
		for k, v := range tt.want.Params {
			if cmd.Params[k] != v {
				t.Errorf("ParseCommand(%q): Params[%s] = %q, want %q", tt.line, k, cmd.Params[k], v)
			}
		}
	}
}

func TestParseFileCommands_KeyValue(t *testing.T) {
	dir := t.TempDir()

	// GITHUB_OUTPUT with simple and heredoc values.
	outputFile := filepath.Join(dir, "output")
	if err := os.WriteFile(outputFile, []byte("simple=hello\nmulti<<EOF\nline1\nline2\nEOF\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// GITHUB_ENV
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte("MY_VAR=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// GITHUB_PATH
	pathFile := filepath.Join(dir, "path")
	if err := os.WriteFile(pathFile, []byte("/usr/local/bin\n/opt/bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fc, err := ParseFileCommands(outputFile, envFile, pathFile)
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}

	if fc.Outputs["simple"] != "hello" {
		t.Errorf("Outputs[simple] = %q, want hello", fc.Outputs["simple"])
	}
	if fc.Outputs["multi"] != "line1\nline2" {
		t.Errorf("Outputs[multi] = %q, want line1\\nline2", fc.Outputs["multi"])
	}
	if fc.EnvVars["MY_VAR"] != "value" {
		t.Errorf("EnvVars[MY_VAR] = %q, want value", fc.EnvVars["MY_VAR"])
	}
	if len(fc.PathAdds) != 2 || fc.PathAdds[0] != "/usr/local/bin" {
		t.Errorf("PathAdds = %v, want [/usr/local/bin /opt/bin]", fc.PathAdds)
	}
}

func TestParseFileCommands_MissingFiles(t *testing.T) {
	fc, err := ParseFileCommands("/nonexistent", "/nonexistent", "/nonexistent")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	if len(fc.Outputs) != 0 || len(fc.EnvVars) != 0 || len(fc.PathAdds) != 0 {
		t.Error("expected empty results for missing files")
	}
}

func TestSecretMasker(t *testing.T) {
	m := NewSecretMasker([]string{"secret-value", "ab", "another-secret"})

	// "ab" is too short (< 3 chars), should not be masked.
	if got := m.Mask("my ab secret-value here"); got != "my ab *** here" {
		t.Errorf("Mask = %q, want 'my ab *** here'", got)
	}

	// AddSecret at runtime.
	m.AddSecret("runtime-secret")
	if got := m.Mask("runtime-secret"); got != "***" {
		t.Errorf("Mask after AddSecret = %q, want ***", got)
	}
}
