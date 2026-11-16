package retry_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/vgeyer/paperless-extract/internal/retry"
)

type statusErr int

func (s statusErr) Error() string   { return fmt.Sprintf("status %d", int(s)) }
func (s statusErr) HTTPStatus() int { return int(s) }

var fast = retry.Policy{Attempts: 3, BaseDelay: time.Millisecond}

func TestDefaultPolicy(t *testing.T) {
	if retry.Default.Attempts != 3 || retry.Default.BaseDelay != time.Second || retry.Default.MaxDelay != 30*time.Second {
		t.Errorf("Default = %+v", retry.Default)
	}
}

func TestDoSuccessFirst(t *testing.T) {
	n := 0
	err := retry.Do(context.Background(), fast, nil, func(ctx context.Context) error { n++; return nil })
	if err != nil || n != 1 {
		t.Errorf("err=%v n=%d", err, n)
	}
}

func TestDoRetriesTransientUntilExhausted(t *testing.T) {
	n := 0
	want := statusErr(503)
	err := retry.Do(context.Background(), fast, nil, func(ctx context.Context) error { n++; return want })
	if n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
	var se statusErr
	if !errors.As(err, &se) || se != 503 {
		t.Errorf("err = %v, want last fn error", err)
	}
}

func TestDoReturnsLastErrorUnchanged(t *testing.T) {
	n := 0
	errs := []error{statusErr(500), statusErr(502), statusErr(503)}
	err := retry.Do(context.Background(), fast, nil, func(ctx context.Context) error { e := errs[n]; n++; return e })
	if err != errs[2] { //nolint:errorlint // contract: last error returned unchanged (identity)
		t.Errorf("err = %v, want exactly last error", err)
	}
}

func TestDoSucceedsAfterRetry(t *testing.T) {
	n := 0
	err := retry.Do(context.Background(), fast, nil, func(ctx context.Context) error {
		n++
		if n < 3 {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("err=%v n=%d", err, n)
	}
}

func TestDoNoRetryOnNonTransient(t *testing.T) {
	n := 0
	sentinel := errors.New("permanent")
	err := retry.Do(context.Background(), fast, nil, func(ctx context.Context) error { n++; return fmt.Errorf("wrap: %w", sentinel) })
	if n != 1 || !errors.Is(err, sentinel) {
		t.Errorf("n=%d err=%v", n, err)
	}
	n = 0
	err = retry.Do(context.Background(), fast, nil, func(ctx context.Context) error { n++; return statusErr(404) })
	if n != 1 || err == nil {
		t.Errorf("404 retried: n=%d", n)
	}
}

func TestDoCustomRetryable(t *testing.T) {
	n := 0
	err := retry.Do(context.Background(), retry.Policy{Attempts: 4, BaseDelay: time.Microsecond},
		func(error) bool { return true },
		func(ctx context.Context) error { n++; return errors.New("x") })
	if n != 4 || err == nil {
		t.Errorf("n=%d err=%v", n, err)
	}
	n = 0
	_ = retry.Do(context.Background(), fast, func(error) bool { return false },
		func(ctx context.Context) error { n++; return statusErr(503) })
	if n != 1 {
		t.Errorf("retryable=false still retried: n=%d", n)
	}
}

func TestDoAttemptsBelowOne(t *testing.T) {
	for _, a := range []int{0, -1} {
		n := 0
		_ = retry.Do(context.Background(), retry.Policy{Attempts: a, BaseDelay: time.Millisecond}, nil,
			func(ctx context.Context) error { n++; return statusErr(503) })
		if n != 1 {
			t.Errorf("Attempts=%d: calls = %d, want 1", a, n)
		}
	}
	n := 0
	_ = retry.Do(context.Background(), retry.Policy{}, nil, func(ctx context.Context) error { n++; return statusErr(503) })
	if n != 1 {
		t.Errorf("zero Policy: calls = %d, want 1", n)
	}
}

func TestDoDelaysExponentialWithCap(t *testing.T) {
	var stamps []time.Time
	p := retry.Policy{Attempts: 4, BaseDelay: 20 * time.Millisecond, MaxDelay: 30 * time.Millisecond}
	_ = retry.Do(context.Background(), p, nil, func(ctx context.Context) error {
		stamps = append(stamps, time.Now())
		return statusErr(500)
	})
	if len(stamps) != 4 {
		t.Fatalf("attempts = %d", len(stamps))
	}
	// delays: 20ms, min(40,30)=30ms, min(80,30)=30ms
	mins := []time.Duration{20 * time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond}
	for i, min := range mins {
		d := stamps[i+1].Sub(stamps[i])
		if d < min {
			t.Errorf("delay %d = %v, want ≥ %v", i+1, d, min)
		}
		if d > min+500*time.Millisecond {
			t.Errorf("delay %d = %v, far above %v (cap not applied?)", i+1, d, min)
		}
	}
}

func TestDoContextCancelledDuringDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	last := statusErr(503)
	start := time.Now()
	err := retry.Do(ctx, retry.Policy{Attempts: 3, BaseDelay: time.Hour}, nil, func(ctx context.Context) error {
		n++
		cancel()
		return last
	})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Do did not return promptly")
	}
	if n != 1 {
		t.Errorf("calls = %d", n)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v does not wrap context.Canceled", err)
	}
	var se statusErr
	if !errors.As(err, &se) || se != 503 {
		t.Errorf("err %v does not wrap last fn error", err)
	}
}

func TestDoContextDoneBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	err := retry.Do(ctx, retry.Policy{Attempts: 3, BaseDelay: time.Hour}, nil, func(ctx context.Context) error {
		n++
		return statusErr(503)
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if n > 1 {
		t.Errorf("calls = %d, want ≤ 1", n)
	}
}

func TestDoPassesContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	_ = retry.Do(ctx, fast, nil, func(c context.Context) error {
		if c.Value(key{}) != "v" {
			t.Errorf("ctx not derived from caller ctx")
		}
		return nil
	})
}

type netErr struct{}

func (netErr) Error() string   { return "net" }
func (netErr) Timeout() bool   { return false }
func (netErr) Temporary() bool { return false }

var _ net.Error = netErr{}

func TestIsTransient(t *testing.T) {
	urlErr := &url.Error{Op: "Get", URL: "http://x", Err: errors.New("connection refused")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("x"), false},
		{"canceled", context.Canceled, false},
		{"wrapped canceled", fmt.Errorf("a: %w", context.Canceled), false},
		{"url error wrapping canceled", &url.Error{Op: "Get", URL: "u", Err: context.Canceled}, false},
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("a: %w", context.DeadlineExceeded), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"url error", urlErr, true},
		{"wrapped url error", fmt.Errorf("get: %w", urlErr), true},
		{"net error", netErr{}, true},
		{"op error", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"400", statusErr(400), false},
		{"401", statusErr(401), false},
		{"404", statusErr(404), false},
		{"408", statusErr(408), true},
		{"429", statusErr(429), true},
		{"500", statusErr(500), true},
		{"503", statusErr(503), true},
		{"599", statusErr(599), true},
		{"wrapped 503", fmt.Errorf("x: %w", statusErr(503)), true},
		{"wrapped 404", fmt.Errorf("x: %w", statusErr(404)), false},
		// status decided first, even if also wrapping a transient error
		{"404 joined with EOF", errors.Join(statusErr(404), io.ErrUnexpectedEOF), false},
		{"EOF", io.EOF, false},
	}
	for _, c := range cases {
		if got := retry.IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
