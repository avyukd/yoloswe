package reviewer

import "strings"

// isResumeUnavailableMessage reports whether a backend error means "the
// session you asked me to resume isn't there" — as opposed to a real failure.
// Callers use it to degrade a stale --resume-session-id into a fresh session
// tagged resume_status=fallback instead of losing the whole review round.
//
// The vocabulary is deliberately cross-backend: cursor/codex/gemini speak of
// sessions, threads, and chats; the Claude CLI reports an unknown --resume id
// as "No conversation found with session ID …".
func isResumeUnavailableMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "thread not found") ||
		strings.Contains(msg, "chat not found") ||
		strings.Contains(msg, "conversation not found") ||
		strings.Contains(msg, "no conversation found") ||
		strings.Contains(msg, "session expired") ||
		strings.Contains(msg, "thread expired") ||
		strings.Contains(msg, "chat expired") ||
		strings.Contains(msg, "conversation expired")
}

func reviewErrorResult(resumeStatus ResumeStatus, err error) (*ReviewResult, error) {
	return &ReviewResult{
		Success:      false,
		ErrorMessage: err.Error(),
		ResumeStatus: resumeStatus,
	}, err
}

func resumeStatusAfterSessionReady(status ResumeStatus, requestedID, actualID string) ResumeStatus {
	if requestedID == "" {
		return status
	}
	if actualID == requestedID {
		return ResumeStatusOK
	}
	return ResumeStatusFallback
}
