package tool

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDynamicToolResultBudgetForWindow(t *testing.T) {
	small := DynamicToolResultBudgetForWindow(15000)
	large := DynamicToolResultBudgetForWindow(200000)

	assert.Less(t, small, large, "smaller context window should produce smaller budget")
	assert.Less(t, small, MaxToolResultSize, "budget for 15K window should be below MaxToolResultSize")

	huge := DynamicToolResultBudgetForWindow(1000000)
	assert.LessOrEqual(t, huge, MaxToolResultSize, "budget should not exceed MaxToolResultSize")
}
