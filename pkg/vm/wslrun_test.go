package vm

import (
	"regexp"
	"sync"
	"testing"
)

func TestWindowsPathToWSL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "simple drive path",
			input: `C:\Users\luthe\repo`,
			want:  "/mnt/c/Users/luthe/repo",
		},
		{
			name:  "drive root",
			input: `D:\`,
			want:  "/mnt/d/",
		},
		{
			name:  "forward slashes",
			input: "C:/Users/luthe/repo",
			want:  "/mnt/c/Users/luthe/repo",
		},
		{
			name:  "uppercase drive letter normalized",
			input: `E:\data\test`,
			want:  "/mnt/e/data/test",
		},
		{
			name:    "UNC path rejected",
			input:   `\\server\share\path`,
			wantErr: true,
		},
		{
			name:    "too short",
			input:   "C",
			wantErr: true,
		},
		{
			name:    "no drive letter",
			input:   "/unix/path",
			wantErr: true,
		},
		{
			name:    "number instead of drive",
			input:   "1:\\path",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := WindowsPathToWSL(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("WindowsPathToWSL(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("WindowsPathToWSL(%q) error = %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("WindowsPathToWSL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateDistroName(t *testing.T) {
	t.Run("format", func(t *testing.T) {
		name := generateDistroName("ephemerd-run")
		// Should match: ephemerd-run-<8 hex chars>
		re := regexp.MustCompile(`^ephemerd-run-[0-9a-f]{8}$`)
		if !re.MatchString(name) {
			t.Errorf("generateDistroName(\"ephemerd-run\") = %q, does not match expected pattern", name)
		}
	})

	t.Run("serve prefix", func(t *testing.T) {
		name := generateDistroName("ephemerd-linux")
		re := regexp.MustCompile(`^ephemerd-linux-[0-9a-f]{8}$`)
		if !re.MatchString(name) {
			t.Errorf("generateDistroName(\"ephemerd-linux\") = %q, does not match expected pattern", name)
		}
	})

	t.Run("uniqueness", func(t *testing.T) {
		seen := make(map[string]bool)
		for range 100 {
			name := generateDistroName("ephemerd-run")
			if seen[name] {
				t.Fatalf("duplicate name generated: %s", name)
			}
			seen[name] = true
		}
	})

	t.Run("concurrent uniqueness", func(t *testing.T) {
		const goroutines = 50
		const namesPerGoroutine = 20

		var mu sync.Mutex
		seen := make(map[string]bool)
		var duplicates []string

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				for range namesPerGoroutine {
					name := generateDistroName("ephemerd-run")
					mu.Lock()
					if seen[name] {
						duplicates = append(duplicates, name)
					}
					seen[name] = true
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if len(duplicates) > 0 {
			t.Fatalf("got %d duplicate names from %d concurrent goroutines: %v",
				len(duplicates), goroutines, duplicates)
		}

		total := goroutines * namesPerGoroutine
		if len(seen) != total {
			t.Errorf("expected %d unique names, got %d", total, len(seen))
		}
	})
}
