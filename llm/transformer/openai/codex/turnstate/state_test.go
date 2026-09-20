package turnstate

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fakeState(n int) string {
	prefix := statePrefix
	if n < len(prefix) {
		n = len(prefix)
	}
	return prefix + strings.Repeat("A", n-len(prefix))
}

func TestValid(t *testing.T) {
	require.True(t, Valid(fakeState(ProLength), ProLength))
	require.True(t, Valid(fakeState(TeamLength), TeamLength))
	require.False(t, Valid(fakeState(ProLength), TeamLength))
	require.False(t, Valid("not-a-state", ProLength))
	require.False(t, Valid("gAAAAA with space", 16))
	require.True(t, Is312(fakeState(Signal312)))
	require.False(t, Is312(fakeState(ProLength)))
}

func TestInjectStripsClientValue(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-turn-state", "client-state")
	Inject(h, fakeState(ProLength))
	require.Equal(t, fakeState(ProLength), HeaderValue(h))
	require.Len(t, h.Values(Header), 1)
}

func TestTargetLength(t *testing.T) {
	require.Equal(t, ProLength, TargetLength("pro"))
	require.Equal(t, TeamLength, TargetLength("team"))
	require.Equal(t, ProLength, TargetLength(""))
	require.Equal(t, PlanTeam, NormalizePlan("TEAM"))
}

func TestCompletionObserverMatchesModel(t *testing.T) {
	o := NewCompletionObserver("gpt-6-astra")
	o.Write([]byte("event: response.completed\n"))
	o.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\"}}\n\n"))
	o.Finish()
	complete, matches, actual := o.Result()
	require.True(t, complete)
	require.True(t, matches)
	require.Equal(t, "gpt-6-astra", actual)

	o = NewCompletionObserver("gpt-6-astra")
	o.WriteJSON([]byte(`{"object":"response","status":"completed","model":"gpt-5.6-luna"}`))
	complete, matches, actual = o.Result()
	require.True(t, complete)
	require.False(t, matches)
	require.Equal(t, "gpt-5.6-luna", actual)
}
