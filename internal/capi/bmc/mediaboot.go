// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package bmc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// attachISO attaches isoRef as the node's bootable media and arms a one-shot
// boot from it. On Intel AMT the ISO is streamed to the host over storage
// redirection (IDE-R/USB-R), which arms the boot itself; the live session
// handle is returned and must be closed once the host has read what it
// needs. Everywhere else bmclib virtual media is inserted and, unless the BMC
// armed the boot on insert, a one-shot EFI boot override selects device
// (CD-ROM when empty). do wraps every BMC call so the caller's retry policy
// applies. A BMC that supports neither path reports ErrUnsupported.
func attachISO(ctx context.Context, b Client, isoRef string, device BootDevice, do func(context.Context, func(context.Context) error) error) (RedirectHandle, error) {
	if isoRef == "" {
		return nil, fmt.Errorf("virtual media: no ISO URL: %w", ErrUnsupported)
	}
	if rd, ok := b.(ISORedirector); ok && rd.RedirectSupported(ctx) {
		handle, err := rd.RedirectISO(ctx, isoRef)
		if err == nil {
			return handle, nil
		}
		if !errors.Is(err, ErrUnsupported) {
			return nil, err
		}
	}
	var armed bool
	if err := do(ctx, func(ctx context.Context) error {
		var err error
		armed, err = b.InsertMedia(ctx, isoRef)
		return err
	}); err != nil {
		return nil, err
	}
	if armed {
		return nil, nil
	}
	if device == "" {
		device = BootDeviceCDROM
	}
	return nil, do(ctx, func(ctx context.Context) error { return b.SetBootDevice(ctx, device, false, true) })
}

// MediaBoot boots a machine from an ISO through its BMC. Boot powers a running
// node off, attaches the image and powers the node on; Release drops a
// storage-redirection session once the booted system no longer reads the
// media; Detach ejects everything.
type MediaBoot struct {
	BMC Client
	// Device is the firmware boot target for attached virtual media, CD-ROM
	// when empty (see BootConfig.Device).
	Device BootDevice
	Logger *log.Logger
	// PowerDown bounds the wait for a node to report off before it is booted
	// again; Poll is that wait's interval.
	PowerDown time.Duration
	Poll      time.Duration
	// Retries, RetryBase and RetryMax configure the retry policy around BMC
	// calls.
	Retries   int
	RetryBase time.Duration
	RetryMax  time.Duration

	handle RedirectHandle
}

// NewMediaBoot returns a MediaBoot with the default timeouts and retry
// policy.
func NewMediaBoot(b Client, device BootDevice, logger *log.Logger) *MediaBoot {
	if logger == nil {
		logger = log.New(log.Writer(), "[media-boot] ", log.LstdFlags)
	}
	return &MediaBoot{
		BMC: b, Device: device, Logger: logger,
		PowerDown: DefaultPowerDown, Poll: DefaultPoll,
		Retries: DefaultRetries, RetryBase: DefaultRetryBase, RetryMax: DefaultRetryMax,
	}
}

func (m *MediaBoot) do(ctx context.Context, fn func(context.Context) error) error {
	return Retry(ctx, m.Retries, m.RetryBase, m.RetryMax, fn)
}

// Boot attaches isoRef and starts the node from it. A node that is on is
// powered off first so the firmware runs its one-shot boot selection, then
// power-cycled; an off node is powered on. The BMC call sequence is the one
// the supported BMCs are verified against.
func (m *MediaBoot) Boot(ctx context.Context, isoRef string) error {
	state := m.powerState(ctx)
	if state == PowerOn {
		if err := m.do(ctx, m.BMC.PowerOff); err != nil {
			return err
		}
		if err := WaitFor(ctx, m.PowerDown, m.Poll, func(ctx context.Context) (bool, error) {
			return m.powerState(ctx) == PowerOff, nil
		}); err != nil && ctx.Err() == nil {
			m.Logger.Printf("node still reports on after power off; continuing with a power cycle")
		}
	}

	m.Detach(ctx)
	handle, err := attachISO(ctx, m.BMC, isoRef, m.Device, m.do)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return fmt.Errorf("%w: %v", ErrNoBootMethod, err)
		}
		return err
	}
	m.handle = handle
	if handle != nil {
		m.Logger.Printf("booting via AMT storage redirection")
	}

	// A BMC that reports "on" for an off machine is common; a cycle is
	// correct either way.
	powerFn := m.BMC.PowerOn
	if state == PowerOn {
		powerFn = m.BMC.PowerCycle
	}
	return m.do(ctx, powerFn)
}

// powerState reads the power state, treating a failure as unknown. A BMC
// that has not pinned a provider yet may answer unknown once; it is asked
// again so the boot path is chosen from a real reading when there is one.
func (m *MediaBoot) powerState(ctx context.Context) PowerState {
	for i := 0; i < 2; i++ {
		var state PowerState
		if err := m.do(ctx, func(ctx context.Context) error {
			var err error
			state, err = m.BMC.PowerState(ctx)
			return err
		}); err != nil {
			m.Logger.Printf("power state: %v", err)
			return PowerUnknown
		}
		if state != PowerUnknown {
			return state
		}
	}
	return PowerUnknown
}

// Release closes a live storage-redirection session. Call it once the booted
// system has read everything it needs from the media (a live image loaded
// into RAM): holding an AMT USB-R device attached after that floods the host
// with USB resets. A no-op without a session.
func (m *MediaBoot) Release() {
	if m.handle == nil {
		return
	}
	if err := m.handle.Close(); err != nil {
		m.Logger.Printf("closing storage redirection: %v", err)
	}
	m.handle = nil
}

// Detach releases any redirection session and ejects virtual media, best
// effort: a BMC without virtual media is not an error.
func (m *MediaBoot) Detach(ctx context.Context) {
	m.Release()
	if err := m.do(ctx, m.BMC.EjectMedia); err != nil && !errors.Is(err, ErrUnsupported) {
		m.Logger.Printf("eject media: %v", err)
	}
}
