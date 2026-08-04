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
	Number            int              `json:"number"`
	// Additions/Deletions/ChangedFiles size the PR's diff. Fetched in the same
	// gh call as everything else, so the scope guard costs no extra round trip.
	Additions    int  `json:"additions"`
	Deletions    int  `json:"deletions"`
	ChangedFiles int  `json:"changedFiles"`
	IsDraft      bool `json:"isDraft"`
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
		failedErr, commentsErr error
	)
	wg.Add(4)
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
	if failedErr != nil {
		return nil, fmt.Errorf("failed runs for %s: %w", pr.HeadRefName, failedErr)
	}
	if commentsErr != nil {
		return nil, fmt.Errorf("comments for #%d: %w", prNumber, commentsErr)
	}
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
	}, nil
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
		"--json", "number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup,latestReviews,additions,deletions,changedFiles",
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
