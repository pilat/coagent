package tool

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDynamicToolResultBudgetForWindow(t *testing.T) {
	small := ToolResultBudgetForWindow(15000, MaxToolResultSize)
	large := ToolResultBudgetForWindow(200000, MaxToolResultSize)

	assert.Less(t, small, large, "smaller context window should produce smaller budget")
	assert.Less(t, small, MaxToolResultSize, "budget for 15K window should be below MaxToolResultSize")

	huge := ToolResultBudgetForWindow(1000000, MaxToolResultSize)
	assert.LessOrEqual(t, huge, MaxToolResultSize, "budget should not exceed MaxToolResultSize")
}
