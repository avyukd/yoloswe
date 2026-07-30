package prdozer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRegistry writes body to a temp file and returns its path.
func writeRegistry(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const twoRepoRegistry = `
defaults:
  flow: pr-polish
  merge_policy: notify
  poll_interval: 20m
  model: opus
  max_budget_usd: 40
  layout: wt
  base_branch: main
  slack_target: "@ming"
  merge_rework:
    rounds:
      - prompt: |
          Merge of PR #{{.PRNumber}} failed: {{.MergeError}}
          Diagnose and fix it.

repos:
  sycamore-labs/kernel:
    worktree_root: /tmp/kernel-root
    merge_policy: queue
    required_checks: ["CI Gate"]
    merge_rework:
      rounds:
        - prompt: |
            PR #{{.PRNumber}} was dequeued: {{.MergeError}}
        - command: |
            gh pr view {{.PRNumber}} --json mergeStateStatus
  bazelment/yoloswe:
    worktree_root: /tmp/yoloswe-root
    merge_policy: squash
`

func TestRegistry_Resolve_MergesDefaults(t *testing.T) {
	t.Parallel()
	r, err := LoadRegistry(writeRegistry(t, twoRepoRegistry))
	require.NoError(t, err)

	got, err := r.Resolve("bazelment/yoloswe")
	require.NoError(t, err)
	// Repo-specific value wins.
	assert.Equal(t, MergePolicySquash, got.MergePolicy)
	// Everything else inherits from defaults.
	assert.Equal(t, "pr-polish", got.Flow)
	assert.Equal(t, "opus", got.Model)
	assert.Equal(t, "@ming", got.SlackTarget)
	assert.Equal(t, 20*time.Minute, got.PollInterval)
	assert.Equal(t, 40.0, got.MaxBudgetUSD)
	assert.Equal(t, LayoutWT, got.Layout)
	assert.Equal(t, "main", got.BaseBranch)
}

func TestRegistry_Resolve_MergeReworkReplacesNotAppends(t *testing.T) {
	t.Parallel()
	// A repo that declares its own rework rounds must get EXACTLY those. If
	// the default round were appended, kernel's queue-specific playbook would
	// be followed by generic advice that contradicts it.
	r, err := LoadRegistry(writeRegistry(t, twoRepoRegistry))
	require.NoError(t, err)

	kernel, err := r.Resolve("sycamore-labs/kernel")
	require.NoError(t, err)
	require.Len(t, kernel.MergeRework.Rounds, 2, "repo rounds replace the default entirely")
	assert.Contains(t, kernel.MergeRework.Rounds[0].Prompt, "dequeued")
	assert.True(t, kernel.MergeRework.Rounds[1].IsCommand())
	for _, round := range kernel.MergeRework.Rounds {
		assert.NotContains(t, round.Prompt, "Diagnose and fix it",
			"the default round must not be appended to a repo's own rounds")
	}

	// A repo that declares none inherits the default rounds.
	yolo, err := r.Resolve("bazelment/yoloswe")
	require.NoError(t, err)
	require.Len(t, yolo.MergeRework.Rounds, 1)
	assert.Contains(t, yolo.MergeRework.Rounds[0].Prompt, "Diagnose and fix it")
}

func TestRegistry_Resolve_UnknownRepoErrorsWithKnownKeys(t *testing.T) {
	t.Parallel()
	// A typo'd repo must NOT silently fall back to defaults — that yields
	// merge_policy "notify" and looks like the run just never finished.
	r, err := LoadRegistry(writeRegistry(t, twoRepoRegistry))
	require.NoError(t, err)

	_, err = r.Resolve("sycamore-labs/kernl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the registry")
	assert.Contains(t, err.Error(), "sycamore-labs/kernel", "the error should list known keys")
	assert.Contains(t, err.Error(), "bazelment/yoloswe")
}

func TestRegistry_Resolve_ExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	r, err := LoadRegistry(writeRegistry(t, `
repos:
  o/r:
    worktree_root: ~/worktrees/r
`))
	require.NoError(t, err)
	got, err := r.Resolve("o/r")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "worktrees", "r"), got.WorktreeRoot)
}

