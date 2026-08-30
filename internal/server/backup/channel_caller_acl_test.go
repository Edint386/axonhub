package backup

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func TestBackupService_ChannelCallerACLRoundTripRemapsSourceIDs(t *testing.T) {
	sourceClient, sourceService, sourceCtx := setupBackupTest(t)
	defer sourceClient.Close()

	sourceUser, err := sourceClient.User.Query().Only(sourceCtx)
	require.NoError(t, err)
	sourceProject := createBackupTestProject(t, sourceClient, sourceCtx, "ACL Project", "source")
	sourceChannel := createBackupTestChannel(t, sourceClient, sourceCtx, "ACL Channel", channel.TypeOpenai)
	sourceChannel, err = sourceClient.Channel.UpdateOne(sourceChannel).
		SetCallerAccessMode(channel.CallerAccessModeAllowlist).
		Save(sourceCtx)
	require.NoError(t, err)
	sourceAPIKey := createBackupTestAPIKey(t, sourceClient, sourceCtx, sourceUser, sourceProject, "ACL Key", "sk-acl-source")

	_, err = sourceClient.ChannelCallerACLMember.Create().
		SetChannelID(sourceChannel.ID).
		SetAPIKeyID(sourceAPIKey.ID).
		Save(sourceCtx)
	require.NoError(t, err)

	data, err := sourceService.Backup(sourceCtx, BackupOptions{
		IncludeProjects: true,
		IncludeChannels: true,
		IncludeAPIKeys:  true,
	})
	require.NoError(t, err)

	var backupData BackupData
	require.NoError(t, json.Unmarshal(data, &backupData))
	require.Equal(t, BackupVersion, backupData.Version)
	require.Len(t, backupData.ChannelCallerACL, 1)
	require.Len(t, backupData.Channels, 1)
	require.Equal(t, sourceChannel.ID, backupData.ChannelCallerACL[0].SourceChannelID)
	require.Equal(t, sourceAPIKey.ID, backupData.ChannelCallerACL[0].SourceAPIKeyID)

	targetClient, targetService, targetCtx := setupBackupTest(t)
	defer targetClient.Close()

	targetUser, err := targetClient.User.Query().Only(targetCtx)
	require.NoError(t, err)
	seedProject := createBackupTestProject(t, targetClient, targetCtx, "Seed Project", "target")
	_ = createBackupTestChannel(t, targetClient, targetCtx, "Seed Channel", channel.TypeOpenai)
	_ = createBackupTestAPIKey(t, targetClient, targetCtx, targetUser, seedProject, "Seed Key", "sk-seed")

	err = targetService.Restore(targetCtx, data, RestoreOptions{
		IncludeProjects:         true,
		IncludeChannels:         true,
		IncludeAPIKeys:          true,
		ProjectConflictStrategy: ConflictStrategyOverwrite,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
		APIKeyConflictStrategy:  ConflictStrategyOverwrite,
	})
	require.NoError(t, err)

	restoredChannel, err := targetClient.Channel.Query().
		Where(channel.Name(sourceChannel.Name)).
		Only(targetCtx)
	require.NoError(t, err)
	require.NotEqual(t, sourceChannel.ID, restoredChannel.ID)
	require.Equal(t, channel.CallerAccessModeAllowlist, restoredChannel.CallerAccessMode)

	restoredAPIKey, err := targetClient.APIKey.Query().
		Where(apikey.Key("sk-acl-source")).
		Only(targetCtx)
	require.NoError(t, err)
	require.NotEqual(t, sourceAPIKey.ID, restoredAPIKey.ID)

	members, err := targetClient.ChannelCallerACLMember.Query().All(targetCtx)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, restoredChannel.ID, members[0].ChannelID)
	require.Equal(t, restoredAPIKey.ID, members[0].APIKeyID)
}

