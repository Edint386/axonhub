package gql

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// Custom guard: realtime per-second billing adds duration_seconds to upstream's
// PriceItemCode enum. Without it GraphQL rejects saving a channel price that uses
// the item (variable validation, HTTP 422). See CUSTOM_BRANCH_RULES.md section 5.
func TestPriceItemCodeEnumHasDurationSeconds(t *testing.T) {
	schema, err := os.ReadFile("price.graphql")
	require.NoError(t, err)

	block := regexp.MustCompile(`(?s)enum PriceItemCode \{(.*?)\n\}`).FindSubmatch(schema)
	require.Len(t, block, 2, "enum PriceItemCode should exist in price.graphql")
	require.Regexp(t, `(?m)^\s*duration_seconds\s*$`, string(block[1]))
}
