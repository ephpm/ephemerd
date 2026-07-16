package config

import "testing"

func TestGitHubTargets(t *testing.T) {
	c := &Config{
		GitHub:      GitHubConfig{Owner: "ephpm", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"},
		GitHubExtra: []GitHubConfig{{Owner: "luthermonson", Token: "ghp_x"}},
	}
	targets := c.GitHubTargets()
	if len(targets) != 2 {
		t.Fatalf("want 2 targets (primary + extra), got %d", len(targets))
	}
	if targets[0].Owner != "ephpm" || targets[1].Owner != "luthermonson" {
		t.Errorf("unexpected target order/owners: %q, %q", targets[0].Owner, targets[1].Owner)
	}
}

func TestGitHubTargetsPrimaryOnly(t *testing.T) {
	c := &Config{GitHub: GitHubConfig{Owner: "ephpm", Token: "x"}}
	if got := len(c.GitHubTargets()); got != 1 {
		t.Fatalf("primary-only: want 1 target, got %d", got)
	}
}

func TestGitHubTargetsExtraOnly(t *testing.T) {
	// No primary [github]; only [[github_extra]]. GitHubTargets does not touch
	// the environment, so an empty primary stays uncounted.
	c := &Config{GitHubExtra: []GitHubConfig{{Owner: "luthermonson", Token: "x"}}}
	if got := len(c.GitHubTargets()); got != 1 {
		t.Fatalf("extra-only: want 1 target, got %d", got)
	}
}

func TestValidateGitHubMultiTarget(t *testing.T) {
	// Valid: primary via App, extra via PAT.
	ok := &Config{
		GitHub:      GitHubConfig{Owner: "ephpm", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k", Token: "x"},
		GitHubExtra: []GitHubConfig{{Owner: "luthermonson", Token: "ghp_x"}},
	}
	if err := ok.validateGitHub(); err != nil {
		t.Fatalf("valid multi-target rejected: %v", err)
	}

	// Invalid: an extra target missing its owner.
	bad := &Config{
		GitHub:      GitHubConfig{Owner: "ephpm", Token: "x"},
		GitHubExtra: []GitHubConfig{{Token: "ghp_x"}},
	}
	if err := bad.validateGitHub(); err == nil {
		t.Fatal("expected error for github_extra target without owner")
	}

	// Invalid: an extra target with app_id but no installation_id.
	bad2 := &Config{
		GitHub:      GitHubConfig{Owner: "ephpm", Token: "x"},
		GitHubExtra: []GitHubConfig{{Owner: "luthermonson", AppID: 9}},
	}
	if err := bad2.validateGitHub(); err == nil {
		t.Fatal("expected error for github_extra app_id without installation_id")
	}
}
