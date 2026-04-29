package main

import "testing"

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{int64(2.5 * 1024 * 1024), "2.5 MB"},
		// Just past 1 KB to trigger the divide-out loop a few times.
		{2048, "2.0 KB"},
		// Negative? formatBytes assumes positive — document the behavior.
		// For -1, b < unit, so it returns "-1 B".
		{-1, "-1 B"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.bytes)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestFormatBytes_Petabyte(t *testing.T) {
	got := formatBytes(int64(1024) * 1024 * 1024 * 1024 * 1024)
	if got != "1.0 PB" {
		t.Errorf("formatBytes(1 PB) = %q, want 1.0 PB", got)
	}
}
