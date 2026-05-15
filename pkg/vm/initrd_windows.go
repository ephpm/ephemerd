//go:build windows

package vm

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

// buildBootInitrd produces the initrd the VM actually boots with by appending
// a tiny cpio archive containing /assets/ephemerd-linux to the embedded base
// initrd. The Linux kernel concatenates initrd cpios into a single initramfs,
// so files in the appended cpio override (or add to) those in the base. This
// lets a fresh `go build` of ephemerd.exe deliver a new ephemerd-linux to the
// VM without any initrd rebuild — the build-time initrd contains only the
// boot scaffolding (busybox, modules, init script), and the binary itself
// rides in via the runtime-generated tail.
func buildBootInitrd(basePath, ephemerdLinuxPath, destPath string) error {
	baseData, err := os.ReadFile(basePath)
	if err != nil {
		return fmt.Errorf("reading base initrd: %w", err)
	}
	binData, err := os.ReadFile(ephemerdLinuxPath)
	if err != nil {
		return fmt.Errorf("reading ephemerd-linux: %w", err)
	}

	var tail bytes.Buffer
	gw := gzip.NewWriter(&tail)
	// Mode 0o40755 = directory; 0o100755 = regular file with 0755 perms.
	// The assets/ dir already exists in the base initrd; re-declaring it is
	// harmless (cpio entries with the same name in later archives override).
	if err := writeCPIOEntry(gw, "assets", 0o40755, nil, ""); err != nil {
		return fmt.Errorf("cpio: assets dir: %w", err)
	}
	if err := writeCPIOEntry(gw, "assets/ephemerd-linux", 0o100755, binData, ""); err != nil {
		return fmt.Errorf("cpio: ephemerd-linux: %w", err)
	}
	if err := writeCPIOEntry(gw, "TRAILER!!!", 0, nil, ""); err != nil {
		return fmt.Errorf("cpio: trailer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("closing gzip writer: %w", err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("creating boot initrd: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing boot initrd: %w", cerr)
		}
	}()
	if _, err := f.Write(baseData); err != nil {
		return fmt.Errorf("writing base initrd: %w", err)
	}
	if _, err := f.Write(tail.Bytes()); err != nil {
		return fmt.Errorf("appending cpio tail: %w", err)
	}
	return nil
}

// writeCPIOEntry writes a single entry in newc (SVR4) cpio format. Mirrors
// the implementation in mage/download/download.go used to build the base
// initrd at build time — kept duplicated here to avoid importing build
// tooling into the runtime daemon.
func writeCPIOEntry(w io.Writer, name string, mode int64, data []byte, linkTarget string) error {
	var body []byte
	if linkTarget != "" {
		body = []byte(linkTarget)
	} else {
		body = data
	}
	nameBytes := append([]byte(name), 0)
	nameSize := len(nameBytes)
	fileSize := len(body)

	hdr := fmt.Sprintf("070701"+
		"%08X"+ // ino
		"%08X"+ // mode
		"%08X"+ // uid
		"%08X"+ // gid
		"%08X"+ // nlink
		"%08X"+ // mtime
		"%08X"+ // filesize
		"%08X"+ // devmajor
		"%08X"+ // devminor
		"%08X"+ // rdevmajor
		"%08X"+ // rdevminor
		"%08X"+ // namesize
		"%08X", // check
		0, mode, 0, 0, 1, 0, fileSize, 0, 0, 0, 0, nameSize, 0,
	)
	if _, err := io.WriteString(w, hdr); err != nil {
		return err
	}
	if _, err := w.Write(nameBytes); err != nil {
		return err
	}
	hdrLen := 110 + nameSize
	if pad := (4 - hdrLen%4) % 4; pad > 0 {
		if _, err := w.Write(make([]byte, pad)); err != nil {
			return err
		}
	}
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
		if pad := (4 - fileSize%4) % 4; pad > 0 {
			if _, err := w.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return nil
}
