package subscription

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// minimumAcceptedCLIClientVersion is the oldest Grok CLI version the xAI CLI
// proxy accepted as of 2026-10-12; older versions are rejected with "Your Grok
// CLI version (...) is outdated". A newer upstream version also satisfies this.
const minimumAcceptedCLIClientVersion = "1.0.13"

func TestCLIClientVersionIsAcceptedByXAI(t *testing.T) {
	require.GreaterOrEqual(t, compareVersions(t, CLIClientVersion, minimumAcceptedCLIClientVersion), 0,
		"CLIClientVersion %s is older than the minimum %s accepted by the xAI CLI proxy", CLIClientVersion, minimumAcceptedCLIClientVersion)
	require.True(t, strings.HasSuffix(CLIUserAgent, "/"+CLIClientVersion))
}

func compareVersions(t *testing.T, a, b string) int {
	t.Helper()

	partsA, partsB := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(partsA), len(partsB)) {
		va, vb := versionPart(t, partsA, i), versionPart(t, partsB, i)
		if va != vb {
			if va < vb {
				return -1
			}
			return 1
		}
	}

	return 0
}

func versionPart(t *testing.T, parts []string, index int) int {
	t.Helper()

	if index >= len(parts) {
		return 0
	}
	value, err := strconv.Atoi(parts[index])
	require.NoError(t, err, "invalid version segment %q", parts[index])

	return value
}
