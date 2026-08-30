package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
)

func TestChannelAllowsCallerAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		mode    channel.CallerAccessMode
		members map[int]struct{}
		keyID   int
		allowed bool
	}{
		{name: "public allows unlisted", mode: channel.CallerAccessModePublic, keyID: 7, allowed: true},
		{name: "allowlist allows member", mode: channel.CallerAccessModeAllowlist, members: map[int]struct{}{7: {}}, keyID: 7, allowed: true},
		{name: "allowlist denies unlisted", mode: channel.CallerAccessModeAllowlist, members: map[int]struct{}{8: {}}, keyID: 7, allowed: false},
		{name: "denylist denies member", mode: channel.CallerAccessModeDenylist, members: map[int]struct{}{7: {}}, keyID: 7, allowed: false},
		{name: "denylist allows unlisted", mode: channel.CallerAccessModeDenylist, members: map[int]struct{}{8: {}}, keyID: 7, allowed: true},
		{name: "unknown fails closed", mode: channel.CallerAccessMode("future-mode"), keyID: 7, allowed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Channel{
				Channel:                  &ent.Channel{CallerAccessMode: tt.mode},
				cachedCallerACLMemberIDs: tt.members,
			}
			require.Equal(t, tt.allowed, ch.AllowsCallerAPIKey(tt.keyID))
		})
	}

}
