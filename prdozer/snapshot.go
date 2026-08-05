package prdozer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// Coarse status-rollup classification used across snapshot and changeset.
const (
	StatusSuccess = "SUCCESS"
	StatusPending = "PENDING"
	StatusFailure = "FAILURE"
)

// Snapshot is a point-in-time view of a PR's state.
type Snapshot struct {
	TakenAt      time.Time
	BaseSHA      string
	StatusRollup string
	Comments     []CommentRef
	FailedRunIDs []int64
	// Degraded names the best-effort fetches that failed this tick, so a
	// partial snapshot is visible instead of silently looking complete. A tick
	// proceeds on partial data — losing one signal for one tick beats losing
	// every tick for as long as the outage lasts — but it must SAY so, or a
	// missing CIFailed trigger reads as "CI is fine". Grouped with the other
	// slices so it packs alongside them (govet fieldalignment).
	Degraded []string
	// ChangesRequestedBy lists the reviewers whose latest review requested
	// changes, so they can be asked to look again once the work is pushed.
	ChangesRequestedBy []string
	// IsBotReviewer marks which of those are apps. Bots cannot be re-requested
	// (GitHub 422: not a collaborator) and do not need to be — they re-review
	// on push.
	IsBotReviewer map[string]bool
	PR            PRDetails
	// UnresolvedThreads counts review threads still awaiting work, or -1 when
	// it could not be read. -1 disables the divergence guard for this tick
	// rather than being mistaken for a healthy zero. Last so the int packs into
	// the tail (govet fieldalignment).
	UnresolvedThreads int
}

// Health summarizes the snapshot for divergence tracking. ok is false when the
// thread count is unknown, in which case the caller must not compare.
//
// CIFailing reads the head commit's rollup ONLY, deliberately not the
// FailedRunIDs that ComputeChangeset also folds into cs.CIFailed. The two serve
// different purposes: cs.CIFailed is an event ("a run failed since last tick"),
// while this is a point-in-time measurement that has to be able to improve.
// FailedRunIDs is branch-scoped and historical — a run that failed five commits
// ago still appears — so folding it in would pin CIFailing true for the rest of
// the run and trip the guard on a PR that had already gone green. Nothing is
// lost by omitting it: summarizeRollup scores failure above pending, so any
// failed check on the head commit shows up as StatusFailure immediately.
func (s *Snapshot) Health() (PRHealth, bool) {
	if s.UnresolvedThreads < 0 {
		return PRHealth{}, false
	}
	return PRHealth{
		UnresolvedThreads: s.UnresolvedThreads,
		CIFailing:         s.StatusRollup == StatusFailure,
	}, true
}

// PRDetails is the fields prdozer cares about from `gh pr view`.
type PRDetails struct {
	URL               string           `json:"url"`
	HeadRefName       string           `json:"headRefName"`
	BaseRefName       string           `json:"baseRefName"`
	HeadRefOid        string           `json:"headRefOid"`
	State             string           `json:"state"`
	ReviewDecision    string           `json:"reviewDecision"`
	Mergeable         string           `json:"mergeable"`
	StatusCheckRollup []statusCheckRow `json:"statusCheckRollup"`
	LatestReviews     []reviewRow      `json:"latestReviews"`
	// Commits is the PR's commit list, carried only so the scope guard can
	// measure how many commits a push actually added. Counting pushes instead
	// under-counts badly: a polish invocation commits once per round and
	// force-pushes ONCE at the end, so a 5-round invocation reads as a single
	// commit and a cap of 12 would sit ~60 commits deep before firing.
	//
	// Read it through CommitCount, never directly: gh caps this list at 100
	// entries with no pagination, so on a large PR its length is a truncation
	// artifact rather than a count.
	Commits []commitRow `json:"commits"`
	// TotalCommits is the exact commit count from GraphQL, which reports it as
	// a scalar and so is immune to the 100-entry cap on Commits. Zero when the
	// query failed; CommitCount falls back to the list length in that case.
	TotalCommits int `json:"-"`
	Number       int `json:"number"`
	// Additions/Deletions/ChangedFiles size the PR's diff. Fetched in the same
	// gh call as everything else, so the scope guard costs no extra round trip.
	Additions    int  `json:"additions"`
	Deletions    int  `json:"deletions"`
	ChangedFiles int  `json:"changedFiles"`
	IsDraft      bool `json:"isDraft"`
}

