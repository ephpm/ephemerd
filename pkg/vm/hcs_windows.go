//go:build windows

package vm

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// newGUID generates a random UUID v4 in standard format (lowercase, with hyphens).
// Used as the HCS compute system ID — GrantVmAccess requires GUID format.
func newGUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// HCS (Host Compute Service) API wrapper for creating Linux VMs via
// vmcompute.dll. We define our own schema structs that match hcsshim's
// internal/hcs/schema2 types. The JSON document produced is equivalent to
// what hcsshim's internal/uvm.makeLCOWDoc generates for a KernelDirect boot.
//
// We cannot import hcsshim's internal/hcs or internal/uvm packages because
// Go's module system prevents importing internal packages of other modules.
// Instead we call vmcompute.dll directly (which is what hcsshim also does
// internally via its internal/vmcompute package).

// hcsHandle is an opaque handle to an HCS compute system (VM or container).
type hcsHandle syscall.Handle

var (
	modvmcompute = windows.NewLazySystemDLL("vmcompute.dll")

	procHcsCreateComputeSystem     = modvmcompute.NewProc("HcsCreateComputeSystem")
	procHcsStartComputeSystem      = modvmcompute.NewProc("HcsStartComputeSystem")
	procHcsShutdownComputeSystem   = modvmcompute.NewProc("HcsShutdownComputeSystem")
	procHcsTerminateComputeSystem  = modvmcompute.NewProc("HcsTerminateComputeSystem")
	procHcsCloseComputeSystem      = modvmcompute.NewProc("HcsCloseComputeSystem")
	procHcsOpenComputeSystem       = modvmcompute.NewProc("HcsOpenComputeSystem")
	procHcsEnumerateComputeSystems = modvmcompute.NewProc("HcsEnumerateComputeSystems")
)

// --- HCS Schema Structs ---
// These mirror hcsschema.ComputeSystem from hcsshim/internal/hcs/schema2.
// Field names and JSON tags match exactly what HCS expects.

type hcsComputeSystem struct {
	Owner                             string     `json:"Owner,omitempty"`
	SchemaVersion                     *hcsVersion `json:"SchemaVersion,omitempty"`
	VirtualMachine                    *hcsVM     `json:"VirtualMachine,omitempty"`
	ShouldTerminateOnLastHandleClosed bool       `json:"ShouldTerminateOnLastHandleClosed,omitempty"`
}

type hcsVersion struct {
	Major int32 `json:"Major,omitempty"`
	Minor int32 `json:"Minor,omitempty"`
}

type hcsVM struct {
	StopOnReset     bool         `json:"StopOnReset,omitempty"`
	Chipset         *hcsChipset  `json:"Chipset,omitempty"`
	ComputeTopology *hcsTopology `json:"ComputeTopology,omitempty"`
	Devices         *hcsDevices  `json:"Devices,omitempty"`
}

type hcsChipset struct {
	LinuxKernelDirect *hcsLinuxKernelDirect `json:"LinuxKernelDirect,omitempty"`
}

type hcsLinuxKernelDirect struct {
	KernelFilePath string `json:"KernelFilePath,omitempty"`
	InitRdPath     string `json:"InitRdPath,omitempty"`
	KernelCmdLine  string `json:"KernelCmdLine,omitempty"`
}

type hcsTopology struct {
	Memory    *hcsMemory    `json:"Memory,omitempty"`
	Processor *hcsProcessor `json:"Processor,omitempty"`
}

type hcsMemory struct {
	SizeInMB        uint64 `json:"SizeInMB,omitempty"`
	AllowOvercommit bool   `json:"AllowOvercommit,omitempty"`
}

type hcsProcessor struct {
	Count uint32 `json:"Count,omitempty"`
}

type hcsDevices struct {
	Scsi            map[string]hcsScsi           `json:"Scsi,omitempty"`
	NetworkAdapters map[string]hcsNetworkAdapter `json:"NetworkAdapters,omitempty"`
	ComPorts        map[string]hcsComPort        `json:"ComPorts,omitempty"`
	Plan9           *hcsPlan9                    `json:"Plan9,omitempty"`
	HvSocket        *hcsHvSocket                 `json:"HvSocket,omitempty"`
}