func TestRegistry_MissingWorktreeRoot_LoadsButFailsOnUse(t *testing.T) {
	t.Parallel()
	// Repos with no local clone (sycaweave, gstack-context-budget) must be
	// listable without breaking the whole registry, and only fail when they
	// are the repo actually being targeted.
	// The present repo needs a real checkout: CheckUsable verifies the root is
	// a git work tree, not merely an existing directory.
	presentRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(presentRoot, "main", ".git"), 0o755))

	r, err := LoadRegistry(writeRegistry(t, `
repos:
  sycamore-labs/sycaweave: {}
  o/present:
    worktree_root: `+presentRoot+`
`))
	require.NoError(t, err, "a repo with no worktree_root must not break load")

	entry, err := r.Resolve("sycamore-labs/sycaweave")
	require.NoError(t, err, "resolving must succeed")
	err = entry.CheckUsable("sycamore-labs/sycaweave")
	require.Error(t, err, "using it must fail")
	assert.Contains(t, err.Error(), "worktree_root")

	present, err := r.Resolve("o/present")
	require.NoError(t, err)
	require.NoError(t, present.CheckUsable("o/present"))
}

func TestRegistry_CheckUsable_RejectsMissingDirAndUnknownFlow(t *testing.T) {
	t.Parallel()
	missing := RepoEntry{WorktreeRoot: "/nonexistent/definitely-not-here", Flow: "pr-polish"}
	assert.Error(t, missing.CheckUsable("o/r"))

	badFlow := RepoEntry{WorktreeRoot: "/tmp", Flow: "jiradozer-validate"}
	err := badFlow.CheckUsable("o/r")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
}

func TestLoadRegistry_EagerTemplateValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "typo in field name",
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - prompt: "PR #{{.PRNumbr}} failed"
`,
			wantErr: "PRNumbr",
		},
		{
			name: "typo hidden behind a conditional branch",
			// The zero-value pass alone would skip this branch entirely; only
			// the filled-sample pass catches it. This is the case the two-pass
			// strategy exists for.
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - prompt: "{{- if .MergeError}}{{.Typoed}}{{- end}}"
`,
			wantErr: "Typoed",
		},
		{
			name: "unparseable template",
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - prompt: "{{.MergeError"
`,
			wantErr: "template",
		},
		{
			name: "bad template in a command round",
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - command: "gh pr view {{.Nope}}"
`,
			wantErr: "Nope",
		},
		{
			name: "bad template in defaults",
			body: `
defaults:
  merge_rework:
    rounds:
      - prompt: "{{.AlsoNope}}"
repos:
  o/r: {}
`,
			wantErr: "AlsoNope",
		},
		{
			name: "invalid merge policy",
			body: `
repos:
  o/r:
    merge_policy: yolo
`,
			wantErr: "merge_policy",
		},
		{
			// A typo'd model must fail at LOAD. Caught only at dispatch it would
			// strand a live PR mid-run, after the run has already started.
			name: "unknown model on a polish step",
			body: `
repos:
  o/r:
    polish:
      model: gpt-does-not-exist
      rounds:
        - prompt: /pr-polish
`,
			wantErr: "polish.model",
		},
		{
			// Same knob, other step: validateStepSpec covers both, so neither can
			// drift into accepting garbage.
			name: "invalid effort on a merge_rework step",
			body: `
repos:
  o/r:
    merge_rework:
      effort: turbo
      rounds:
        - prompt: fix the merge
`,
			wantErr: "merge_rework.effort",
		},
		{
			// Two once rounds with the same text share an onceKey, so the record
			// of one completing retires both — and within a tick both still run.
			// Neither reading is what the author meant, so the load fails.
			name: "duplicate once rounds on a polish step",
			body: `
repos:
  o/r:
    polish:
      rounds:
        - prompt: /simplify-branch
          once: true
        - prompt: /pr-polish
        - prompt: /simplify-branch
          once: true
`,
			wantErr: "repeats the once round at index 0",
		},
		{
			name: "invalid layout",
			body: `
repos:
  o/r:
    layout: sideways
`,
			wantErr: "layout",
		},
		{
			name: "round with both prompt and command",
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - prompt: "fix it"
          command: "echo hi"
`,
			wantErr: "exactly one",
		},
		{
			name: "round with neither prompt nor command",
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - {}
`,
			wantErr: "exactly one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadRegistry(writeRegistry(t, tc.body))
			require.Error(t, err, "bad registry must fail at load, not at merge time")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestLoadRegistry_ValidTemplatesPass(t *testing.T) {
	t.Parallel()
	// Every field of ReworkData is part of the config contract; exercising all
	// of them here means renaming one breaks this test loudly.
	_, err := LoadRegistry(writeRegistry(t, `
repos:
  o/r:
    merge_rework:
      rounds:
        - prompt: |
            {{.Repo}}#{{.PRNumber}} ({{.PRURL}}) on {{.Branch}}
            policy={{.MergePolicy}} attempt={{.Attempt}}
            {{- if .MergeError}}
            error: {{.MergeError}}
            {{- end}}
            {{- if .PrevOutput}}
            prev: {{.PrevOutput}}
            {{- end}}
    polish:
      rounds:
        - prompt: /simplify-branch
          once: true
        - prompt: "{{.DefaultPolishPrompt}}"
`))
	require.NoError(t, err)
}

// A field the consumer never fills renders as "" at runtime — a prompt that
// quietly loses its evidence, or a command that runs with an empty argument.
// Validation knows which consumer a step belongs to, so it rejects those.
func TestLoadRegistry_RejectsFieldsTheRouteNeverFills(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "polish round naming a merge_rework field",
			body: `
repos:
  o/r:
    polish:
      rounds:
        - prompt: "the merge failed: {{.MergeError}}"
`,
			wantErr: "MergeError",
		},
		{
			name: "polish round naming the merge attempt",
			body: `
repos:
  o/r:
    polish:
      rounds:
        - command: "gh pr view {{.PRNumber}} --json state # attempt {{.Attempt}}"
`,
			wantErr: "Attempt",
		},
		{
			name: "merge_rework round naming the polish default prompt",
			body: `
repos:
  o/r:
    merge_rework:
      rounds:
        - prompt: "{{.DefaultPolishPrompt}}"
`,
			wantErr: "DefaultPolishPrompt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadRegistry(writeRegistry(t, tc.body))
			require.Error(t, err, "a field the route never fills must fail at load")
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "round is given:",
				"the error must say what the route DOES supply")
		})
	}
}

