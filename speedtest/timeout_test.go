package speedtest

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

type fakeNetError struct{ timeout bool }

func (e fakeNetError) Error() string   { return "fake net error" }
func (e fakeNetError) Timeout() bool   { return e.timeout }
func (e fakeNetError) Temporary() bool { return false }

// A transfer stopped by the run's own clock is a measurement, not a failure.
// Before this, every such transfer was stored as "context deadline exceeded",
// which hid the rate the node had just demonstrated and classified a slow node
// as a technical fault.
func TestTransferOutcomeKeepsShortenedTransferAsMeasurement(t *testing.T) {
	budget := 30 * time.Second
	deadlineErr := context.DeadlineExceeded

	tests := []struct {
		name         string
		readErr      error
		deadline     bool
		bytes        int64
		wantTimedOut bool
		wantError    string
	}{
		{
			name:  "complete transfer",
			bytes: 21 * 1024 * 1024,
		},
		{
			name:      "nothing arrived",
			wantError: "empty response",
		},
		{
			name:         "deadline after partial transfer",
			readErr:      deadlineErr,
			deadline:     true,
			bytes:        21 * 1024 * 1024,
			wantTimedOut: true,
		},
		{
			// Nothing was measured, so there is no rate to keep and the result
			// stays a failure - but it says what happened instead of quoting a
			// transport error.
			name:      "deadline with no data",
			readErr:   deadlineErr,
			deadline:  true,
			wantError: "timed out after 30s without receiving data",
		},
		{
			name:      "connection reset mid-transfer",
			readErr:   errors.New("read: connection reset by peer"),
			bytes:     4 * 1024 * 1024,
			wantError: "read: connection reset by peer",
		},
		{
			name:      "unexpected EOF",
			readErr:   io.ErrUnexpectedEOF,
			bytes:     1024,
			wantError: io.ErrUnexpectedEOF.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timedOut, failure := transferOutcome(tt.readErr, tt.deadline, tt.bytes, budget)
			if timedOut != tt.wantTimedOut {
				t.Fatalf("timedOut = %v, want %v", timedOut, tt.wantTimedOut)
			}
			if failure != tt.wantError {
				t.Fatalf("error = %q, want %q", failure, tt.wantError)
			}
			if timedOut && failure != "" {
				t.Fatalf("a shortened transfer must not also carry an error: %q", failure)
			}
		})
	}
}

func TestTransferDeadlineReachedSeparatesTheClockFromFailures(t *testing.T) {
	budget := 30 * time.Second

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		err     error
		elapsed time.Duration
		want    bool
	}{
		{
			name: "no error at all",
			ctx:  context.Background(),
		},
		{
			name:    "run deadline expired",
			ctx:     expired,
			err:     context.DeadlineExceeded,
			elapsed: budget,
			want:    true,
		},
		{
			// A cancelled run abandons its measurement rather than shortening
			// it, so the result must keep an error and be dropped by the run.
			name:    "run cancelled by the operator",
			ctx:     cancelled,
			err:     context.Canceled,
			elapsed: 3 * time.Second,
		},
		{
			name:    "client timeout landing on the budget",
			ctx:     context.Background(),
			err:     fakeNetError{timeout: true},
			elapsed: budget - 100*time.Millisecond,
			want:    true,
		},
		{
			// A socket that gave up long before the budget stalled; that is a
			// failure and stays reported as one.
			name:    "socket timeout well before the budget",
			ctx:     context.Background(),
			err:     fakeNetError{timeout: true},
			elapsed: 5 * time.Second,
		},
		{
			name:    "plain transport failure",
			ctx:     context.Background(),
			err:     errors.New("connection reset by peer"),
			elapsed: 2 * time.Second,
		},
		{
			// The deadline landed a moment later, but the connection had
			// already been reset: that is a reset, not a shortened transfer.
			name:    "reset shortly before an expiring deadline",
			ctx:     expired,
			err:     errors.New("connection reset by peer"),
			elapsed: 4 * time.Second,
		},
		{
			// Nothing in the error says deadline, and the run's context did
			// expire after the budget was spent.
			name:    "opaque error at the end of an expired budget",
			ctx:     expired,
			err:     errors.New("http: read on closed response body"),
			elapsed: budget,
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transferDeadlineReached(tt.ctx, tt.err, tt.elapsed, budget); got != tt.want {
				t.Fatalf("transferDeadlineReached = %v, want %v", got, tt.want)
			}
		})
	}
}

