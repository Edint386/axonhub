package gql

import (
	"context"
	"fmt"
	"testing"

	"github.com/99designs/gqlgen/client"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

// TestUpdateChannelQuotaTimezone covers the custom per-channel quota timezone
// over GraphQL: it is stored and read back, kept when another setting is saved,
// rejected when unknown, and cleared by an explicit null.
func TestUpdateChannelQuotaTimezone(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer db.Close()

	ctx := authz.WithTestBypass(context.Background())
	ch, err := db.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Channel quota timezone").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		Save(ctx)
	require.NoError(t, err)

	channelService := biz.NewChannelServiceForTest(db)
	defer channelService.Stop()
	handler := NewGraphqlHandlers(Dependencies{
		Ent:            db,
		ChannelService: channelService,
	})
	graphqlClient := client.New(handler.Graphql, func(request *client.Request) {
		request.HTTP = request.HTTP.WithContext(authz.WithTestBypass(request.HTTP.Context()))
	})
	channelID := fmt.Sprintf("gid://axonhub/%s/%d", ent.TypeChannel, ch.ID)

	storedTimezone := func() *string {
		t.Helper()

		got, err := db.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		if got.Settings == nil {
			return nil
		}

		return got.Settings.QuotaTimezone
	}
	update := func(settings string) error {
		var response map[string]any

		return graphqlClient.Post(fmt.Sprintf(`mutation { updateChannel(id: %q, input: {settings: %s}) { id settings { quotaTimezone } } }`, channelID, settings), &response)
	}

	require.NoError(t, update(`{quotaTimezone: "Asia/Shanghai"}`))
	require.Equal(t, "Asia/Shanghai", lo.FromPtr(storedTimezone()))

	var readBack struct {
		Node struct {
			Settings struct {
				QuotaTimezone *string `json:"quotaTimezone"`
			} `json:"settings"`
		} `json:"node"`
	}
	require.NoError(t, graphqlClient.Post(fmt.Sprintf(`query { node(id: %q) { ... on Channel { settings { quotaTimezone } } } }`, channelID), &readBack))
	require.Equal(t, "Asia/Shanghai", lo.FromPtr(readBack.Node.Settings.QuotaTimezone))

	require.NoError(t, update(`{extraModelPrefix: "kept-"}`))
	require.Equal(t, "Asia/Shanghai", lo.FromPtr(storedTimezone()), "saving another setting must keep the timezone")

	require.ErrorContains(t, update(`{quotaTimezone: "Mars/Olympus"}`), "invalid quota timezone")
	require.Equal(t, "Asia/Shanghai", lo.FromPtr(storedTimezone()))

	require.NoError(t, update(`{quotaTimezone: null}`))
	require.Nil(t, storedTimezone(), "an explicit null switches back to the system timezone")
}
