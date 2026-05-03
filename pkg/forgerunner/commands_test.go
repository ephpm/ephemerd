package forgerunner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// --- ParseCommand edge cases (item #9) ---

func TestParseCommand_EqualsInsideValue(t *testing.T) {
	// strings.Cut splits on the FIRST '=' so name=foo=bar parses as name -> "foo=bar".
	// This matches GitHub Actions semantics where `=` is allowed inside values.
	cmd, ok := ParseCommand("::set-output name=key=value::done")
	if !ok {
		t.Fatal("expected command")
	}
	if got, want := cmd.Params["name"], "key=value"; got != want {
		t.Errorf("Params[name] = %q, want %q", got, want)
	}
	if cmd.Value != "done" {
		t.Errorf("Value = %q, want %q", cmd.Value, "done")
	}
}

func TestParseCommand_CommasInsideValuesNotQuoted(t *testing.T) {
	// The current parser splits params naively on ','. This test documents
	// that quoted commas are NOT supported — the comma still terminates a
	// param. If we ever add quoting, this test will need to change.
	cmd, ok := ParseCommand(`::error file="a,b.go",line=10::msg`)
	if !ok {
		t.Fatal("expected command")
	}
	// "a,b.go" is split on the comma, leaving file=`"a` and a stray `b.go"`
	// fragment that has no `=` and is therefore dropped.
	if got, want := cmd.Params["file"], `"a`; got != want {
		t.Errorf("Params[file] = %q, want %q (naive split, no quote support)", got, want)
	}
	if got, want := cmd.Params["line"], "10"; got != want {
		t.Errorf("Params[line] = %q, want %q", got, want)
	}
}

func TestParseCommand_EmptyMessage(t *testing.T) {
	cmd, ok := ParseCommand("::warning::")
	if !ok {
		t.Fatal("expected command")
	}
	if cmd.Name != "warning" {
		t.Errorf("Name = %q, want warning", cmd.Name)
	}
	if cmd.Value != "" {
		t.Errorf("Value = %q, want empty", cmd.Value)
	}
}

func TestParseCommand_EmptyMessageWithParams(t *testing.T) {
	cmd, ok := ParseCommand("::error file=main.go,line=1::")
	if !ok {
		t.Fatal("expected command")
	}
	if cmd.Value != "" {
		t.Errorf("Value = %q, want empty", cmd.Value)
	}
	if cmd.Params["file"] != "main.go" {
		t.Errorf("Params[file] = %q, want main.go", cmd.Params["file"])
	}
}

func TestParseCommand_MalformedPrefixes(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"single colon", ":not a command"},
		{"single colon with content", ":group::value"},
		{"missing closing", "::group value with no terminator"},
		{"only opening", "::"},
		{"opening with name no closing", "::set-output name=foo"},
		{"empty string", ""},
		{"unrelated text", "regular log line"},
		{"trailing colon only", "::set-output name=foo:"},
		{"empty name with closing", "::::msg"},
		{"empty name with params", ":: param=value::msg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if cmd, ok := ParseCommand(tc.line); ok {
				t.Errorf("ParseCommand(%q) returned ok=true (cmd=%+v), want false", tc.line, cmd)
			}
		})
	}
}

func TestParseCommand_ParamWithoutEqualsIsIgnored(t *testing.T) {
	// "key" with no '=' should be ignored cleanly.
	cmd, ok := ParseCommand("::error solo,line=5::msg")
	if !ok {
		t.Fatal("expected command")
	}
	if _, found := cmd.Params["solo"]; found {
		t.Error("param 'solo' (no '=') should be ignored")
	}
	if cmd.Params["line"] != "5" {
		t.Errorf("Params[line] = %q, want 5", cmd.Params["line"])
	}
}

func TestParseCommand_DoubleColonInValue(t *testing.T) {
	// The first "::" after the head terminates the head, so the value
	// can contain additional "::" sequences as literal text.
	cmd, ok := ParseCommand("::notice::see https://example.com::page")
	if !ok {
		t.Fatal("expected command")
	}
	if cmd.Value != "see https://example.com::page" {
		t.Errorf("Value = %q", cmd.Value)
	}
}

// --- ParseFileCommands heredoc edge cases (item #10) ---

