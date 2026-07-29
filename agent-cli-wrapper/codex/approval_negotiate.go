package codex

import (
	"errors"
	"regexp"
	"strings"
)

// The set of approval policies codex accepts is version-dependent: codex-cli
// 0.145.0 removed "on-failure", which 0.141.0 still accepts. Hosts across a
// fleet are not uniformly upgraded, so the wrapper cannot hardcode a single
// literal and cannot assume the constants in approval.go are current.
//
// Rather than probing (`codex app-server generate-json-schema` is authoritative
// but writes ~3.3MB per call), negotiation is driven by the app-server's own
// rejection, which enumerates what it supports:
//
//	{"error":{"code":-32600,"message":"Invalid request: unknown variant
//	 `on-failure`, expected one of `untrusted`, `on-request`, `granular`, `never`"}}
//
// Parsing that list keeps the wrapper correct as codex's enum drifts again,
// instead of trading one stale literal for another.

// unknownVariantRe matches the app-server's "unknown variant" rejection and
// captures the offending value and the backtick-quoted list of accepted ones.
var unknownVariantRe = regexp.MustCompile(
	"unknown variant `([^`]*)`, expected one of (.+)")

// backtickedRe extracts each `quoted` item from the "expected one of" tail.
var backtickedRe = regexp.MustCompile("`([^`]+)`")

// approvalPolicyRejection describes an app-server rejection of an approval
// policy value, including the policies it reported as supported.
type approvalPolicyRejection struct {
	// Rejected is the policy value the server refused.
	Rejected string
	// Supported lists the policy values the server said it accepts, in the
	// order the server reported them.
	Supported []string
}

// parseApprovalPolicyRejection reports whether err is an app-server rejection
// of an unknown approval-policy variant, and if so what the server said it
// supports.
//
// It only claims a rejection when the offending variant is one this wrapper
// could plausibly have sent as an approval policy. The "unknown variant"
// phrasing is generic serde output and can describe an unrelated enum field;
// treating those as approval-policy rejections would send the caller down a
// renegotiation path that cannot help.
func parseApprovalPolicyRejection(err error) (approvalPolicyRejection, bool) {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return approvalPolicyRejection{}, false
	}

	m := unknownVariantRe.FindStringSubmatch(rpcErr.Message)
	if m == nil {
		return approvalPolicyRejection{}, false
	}
	rejected := m[1]
	if !isKnownApprovalPolicyName(rejected) {
		return approvalPolicyRejection{}, false
	}

	var supported []string
	for _, item := range backtickedRe.FindAllStringSubmatch(m[2], -1) {
		supported = append(supported, item[1])
	}
	if len(supported) == 0 {
		return approvalPolicyRejection{}, false
	}
	return approvalPolicyRejection{Rejected: rejected, Supported: supported}, true
}

// isKnownApprovalPolicyName reports whether name is a value this wrapper may
// have sent as an approval policy. It covers both the policies the wrapper can
// emit today and ones removed by newer codex versions, since a rejection is
// precisely the case where the sent value is no longer in the server's enum.
func isKnownApprovalPolicyName(name string) bool {
	switch name {
	case "untrusted", "on-failure", "on-request", "never":
		return true
	}
	return false
}

// interactiveApprovalPreference lists the policies that cause codex to route
// approval requests to a wired ApprovalHandler, best first.
//
// "never" is deliberately absent: it auto-approves everything, which would
// silently disable the read-only guard. Falling back to it would turn a loud
// startup crash into an unnoticed loss of the only write protection available
// when bubblewrap sandboxing is unavailable — strictly worse than failing.
var interactiveApprovalPreference = []ApprovalPolicy{
	ApprovalPolicyOnRequest,
	ApprovalPolicyOnFailure,
	ApprovalPolicyUntrusted,
}

// negotiateInteractiveApprovalPolicy picks the most preferred approval policy
// that both triggers the approval handler and appears in supported.
//
// It returns false when the server supports no interactive policy at all, so
// the caller can fail loudly rather than silently downgrading to "never".
func negotiateInteractiveApprovalPolicy(supported []string) (ApprovalPolicy, bool) {
	set := make(map[string]struct{}, len(supported))
	for _, s := range supported {
		set[s] = struct{}{}
	}
	for _, p := range interactiveApprovalPreference {
		if _, ok := set[string(p)]; ok {
			return p, true
		}
	}
	return "", false
}

// ErrNoInteractiveApprovalPolicy is returned when the app-server rejected the
// requested approval policy and none of the interactive alternatives this
// wrapper knows about are supported either. The read-only guard cannot be
// honored in that case, and the caller must not proceed as if it were.
var ErrNoInteractiveApprovalPolicy = errors.New("no supported interactive codex approval policy")

// approvalPolicyNegotiationError explains an unrecoverable approval-policy
// mismatch in terms an operator can act on: what was sent, what the installed
// codex accepts, and why falling back is not an option.
type approvalPolicyNegotiationError struct {
	Rejected  string
	Supported []string
}

func (e *approvalPolicyNegotiationError) Error() string {
	return "codex rejected approval policy " + quoteBacktick(e.Rejected) +
		" and supports no interactive alternative (server accepts: " +
		strings.Join(e.Supported, ", ") + "; this wrapper can use: " +
		joinPolicies(interactiveApprovalPreference) +
		"). The read-only guard requires an interactive policy so Codex sends " +
		"approval requests to the handler; refusing to continue rather than " +
		"silently allowing writes. Upgrade or downgrade codex-cli, or rerun " +
		"with --read-only=false if unguarded writes are acceptable"
}

func (e *approvalPolicyNegotiationError) Unwrap() error {
	return ErrNoInteractiveApprovalPolicy
}

func quoteBacktick(s string) string { return "`" + s + "`" }

func joinPolicies(ps []ApprovalPolicy) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return strings.Join(out, ", ")
}
