package cni

import "testing"

func TestSelectCNITarball(t *testing.T) {
	both := []string{"cni-plugins-linux-amd64-v1.6.2.tgz", "cni-plugins-linux-arm64-v1.6.2.tgz"}

	tests := []struct {
		name    string
		names   []string
		goarch  string
		want    string
		wantErr bool
	}{
		{"arm64 picks arm64 from both", both, "arm64", "cni-plugins-linux-arm64-v1.6.2.tgz", false},
		{"amd64 picks amd64 from both", both, "amd64", "cni-plugins-linux-amd64-v1.6.2.tgz", false},
		{"amd64 alone", both[:1], "amd64", "cni-plugins-linux-amd64-v1.6.2.tgz", false},
		{"skips gitkeep", []string{".gitkeep", "cni-plugins-linux-arm64-v1.6.2.tgz"}, "arm64", "cni-plugins-linux-arm64-v1.6.2.tgz", false},
		{"no match errors", both[:1], "arm64", "", true},
		{"empty errors", []string{".gitkeep"}, "arm64", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectCNITarball(tt.names, tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
