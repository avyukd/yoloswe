package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// HostRuns is one host's answer to `<tool> runs --json`.
type HostRuns struct {
	// Err records why this host could not be read. It is a FIELD rather than a
	// dropped row on purpose: a stopped or unreachable box would otherwise look
	// exactly like a box with no runs, and "no runs anywhere" is the answer a
	// dispatcher acts on.
	Err   error
	Host  string
	Runs  []json.RawMessage
	Empty bool
}

// GatherRuns asks every host in the fleet what it is running, concurrently.
//
// Run-logs are per-host local files, so this is the only way to answer "what is
// every dispatched task doing right now" — the question a scatter-gather loop
// is built around. Rows come back as raw JSON so this package stays independent
// of either tool's run schema.
func GatherRuns(ctx context.Context, ssh SSHRunner, tool Tool, hosts []Host, extraArgs ...string) []HostRuns {
	out := make([]HostRuns, len(hosts))
	var wg sync.WaitGroup
	for i := range hosts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = gatherOne(ctx, ssh, tool, hosts[i], extraArgs)
		}(i)
	}
	wg.Wait()
	return out
}

func gatherOne(ctx context.Context, ssh SSHRunner, tool Tool, h Host, extraArgs []string) HostRuns {
	hr := HostRuns{Host: h.Hostname}

	// Resolve the binary the same way the probe does. ~/bin is not on a
	// non-interactive SSH shell's PATH, so a bare name here would report
	// "command not found" on a perfectly healthy box.
	cmd := fmt.Sprintf(`b=$(command -v %s || echo "$HOME/bin/%s"); "$b" runs --json`, tool.Name, tool.Name)
	for _, a := range extraArgs {
		cmd += " " + shellQuote(a)
	}

	raw, err := ssh.Run(ctx, h.Target(), cmd)
	if err != nil {
		hr.Err = fmt.Errorf("query %s: %w", h.Hostname, err)
		return hr
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		hr.Err = fmt.Errorf("parse %s output: %w", h.Hostname, err)
		return hr
	}
	hr.Runs = rows
	hr.Empty = len(rows) == 0
	return hr
}
