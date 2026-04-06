//go:build !windows

package runtime

func defaultImage() string {
	return defaultImageLinux
}