func TestParseFileCommands_HeredocEmptyDelimiter(t *testing.T) {
	dir := t.TempDir()
	// `key<<` with empty delimiter: an empty line will terminate the heredoc.
	out := filepath.Join(dir, "out")
	content := "key<<\nbody1\n\nafter=other\n"
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := ParseFileCommands(out, "", "")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	// With empty delim, the body terminates at the first blank line.
	// "body1" gets collected, then "" matches the empty delim and terminates.
	if got, want := fc.Outputs["key"], "body1"; got != want {
		t.Errorf("Outputs[key] = %q, want %q", got, want)
	}
	// after=other should still parse normally.
	if got, want := fc.Outputs["after"], "other"; got != want {
		t.Errorf("Outputs[after] = %q, want %q", got, want)
	}
}

func TestParseFileCommands_HeredocMissingEOF(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// No closing EOF — the body should consume the remaining file.
	content := "key<<EOF\nline1\nline2\nline3\n"
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := ParseFileCommands(out, "", "")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	got := fc.Outputs["key"]
	if got != "line1\nline2\nline3" {
		t.Errorf("Outputs[key] = %q, want %q", got, "line1\\nline2\\nline3")
	}
}

func TestParseFileCommands_HeredocCRLF(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// CRLF line endings. bufio.Scanner strips the trailing \r? No —
	// bufio.Scanner only strips \n. The \r remains in the line text, which
	// means "EOF\r" != "EOF" and the heredoc would not terminate. This
	// test documents the current behavior so we don't regress accidentally.
	content := "key<<EOF\r\nbody\r\nEOF\r\n"
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := ParseFileCommands(out, "", "")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	got, ok := fc.Outputs["key"]
	if !ok {
		t.Fatal("expected key in Outputs")
	}
	// Whatever the parser does, the body should at least contain the literal
	// "body" payload (possibly with trailing \r). Fail loudly if it disappears.
	if !strings.Contains(got, "body") {
		t.Errorf("Outputs[key] = %q, expected to contain 'body'", got)
	}
}

func TestParseFileCommands_HeredocNestedEOFToken(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// A line containing "EOF" but with extra characters should NOT terminate.
	// Only a line that equals the delimiter exactly terminates.
	content := "key<<EOF\nfirst\nEOF_NOT_REAL\nsecond\nEOF\nafter=x\n"
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := ParseFileCommands(out, "", "")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	want := "first\nEOF_NOT_REAL\nsecond"
	if got := fc.Outputs["key"]; got != want {
		t.Errorf("Outputs[key] = %q, want %q", got, want)
	}
	if got, want := fc.Outputs["after"], "x"; got != want {
		t.Errorf("Outputs[after] = %q, want %q", got, want)
	}
}

func TestParseFileCommands_HeredocImmediateEOF(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// Heredoc with no body before the delimiter.
	content := "empty<<EOF\nEOF\nfollow=ok\n"
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := ParseFileCommands(out, "", "")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	if got, want := fc.Outputs["empty"], ""; got != want {
		t.Errorf("Outputs[empty] = %q, want empty", got)
	}
	if got, want := fc.Outputs["follow"], "ok"; got != want {
		t.Errorf("Outputs[follow] = %q, want %q", got, want)
	}
}

func TestParseFileCommands_HeredocMultipleBlocks(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	content := "a<<EOF\nfoo\nEOF\nb<<DONE\nbar\nbaz\nDONE\nc=plain\n"
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fc, err := ParseFileCommands(out, "", "")
	if err != nil {
		t.Fatalf("ParseFileCommands: %v", err)
	}
	if got := fc.Outputs["a"]; got != "foo" {
		t.Errorf("Outputs[a] = %q, want foo", got)
	}
	if got := fc.Outputs["b"]; got != "bar\nbaz" {
		t.Errorf("Outputs[b] = %q, want bar\\nbaz", got)
	}
	if got := fc.Outputs["c"]; got != "plain" {
		t.Errorf("Outputs[c] = %q, want plain", got)
	}
}

// --- SecretMasker edge cases (item #11) ---

