package gql

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestAnalyticsMetadataAllowsDashboardScope(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:analytics-metadata?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	resolver := &queryResolver{&Resolver{
		client:        client,
		systemService: biz.NewSystemService(biz.SystemServiceParams{Ent: client}),
	}}

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.NewUserContext(ctx, 1)
	ctx = contexts.WithUser(ctx, &ent.User{
		ID:     1,
		Scopes: []string{string(scopes.ScopeReadDashboard)},
	})

	metadata, err := resolver.AnalyticsMetadata(ctx)
	require.NoError(t, err)
	require.NotNil(t, metadata)
}

func TestAnalyticsDailyStatsAddsDefaultDateBounds(t *testing.T) {
	startDay := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	endDay := startDay.AddDate(0, 0, 29)
	selector := sql.Select().From(sql.Table(usagelog.Table))

	applyAnalyticsDailyStatsDateBounds(selector, nil, startDay, endDay)

	query, args := selector.Query()
	require.Contains(t, query, usagelog.FieldCreatedAt)
	require.Len(t, args, 2)
	require.Equal(t, startDay.UTC(), args[0])
	require.Equal(t, endDay.AddDate(0, 0, 1).UTC(), args[1])
}
