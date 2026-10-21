package gql

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// Custom guard: the admin UI sends settings-only edits through upstream's
// updateChannelSettings patch mutation. Its GraphQL input must list the custom
// settings fields, otherwise a patch that carries the channel quota, its
// timezone or the health probe settings is rejected as an unknown field.
func TestUpdateChannelSettingsInputSchemaHasCustomFields(t *testing.T) {
	schema, err := os.ReadFile("axonhub.graphql")
	require.NoError(t, err)

	block := regexp.MustCompile(`(?s)input UpdateChannelSettingsInput \{(.*?)\n\}`).FindSubmatch(schema)
	require.Len(t, block, 2, "input UpdateChannelSettingsInput should exist in axonhub.graphql")

	for _, field := range []string{
		"quota: APIKeyQuotaInput",
		"quotaTimezone: String",
		"healthProbe: ChannelHealthProbeSettingsInput",
	} {
		require.Contains(t, string(block[1]), field)
	}
}
