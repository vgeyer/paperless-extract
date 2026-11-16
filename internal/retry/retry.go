// Package retry provides retries with exponential backoff.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"
)

// Policy configures Do.
type Policy struct {
	Attempts  int
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// Default: 3 attempts, delays 1s then 2s.
var Default = Policy{Attempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

func (p Policy) delay(n int) time.Duration {
	d := p.BaseDelay
	for i := 2; i < n; i++ {
		d *= 2
		if p.MaxDelay > 0 && d >= p.MaxDelay {
			break
		}
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	return d
}

// Do calls fn until it succeeds, fails with a non-retryable error or attempts
// are exhausted. It returns fn's last error unchanged, except when ctx ends
// first, in which case both ctx.Err() and the last error are wrapped.
func Do(ctx context.Context, p Policy, retryable func(error) bool, fn func(ctx context.Context) error) error {
	if retryable == nil {
		retryable = IsTransient
	}
	attempts := max(p.Attempts, 1)
	var err error
	for n := 1; n <= attempts; n++ {
		if n > 1 {
			t := time.NewTimer(p.delay(n))
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("retry aborted: %w: %w", ctx.Err(), err)
			case <-t.C:
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			if err == nil {
				return cerr
			}
			return fmt.Errorf("retry aborted: %w: %w", cerr, err)
		}
		err = fn(ctx)
		if err == nil || !retryable(err) {
			return err
		}
	}
	return err
}

type httpStatuser interface{ HTTPStatus() int }

// IsTransient reports whether err is worth retrying.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var hs httpStatuser
	if errors.As(err, &hs) {
		s := hs.HTTPStatus()
		return s == 408 || s == 429 || s >= 500
	}
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF)
}
