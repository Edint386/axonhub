package gql

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoUnimplementedResolverStubs fails when gqlgen leaves a generated
// "not implemented" stub behind, e.g. after a merge keeps a GraphQL field but
// drops its gqlgen.yml binding. Such a stub panics whenever the field is queried.
func TestNoUnimplementedResolverStubs(t *testing.T) {
	// Upstream itself leaves Query.nodes unimplemented.
	allowed := map[string]bool{"Nodes - nodes": true}
	stub := regexp.MustCompile(`panic\(fmt\.Errorf\("not implemented: ([^"]+)"\)\)`)

	files, err := filepath.Glob("*.resolvers.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		content, err := os.ReadFile(file)
		require.NoError(t, err)

		for _, match := range stub.FindAllSubmatch(content, -1) {
			name := string(match[1])
			require.True(t, allowed[name], "%s has an unimplemented resolver stub: %s", file, name)
		}
	}
}
