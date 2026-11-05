// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package bmc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetry_SucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 3, time.Millisecond, 2*time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetry_ReturnsLastErrorAfterAttempts(t *testing.T) {
	calls := 0
	want := errors.New("still broken")
	err := Retry(context.Background(), 2, time.Millisecond, time.Millisecond, func(context.Context) error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Retry() error = %v, want %v", err, want)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestRetry_DoesNotRetryUnsupported(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 3, time.Millisecond, time.Millisecond, func(context.Context) error {
		calls++
		return ErrUnsupported
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Retry() error = %v, want ErrUnsupported", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRetry_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Retry(ctx, 5, 50*time.Millisecond, 50*time.Millisecond, func(context.Context) error {
		calls++
		cancel()
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry() error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestWaitFor_ConditionBecomesTrue(t *testing.T) {
	n := 0
	err := WaitFor(context.Background(), time.Second, time.Millisecond, func(context.Context) (bool, error) {
		n++
		return n >= 3, nil
	})
	if err != nil {
		t.Fatalf("WaitFor() error = %v", err)
	}
}

func TestWaitFor_Timeout(t *testing.T) {
	err := WaitFor(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitFor() error = %v, want DeadlineExceeded", err)
	}
}

func TestWaitFor_ConditionError(t *testing.T) {
	want := errors.New("boom")
	err := WaitFor(context.Background(), time.Second, time.Millisecond, func(context.Context) (bool, error) {
		return false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("WaitFor() error = %v, want %v", err, want)
	}
}
