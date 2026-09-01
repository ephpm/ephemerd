//go:build windows

package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestServiceBinPath(t *testing.T) {
	got := serviceBinPath(`C:\Program Files\ephemerd\ephemerd.exe`, `C:\ProgramData\ephemerd`)
	// This exact composition is what pkg/upgrade/swap_windows.go relies on
	// staying registered: quoted exe, `serve`, quoted --data-dir.
	want := `"C:\Program Files\ephemerd\ephemerd.exe" serve --data-dir "C:\ProgramData\ephemerd"`
	if got != want {
		t.Errorf("serviceBinPath =\n  %s\nwant\n  %s", got, want)
	}
}

func TestCreateServiceArgs(t *testing.T) {
	args := createServiceArgs(`C:\Program Files\ephemerd\ephemerd.exe`, `C:\ProgramData\ephemerd`)

	want := []string{
		"create", "ephemerd",
		"binPath=", `"C:\Program Files\ephemerd\ephemerd.exe" serve --data-dir "C:\ProgramData\ephemerd"`,
		"start=", "delayed-auto",
		"depend=", "hns/vmcompute/Tcpip",
		"DisplayName=", "ephemerd - Ephemeral GitHub Actions Runner",
	}
	if len(args) != len(want) {
		t.Fatalf("createServiceArgs = %q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("createServiceArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestConfigServiceArgs_ConvergesSameDefinition(t *testing.T) {
	create := createServiceArgs(`C:\x\ephemerd.exe`, `C:\data`)
	config := configServiceArgs(`C:\x\ephemerd.exe`, `C:\data`)

	if config[0] != "config" || config[1] != "ephemerd" {
		t.Fatalf("configServiceArgs verb = %q %q, want config ephemerd", config[0], config[1])
	}

	// Every setting the config path writes must match what a fresh create
	// would have written — otherwise fresh installs and converged installs
	// drift apart.
	createSet := scArgPairs(t, create[2:])
	configSet := scArgPairs(t, config[2:])
	for _, key := range []string{"binPath=", "start=", "depend="} {
		cv, ok := createSet[key]
		if !ok {
			t.Fatalf("create args missing %q", key)
		}
		gv, ok := configSet[key]
		if !ok {
			t.Fatalf("config args missing %q", key)
		}
		if cv != gv {
			t.Errorf("%s differs: create=%q config=%q", key, cv, gv)
		}
	}
}

// scArgPairs turns sc.exe's ["key=", "value", ...] argument convention into a
// map for comparison.
func scArgPairs(t *testing.T, args []string) map[string]string {
	t.Helper()
	if len(args)%2 != 0 {
		t.Fatalf("odd sc.exe arg list: %q", args)
	}
	m := map[string]string{}
	for i := 0; i < len(args); i += 2 {
		if !strings.HasSuffix(args[i], "=") {
			t.Fatalf("arg %q is not a key= token", args[i])
		}
		m[args[i]] = args[i+1]
	}
	return m
}

func TestServiceDependencies_ScSyntax(t *testing.T) {
	// sc.exe separates multiple dependencies with forward slashes; spaces or
	// commas silently register a single bogus dependency name.
	if strings.ContainsAny(serviceDependencies, " ,;") {
		t.Errorf("serviceDependencies %q must be forward-slash separated with no spaces", serviceDependencies)
	}
	deps := strings.Split(serviceDependencies, "/")
	want := map[string]bool{"hns": true, "vmcompute": true, "Tcpip": true}
	if len(deps) != len(want) {
		t.Fatalf("dependencies = %q, want exactly %v", deps, want)
	}
	for _, d := range deps {
		if !want[d] {
			t.Errorf("unexpected dependency %q", d)
		}
	}
}

func TestIsServiceExists(t *testing.T) {
	// A real *exec.ExitError with code 1073 requires running a process; the
	// output-text signals are what we can exercise hermetically, plus the
	// negative cases.
	cases := []struct {
		name string
		err  error
		out  string
		want bool
	}{
		{
			name: "sc create FAILED 1073 output",
			err:  errors.New("exit status 1073"),
			out:  "[SC] CreateService FAILED 1073:\r\n\r\nThe specified service already exists.\r\n",
			want: true,
		},
		{
			name: "already exists text only (localized code path lost)",
			err:  errors.New("exit status 1"),
			out:  "The specified service ALREADY EXISTS.",
			want: true,
		},
		{
			name: "numeric code only (localized message)",
			err:  errors.New("exit status 1073"),
			out:  "[SC] CreateService FAILED 1073:\r\n\r\nDer angegebene Dienst ist bereits vorhanden.\r\n",
			want: true,
		},
		{
			name: "access denied is not exists",
			err:  errors.New("exit status 5"),
			out:  "[SC] OpenSCManager FAILED 5:\r\n\r\nAccess is denied.\r\n",
			want: false,
		},
		{
			name: "marked-for-delete is not exists",
			err:  errors.New("exit status 1072"),
			out:  "[SC] CreateService FAILED 1072:\r\n\r\nThe specified service has been marked for deletion.\r\n",
			want: false,
		},
		{
			name: "no error",
			err:  nil,
			out:  "[SC] CreateService SUCCESS",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isServiceExists(tc.err, tc.out); got != tc.want {
				t.Errorf("isServiceExists(%v, %q) = %v, want %v", tc.err, tc.out, got, tc.want)
			}
		})
	}
}

func TestIsServiceExists_RealExitCode(t *testing.T) {
	// Drive a real *exec.ExitError carrying 1073 so the errors.As branch is
	// covered, with output that deliberately does NOT contain the text
	// signals.
	err := exec.Command("cmd.exe", "/c", "exit", "1073").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %v", err)
	}
	if !isServiceExists(err, "unrelated output") {
		t.Errorf("isServiceExists should recognize exit code 1073 regardless of output text")
	}
	if isServiceExists(exec.Command("cmd.exe", "/c", "exit", "1").Run(), "unrelated output") {
		t.Errorf("exit code 1 with no exists-text must not read as ERROR_SERVICE_EXISTS")
	}
}