func TestSecretMasker_OverlappingSecrets(t *testing.T) {
	// When secrets share a substring, the masking order matters. The
	// current implementation iterates in registration order, replacing
	// each in full. Verify that a longer secret containing a shorter
	// one is fully masked even if the shorter secret was registered first.
	m := NewSecretMasker([]string{"abc", "abcdef"})
	// "abcdef" appears in input. Replacement order: "abc" first turns
	// "abcdef" into "***def"; then "abcdef" no longer appears — the
	// shorter secret prefix-strips it. Document this behavior.
	got := m.Mask("hello abcdef world")
	if got != "hello ***def world" {
		t.Errorf("Mask = %q, want %q (shorter secret runs first, prefix wins)", got, "hello ***def world")
	}

	// Order reversed: register longer first, it gets masked whole.
	m2 := NewSecretMasker([]string{"abcdef", "abc"})
	got2 := m2.Mask("hello abcdef world")
	if got2 != "hello *** world" {
		t.Errorf("Mask = %q, want %q (longer secret runs first)", got2, "hello *** world")
	}
}

func TestSecretMasker_EmptySecretList(t *testing.T) {
	m := NewSecretMasker(nil)
	if got := m.Mask("nothing to mask"); got != "nothing to mask" {
		t.Errorf("Mask with no secrets = %q, want unchanged", got)
	}

	m2 := NewSecretMasker([]string{})
	if got := m2.Mask("nothing to mask"); got != "nothing to mask" {
		t.Errorf("Mask with empty slice = %q, want unchanged", got)
	}
}

func TestSecretMasker_ShortSecretsDropped(t *testing.T) {
	// All secrets under 3 chars must be filtered out — verify they are
	// not masked even though the input contains them.
	m := NewSecretMasker([]string{"a", "bc", "", "xy"})
	in := "a bc xy"
	if got := m.Mask(in); got != in {
		t.Errorf("Mask = %q, want %q (all secrets too short)", got, in)
	}

	// AddSecret also enforces the length filter.
	m.AddSecret("z")
	m.AddSecret("ab")
	if got := m.Mask("z ab"); got != "z ab" {
		t.Errorf("Mask after short AddSecret = %q, want unchanged", got)
	}
}

func TestSecretMasker_Unicode(t *testing.T) {
	// Multi-byte UTF-8 secret. strings.ReplaceAll is byte-safe for valid
	// UTF-8 so the secret should be masked correctly.
	m := NewSecretMasker([]string{"パスワード", "héllo-secret"})

	if got := m.Mask("user said パスワード loud"); got != "user said *** loud" {
		t.Errorf("Mask Japanese = %q, want %q", got, "user said *** loud")
	}
	if got := m.Mask("say héllo-secret here"); got != "say *** here" {
		t.Errorf("Mask accented = %q, want %q", got, "say *** here")
	}
}

func TestSecretMasker_OrderingStabilityLargeList(t *testing.T) {
	// Register 100+ secrets, then mask a string that contains several of
	// them. The output should be deterministic across runs (no randomized
	// map iteration), and every registered secret that appears must be
	// masked to "***".
	const N = 150
	secrets := make([]string, N)
	for i := 0; i < N; i++ {
		secrets[i] = fmt.Sprintf("secret-token-%03d", i)
	}
	m := NewSecretMasker(secrets)

	// Build input containing a sample of secrets in arbitrary positions.
	sample := []int{0, 7, 42, 99, 149}
	var sb strings.Builder
	for _, i := range sample {
		sb.WriteString("prefix ")
		sb.WriteString(secrets[i])
		sb.WriteString(" suffix\n")
	}
	input := sb.String()

	// Run mask twice — must produce identical output (deterministic).
	out1 := m.Mask(input)
	out2 := m.Mask(input)
	if out1 != out2 {
		t.Fatal("Mask is not deterministic across calls")
	}

	// Every sampled secret must be replaced with ***.
	for _, i := range sample {
		if strings.Contains(out1, secrets[i]) {
			t.Errorf("secret %q still present in masked output", secrets[i])
		}
	}

	// Count "***" — must equal the number of distinct sampled secrets.
	if got, want := strings.Count(out1, "***"), len(sample); got != want {
		t.Errorf("got %d masked occurrences, want %d", got, want)
	}
}

func TestSecretMasker_MultipleOccurrencesOfSameSecret(t *testing.T) {
	m := NewSecretMasker([]string{"shh"})
	got := m.Mask("shh shh shh quiet shh!")
	want := "*** *** *** quiet ***!"
	if got != want {
		t.Errorf("Mask = %q, want %q", got, want)
	}
}
