package main

import (
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

// fakeIPCServer listens on a temp Unix socket and records the notify requests
// it receives, so a test can assert what notifyCmd actually put on the wire
// rather than what a helper returned in isolation.
type fakeIPCServer struct {
	requests chan ipc.Request
	sockPath string
}

func startFakeIPCServer(t *testing.T) *fakeIPCServer {
	t.Helper()

	// Unix socket paths are length-limited, so keep the name short rather than
	// nesting under the (long) test temp dir name.
	sockPath := filepath.Join(t.TempDir(), "b.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err, "listen on %s", sockPath)
	t.Cleanup(func() { _ = ln.Close() })

	s := &fakeIPCServer{sockPath: sockPath, requests: make(chan ipc.Request, 4)}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go func() {
				defer conn.Close()
				var req ipc.Request
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				s.requests <- req
				_ = json.NewEncoder(conn).Encode(ipc.Response{ID: req.ID, OK: true})
			}()
		}
	}()

	return s
}

// The tmux window exports BRAMBLE_SESSION_ID; notify is the consumer that makes
// the export worth having. TestResolveOwnSessionID pins the helper, but only
// this test pins the CLI wiring: drop the os.Getenv argument in notifyCmd.RunE
// and the helper tests still pass while notify stops working inside tmux.
func TestNotifyCmdResolvesSessionIDFromEnv(t *testing.T) {
	tests := []struct {
		name   string
		flagID string
		envID  string
		want   string
	}{
		{
			name:  "env supplies the session ID when --session-id is omitted",
			envID: "builder-from-env",
			want:  "builder-from-env",
		},
		{
			name:   "explicit flag still wins over the ambient env",
			flagID: "builder-from-flag",
			envID:  "builder-from-env",
			want:   "builder-from-flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startFakeIPCServer(t)
			t.Setenv(ipc.SockEnvVar, srv.sockPath)
			t.Setenv(session.SessionIDEnvVar, tt.envID)

			args := []string{}
			if tt.flagID != "" {
				args = append(args, "--session-id", tt.flagID)
			}
			require.NoError(t, notifyCmd.Flags().Parse(args))
			t.Cleanup(func() { _ = notifyCmd.Flags().Set("session-id", "") })

			require.NoError(t, notifyCmd.RunE(notifyCmd, nil))

			select {
			case req := <-srv.requests:
				assert.Equal(t, ipc.RequestNotify, req.Type)
				// Params round-trips through JSON as a map.
				params, ok := req.Params.(map[string]any)
				require.True(t, ok, "unexpected params type %T", req.Params)
				assert.Equal(t, tt.want, params["session_id"])
			default:
				t.Fatal("notifyCmd sent no IPC request")
			}
		})
	}
}

// Outside a bramble session neither source yields an ID. Without --silent that
// must surface as an error instead of notifying the empty session.
func TestNotifyCmdErrorsWithNoSessionID(t *testing.T) {
	srv := startFakeIPCServer(t)
	t.Setenv(ipc.SockEnvVar, srv.sockPath)
	t.Setenv(session.SessionIDEnvVar, "")

	require.NoError(t, notifyCmd.Flags().Parse(nil))
	t.Cleanup(func() { _ = notifyCmd.Flags().Set("session-id", "") })

	err := notifyCmd.RunE(notifyCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), session.SessionIDEnvVar)
}

// The baked-in stop hook runs with --silent, where a missing ID is expected
// (session already cleaned up) and must not write to stderr or fail the hook.
func TestNotifyCmdSilentSwallowsMissingSessionID(t *testing.T) {
	srv := startFakeIPCServer(t)
	t.Setenv(ipc.SockEnvVar, srv.sockPath)
	t.Setenv(session.SessionIDEnvVar, "")

	require.NoError(t, notifyCmd.Flags().Parse([]string{"--silent"}))
	t.Cleanup(func() {
		_ = notifyCmd.Flags().Set("session-id", "")
		_ = notifyCmd.Flags().Set("silent", "false")
	})

	assert.NoError(t, notifyCmd.RunE(notifyCmd, nil))
}