func TestBackupService_RestoreChannelsOnlyNonPublicNewChannelFailsClosed(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	data, err := json.Marshal(BackupData{
		Version: BackupVersion,
		Channels: []*BackupChannel{{
			Channel: ent.Channel{
				ID:               100,
				Name:             "Private Partial Channel",
				Type:             channel.TypeOpenai,
				Status:           channel.StatusEnabled,
				CallerAccessMode: channel.CallerAccessModeDenylist,
			},
			Credentials: objects.ChannelCredentials{APIKey: "provider-key"},
		}},
	})
	require.NoError(t, err)

	err = service.Restore(ctx, data, RestoreOptions{
		IncludeChannels:         true,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
	})
	require.ErrorContains(t, err, "cannot restore non-public Channel")
	require.Zero(t, client.Channel.Query().Where(channel.Name("Private Partial Channel")).CountX(ctx))
}

func TestBackupService_ChannelCallerACLChannelsOnlyPreservesModeAndOmitsMembers(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	user, err := client.User.Query().Only(ctx)
	require.NoError(t, err)
	project := createBackupTestProject(t, client, ctx, "Channels Only", "test")
	ch := createBackupTestChannel(t, client, ctx, "Private Channel", channel.TypeOpenai)
	ch, err = client.Channel.UpdateOne(ch).
		SetCallerAccessMode(channel.CallerAccessModeDenylist).
		Save(ctx)
	require.NoError(t, err)
	apiKey := createBackupTestAPIKey(t, client, ctx, user, project, "Denied Key", "sk-denied")
	_, err = client.ChannelCallerACLMember.Create().
		SetChannelID(ch.ID).
		SetAPIKeyID(apiKey.ID).
		Save(ctx)
	require.NoError(t, err)

	data, err := service.Backup(ctx, BackupOptions{IncludeChannels: true})
	require.NoError(t, err)

	var backupData BackupData
	require.NoError(t, json.Unmarshal(data, &backupData))
	require.Empty(t, backupData.ChannelCallerACL)
	require.Len(t, backupData.Channels, 1)
	require.Equal(t, channel.CallerAccessModeDenylist, backupData.Channels[0].CallerAccessMode)
}

func TestBackupService_RestorePartialChannelCallerACLPreservesTargetPolicy(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	user, err := client.User.Query().Only(ctx)
	require.NoError(t, err)
	project := createBackupTestProject(t, client, ctx, "Legacy Target", "test")
	ch := createBackupTestChannel(t, client, ctx, "Legacy ACL Channel", channel.TypeOpenai)
	ch, err = client.Channel.UpdateOne(ch).
		SetCallerAccessMode(channel.CallerAccessModeAllowlist).
		Save(ctx)
	require.NoError(t, err)
	apiKey := createBackupTestAPIKey(t, client, ctx, user, project, "Legacy Key", "sk-legacy")
	_, err = client.ChannelCallerACLMember.Create().
		SetChannelID(ch.ID).
		SetAPIKeyID(apiKey.ID).
		Save(ctx)
	require.NoError(t, err)

	legacyData, err := json.Marshal(BackupData{
		Version: BackupVersionV6,
		Channels: []*BackupChannel{{
			Channel: ent.Channel{
				ID:               101,
				Name:             ch.Name,
				Type:             channel.TypeOpenai,
				Status:           channel.StatusEnabled,
				SupportedModels:  []string{"gpt-4"},
				DefaultTestModel: "gpt-4",
				Settings:         &objects.ChannelSettings{},
				CallerAccessMode: channel.CallerAccessModeDenylist,
			},
			Credentials: objects.ChannelCredentials{APIKey: "legacy-provider-key"},
		}},
	})
	require.NoError(t, err)

	err = service.Restore(ctx, legacyData, RestoreOptions{
		IncludeChannels:         true,
		ChannelConflictStrategy: ConflictStrategySkip,
	})
	require.NoError(t, err)

	skipped, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.CallerAccessModeAllowlist, skipped.CallerAccessMode)
	memberCount, err := client.ChannelCallerACLMember.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, memberCount)

	err = service.Restore(ctx, legacyData, RestoreOptions{
		IncludeChannels:         true,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
	})
	require.NoError(t, err)

	overwritten, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.CallerAccessModeAllowlist, overwritten.CallerAccessMode)
	memberCount, err = client.ChannelCallerACLMember.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, memberCount)
}