// commitRow is one entry in gh's commits list. Only the count is ever read —
// the guard needs to know how many commits arrived, not what is in them — but
// `gh pr view --json` exposes no count-only field, so the list is decoded and
// measured.
type commitRow struct {
	OID string `json:"oid"`
}

// commitListCap is the page size `gh pr view --json commits` fetches. The query
// is unpaginated, so a list this long is a truncation and its length says only
// "at least this many".
const commitListCap = 100

// CommitCount reports how many commits the PR carries, or 0 when that is
// unknown.
//
// The exact GraphQL count wins over the list length because `gh pr view --json
// commits` issues an unpaginated `commits(first:100)` and silently truncates
// there. On a PR already 100 commits deep the list length is pinned at 100, so
// every subsequent push measures as zero growth and pushedCommits charges its
// floor of one instead of the true delta — the scope brake under-counting,
// which is the one direction it must not be wrong in.
//
// The list length survives as the fallback rather than being dropped: for the
// PRs that fit under the cap — nearly all of them — it is exact, and dropping it
// would floor every push to one whenever the GraphQL call blipped.
//
// A saturated list is reported as UNKNOWN rather than as 100, because a caller
// cannot tell the two apart and both misuse it. Persisting 100 as the baseline
// for a 137-commit PR makes the next successful tick read 37 commits of growth
// that never happened and slam the brake shut; charging against it goes wrong
// the same way. Unknown costs a floor charge of one for that tick and leaves
// the last authoritative baseline intact.
func (p PRDetails) CommitCount() int {
	if p.TotalCommits > 0 {
		return p.TotalCommits
	}
	if len(p.Commits) >= commitListCap {
		return 0
	}
	return len(p.Commits)
}

