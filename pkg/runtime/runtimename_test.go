package runtime

import "testing"

func TestResolveRuntimeName(t *testing.T) {
	tests := []struct {
		name         string
		linuxRuntime string
		goos         string
		want         string
	}{
		{
			name:         "linux default is runc when unset",
			linuxRuntime: "",
			goos:         "linux",
			want:         "io.containerd.runc.v2",
		},
		{
			name:         "linux honours an explicit runc handler",
			linuxRuntime: "io.containerd.runc.v2",
			goos:         "linux",
			want:         "io.containerd.runc.v2",
		},
		{
			name:         "linux honours the kata handler",
			linuxRuntime: "io.containerd.kata.v2",
			goos:         "linux",
			want:         "io.containerd.kata.v2",
		},
		{
			// The Linux knob must never leak into Windows job containers,
			// which require the host runhcs shim.
			name:         "windows always uses runhcs even if kata is configured",
			linuxRuntime: "io.containerd.kata.v2",
			goos:         "windows",
			want:         "io.containerd.runhcs.v1",
		},
		{
			name:         "windows with unset linux runtime uses runhcs",
			linuxRuntime: "",
			goos:         "windows",
			want:         "io.containerd.runhcs.v1",
		},
		{
			name:         "darwin falls through to the linux handling",
			linuxRuntime: "io.containerd.kata.v2",
			goos:         "darwin",
			want:         "io.containerd.kata.v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRuntimeName(tt.linuxRuntime, tt.goos); got != tt.want {
				t.Errorf("resolveRuntimeName(%q, %q) = %q, want %q",
					tt.linuxRuntime, tt.goos, got, tt.want)
			}
		})
	}
}
