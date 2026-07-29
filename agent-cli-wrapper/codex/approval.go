package codex

import "context"

// ApprovalPolicy controls tool execution approval.
type ApprovalPolicy string

// Not every constant below is accepted by every codex-cli: the app-server's
// approval enum drifts across versions (0.145.0 removed "on-failure", which
// 0.141.0 accepts). Prefer ApprovalPolicyOnRequest, which both accept. When the
// server rejects a policy it enumerates the ones it supports, and the client
// renegotiates from that list — see approval_negotiate.go.
const (
	// ApprovalPolicyUntrusted requires approval for everything.
	ApprovalPolicyUntrusted ApprovalPolicy = "untrusted"

	// ApprovalPolicyOnFailure approves unless command fails.
	//
	// Removed in codex-cli 0.145.0; sending it to a newer server fails with
	// an "unknown variant" error. Kept for older installs and for the
	// renegotiation preference list.
	ApprovalPolicyOnFailure ApprovalPolicy = "on-failure"

	// ApprovalPolicyOnRequest approves on explicit request. Accepted by every
	// codex-cli version we run; the default for the read-only guard.
	ApprovalPolicyOnRequest ApprovalPolicy = "on-request"

	// ApprovalPolicyNever auto-approves everything (use with caution).
	ApprovalPolicyNever ApprovalPolicy = "never"

	// Deprecated aliases for backwards compatibility
	ApprovalPolicySuggest  ApprovalPolicy = "untrusted"
	ApprovalPolicyAutoEdit ApprovalPolicy = "on-failure"
	ApprovalPolicyFullAuto ApprovalPolicy = "never"
)

// ApprovalRequest contains data for an approval request.
type ApprovalRequest struct {
	Input    map[string]interface{}
	ThreadID string
	TurnID   string
	ToolName string
}

// ApprovalResponse contains the response to an approval request.
type ApprovalResponse struct {
	UpdatedInput map[string]interface{}
	Message      string
	Approved     bool
}

// ApprovalHandler handles tool execution approval requests.
type ApprovalHandler interface {
	HandleApproval(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error)
}

// ApprovalHandlerFunc is a function adapter for ApprovalHandler.
type ApprovalHandlerFunc func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error)

// HandleApproval implements ApprovalHandler.
func (f ApprovalHandlerFunc) HandleApproval(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
	return f(ctx, req)
}

// AutoApproveHandler returns a handler that auto-approves all tools.
func AutoApproveHandler() ApprovalHandler {
	return ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		return &ApprovalResponse{Approved: true}, nil
	})
}

// DenyAllHandler returns a handler that denies all tools.
func DenyAllHandler() ApprovalHandler {
	return ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		return &ApprovalResponse{
			Approved: false,
			Message:  "tool execution denied by policy",
		}, nil
	})
}

// ReadOnlyHandler returns a handler that approves Bash (shell/read) tools
// but denies Write tools. This is a software-level read-only guard for use
// when the bubblewrap sandbox modes ("read-only", "workspace-write") are
// unavailable—e.g. on Ubuntu 24.04+ where AppArmor's
// apparmor_restrict_unprivileged_userns sysctl blocks bwrap from creating
// user namespaces.
//
// Limitation: this only blocks the Codex "Write" tool. Destructive shell
// commands (rm, git reset --hard, etc.) pass through Bash and cannot be
// reliably filtered here. The review prompt's instructions are the
// remaining constraint for shell behavior.
//
// Requires a non-"never" approval policy (e.g. "on-request") on the thread
// so that Codex actually sends approval requests to the handler.
func ReadOnlyHandler() ApprovalHandler {
	return ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		if req.ToolName == "Write" {
			return &ApprovalResponse{
				Approved: false,
				Message:  "file writes denied by read-only review policy",
			}, nil
		}
		return &ApprovalResponse{Approved: true}, nil
	})
}