// The routes together must account for every ReworkData field. A field claimed
// by neither is unreachable from any template; one silently added to the struct
// and to only one producer is the drift this catches.
func TestReworkDataFieldsAreRouted(t *testing.T) {
	t.Parallel()
	routed := make(map[string]bool)
	for _, r := range []stepRoute{routePolish, routeMergeRework} {
		for _, f := range r.fields() {
			routed[f] = true
		}
	}
	typ := reflect.TypeOf(ReworkData{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		assert.True(t, routed[name],
			"ReworkData.%s is filled by no route, so no template can ever use it", name)
	}
}

// Only the once gate cannot tell two identical rounds apart. A round that
// repeats every tick anyway may appear twice — rejecting that would outlaw a
// legitimate spec.
func TestLoadRegistry_DuplicateRepeatableRoundsAllowed(t *testing.T) {
	t.Parallel()
	_, err := LoadRegistry(writeRegistry(t, `
repos:
  o/r:
    polish:
      rounds:
        - prompt: /pr-polish
        - prompt: /pr-polish
`))
	require.NoError(t, err)
}

// The documented way to say "do the normal polish" in a spec round. Rounds are
// sent verbatim, so this placeholder is the only thing that carries prdozer's
// per-tick --rounds cap into a configured round.
func TestRenderRound_DefaultPolishPrompt(t *testing.T) {
	t.Parallel()
	got, err := RenderRound("{{.DefaultPolishPrompt}}", ReworkData{
		DefaultPolishPrompt: "/pr-polish --rounds 3 8123",
	})
	require.NoError(t, err)
	assert.Equal(t, "/pr-polish --rounds 3 8123", got)
}

func TestRenderRound(t *testing.T) {
	t.Parallel()
	got, err := RenderRound("PR #{{.PRNumber}}: {{.MergeError}} (attempt {{.Attempt}})", ReworkData{
		PRNumber:   8123,
		MergeError: "dequeued",
		Attempt:    3,
	})
	require.NoError(t, err)
	assert.Equal(t, "PR #8123: dequeued (attempt 3)", got)
}

func TestParsePRRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantRepo  string
		wantPR    int
		wantError bool
	}{
		{in: "sycamore-labs/kernel#8123", wantRepo: "sycamore-labs/kernel", wantPR: 8123},
		{in: "  bazelment/yoloswe#7  ", wantRepo: "bazelment/yoloswe", wantPR: 7},
		{in: "8123", wantError: true},        // no repo: ambiguous
		{in: "kernel#8123", wantError: true}, // not owner/repo
		{in: "a/b/c#1", wantError: true},     // too many segments
		{in: "o/r#", wantError: true},        // missing number
		{in: "o/r#abc", wantError: true},     // non-numeric
		{in: "o/r#0", wantError: true},       // zero is not a PR
		{in: "/r#1", wantError: true},        // empty owner
		{in: "o/#1", wantError: true},        // empty repo
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			repo, pr, err := ParsePRRef(tc.in)
			if tc.wantError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantRepo, repo)
			assert.Equal(t, tc.wantPR, pr)
		})
	}
}

// Under the "wt" layout, worktree_root is the bare-repo PARENT: it holds
// `.bare` and one directory per branch, and is itself outside any work tree.
// Running git or gh there fails with "not a git repository" — which is exactly
// how a dispatched run died before GitDir existed.
func TestRepoEntry_GitDir_WTLayoutUsesBaseCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "main")
	require.NoError(t, os.MkdirAll(filepath.Join(base, ".git"), 0o755))

	e := RepoEntry{WorktreeRoot: root, Layout: LayoutWT, BaseBranch: "main"}
	assert.Equal(t, base, e.GitDir(),
		"a wt-layout repo must resolve to its base checkout, not the bare parent")
}

