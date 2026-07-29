package prdozer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RunsRoot is the directory holding every babysit run's artifacts. It lives
// OUTSIDE any worktree so GC of the ephemeral worktree never takes the logs
// with it: the worktree is disposable, the record of what happened is not.
const RunsRoot = "~/.prdozer/runs"

// TerminalState is the outcome a babysit run finished in.
type TerminalState string

const (
	TerminalMerged     TerminalState = "merged"
	TerminalClosed     TerminalState = "closed"
	TerminalNeedsHuman TerminalState = "needs-human"
	TerminalFailed     TerminalState = "failed"
	// TerminalRunning marks a run that has not finished. meta.json is written
	// at start with this value so a harvester can tell "still going" from
	// "died without ever updating".
	TerminalRunning TerminalState = "running"
)

// RunMeta is the machine-readable summary of one babysit run, written at start
// and rewritten on every terminal transition. A harvester can find and
// classify every run without parsing a single log line.
type RunMeta struct {
	StartedAt    time.Time     `json:"started_at"`
	EndedAt      time.Time     `json:"ended_at,omitempty"`
	Repo         string        `json:"repo"`
	RunID        string        `json:"run_id"`
	Host         string        `json:"host"`
	WorktreePath string        `json:"worktree_path"`
	LogDir       string        `json:"log_dir"`
	Branch       string        `json:"branch"`
	PRURL        string        `json:"pr_url"`
	TmuxSession  string        `json:"tmux_session,omitempty"`
	Note         string        `json:"note,omitempty"`
	MergePolicy  MergePolicy   `json:"merge_policy"`
	State        TerminalState `json:"state"`
	PRNumber     int           `json:"pr_number"`
	PolishRounds int           `json:"polish_rounds"`
	MergeAttempt int           `json:"merge_attempts"`
	// WorktreeKept records that GC deliberately left the worktree in place
	// (unpushed commits, or --keep-worktree). Surfaced in the notification so
	// the disk cost is never silent.
	WorktreeKept bool `json:"worktree_kept,omitempty"`
}

// RunLog owns one run's directory under RunsRoot: meta.json, the append-only
// events.jsonl audit trail, and per-round agent logs.
type RunLog struct {
	dir  string
	meta RunMeta
	mu   sync.Mutex
}

// runIDPattern-safe sanitization: a repo slug contains a "/" which must not
// become a directory separator in the run-dir name.
func sanitizeSlug(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "/", "-"), string(filepath.Separator), "-")
}

// RunDirFor returns the run directory for a repo/PR/run triple.
func RunDirFor(repo string, prNumber int, runID string) string {
	return filepath.Join(ExpandHome(RunsRoot), fmt.Sprintf("%s-%d-%s", sanitizeSlug(repo), prNumber, runID))
}

// NewRunLog creates the run directory and writes the initial meta.json.
func NewRunLog(meta RunMeta) (*RunLog, error) {
	dir := RunDirFor(meta.Repo, meta.PRNumber, meta.RunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	meta.LogDir = dir
	if meta.State == "" {
		meta.State = TerminalRunning
	}
	if meta.StartedAt.IsZero() {
		meta.StartedAt = time.Now().UTC()
	}
	if meta.Host == "" {
		meta.Host, _ = os.Hostname()
	}
	rl := &RunLog{dir: dir, meta: meta}
	if err := rl.writeMeta(); err != nil {
		return nil, err
	}
	return rl, nil
}

// Dir returns the run's log directory. It outlives the worktree.
func (r *RunLog) Dir() string { return r.dir }

// Meta returns a copy of the current metadata.
func (r *RunLog) Meta() RunMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta
}

// writeMeta persists meta.json. Caller must hold the lock, except in
// NewRunLog where no other reference exists yet.
func (r *RunLog) writeMeta() error {
	data, err := json.MarshalIndent(r.meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run meta: %w", err)
	}
	path := filepath.Join(r.dir, "meta.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// UpdateMeta applies fn to the metadata and rewrites meta.json.
func (r *RunLog) UpdateMeta(fn func(*RunMeta)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.meta)
	return r.writeMeta()
}

// Finish records the terminal state and end time.
func (r *RunLog) Finish(state TerminalState, note string) error {
	return r.UpdateMeta(func(m *RunMeta) {
		m.State = state
		m.EndedAt = time.Now().UTC()
		if note != "" {
			m.Note = note
		}
	})
}

// Event is one line of the append-only audit trail. It is what makes "why did
// this PR take six rounds" answerable a week after the fact.
type Event struct {
	At     time.Time      `json:"at"`
	Fields map[string]any `json:"fields,omitempty"`
	Kind   string         `json:"kind"`
	Detail string         `json:"detail,omitempty"`
}

// Append writes one event to events.jsonl. Errors are returned rather than
// swallowed, but callers generally log-and-continue: losing an audit line must
// not abort a run that is otherwise healthy.
func (r *RunLog) Append(kind, detail string, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := Event{At: time.Now().UTC(), Kind: kind, Detail: detail, Fields: fields}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events.jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// WriteRoundLog stores a named per-round log (e.g. "polish-r1", "rework-r2",
// "merge-attempt-1") in the run directory.
func (r *RunLog) WriteRoundLog(name, content string) error {
	path := filepath.Join(r.dir, name+".log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadRunMeta reads a single run's meta.json.
func LoadRunMeta(dir string) (RunMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return RunMeta{}, fmt.Errorf("read run meta: %w", err)
	}
	var m RunMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return RunMeta{}, fmt.Errorf("parse run meta %s: %w", dir, err)
	}
	return m, nil
}

// ListRuns returns every run under RunsRoot, newest first. Directories without
// a readable meta.json are skipped rather than failing the listing — a run
// killed before its first write must not break `prdozer runs`.
func ListRuns() ([]RunMeta, error) {
	root := ExpandHome(RunsRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs dir: %w", err)
	}
	out := make([]RunMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := LoadRunMeta(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sortRunsNewestFirst(out)
	return out, nil
}

func sortRunsNewestFirst(runs []RunMeta) {
	// Simple insertion sort: run counts are small and this avoids pulling in
	// a comparator just to reverse-sort by time.
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].StartedAt.After(runs[j-1].StartedAt); j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}
