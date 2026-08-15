//go:build windows

package networking

import (
	"strings"
)

// specValue extracts the value of a key=value pair from a rule spec, or ""
// when the key is absent. Shared by the L2Bridge host-firewall backstop tests.
func specValue(r winFirewallRule, key string) string {
	for _, s := range r.spec {
		if v, ok := strings.CutPrefix(s, key+"="); ok {
			return v
		}
	}
	return ""
}
