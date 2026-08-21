package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckProvider_NonExistent(t *testing.T) {
	status := checkProvider("nonexistent-provider-xyz")
	assert.False(t, status.Installed)
	assert.Equal(t, "unknown provider", status.Error)
}

func TestProviderAvailability_UnknownProvider(t *testing.T) {
	pa := &ProviderAvailability{statuses: map[string]ProviderStatus{}}
	assert.False(t, pa.IsInstalled("unknown"))
	status := pa.Status("unknown")
	assert.False(t, status.Installed)
	assert.Equal(t, "unknown", status.Provider)
}

func TestProviderAvailability_Accessors(t *testing.T) {
	pa := &ProviderAvailability{
		statuses: map[string]ProviderStatus{
			ProviderClaude: {Provider: ProviderClaude, Installed: true, Version: "1.0.0"},
			ProviderCodex:  {Provider: ProviderCodex, Installed: false, Error: "not found in PATH"},
			ProviderGemini: {Provider: ProviderGemini, Installed: true, Version: "2.0.0"},
			ProviderCursor: {Provider: ProviderCursor, Installed: false, Error: "not found in PATH"},
			ProviderAgy:    {Provider: ProviderAgy, Installed: false, Error: "not found in PATH"},
		},
	}

	assert.True(t, pa.IsInstalled(ProviderClaude))
	assert.False(t, pa.IsInstalled(ProviderCodex))
	assert.True(t, pa.IsInstalled(ProviderGemini))
	assert.False(t, pa.IsInstalled(ProviderCursor))
	assert.False(t, pa.IsInstalled(ProviderAgy))

	s := pa.Status(ProviderClaude)
	assert.Equal(t, "1.0.0", s.Version)

	all := pa.AllStatuses()
	require.Len(t, all, 5)
	assert.Equal(t, ProviderClaude, all[0].Provider)
	assert.Equal(t, ProviderCodex, all[1].Provider)
	assert.Equal(t, ProviderGemini, all[2].Provider)
	assert.Equal(t, ProviderCursor, all[3].Provider)
	assert.Equal(t, ProviderAgy, all[4].Provider)

	installed := pa.InstalledProviders()
	assert.Equal(t, []string{ProviderClaude, ProviderGemini}, installed)
}

func TestAllProviders(t *testing.T) {
	assert.Equal(t, []string{ProviderClaude, ProviderCodex, ProviderGemini, ProviderCursor, ProviderAgy}, AllProviders)
}

func TestBinaryForProvider(t *testing.T) {
	// "cursor" on PATH is the IDE launcher; the agent CLI is "agent".
	assert.Equal(t, "agent", BinaryForProvider(ProviderCursor))
	assert.Equal(t, "claude", BinaryForProvider(ProviderClaude))
	assert.Equal(t, "codex", BinaryForProvider(ProviderCodex))
	assert.Equal(t, "gemini", BinaryForProvider(ProviderGemini))
	assert.Equal(t, "agy", BinaryForProvider(ProviderAgy))
	// Unknown providers fall back to their own name.
	assert.Equal(t, "future-cli", BinaryForProvider("future-cli"))
}

func TestProviderInstallLabel(t *testing.T) {
	// Naming only the provider sends a user to the Cursor IDE, whose launcher
	// on PATH is the binary that made every cursor session die.
	assert.Equal(t, "cursor (install agent)", ProviderInstallLabel(ProviderCursor))
	// Providers whose binary is their own name stay unadorned.
	for _, p := range []string{ProviderClaude, ProviderCodex, ProviderGemini, ProviderAgy} {
		assert.Equal(t, p, ProviderInstallLabel(p))
	}
}
