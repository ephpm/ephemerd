//go:build linux

package networking

// subnet returns the configured subnet, or auto-selects one that doesn't
// conflict with existing network interfaces.
func (c Config) subnet() string {
	if c.Subnet != "" {
		return c.Subnet
	}
	return pickSubnet(c.Log)
}
