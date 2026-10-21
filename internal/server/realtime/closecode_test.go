package realtime

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestMapCloseCode(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{1000, 1000},
		{1001, 1001},
		{1002, 1002},
		{1003, 1003},
		{1004, 1011},
		{1005, 1000},
		{1006, 1011},
		{1007, 1007},
		{1008, 1008},
		{1009, 1009},
		{1010, 1011},
		{1011, 1011},
		{1012, 1012},
		{1013, 1013},
		{1014, 1011},
		{1015, 1011},
		{2999, 1011},
		{3000, 3000},
		{4999, 4999},
		{5000, 1011},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, mapCloseCode(tt.in), "code %d", tt.in)
	}
}

func TestTruncateReason(t *testing.T) {
	short := "bye"
	require.Equal(t, short, truncateReason(short))

	long := strings.Repeat("a", 200)
	require.Len(t, truncateReason(long), maxCloseReasonBytes)

	chinese := strings.Repeat("百炼会话结束", 30)
	got := truncateReason(chinese)
	require.LessOrEqual(t, len(got), maxCloseReasonBytes)
	require.True(t, utf8.ValidString(got))
}

func TestCloseInfo(t *testing.T) {
	code, reason, ok := closeInfo(&websocket.CloseError{Code: 1000, Text: "x"})
	require.True(t, ok)
	require.Equal(t, 1000, code)
	require.Equal(t, "x", reason)

	_, _, ok = closeInfo(&websocket.CloseError{Code: websocket.CloseAbnormalClosure})
	require.False(t, ok)

	_, _, ok = closeInfo(errors.New("boom"))
	require.False(t, ok)
}
