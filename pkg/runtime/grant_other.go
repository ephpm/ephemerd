//go:build !windows

package runtime

// grantHyperVTraverse and grantHyperVModify are no-ops on non-Windows.
// Only Hyper-V utility VMs need these ACLs on the host path.

func grantHyperVTraverse(_ string) error { return nil }
func grantHyperVModify(_ string) error   { return nil }
