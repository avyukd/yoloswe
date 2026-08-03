package main

import "testing"

// notify speaks for its caller, so exporting BRAMBLE_SESSION_ID into the tmux
// window is only useful if notify actually reads it back. These cases pin that
// consumer: without them the export is write-only and can silently rot.
func TestResolveOwnSessionID(t *testing.T) {
	tests := []struct {
		name    string
		flagID  string
		envID   string
		want    string
		wantErr bool
	}{
		{
			name:   "flag wins over env so the baked-in stop hook is unaffected",
			flagID: "builder-flag",
			envID:  "builder-env",
			want:   "builder-flag",
		},
		{
			name:  "env supplies the return address when the flag is omitted",
			envID: "builder-env",
			want:  "builder-env",
		},
		{
			name:   "flag alone still works outside a bramble session",
			flagID: "builder-flag",
			want:   "builder-flag",
		},
		{
			name:    "neither source is an error rather than a notify for empty ID",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOwnSessionID(tt.flagID, tt.envID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got session ID %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveOwnSessionID(%q, %q) = %q, want %q", tt.flagID, tt.envID, got, tt.want)
			}
		})
	}
}