// reviewRow is one entry in gh's latestReviews: the most recent review per
// reviewer, which is what decides the overall reviewDecision.
type reviewRow struct {
	State  string `json:"state"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

// statusCheckRow is a single entry in gh's statusCheckRollup. Some checks set
// `conclusion`, others use `state` — we collapse both downstream.
type statusCheckRow struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// CommentRef is a single comment we've observed; we keep enough to dedupe and
// classify human vs bot.
type CommentRef struct {
	Created time.Time `json:"created_at"`
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Author  string    `json:"author"`
	IsBot   bool      `json:"is_bot"`
	IsSelf  bool      `json:"is_self"`
}

// SnapshotOptions controls how a snapshot is taken.
type SnapshotOptions struct {
	CommentsSince time.Time
	Self          string
}

// TakeSnapshot fetches the current state of a PR via gh. The initial pr view
// call runs synchronously (we need its URL/HeadRefName to parameterize the
// follow-up calls); independent follow-ups run concurrently.
func TakeSnapshot(ctx context.Context, gh wt.GHRunner, dir string, prNumber int, opts SnapshotOptions) (*Snapshot, error) {
	pr, err := fetchPRDetails(ctx, gh, dir, prNumber)
	if err != nil {
		return nil, fmt.Errorf("pr view #%d: %w", prNumber, err)
	}
	owner, repo, err := repoSlugFromURL(pr.URL)
	if err != nil {
		return nil, fmt.Errorf("derive owner/repo from %s: %w", pr.URL, err)
	}

	var (
		wg                     sync.WaitGroup
		failed                 []int64
		comments               []CommentRef
		baseSHA                string
		unresolved             int
		totalCommits           int
		failedErr, commentsErr error
		degraded               []string
	)
	wg.Add(5)
	go func() {
		defer wg.Done()
		// Best-effort: an unreadable count leaves PRDetails.TotalCommits at
		// zero and CommitCount falls back to the (possibly truncated) list,
		// which is what the scope guard read before this query existed.
		totalCommits, _ = fetchCommitCount(ctx, gh, dir, owner, repo, prNumber)
	}()
	go func() {
		defer wg.Done()
		// Best-effort, like base detection: an unreadable thread count only
		// disables the divergence guard for this tick rather than failing the
		// whole snapshot. -1 marks "unknown" so the guard can tell it apart
		// from a genuine zero.
		unresolved = -1
		if n, err := fetchUnresolvedThreads(ctx, gh, dir, owner, repo, prNumber); err == nil {
			unresolved = n
		}
	}()
	go func() {
		defer wg.Done()
		failed, failedErr = fetchFailedRunIDs(ctx, gh, dir, pr.HeadRefName)
	}()
	go func() {
		defer wg.Done()
		comments, commentsErr = fetchAllComments(ctx, gh, dir, owner, repo, prNumber, opts)
	}()
	go func() {
		defer wg.Done()
		// Base detection is best-effort — the changeset will skip the
		// BaseMoved signal if we can't read the SHA, so we swallow errors here.
		baseSHA, _ = fetchBaseSHA(ctx, gh, dir, pr.BaseRefName)
	}()
	wg.Wait()
	// A best-effort fetch that dies is DEGRADED, not fatal.
	//
	// Two of the four concurrent fetches used to abort the whole snapshot, while
	// the other two already tolerated failure — unresolved threads mark -1 for
	// "unknown" and the base SHA is swallowed outright. So a tick always knew how
	// to proceed on partial data; these two calls just did not let it.
	//
	// Both are throttle-prone: `actions/runs` and the comments pagination are
	// polled every tick per PR, and are the first endpoints GitHub's secondary
	// rate limit hits. Concurrent runs tripped it and then could not tick at all
	// for 35 minutes, each returning HTTP 403 here while every other signal in the
	// snapshot was fine — prdozer caused the outage and had no way to ride it out.
	//
	// Losing one signal costs one trigger for one tick: CIFailed for failed runs,
	// NewComments for comments. Losing the snapshot costs the tick, and every tick
	// after it for as long as the throttle lasts.
	// Every best-effort fetch degrades through ONE list, so a new fetch cannot be
	// added on the fatal path by omission.
	//
	// The previous shape fixed only the failed-runs branch and left the comments
	// branch three lines below it still returning an error. A throttled tick got
	// exactly one step further before aborting on the next fatal fetch — the same
	// wedge, a different message. Two adjacent branches that must behave
	// identically is a class of bug, not an instance; collapsing them removes the
	// place the mistake can live.
	//
	// Errors are SCRUBBED, not raw: these strings are logged, persisted into the
	// run record and Slacked, and a gh error can carry the endpoint config
	// including key-bearing env vars.
	//
	// The entry deliberately holds no per-fetch "clear" closure: a struct with a
	// string beside a func pointer trips govet fieldalignment (40 pointer bytes,
	// could be 32), which gates this repo — govet runs enable-all and
	// fieldalignment is not in .golangci.yml's disable list, the same constraint
	// the Snapshot struct above is ordered under. No permutation helps; all six
	// are 40. Nothing is lost by dropping it: see the zeroing note below.
	for _, f := range []struct {
		err   error
		label string
	}{
		{failedErr, fmt.Sprintf("failed runs for %s", pr.HeadRefName)},
		{commentsErr, fmt.Sprintf("comments for #%d", prNumber)},
	} {
		if f.err == nil {
			continue
		}
		degraded = append(degraded, fmt.Sprintf("%s: %s", f.label, safeErrString(f.err)))
	}
	// Belt-and-braces: a degraded signal must contribute no data, so a caller
	// cannot read a half-populated slice as a complete answer.
	//
	// These are UNREACHABLE today and no test can kill them: every error path in
	// both fetchFailedRunIDs and fetchAllComments returns a nil slice beside its
	// error, so the assignments never observably change anything (deleting them
	// leaves the suite green — verified). They are kept as a local guard rather
	// than deleted because the property they enforce — errored ⇒ no data — is
	// currently a convention of two producers three call layers away that no
	// signature enforces. A future fetch that returns a partially paginated list
	// with its error would otherwise leak it into the snapshot silently, and the
	// cost of holding the line here is two branches.
	if failedErr != nil {
		failed = nil
	}
	if commentsErr != nil {
		comments = nil
	}
	pr.TotalCommits = totalCommits
	return &Snapshot{
		TakenAt:            time.Now().UTC(),
		PR:                 *pr,
		Comments:           comments,
		FailedRunIDs:       failed,
		BaseSHA:            baseSHA,
		StatusRollup:       summarizeRollup(pr.StatusCheckRollup),
		ChangesRequestedBy: changesRequestedBy(pr.LatestReviews),
		IsBotReviewer:      botReviewers(pr.LatestReviews),
		UnresolvedThreads:  unresolved,
		Degraded:           degraded,
	}, nil
}

// fetchCommitCount reads the PR's exact commit count.
//
// GraphQL exposes it as a scalar `totalCount`, so one unpaginated query is
// enough — unlike `gh pr view --json commits`, which fetches a `first:100` page
// and reports its length. That difference is the whole point of this call: the
// scope guard charges pushes by commit delta, and a length pinned at 100
// charges a growing PR its floor of one per tick forever.
//
// The missing `first:`/`last:` is deliberate, not an oversight: GitHub only
// demands a page size when the selection reaches into the connection's
// `nodes`/`edges`. A selection of nothing but the `totalCount` scalar is served
// as-is — verified against this very PR, which answers `{"totalCount":9}`. No
// unit test can pin that, since the schema lives on GitHub's side; adding a
// page size would be harmless but would also re-import the 100-cap thinking
// this call exists to escape.
func fetchCommitCount(ctx context.Context, gh wt.GHRunner, dir, owner, repo string, prNumber int) (int, error) {
	const query = `query($owner:String!,$repo:String!,$pr:Int!){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$pr){
      commits{ totalCount }
    }
  }
}`
	res, err := gh.Run(ctx, []string{
		"api", "graphql",
		"-f", "query=" + query,
		"-F", "owner=" + owner,
		"-F", "repo=" + repo,
		"-F", fmt.Sprintf("pr=%d", prNumber),
		"--jq", `.data.repository.pullRequest.commits.totalCount`,
	}, dir)
	if err != nil {
		return 0, ghError(err, res)
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		// Unknown, not zero: zero would read as a PR with no commits and hand
		// the next push the floor charge instead of its real delta.
		return 0, fmt.Errorf("empty commit count")
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parse commit count %q: %w", out, err)
	}
	return n, nil
}

// fetchUnresolvedThreads counts review threads that are neither resolved nor
// outdated — the work reviewers are still asking for, and the primary signal
// for whether a PR is getting better or worse across polish rounds.
//
// Outdated threads are excluded deliberately: they attach to lines a later
// commit already replaced, so counting them would make any rebase look like a
// regression.
//
// Paginated rather than capped at one page: a truncated count reads as a
// healthier PR than it is, which is the one direction the divergence guard must
// not be wrong in — it would let a regression past. `gh api graphql --paginate`
// walks pageInfo for us and emits one --jq result per page, so the counts are
// summed here.
func fetchUnresolvedThreads(ctx context.Context, gh wt.GHRunner, dir, owner, repo string, prNumber int) (int, error) {
	const query = `query($owner:String!,$repo:String!,$pr:Int!,$endCursor:String){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$pr){
      reviewThreads(first:100,after:$endCursor){
        pageInfo{ hasNextPage endCursor }
        nodes { isResolved isOutdated }
      }
    }
  }
}`
	res, err := gh.Run(ctx, []string{
		"api", "graphql", "--paginate",
		"-f", "query=" + query,
		"-F", "owner=" + owner,
		"-F", "repo=" + repo,
		"-F", fmt.Sprintf("pr=%d", prNumber),
		"--jq", `[.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved==false and .isOutdated==false)] | length`,
	}, dir)
	if err != nil {
		return 0, ghError(err, res)
	}
	pages := strings.Fields(res.Stdout)
	if len(pages) == 0 {
		// No count at all is unknown, not zero. Returning 0 here would read as a
		// perfectly healthy PR and hand the guard a baseline nothing can beat.
		return 0, fmt.Errorf("empty unresolved thread count")
	}
	total := 0
	for _, page := range pages {
		n, err := strconv.Atoi(page)
		if err != nil {
			return 0, fmt.Errorf("parse unresolved thread count %q: %w", strings.TrimSpace(res.Stdout), err)
		}
		total += n
	}
	return total, nil
}

// botReviewers maps the stripped login of each app reviewer to true, keyed the
// same way as ChangesRequestedBy so the two line up.
func botReviewers(reviews []reviewRow) map[string]bool {
	out := make(map[string]bool)
	for _, r := range reviews {
		if login, ok := strings.CutSuffix(r.Author.Login, "[bot]"); ok {
			out[login] = true
		}
	}
	return out
}

// changesRequestedBy returns the logins whose most recent review requested
// changes. gh's latestReviews already collapses to one entry per reviewer, so
// a reviewer who later approved does not appear here.
func changesRequestedBy(reviews []reviewRow) []string {
	var out []string
	seen := make(map[string]bool, len(reviews))
	for _, r := range reviews {
		if r.State != "CHANGES_REQUESTED" || r.Author.Login == "" {
			continue
		}
		// A bot's review author is "app-name[bot]", but it must be re-requested
		// as "app-name" — the [bot] suffix is a display form the reviewer API
		// does not accept.
		login := strings.TrimSuffix(r.Author.Login, "[bot]")
		if seen[login] {
			continue
		}
		seen[login] = true
		out = append(out, login)
	}
	return out
}

func fetchPRDetails(ctx context.Context, gh wt.GHRunner, dir string, n int) (*PRDetails, error) {
	args := []string{
		"pr", "view", strconv.Itoa(n),
		"--json", "number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup,latestReviews,additions,deletions,changedFiles,commits",
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	var pr PRDetails
	if err := json.Unmarshal([]byte(res.Stdout), &pr); err != nil {
		return nil, fmt.Errorf("parse pr view: %w", err)
	}
	return &pr, nil
}

func summarizeRollup(rows []statusCheckRow) string {
	if len(rows) == 0 {
		return ""
	}
	anyPending := false
	anyFailure := false
	for _, c := range rows {
		concl := strings.ToUpper(c.Conclusion)
		if concl == "" {
			concl = strings.ToUpper(c.State)
		}
		switch {
		case concl == "FAILURE", concl == "TIMED_OUT", concl == "CANCELLED", concl == "ERROR":
			anyFailure = true
		case strings.EqualFold(c.Status, "IN_PROGRESS"), strings.EqualFold(c.Status, "QUEUED"), concl == "PENDING":
			anyPending = true
		}
	}
	switch {
	case anyFailure:
		return StatusFailure
	case anyPending:
		return StatusPending
	default:
		return StatusSuccess
	}
}

type ghRunListItem struct {
	DatabaseID int64 `json:"databaseId"`
}

func fetchFailedRunIDs(ctx context.Context, gh wt.GHRunner, dir, branch string) ([]int64, error) {
	if branch == "" {
		return nil, nil
	}
	args := []string{
		"run", "list",
		"--branch", branch,
		"--status", "failure",
		"--json", "databaseId",
		"--limit", "10",
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	var items []ghRunListItem
	if err := json.Unmarshal([]byte(res.Stdout), &items); err != nil {
		return nil, fmt.Errorf("parse run list: %w", err)
	}
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.DatabaseID)
	}
	return out, nil
}

type ghComment struct {
	CreatedAt time.Time   `json:"created_at"`
	User      ghUser      `json:"user"`
	ID        json.Number `json:"id"`
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "Bot" for bot accounts
}

func fetchAllComments(ctx context.Context, gh wt.GHRunner, dir, owner, repo string, n int, opts SnapshotOptions) ([]CommentRef, error) {
	inline, err := fetchComments(ctx, gh, dir, fmt.Sprintf("repos/%s/%s/pulls/%d/comments", owner, repo, n), "inline", opts)
	if err != nil {
		return nil, err
	}
	issue, err := fetchComments(ctx, gh, dir, fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, repo, n), "issue", opts)
	if err != nil {
		return nil, err
	}
	return append(inline, issue...), nil
}

func fetchComments(ctx context.Context, gh wt.GHRunner, dir, endpoint, source string, opts SnapshotOptions) ([]CommentRef, error) {
	args := []string{"api", "--paginate", endpoint}
	if !opts.CommentsSince.IsZero() {
		args = append(args, "-f", "since="+opts.CommentsSince.UTC().Format(time.RFC3339))
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	body := strings.TrimSpace(res.Stdout)
	if body == "" {
		return nil, nil
	}
	// `gh api --paginate` emits one JSON array per page concatenated back-to-
	// back (e.g. `[a,b][c,d]`), which is NOT valid as a single JSON value.
	// Decode page arrays in a loop and flatten.
	dec := json.NewDecoder(strings.NewReader(body))
	var raw []ghComment
	for dec.More() {
		var page []ghComment
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("parse comments (%s): %w", source, err)
		}
		raw = append(raw, page...)
	}
	out := make([]CommentRef, 0, len(raw))
	for _, c := range raw {
		// Inline (/pulls/{n}/comments) and issue (/issues/{n}/comments) comment
		// IDs come from separate GitHub ID sequences and can collide. Namespace
		// the dedup key by source so two distinct comments with the same
		// numeric ID aren't silently dropped as duplicates.
		out = append(out, CommentRef{
			ID:      source + ":" + string(c.ID),
			Source:  source,
			Author:  c.User.Login,
			IsBot:   c.User.Type == "Bot" || strings.HasSuffix(c.User.Login, "[bot]"),
			IsSelf:  opts.Self != "" && c.User.Login == opts.Self,
			Created: c.CreatedAt,
		})
	}
	return out, nil
}

func fetchBaseSHA(ctx context.Context, gh wt.GHRunner, dir, base string) (string, error) {
	if base == "" {
		return "", nil
	}
	// Branch names may contain slashes (e.g. "release/1.0"), which would be
	// parsed as extra path segments. Escape the branch as a single segment.
	res, err := gh.Run(ctx, []string{
		"api", fmt.Sprintf("repos/{owner}/{repo}/git/refs/heads/%s", url.PathEscape(base)),
		"--jq", ".object.sha",
	}, dir)
	if err != nil {
		return "", ghError(err, res)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// repoSlugFromURL parses an HTML PR URL to (owner, repo).
// Example: https://github.com/sycamore-labs/kernel/pull/2318 → ("sycamore-labs", "kernel").
func repoSlugFromURL(url string) (string, string, error) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(url, prefix) {
		return "", "", fmt.Errorf("not a github.com URL")
	}
	rest := strings.TrimPrefix(url, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("malformed URL")
	}
	return parts[0], parts[1], nil
}

func ghError(err error, res *wt.CmdResult) error {
	if res != nil && res.Stderr != "" {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(res.Stderr))
	}
	return err
}

// CurrentGitHubLogin reports the login `gh` is authenticated as, for
// self-comment filtering.
//
// Load-bearing rather than cosmetic: a snapshot marks a comment IsSelf only
// when its author matches, and NewComments is an unconditional polish trigger.
// With no login every reply prdozer posts comes back as somebody else's new
// comment, so the run polishes in response to itself. See WithSelfLogin.
func CurrentGitHubLogin(ctx context.Context, gh wt.GHRunner) (string, error) {
	res, err := gh.Run(ctx, []string{"api", "user", "--jq", ".login"}, "")
	if err != nil {
		if res != nil && res.Stderr != "" {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(res.Stderr))
		}
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
