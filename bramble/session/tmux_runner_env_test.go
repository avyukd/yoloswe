package session

import (
	"slices"
	"strings"
	"testing"
)

// A session that does not receive its own ID and the control socket can read
// its peers (capture-pane rides BRAMBLE_SOCK) but can never address itself or
// write to them. Assert both are exported into the tmux window.
func TestTmuxRunnerEnvArgs_CarriesIdentityAndControlSocket(t *testing.T) {
	r := &tmuxRunner{
		sessionID:   "builder-abc123",
		brambleSock: "/run/user/1000/bramble-42.sock",
		controlSock: "/run/user/1000/bramble-control-42.sock",
	}

	args := r.envArgs()

	want := []string{
		"BRAMBLE_SOCK=/run/user/1000/bramble-42.sock",
		"BRAMBLE_SESSION_ID=builder-abc123",
		"BRAMBLE_CONTROL_SOCK=/run/user/1000/bramble-control-42.sock",
	}
	for _, kv := range want {
		if !slices.Contains(args, kv) {
			t.Errorf("envArgs() missing %q\ngot: %v", kv, args)
		}
	}

	// Every value must be preceded by its own -e flag.
	for i, a := range args {
		if strings.Contains(a, "=") && (i == 0 || args[i-1] != "-e") {
			t.Errorf("value %q at index %d is not preceded by -e\ngot: %v", a, i, args)
		}
	}
	if len(args) != 2*len(want) {
		t.Errorf("expected %d args (flag+value per var), got %d: %v", 2*len(want), len(args), args)
	}
}

// A manager configured before the control server starts leaves controlSock
// empty; the window must still be launchable rather than exporting VAR=.
func TestTmuxRunnerEnvArgs_OmitsUnsetValues(t *testing.T) {
	r := &tmuxRunner{brambleSock: "/run/bramble.sock"}

	args := r.envArgs()

	if len(args) != 2 || args[0] != "-e" || args[1] != "BRAMBLE_SOCK=/run/bramble.sock" {
		t.Fatalf("expected only the IPC socket pair, got: %v", args)
	}
	for _, a := range args {
		if strings.HasSuffix(a, "=") {
			t.Errorf("emitted empty assignment %q", a)
		}
	}
}