func TestRepoEntry_GitDir_PlainLayoutUsesRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))

	e := RepoEntry{WorktreeRoot: root, Layout: LayoutPlain}
	assert.Equal(t, root, e.GitDir(), "a plain clone IS the git dir")
}

func TestRepoEntry_CheckUsable_RejectsNonGitRoot(t *testing.T) {
	t.Parallel()
	// A directory that exists but holds no checkout: the failure mode that
	// previously surfaced mid-run as an opaque gh subprocess error.
	root := t.TempDir()
	e := RepoEntry{WorktreeRoot: root, Layout: LayoutWT, BaseBranch: "main", Flow: "pr-polish"}

	err := e.CheckUsable("bazelment/yoloswe")
	require.Error(t, err, "a root with no work tree must fail before dispatch")
	assert.Contains(t, err.Error(), "not a git work tree")
}

func TestRepoEntry_CheckUsable_AcceptsWTLayoutWithBaseCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", ".git"), 0o755))

	e := RepoEntry{WorktreeRoot: root, Layout: LayoutWT, BaseBranch: "main", Flow: "pr-polish"}
	assert.NoError(t, e.CheckUsable("bazelment/yoloswe"))
}

// Every other RepoEntry field inherits from defaults; this one must too, or a
// fleet-wide self_review would be silently ignored.
func TestRegistry_SelfReviewInheritsFromDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", ".git"), 0o755))

	r, err := LoadRegistry(writeRegistry(t, `
defaults:
  self_review: true
repos:
  o/inherits:
    worktree_root: `+root+`
  o/explicit:
    worktree_root: `+root+`
    self_review: true
`))
	require.NoError(t, err)

	for _, name := range []string{"o/inherits", "o/explicit"} {
		e, rerr := r.Resolve(name)
		require.NoError(t, rerr)
		assert.True(t, e.SelfReview, "%s must have self_review enabled", name)
	}
}

// A step that declares only model/effort has no rounds, so merging the whole
// step behind one Empty() check replaced it wholesale with the default and threw
// the override away — even though modelID()/applyEffort() are written to honor
// it on the default single-call path.
func TestRegistry_Resolve_StepModelOnlyOverrideSurvivesMerge(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", ".git"), 0o755))

	r, err := LoadRegistry(writeRegistry(t, `
defaults:
  polish:
    rounds:
      - prompt: /pr-polish
  merge_rework:
    rounds:
      - prompt: /fix-merge
repos:
  o/modelonly:
    worktree_root: `+root+`
    polish:
      model: opus
      effort: high
    merge_rework:
      model: sonnet
`))
	require.NoError(t, err)

	e, err := r.Resolve("o/modelonly")
	require.NoError(t, err)
	assert.Equal(t, "opus", e.Polish.Model, "a model-only polish override must survive merging")
	assert.Equal(t, "high", e.Polish.Effort, "an effort-only polish override must survive merging")
	// The same bug, same fix, on the sibling StepSpec field.
	assert.Equal(t, "sonnet", e.MergeRework.Model, "merge_rework overrides merge the same way")

	// Declaring no rounds still inherits the default rounds — the override is
	// additive to the default step, not a replacement of it.
	require.Len(t, e.Polish.Rounds, 1)
	assert.Equal(t, "/pr-polish", e.Polish.Rounds[0].Prompt)
	require.Len(t, e.MergeRework.Rounds, 1)
	assert.Equal(t, "/fix-merge", e.MergeRework.Rounds[0].Prompt)
}

// Rounds must keep replacing rather than appending or inheriting, even now that
// model/effort merge field-wise around them.
func TestRegistry_Resolve_StepRoundsStillReplaceUnderFieldwiseMerge(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", ".git"), 0o755))

	r, err := LoadRegistry(writeRegistry(t, `
defaults:
  polish:
    model: sonnet
    rounds:
      - prompt: /generic
repos:
  o/ownrounds:
    worktree_root: `+root+`
    polish:
      rounds:
        - prompt: /specific
`))
	require.NoError(t, err)

	e, err := r.Resolve("o/ownrounds")
	require.NoError(t, err)
	require.Len(t, e.Polish.Rounds, 1, "repo rounds replace the default entirely")
	assert.Equal(t, "/specific", e.Polish.Rounds[0].Prompt)
	assert.Equal(t, "sonnet", e.Polish.Model,
		"model still falls back to the default like every other scalar")
}