// net.Error is the interface the client timeout arrives as; the fake stands in
// for it, so it has to satisfy the real one.
func TestFakeNetErrorSatisfiesNetError(t *testing.T) {
	var err error = fakeNetError{timeout: true}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatal("fakeNetError must satisfy net.Error with Timeout() == true")
	}
}

func TestShouldAttemptFallbackForShortenedTransfer(t *testing.T) {
	tests := []struct {
		name      string
		primary   Result
		threshold float64
		want      bool
	}{
		{
			name:      "complete transfer above the threshold",
			primary:   Result{Mbps: 500, DownloadedBytes: 1024},
			threshold: 100,
		},
		{
			// The run did not get the amount it asked for, and the endpoint is
			// one of the things that can explain why - even at a good rate.
			name:      "shortened transfer above the threshold",
			primary:   Result{Mbps: 500, DownloadedBytes: 1024, TimedOut: true},
			threshold: 100,
			want:      true,
		},
		{
			name:      "shortened transfer with no threshold in force",
			primary:   Result{Mbps: 500, DownloadedBytes: 1024, TimedOut: true},
			threshold: 0,
			want:      true,
		},
		{
			name:      "slow complete transfer",
			primary:   Result{Mbps: 5.79, DownloadedBytes: 1024},
			threshold: 100,
			want:      true,
		},
		{
			name:      "offline node has nothing to retry",
			primary:   Result{Offline: true, TimedOut: true},
			threshold: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAttemptFallback(tt.primary, tt.threshold); got != tt.want {
				t.Fatalf("shouldAttemptFallback = %v, want %v", got, tt.want)
			}
		})
	}
}

// A reserve endpoint replaces the primary measurement, so it has to have
// delivered the whole requested amount. A reserve that also ran out of time
// answers nothing and must not take the primary's place.
func TestSuccessfulSpeedResultRejectsShortenedTransfer(t *testing.T) {
	if !successfulSpeedResult(Result{DownloadedBytes: 1024, Mbps: 50}) {
		t.Fatal("a complete transfer must count as successful")
	}
	if successfulSpeedResult(Result{DownloadedBytes: 1024, Mbps: 50, TimedOut: true}) {
		t.Fatal("a shortened transfer must not qualify a reserve endpoint")
	}
}

// The requested amount is what makes "20.7 of 100 MB" readable later, so it is
// recorded on every result rather than only on the shortened ones.
func TestTestProxyRecordsTheRequestedAmount(t *testing.T) {
	proxies := []*models.ProxyConfig{{StableID: "node-1", Name: "Node 1", Protocol: "vless"}}
	manager := NewManager(checker.NewProxyChecker(proxies, 10000, "", 1, "", "", 1, 0, "status"), 10000, "", TestConfig{})

	// Nothing listens on the SOCKS port, so the attempt fails at once - which
	// is the point: the amount asked for is known before any byte moves.
	result := manager.testProxy(proxies[0], TestConfig{
		URL:        "http://127.0.0.1:1/file.bin",
		MaxBytes:   100 * 1024 * 1024,
		TimeoutSec: 5,
	}, ManualSource)

	if result.RequestedBytes != 100*1024*1024 {
		t.Fatalf("RequestedBytes = %d, want %d", result.RequestedBytes, 100*1024*1024)
	}
	if result.Error == "" {
		t.Fatal("a refused proxy connection must stay an error")
	}
	if result.TimedOut {
		t.Fatal("a refused connection is not a deadline")
	}
}
