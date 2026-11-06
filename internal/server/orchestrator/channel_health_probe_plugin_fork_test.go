package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelhealthproberun"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xerrors"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

// Upstream rejects plugin channel tests before any persistence work (tester.go). The custom manual
// health probe stores its run before it reaches the tester, so it rejects plugins itself;
// otherwise the refusal would be saved as an unhealthy run. See CUSTOM_BRANCH_RULES.md section 3
// (semantic review after a merge).
func TestChannelHealthProbeRunManual_RejectsPluginsBeforePersisting(t *testing.T) {
	ctx, client := setupTest(t)
	ch, err := client.Channel.Create().SetType(channel.TypeOpenai).SetName("probe").
		SetBaseURL("https://example.invalid/v1").SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"test-model"}).SetDefaultTestModel("test-model").Save(ctx)
	require.NoError(t, err)

	channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
	protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{Ent: client})
	t.Cleanup(protection.Stop)
	svc := biz.NewChannelHealthProbeService(biz.ChannelHealthProbeServiceParams{
		Ent:            client,
		ChannelService: channelService,
		SystemService:  systemService,
	})
	runner := NewChannelHealthProbeRunner(ChannelHealthProbeRunnerParams{
		Service: svc,
		Tester:  NewTestChannelOrchestrator(channelService, requestService, systemService, usageLogService, protection, httpclient.NewHttpClient()),
	})

	owner := &ent.User{ID: 1, IsOwner: true}
	parent := contexts.WithUser(authz.NewUserContext(ent.NewContext(context.Background(), client), owner.ID), owner)
	plugin, err := authz.DelegatePlugin(parent, 7, []string{"read_channels", "write_channels"})
	require.NoError(t, err)

	record, err := runner.RunManual(plugin, biz.RunChannelHealthProbeInput{
		ChannelID: objects.GUID{Type: ent.TypeChannel, ID: ch.ID},
		ModelID:   "test-model",
	})
	require.Nil(t, record)
	coded, ok := xerrors.IsCodedError(err)
	require.True(t, ok, "got %v", err)
	require.Equal(t, xerrors.ErrCodeForbidden, coded.Code)

	runs, err := client.ChannelHealthProbeRun.Query().Where(channelhealthproberun.ChannelIDEQ(ch.ID)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, runs, "a rejected plugin probe must not leave a run behind")
}
