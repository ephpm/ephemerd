package workflow

import "testing"

// --- parseRepoFromURL tests ---

func TestParseRepoFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"HTTPS with .git", "https://github.com/ephpm/ephemerd.git", "ephpm/ephemerd"},
		{"HTTPS without .git", "https://github.com/ephpm/ephemerd", "ephpm/ephemerd"},
		{"SSH format", "git@github.com:ephpm/ephemerd.git", "ephpm/ephemerd"},
		{"SSH without .git", "git@github.com:ephpm/ephemerd", "ephpm/ephemerd"},
		{"SSH nested org", "git@github.com:my-org/my-repo.git", "my-org/my-repo"},
		{"HTTPS GitLab", "https://gitlab.com/group/project.git", "group/project"},
		{"HTTPS with trailing slash", "https://github.com/owner/repo/", "repo/"},
		{"bare word", "something", "something"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRepoFromURL(tt.url)
			if got != tt.want {
				t.Errorf("parseRepoFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// --- sniffGitInfo tests ---

func TestSniffGitInfo_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	gi := sniffGitInfo(dir)

	// Should return defaults when not a git repo
	if gi.SHA != "unknown" {
		t.Errorf("SHA = %q, want %q", gi.SHA, "unknown")
	}
	if gi.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q, want %q", gi.Ref, "refs/heads/main")
	}
	if gi.Repository != "local/repo" {
		t.Errorf("Repository = %q, want %q", gi.Repository, "local/repo")
	}
}

func TestSniffGitInfo_CurrentRepo(t *testing.T) {
	// Run against the actual ephemerd repo
	gi := sniffGitInfo(".")

	if gi.SHA == "unknown" {
		t.Error("expected real SHA from current repo")
	}
	if len(gi.SHA) < 7 {
		t.Errorf("SHA = %q, expected full hash", gi.SHA)
	}
	if gi.Repository == "local/repo" {
		t.Error("expected real repository from current repo")
	}
}

// --- gitCmd tests ---

func TestGitCmd_Version(t *testing.T) {
	out, err := gitCmd(".", "--version")
	if err != nil {
		t.Fatalf("gitCmd(--version) error: %v", err)
	}
	if out == "" {
		t.Error("gitCmd(--version) returned empty")
	}
}

func TestGitCmd_InvalidDir(t *testing.T) {
	_, err := gitCmd("/nonexistent/dir/that/does/not/exist", "status")
	if err == nil {
		t.Error("expected error for nonexistent dir")
	}
}
