package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// realRejection0145 is the verbatim error returned by codex-cli 0.145.0 for a
// thread/start carrying approvalPolicy "on-failure". Keeping the real string
// here means the parser is tested against what the server actually emits, not
// against a paraphrase of it.
const realRejection0145 = "Invalid request: unknown variant `on-failure`, " +
	"expected one of `untrusted`, `on-request`, `granular`, `never`"

func TestParseApprovalPolicyRejection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err           error
		name          string
		wantRejected  string
		wantSupported []string
		wantOK        bool
	}{
		{
			name:          "codex 0.145 rejects on-failure",
			err:           &RPCError{Code: -32600, Message: realRejection0145},
			wantOK:        true,
			wantRejected:  "on-failure",
			wantSupported: []string{"untrusted", "on-request", "granular", "never"},
		},
		{
			name:          "wrapped rpc error is still parsed",
			err:           fmt.Errorf("thread/start: %w", &RPCError{Code: -32600, Message: realRejection0145}),
			wantOK:        true,
			wantRejected:  "on-failure",
			wantSupported: []string{"untrusted", "on-request", "granular", "never"},
		},
		{
			name:   "unknown variant for an unrelated enum is not an approval rejection",
			err:    &RPCError{Code: -32600, Message: "Invalid request: unknown variant `banana`, expected one of `http`, `https`"},
			wantOK: false,
		},
		{
			name:   "non-rpc error",
			err:    errors.New("connection reset"),
			wantOK: false,
		},
		{
			name:   "rpc error that is not a variant rejection",
			err:    &RPCError{Code: -32603, Message: "Internal error"},
			wantOK: false,
		},
		{
			name:   "nil error",
			err:    nil,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parseApprovalPolicyRejection(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("parseApprovalPolicyRejection() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.Rejected != tt.wantRejected {
				t.Errorf("Rejected = %q, want %q", got.Rejected, tt.wantRejected)
			}
			if len(got.Supported) != len(tt.wantSupported) {
				t.Fatalf("Supported = %v, want %v", got.Supported, tt.wantSupported)
			}
			for i, want := range tt.wantSupported {
				if got.Supported[i] != want {
					t.Errorf("Supported[%d] = %q, want %q", i, got.Supported[i], want)
				}
			}
		})
	}
}

func TestNegotiateInteractiveApprovalPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		want      ApprovalPolicy
		supported []string
		wantOK    bool
	}{
		{
			name:      "codex 0.145 set picks on-request",
			supported: []string{"untrusted", "on-request", "granular", "never"},
			want:      ApprovalPolicyOnRequest,
			wantOK:    true,
		},
		{
			name:      "codex 0.141 set also picks on-request",
			supported: []string{"untrusted", "on-failure", "on-request", "never"},
			want:      ApprovalPolicyOnRequest,
			wantOK:    true,
		},
		{
			name:      "falls back to on-failure when on-request is gone",
			supported: []string{"untrusted", "on-failure", "never"},
			want:      ApprovalPolicyOnFailure,
			wantOK:    true,
		},
		{
			name:      "falls back to untrusted as the last interactive option",
			supported: []string{"untrusted", "never"},
			want:      ApprovalPolicyUntrusted,
			wantOK:    true,
		},
		{
			// The critical safety case: "never" auto-approves everything, so
			// selecting it would silently disable the read-only write guard.
			// Failing is the correct outcome.
			name:      "never alone is not an acceptable fallback",
			supported: []string{"never"},
			wantOK:    false,
		},
		{
			name:      "empty support list fails",
			supported: nil,
			wantOK:    false,
		},
		{
			name:      "unrecognized policies only",
			supported: []string{"granular", "something-new"},
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := negotiateInteractiveApprovalPolicy(tt.supported)
			if ok != tt.wantOK {
				t.Fatalf("negotiateInteractiveApprovalPolicy() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("policy = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNegotiateApprovalPolicySend covers the end-to-end retry: this is the
// regression test for codex reviewers dying at startup on codex-cli 0.145.0,
// which rejects the "on-failure" policy the wrapper used to send.
func TestNegotiateApprovalPolicySend(t *testing.T) {
	t.Parallel()

	reject0145 := &RPCError{Code: -32600, Message: realRejection0145}
	okResp := &JSONRPCResponse{}

	t.Run("retries with a supported policy after rejection", func(t *testing.T) {
		t.Parallel()

		var sent []ApprovalPolicy
		resp, negotiated, err := negotiateApprovalPolicySend(
			context.Background(), "thread/start", ApprovalPolicyOnFailure,
			func(p ApprovalPolicy) interface{} { return p },
			func(_ context.Context, _ string, params interface{}) (*JSONRPCResponse, error) {
				p := params.(ApprovalPolicy)
				sent = append(sent, p)
				if p == ApprovalPolicyOnFailure {
					return nil, reject0145
				}
				return okResp, nil
			})

		if err != nil {
			t.Fatalf("negotiateApprovalPolicySend() error = %v, want nil", err)
		}
		if resp != okResp {
			t.Error("expected the retry's response to be returned")
		}
		if negotiated != ApprovalPolicyOnRequest {
			t.Errorf("negotiated = %q, want %q", negotiated, ApprovalPolicyOnRequest)
		}
		want := []ApprovalPolicy{ApprovalPolicyOnFailure, ApprovalPolicyOnRequest}
		if len(sent) != len(want) || sent[0] != want[0] || sent[1] != want[1] {
			t.Errorf("sent policies = %v, want %v", sent, want)
		}
	})

	t.Run("no retry when the first attempt succeeds", func(t *testing.T) {
		t.Parallel()

		calls := 0
		_, negotiated, err := negotiateApprovalPolicySend(
			context.Background(), "thread/start", ApprovalPolicyOnRequest,
			func(p ApprovalPolicy) interface{} { return p },
			func(_ context.Context, _ string, _ interface{}) (*JSONRPCResponse, error) {
				calls++
				return okResp, nil
			})

		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
		if calls != 1 {
			t.Errorf("send calls = %d, want 1", calls)
		}
		if negotiated != "" {
			t.Errorf("negotiated = %q, want empty (no renegotiation)", negotiated)
		}
	})

	t.Run("fails loudly rather than downgrading to never", func(t *testing.T) {
		t.Parallel()

		// A server offering only "never" cannot honor the read-only guard.
		// Silently accepting it would permit writes unnoticed.
		onlyNever := &RPCError{
			Code:    -32600,
			Message: "Invalid request: unknown variant `on-failure`, expected one of `never`",
		}
		calls := 0
		_, _, err := negotiateApprovalPolicySend(
			context.Background(), "thread/start", ApprovalPolicyOnFailure,
			func(p ApprovalPolicy) interface{} { return p },
			func(_ context.Context, _ string, _ interface{}) (*JSONRPCResponse, error) {
				calls++
				return nil, onlyNever
			})

		if !errors.Is(err, ErrNoInteractiveApprovalPolicy) {
			t.Fatalf("error = %v, want ErrNoInteractiveApprovalPolicy", err)
		}
		if calls != 1 {
			t.Errorf("send calls = %d, want 1 (must not retry with never)", calls)
		}
	})

	t.Run("unrelated errors propagate without a retry", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("connection reset")
		calls := 0
		_, _, err := negotiateApprovalPolicySend(
			context.Background(), "thread/start", ApprovalPolicyOnRequest,
			func(p ApprovalPolicy) interface{} { return p },
			func(_ context.Context, _ string, _ interface{}) (*JSONRPCResponse, error) {
				calls++
				return nil, boom
			})

		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if calls != 1 {
			t.Errorf("send calls = %d, want 1", calls)
		}
	})

	t.Run("no retry loop when the rejected policy is the best candidate", func(t *testing.T) {
		t.Parallel()

		// Server claims to support on-request yet rejected it. Retrying with
		// the same value would fail identically, so fail on the first attempt.
		contradictory := &RPCError{
			Code:    -32600,
			Message: "Invalid request: unknown variant `on-request`, expected one of `on-request`, `never`",
		}
		calls := 0
		_, _, err := negotiateApprovalPolicySend(
			context.Background(), "thread/start", ApprovalPolicyOnRequest,
			func(p ApprovalPolicy) interface{} { return p },
			func(_ context.Context, _ string, _ interface{}) (*JSONRPCResponse, error) {
				calls++
				return nil, contradictory
			})

		if err == nil {
			t.Fatal("expected an error")
		}
		if calls != 1 {
			t.Errorf("send calls = %d, want 1 (must not retry identically)", calls)
		}
	})

	t.Run("caller asking for never is left alone", func(t *testing.T) {
		t.Parallel()

		// "never" needs no handler, so a caller using it is not relying on the
		// approval guard; upgrading it to an interactive policy would block
		// forever waiting on approvals nobody answers.
		calls := 0
		_, _, err := negotiateApprovalPolicySend(
			context.Background(), "thread/start", ApprovalPolicyNever,
			func(p ApprovalPolicy) interface{} { return p },
			func(_ context.Context, _ string, _ interface{}) (*JSONRPCResponse, error) {
				calls++
				return nil, reject0145
			})

		if err == nil {
			t.Fatal("expected the original error")
		}
		if calls != 1 {
			t.Errorf("send calls = %d, want 1 (never must not be renegotiated)", calls)
		}
	})
}

// TestApprovalPolicyNegotiationErrorIsActionable pins the failure mode the fix
// exists to preserve: when no interactive policy is available the error must
// name the mismatch and stay matchable, so the caller fails loudly instead of
// continuing without the read-only guard.
func TestApprovalPolicyNegotiationErrorIsActionable(t *testing.T) {
	t.Parallel()

	err := &approvalPolicyNegotiationError{
		Rejected:  "on-failure",
		Supported: []string{"never"},
	}

	if !errors.Is(err, ErrNoInteractiveApprovalPolicy) {
		t.Fatalf("error not matchable via ErrNoInteractiveApprovalPolicy: %v", err)
	}

	msg := err.Error()
	for _, want := range []string{"on-failure", "never", "--read-only=false"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}
