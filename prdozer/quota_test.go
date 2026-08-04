package prdozer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClaudeUsage_Exhausted(t *testing.T) {
	t.Parallel()
	assert.False(t, ClaudeUsage{FiveHourUtilization: 26}.Exhausted(), "the real observed value should pass")
	assert.False(t, ClaudeUsage{FiveHourUtilization: 90}.Exhausted(), "the threshold itself is not over")
	assert.True(t, ClaudeUsage{FiveHourUtilization: 91}.Exhausted())
}
