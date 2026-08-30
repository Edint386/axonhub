package biz

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelcalleraclmember"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

func setupChannelCallerAccessTest(t *testing.T) (*ChannelService, *ent.Client, context.Context) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := ent.NewContext(t.Context(), client)
	ctx = authz.WithTestBypass(ctx)

	return svc, client, ctx
}

func createChannelCallerAccessTestChannel(t *testing.T, client *ent.Client, ctx context.Context, name string) *ent.Channel {
	t.Helper()

	entity, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName(name).
		SetBaseURL("https://api.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-key"}).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	return entity
}

func TestChannelCallerAccessCacheDuplicateAndDeleteLifecycle(t *testing.T) {
	svc, client, ctx := setupChannelCallerAccessTest(t)
	source := createChannelCallerAccessTestChannel(t, client, ctx, "caller-access-lifecycle-source")
	member := createChannelCallerAccessTestAPIKey(t, client, ctx, "lifecycle-member")
	other := createChannelCallerAccessTestAPIKey(t, client, ctx, "lifecycle-other")

	_, err := svc.SetChannelCallerAccessPolicy(ctx, SetChannelCallerAccessPolicyInput{
		ChannelID:       source.ID,
		Mode:            channel.CallerAccessModeAllowlist,
		MemberAPIKeyIDs: []int{member.ID},
	})
	require.NoError(t, err)

	loaded, _, _, err := svc.reloadEnabledChannels(ctx, nil, time.Time{})
	require.NoError(t, err)
	svc.SetEnabledChannelsForTest(loaded)
	cached := svc.GetEnabledChannel(source.ID)
	require.NotNil(t, cached)
	require.True(t, cached.AllowsCallerAPIKey(member.ID))
	require.False(t, cached.AllowsCallerAPIKey(other.ID))

	duplicated, err := svc.DuplicateChannel(ctx, source.ID, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		BaseURL:          lo.ToPtr("https://api.example.com/v1"),
		Name:             "caller-access-lifecycle-copy",
		Credentials:      objects.ChannelCredentials{APIKey: "provider-key-copy"},
		SupportedModels:  []string{"test-model"},
		DefaultTestModel: "test-model",
	})
	require.NoError(t, err)
	require.Equal(t, channel.CallerAccessModeAllowlist, duplicated.CallerAccessMode)
	copiedMembers, err := client.ChannelCallerACLMember.Query().
		Where(channelcalleraclmember.ChannelIDEQ(duplicated.ID)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, copiedMembers, 1)
	require.Equal(t, member.ID, copiedMembers[0].APIKeyID)

	require.NoError(t, svc.DeleteChannel(ctx, duplicated.ID))
	memberCount, err := client.ChannelCallerACLMember.Query().
		Where(channelcalleraclmember.ChannelIDEQ(duplicated.ID)).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, memberCount)
}

func createChannelCallerAccessTestAPIKey(t *testing.T, client *ent.Client, ctx context.Context, name string) *ent.APIKey {
	t.Helper()

	entity, err := client.APIKey.Create().
		SetProjectID(1).
		SetName(name).
		SetKey("channel-caller-access-" + name).
		Save(ctx)
	require.NoError(t, err)

	return entity
}

func callerAccessForChannel(t *testing.T, items []*APIKeyChannelCallerAccess, channelID int) *APIKeyChannelCallerAccess {
	t.Helper()

	for _, item := range items {
		if item.Channel.ID == channelID {
			return item
		}
	}

	t.Fatalf("channel %d was not present in API key caller access projection", channelID)
	return nil
}