type hcsComPort struct {
	NamedPipe string `json:"NamedPipe,omitempty"`
}

type hcsScsi struct {
	Attachments map[string]hcsAttachment `json:"Attachments,omitempty"`
}

type hcsAttachment struct {
	Type_    string `json:"Type,omitempty"`
	Path     string `json:"Path,omitempty"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type hcsNetworkAdapter struct {
	EndpointId string `json:"EndpointId,omitempty"`
	MacAddress string `json:"MacAddress,omitempty"`
}

type hcsPlan9 struct {
	Shares []hcsPlan9Share `json:"Shares,omitempty"`
}

type hcsPlan9Share struct {
	Name       string `json:"Name,omitempty"`
	AccessName string `json:"AccessName,omitempty"` // guest mount tag
	Path       string `json:"Path,omitempty"`       // host path
	ReadOnly   bool   `json:"ReadOnly,omitempty"`
	Flags      int32  `json:"Flags,omitempty"` // 0x04=LinuxMetadata, 0x08=CaseSensitive
}

type hcsHvSocket struct {
	Config *hcsHvSocketConfig `json:"HvSocketConfig,omitempty"`
}

type hcsHvSocketConfig struct {
	DefaultBindSecurityDescriptor string `json:"DefaultBindSecurityDescriptor,omitempty"`
}

// --- HCS Enumerate result ---

type hcsComputeSystemInfo struct {
	ID    string `json:"Id"`
	State string `json:"State"`
	Owner string `json:"Owner"`
}

// --- SCSI Controller GUIDs ---
// These match hcsshim's internal/protocol/guestrequest.ScsiControllerGuids.
// Created with namespace GUID "d422512d-2bf2-4752-809d-7b82b5fcb1b4" and
// index as names (e.g. guid.NewV5(namespace, []byte("0"))).
var scsiControllerGUIDs = []string{
	"df6d0690-79e5-55b6-a5ec-c1e2f77f580a",
	"0110f83b-de10-5172-a266-78bca56bf50a",
	"b5d2d8d4-3a75-51bf-945b-3444dc6b8579",
	"305891a9-b251-5dfe-91a2-c25d9212275b",
}

// --- HCS API Functions ---

// HCS_E_OPERATION_PENDING indicates an async operation is still in progress.
// The handle is valid; the caller should wait before using the compute system.
const hcsOperationPending = 0xC0370103

// hcsCreate creates and returns a handle to a new HCS compute system (VM).
// HCS create is asynchronous -- if the operation is pending, we poll until
// the system is ready (up to 30 seconds).
func hcsCreate(id string, doc *hcsComputeSystem) (hcsHandle, error) {
	configJSON, err := json.Marshal(doc)
	if err != nil {
		return 0, fmt.Errorf("marshaling HCS config: %w", err)
	}

	idPtr, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return 0, fmt.Errorf("converting id to UTF-16: %w", err)
	}
	configPtr, err := syscall.UTF16PtrFromString(string(configJSON))
	if err != nil {
		return 0, fmt.Errorf("converting config to UTF-16: %w", err)
	}

	var handle hcsHandle
	var resultPtr *uint16
	r0, _, _ := syscall.SyscallN(
		procHcsCreateComputeSystem.Addr(),
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(unsafe.Pointer(configPtr)),
		0, // identity (not used)
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(&resultPtr)),
	)

	if uint32(r0) == hcsOperationPending {
		// Async operation -- the handle from the syscall is valid even while
		// the operation is pending. Just wait a moment for HCS to finish
		// setting up the VM internally, then return the handle.
		hcsResultString(resultPtr) // free result
		time.Sleep(3 * time.Second)
		return handle, nil
	}

	if int32(r0) < 0 {
		detail := hcsResultString(resultPtr)
		return 0, fmt.Errorf("HcsCreateComputeSystem: HRESULT 0x%08x: %s", r0, detail)
	}
	return handle, nil
}

// hcsStart starts a previously created compute system.
// Like hcsCreate, this may return HCS_E_OPERATION_PENDING.
func hcsStart(handle hcsHandle) error {
	var resultPtr *uint16
	r0, _, _ := syscall.SyscallN(
		procHcsStartComputeSystem.Addr(),
		uintptr(handle),
		0, // options
		uintptr(unsafe.Pointer(&resultPtr)),
	)
	if uint32(r0) == hcsOperationPending {
		// Start is async -- the VM is booting. The caller will poll for
		// containerd's TCP port to detect when boot completes.
		hcsResultString(resultPtr)
		return nil
	}
	if int32(r0) < 0 {
		detail := hcsResultString(resultPtr)
		return fmt.Errorf("HcsStartComputeSystem: HRESULT 0x%08x: %s", r0, detail)
	}
	return nil
}

// hcsShutDown sends a shutdown signal to the compute system.
func hcsShutDown(handle hcsHandle) error {
	var resultPtr *uint16
	r0, _, _ := syscall.SyscallN(
		procHcsShutdownComputeSystem.Addr(),
		uintptr(handle),
		0, // options
		uintptr(unsafe.Pointer(&resultPtr)),
	)
	if int32(r0) < 0 {
		return fmt.Errorf("HcsShutdownComputeSystem: HRESULT 0x%08x", r0)
	}
	return nil
}

// hcsTerminate forcefully terminates the compute system.
func hcsTerminate(handle hcsHandle) error {
	var resultPtr *uint16
	r0, _, _ := syscall.SyscallN(
		procHcsTerminateComputeSystem.Addr(),
		uintptr(handle),
		0, // options
		uintptr(unsafe.Pointer(&resultPtr)),
	)
	if int32(r0) < 0 {
		return fmt.Errorf("HcsTerminateComputeSystem: HRESULT 0x%08x", r0)
	}
	return nil
}

func hcsClose(handle hcsHandle) error {
	r0, _, _ := syscall.SyscallN(
		procHcsCloseComputeSystem.Addr(),
		uintptr(handle),
	)
	if int32(r0) < 0 {
		return fmt.Errorf("HcsCloseComputeSystem: HRESULT 0x%08x", r0)
	}
	return nil
}

// hcsOpen opens an existing compute system by ID and returns a handle.
// Used to obtain a handle for stale VMs found via hcsEnumerate.
func hcsOpen(id string) (hcsHandle, error) {
	idPtr, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return 0, fmt.Errorf("converting id to UTF-16: %w", err)
	}

	var handle hcsHandle
	var resultPtr *uint16
	r0, _, _ := syscall.SyscallN(
		procHcsOpenComputeSystem.Addr(),
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(&resultPtr)),
	)
	if int32(r0) < 0 {
		detail := hcsResultString(resultPtr)
		return 0, fmt.Errorf("HcsOpenComputeSystem: HRESULT 0x%08x: %s", r0, detail)
	}
	return handle, nil
}

// hcsEnumerate lists compute systems matching a query.
// An empty query returns all systems.
func hcsEnumerate(query string) ([]hcsComputeSystemInfo, error) {
	queryPtr, err := syscall.UTF16PtrFromString(query)
	if err != nil {
		return nil, fmt.Errorf("converting query to UTF-16: %w", err)
	}

	var computeSystemsPtr *uint16
	var resultPtr *uint16
	r0, _, _ := syscall.SyscallN(
		procHcsEnumerateComputeSystems.Addr(),
		uintptr(unsafe.Pointer(queryPtr)),
		uintptr(unsafe.Pointer(&computeSystemsPtr)),
		uintptr(unsafe.Pointer(&resultPtr)),
	)
	if int32(r0) < 0 {
		return nil, fmt.Errorf("HcsEnumerateComputeSystems: HRESULT 0x%08x", r0)
	}

	if computeSystemsPtr == nil {
		return nil, nil
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(computeSystemsPtr))

	listJSON := windows.UTF16PtrToString(computeSystemsPtr)
	var systems []hcsComputeSystemInfo
	if err := json.Unmarshal([]byte(listJSON), &systems); err != nil {
		return nil, fmt.Errorf("parsing enumerate result: %w", err)
	}
	return systems, nil
}

// hcsResultString converts an HCS result pointer to a Go string.
// The pointer may be nil.
func hcsResultString(result *uint16) string {
	if result == nil {
		return ""
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(result))
	return windows.UTF16PtrToString(result)
}
