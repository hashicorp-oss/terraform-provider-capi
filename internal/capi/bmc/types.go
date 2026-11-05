// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package bmc is the out-of-band layer a bootstrapper uses to boot a
// bare-metal machine from an image: bmclib-backed power, boot device and
// virtual media control, Intel AMT storage redirection, the media boot
// sequence the supported BMCs are verified against, and the retry policy
// around BMC calls.
package bmc

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PowerState is the BMC-reported power state of the machine.
type PowerState string

// PowerState values.
const (
	PowerUnknown PowerState = "unknown"
	PowerOn      PowerState = "on"
	PowerOff     PowerState = "off"
)

// BootDevice is a bmclib boot device name.
type BootDevice string

// BootDevice values.
const (
	BootDeviceCDROM    BootDevice = "cdrom"
	BootDeviceUSB      BootDevice = "usb"
	BootDeviceUEFIHTTP BootDevice = "uefi_http"
	BootDeviceDisk     BootDevice = "disk"
)

// BootMethod selects how a machine is booted into an image.
type BootMethod string

// BootMethod values.
const (
	// BootMethodAuto tries virtual media (storage redirection on Intel AMT,
	// bmclib virtual media elsewhere) and falls back to UEFI HTTP boot.
	BootMethodAuto BootMethod = "auto"
	// BootMethodVirtualMedia attaches an ISO and arms a one-shot boot from it.
	BootMethodVirtualMedia BootMethod = "virtual_media"
	// BootMethodHTTP sets a UEFI HTTP boot URI (a UKI); not for live ISOs.
	BootMethodHTTP BootMethod = "http"
	// BootMethodManual drives no BMC: the machine is booted by hand (a USB
	// stick and a power button) and the bootstrapper only waits for it.
	BootMethodManual BootMethod = "manual"
)

// Defaults for BootConfig and the retry policy.
const (
	DefaultBootTimeout  = 15 * time.Minute
	DefaultBootAttempts = 3
	// DefaultPowerDown bounds the wait for a node to report off before it
	// is booted again; DefaultPoll is the interval inside such waits.
	DefaultPowerDown = 2 * time.Minute
	DefaultPoll      = 5 * time.Second
	// DefaultRetries, DefaultRetryBase and DefaultRetryMax are the retry
	// policy around BMC calls.
	DefaultRetries   = 3
	DefaultRetryBase = 500 * time.Millisecond
	DefaultRetryMax  = 5 * time.Second
)

// BootConfig controls how a machine is booted into an image.
type BootConfig struct {
	Method   BootMethod
	Timeout  time.Duration
	Attempts int
	// Device is the firmware boot target for attached virtual media. Empty
	// defaults to CD-ROM. BMCs that expose virtual media as a USB device (many
	// KVM-over-IP dongles, e.g. NanoKVM) need "usb" or the Cd override never
	// boots the media.
	Device BootDevice
}

// Sentinel errors.
var (
	ErrUnsupported  = errors.New("operation not supported by this BMC")
	ErrNoBootMethod = errors.New("no supported boot method: BMC supports neither virtual media nor UEFI HTTP boot")
)

// Error carries bmclib's per-provider metadata alongside the failure.
type Error struct {
	Op        string
	Err       error
	Attempted []string
	Failed    map[string]string
}

// Error renders the operation, the underlying error, and which providers failed.
func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bmc %s: %v", e.Op, e.Err)
	if len(e.Attempted) > 0 {
		fmt.Fprintf(&b, " (providers attempted: %s)", strings.Join(e.Attempted, ", "))
	}
	if len(e.Failed) > 0 {
		names := make([]string, 0, len(e.Failed))
		for name := range e.Failed {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, name+": "+e.Failed[name])
		}
		fmt.Fprintf(&b, " (provider failures: %s)", strings.Join(parts, "; "))
	}
	return b.String()
}

// Unwrap exposes the underlying error for errors.Is.
func (e *Error) Unwrap() error { return e.Err }
