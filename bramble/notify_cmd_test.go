package main

import (
	"encoding/json"
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

// serveIPCAt answers notify requests on an already-created listener, mirroring
// startFakeIPCServer's handler. It exists so a test can control *when* the
// socket appears.
func serveIPCAt(t *testing.T, ln net.Listener) {
	t.Helper()
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
				_ = json.NewEncoder(conn).Encode(ipc.Response{ID: req.ID, OK: true})
			}()
		}
	}()
}

// The retry loop is the only thing keeping a session from being stuck Running
// forever: notify is the sole caller of SetSessionIdle, and the stop hook runs
// with --silent, so a notify that lands while the replacement image has not yet
// re-bound the IPC socket fails invisibly. Bind deliberately later than the
// pre-budget backoff (100+300+900ms) ever waited, so shrinking the budget back
// to a handful of fixed attempts fails here rather than in production.
func TestSendNotifyWithRetrySpansTheRestartGap(t *testing.T) {
	// Unix socket paths are length-limited; keep the name short.
	sockPath := filepath.Join(t.TempDir(), "b.sock")

	const bindAfter = 1500 * time.Millisecond
	bound := make(chan struct{})
	go func() {
		time.Sleep(bindAfter)
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			close(bound)
			return
		}
		t.Cleanup(func() { _ = ln.Close() })
		serveIPCAt(t, ln)
		close(bound)
	}()
	t.Cleanup(func() { <-bound })

	start := time.Now()
	resp, err := sendNotifyWithRetry(ipc.NewClient(sockPath), "sess-1")
	require.NoError(t, err, "notify should survive the socket being absent at first")
	assert.True(t, resp.OK)
	assert.GreaterOrEqual(t, time.Since(start), bindAfter,
		"a success before the socket existed would mean the test proved nothing")
}

// The budget is also a ceiling: when bramble is gone for good rather than
// restarting, the stop hook must not block indefinitely waiting for a socket
// that is never coming back.
func TestSendNotifyWithRetryGivesUpWithinBudget(t *testing.T) {
	// Nothing ever listens here.
	sockPath := filepath.Join(t.TempDir(), "b.sock")

	start := time.Now()
	_, err := sendNotifyWithRetry(ipc.NewClient(sockPath), "sess-1")
	elapsed := time.Since(start)

	require.Error(t, err, "an unreachable socket is still a failure once the budget is spent")
	assert.GreaterOrEqual(t, elapsed, notifyRetryBudget, "gave up before spending the budget")
	// Generous headroom: the bound that matters is that it terminates near the
	// budget rather than retrying forever, not the exact overshoot.
	assert.Less(t, elapsed, notifyRetryBudget+2*time.Second, "overshot the budget")
}