// A fake policy: same shape as the real one, fast enough that the retry tests
// are bounded by call counts rather than by wall-clock backoff.
var testNotifyRetryPolicy = notifyRetryPolicy{
	budget:     100 * time.Millisecond,
	baseDelay:  time.Millisecond,
	maxDelay:   2 * time.Millisecond,
	multiplier: 3,
}

// The retry loop is the only thing keeping a session from being stuck Running
// forever: notify is the sole caller of SetSessionIdle, and the stop hook runs
// with --silent, so a notify that lands while the replacement image has not yet
// re-bound the IPC socket fails invisibly.
func TestRetryNotifyOutlastsATransportOutage(t *testing.T) {
	t.Parallel()

	// Fail the dial the first few times, exactly as an unbound socket does,
	// then answer. Synchronizing on the call count rather than on elapsed time
	// keeps this deterministic under load.
	const failures = 4
	calls := 0
	resp, err := retryNotify(testNotifyRetryPolicy, func() (*ipc.Response, error) {
		calls++
		if calls <= failures {
			return nil, errors.New("dial unix: connect: no such file or directory")
		}
		return &ipc.Response{ID: "cli-notify", OK: true}, nil
	})

	require.NoError(t, err, "notify should survive the socket being absent at first")
	assert.True(t, resp.OK)
	assert.Equal(t, failures+1, calls, "should have retried until the socket answered")
}

// A response from the server is a real answer even when it is an error one.
// Retrying it would notify the same session repeatedly.
func TestRetryNotifyDoesNotRetryAServerError(t *testing.T) {
	t.Parallel()

	calls := 0
	resp, err := retryNotify(testNotifyRetryPolicy, func() (*ipc.Response, error) {
		calls++
		return &ipc.Response{ID: "cli-notify", OK: false, Error: "no such session"}, nil
	})

	require.NoError(t, err)
	assert.False(t, resp.OK)
	assert.Equal(t, 1, calls, "a served error is an answer, not a transport failure")
}

// The budget is also a ceiling: when bramble is gone for good rather than
// restarting, the stop hook must not block waiting for a socket that is never
// coming back.
func TestRetryNotifyGivesUpWhenTheBudgetIsSpent(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dial unix: connect: no such file or directory")
	calls := 0
	start := time.Now()
	_, err := retryNotify(testNotifyRetryPolicy, func() (*ipc.Response, error) {
		calls++
		return nil, wantErr
	})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, wantErr, "the last transport error is what the caller gets")
	assert.Greater(t, calls, 1, "should have retried before giving up")
	assert.GreaterOrEqual(t, elapsed, testNotifyRetryPolicy.budget, "gave up before spending the budget")
	// Generous headroom: what matters is that it terminates near the budget
	// rather than retrying forever, not the exact overshoot.
	assert.Less(t, elapsed, testNotifyRetryPolicy.budget+2*time.Second, "overshot the budget")
}

// The behaviour tests above run on a fake policy, so they would still pass if
// the real budget were shrunk back to the fixed 100/300/900ms backoff that
// preceded it — which did not span the gap it was written for. This pins the
// shipped number: the comment on defaultNotifyRetryPolicy's gap says the
// replacement image routinely takes more than a second to re-bind, so a budget
// in the low hundreds of milliseconds is a regression, not a tuning choice.
func TestDefaultNotifyRetryPolicyCoversTheRestartGap(t *testing.T) {
	t.Parallel()

	assert.GreaterOrEqual(t, defaultNotifyRetryPolicy.budget, 5*time.Second,
		"the notify budget must outlast a replacement image's time to IPC bind")
	assert.Positive(t, defaultNotifyRetryPolicy.baseDelay, "a zero base delay busy-loops the budget")
	assert.LessOrEqual(t, defaultNotifyRetryPolicy.maxDelay, defaultNotifyRetryPolicy.budget,
		"a delay cap above the budget makes the tail one long sleep")
	assert.Greater(t, defaultNotifyRetryPolicy.multiplier, 1, "a multiplier of 1 is a fixed-interval poll")
}
