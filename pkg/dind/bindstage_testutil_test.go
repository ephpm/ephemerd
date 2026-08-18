package dind

// unstagedTestStager returns the resolved path unchanged instead of publishing
// it as a bind mount.
//
// IT DELIBERATELY DOES NOT EXIST IN A NON-TEST BUILD. Staging needs mount(2),
// which needs CAP_SYS_ADMIN, and the project's Linux CI runner is unprivileged
// (see the "Privileged-test coverage report" step in .github/workflows/ci.yml,
// which exists for the same reason). Tests that only care about translation
// POLICY — which sources are accepted, which are rejected, what gets
// auto-created, which are forced read-only — install this so they run
// everywhere, on every platform, without privilege.
//
// The security property staging provides is therefore NOT covered by these
// tests, by construction. It is covered by:
//
//   - TestMountStager_* in bindstage_linux_test.go (Linux, root-gated), which
//     performs the real bind and proves the staged content survives an
//     adversarial swap of the original path;
//   - TestBindStaging_RealRunc_* in bindstage_runc_linux_test.go (Linux, root,
//     EPHEMERD_TEST_RUNC=<path to runc>), which drives the whole path through
//     a real runc and asserts what the container actually saw.
//
// Keeping the unsafe shortcut in a _test.go file is the point: no production
// wiring can select it, and dind.New never sees it.
type unstagedTestStager struct{}

func (unstagedTestStager) stage(p *bindPin) (string, error) { return p.Logical(), nil }

func (unstagedTestStager) teardown() {}
