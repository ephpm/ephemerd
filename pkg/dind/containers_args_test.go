package dind

import "testing"

func TestArgsStrategyFor(t *testing.T) {
	cases := []struct {
		name       string
		entrypoint []string
		cmd        []string
		want       argsStrategy
	}{
		{
			name: "no overrides uses image config",
			want: argsUseImageConfig,
		},
		{
			name: "buildx-style cmd flags preserve image entrypoint",
			cmd:  []string{"--allow-insecure-entitlement", "security.insecure", "--allow-insecure-entitlement", "network.host"},
			want: argsImageEntrypointWithCmd,
		},
		{
			name:       "explicit entrypoint overrides image",
			entrypoint: []string{"/bin/sh"},
			cmd:        []string{"-c", "echo hi"},
			want:       argsFullOverride,
		},
		{
			name:       "entrypoint alone still full-override",
			entrypoint: []string{"/bin/true"},
			want:       argsFullOverride,
		},
		{
			name: "empty slices treated as absent",
			cmd:  []string{},
			want: argsUseImageConfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := argsStrategyFor(tc.entrypoint, tc.cmd)
			if got != tc.want {
				t.Errorf("argsStrategyFor(%v, %v) = %d, want %d", tc.entrypoint, tc.cmd, got, tc.want)
			}
		})
	}
}
