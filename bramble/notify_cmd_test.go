package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

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
