//go:build windows

package diskspace

import (
	"fmt"
	"syscall"
	"unsafe"
)

// check reads capacity via GetDiskFreeSpaceExW. lpFreeBytesAvailable is the
// free space available to the calling user (quota-aware), which is what we
// want for "can this node still take a job"; lpTotalNumberOfBytes is the
// volume capacity. Same call cmd/ephemerd/doctor_windows.go makes.
func check(path string) (Usage, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return Usage{Path: path}, fmt.Errorf("converting path %s: %w", path, err)
	}

	var freeAvailable, totalBytes, totalFree uint64
	ret, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return Usage{Path: path}, fmt.Errorf("GetDiskFreeSpaceExW %s: %w", path, callErr)
	}

	return Usage{
		Path:       path,
		TotalBytes: totalBytes,
		FreeBytes:  freeAvailable,
	}, nil
}
