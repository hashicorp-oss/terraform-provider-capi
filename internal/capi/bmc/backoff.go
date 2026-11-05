// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package bmc

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Retry calls fn up to attempts times with jittered exponential backoff
// between calls. It stops early on context cancellation and on
// ErrUnsupported, which no retry can fix.
func Retry(ctx context.Context, attempts int, base, maxDelay time.Duration, fn func(context.Context) error) error {
	var err error
	delay := base
	for i := 0; i < attempts; i++ {
		err = fn(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrUnsupported) || ctx.Err() != nil {
			break
		}
		if i == attempts-1 {
			break
		}
		jitter := time.Duration(rand.Int64N(int64(delay)/2 + 1))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay + jitter):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// WaitFor polls cond every interval until it returns true, returns an error,
// or timeout elapses. A timeout surfaces as context.DeadlineExceeded.
func WaitFor(ctx context.Context, timeout, interval time.Duration, cond func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		ok, err := cond(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