func TestChannelService_ChannelCallerAccessPolicy(t *testing.T) {
	svc, client, ctx := setupChannelCallerAccessTest(t)
	ch := createChannelCallerAccessTestChannel(t, client, ctx, "caller-access-channel")
	keyA := createChannelCallerAccessTestAPIKey(t, client, ctx, "key-a")
	keyB := createChannelCallerAccessTestAPIKey(t, client, ctx, "key-b")
	keyC := createChannelCallerAccessTestAPIKey(t, client, ctx, "key-c")

	t.Run("defaults to public with an empty member set", func(t *testing.T) {
		policy, err := svc.ChannelCallerAccessPolicy(ctx, ch.ID)
		require.NoError(t, err)
		require.Equal(t, channel.CallerAccessModePublic, policy.Mode)
		require.Empty(t, policy.Members)
	})

	t.Run("allowlist replacement deduplicates members and projects by API key", func(t *testing.T) {
		policy, err := svc.SetChannelCallerAccessPolicy(ctx, SetChannelCallerAccessPolicyInput{
			ChannelID:       ch.ID,
			Mode:            channel.CallerAccessModeAllowlist,
			MemberAPIKeyIDs: []int{keyB.ID, keyA.ID, keyB.ID},
		})
		require.NoError(t, err)
		require.Equal(t, channel.CallerAccessModeAllowlist, policy.Mode)
		require.Len(t, policy.Members, 2)
		require.ElementsMatch(t, []int{keyA.ID, keyB.ID}, []int{policy.Members[0].ID.ID, policy.Members[1].ID.ID})

		keyAAccess, err := svc.APIKeyChannelCallerAccess(ctx, keyA.ID)
		require.NoError(t, err)
		keyAChannel := callerAccessForChannel(t, keyAAccess, ch.ID)
		require.True(t, keyAChannel.IsMember)
		require.True(t, keyAChannel.Allowed)

		keyCAccess, err := svc.APIKeyChannelCallerAccess(ctx, keyC.ID)
		require.NoError(t, err)
		keyCChannel := callerAccessForChannel(t, keyCAccess, ch.ID)
		require.False(t, keyCChannel.IsMember)
		require.False(t, keyCChannel.Allowed)
	})

	t.Run("denylist reverses membership meaning", func(t *testing.T) {
		policy, err := svc.SetChannelCallerAccessPolicy(ctx, SetChannelCallerAccessPolicyInput{
			ChannelID:       ch.ID,
			Mode:            channel.CallerAccessModeDenylist,
			MemberAPIKeyIDs: []int{keyB.ID},
		})
		require.NoError(t, err)
		require.Equal(t, channel.CallerAccessModeDenylist, policy.Mode)

		keyAAccess, err := svc.APIKeyChannelCallerAccess(ctx, keyA.ID)
		require.NoError(t, err)
		require.False(t, callerAccessForChannel(t, keyAAccess, ch.ID).IsMember)
		require.True(t, callerAccessForChannel(t, keyAAccess, ch.ID).Allowed)

		keyBAccess, err := svc.APIKeyChannelCallerAccess(ctx, keyB.ID)
		require.NoError(t, err)
		require.True(t, callerAccessForChannel(t, keyBAccess, ch.ID).IsMember)
		require.False(t, callerAccessForChannel(t, keyBAccess, ch.ID).Allowed)
	})

	t.Run("public canonicalizes away irrelevant members", func(t *testing.T) {
		policy, err := svc.SetChannelCallerAccessPolicy(ctx, SetChannelCallerAccessPolicyInput{
			ChannelID:       ch.ID,
			Mode:            channel.CallerAccessModePublic,
			MemberAPIKeyIDs: []int{keyA.ID, keyB.ID},
		})
		require.NoError(t, err)
		require.Empty(t, policy.Members)
		require.Empty(t, client.ChannelCallerACLMember.Query().AllX(ctx))
	})

	t.Run("invalid member rolls back the policy", func(t *testing.T) {
		_, err := svc.SetChannelCallerAccessPolicy(ctx, SetChannelCallerAccessPolicyInput{
			ChannelID:       ch.ID,
			Mode:            channel.CallerAccessModeAllowlist,
			MemberAPIKeyIDs: []int{keyA.ID, 999999},
		})
		require.ErrorContains(t, err, "not found")

		policy, err := svc.ChannelCallerAccessPolicy(ctx, ch.ID)
		require.NoError(t, err)
		require.Equal(t, channel.CallerAccessModePublic, policy.Mode)
		require.Empty(t, policy.Members)
	})
}

func TestChannelService_ChannelCallerAccessRequiresAllSystemScopes(t *testing.T) {
	svc, client, setupCtx := setupChannelCallerAccessTest(t)
	ch := createChannelCallerAccessTestChannel(t, client, setupCtx, "scope-channel")

	user := &ent.User{
		ID:    100,
		Email: "acl-scope-test@example.com",
		Scopes: []string{
			string(scopes.ScopeReadChannels),
			string(scopes.ScopeReadAPIKeys),
			string(scopes.ScopeWriteChannels),
		},
	}
	ctx := authz.NewUserContext(ent.NewContext(t.Context(), client), user.ID)
	ctx = contexts.WithUser(ctx, user)

	_, err := svc.SetChannelCallerAccessPolicy(ctx, SetChannelCallerAccessPolicyInput{
		ChannelID: ch.ID,
		Mode:      channel.CallerAccessModeAllowlist,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, string(scopes.ScopeWriteAPIKeys))
}
