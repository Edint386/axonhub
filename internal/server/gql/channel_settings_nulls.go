package gql

import (
	"context"

	"github.com/99designs/gqlgen/graphql"

	"github.com/looplj/axonhub/internal/server/biz"
)

// withChannelSettingsExplicitNulls tells the biz layer which settings fields of
// the current updateChannel call were sent as an explicit null, so an omitted
// quota keeps the stored value while `quota: null` still clears it.
func withChannelSettingsExplicitNulls(ctx context.Context) context.Context {
	fieldCtx := graphql.GetFieldContext(ctx)
	if fieldCtx == nil || fieldCtx.Field.Field == nil || !graphql.HasOperationContext(ctx) {
		return ctx
	}

	args := fieldCtx.Field.ArgumentMap(graphql.GetOperationContext(ctx).Variables)
	input, _ := args["input"].(map[string]any)
	settings, _ := input["settings"].(map[string]any)

	nulls := make([]string, 0, len(settings))
	for field, value := range settings {
		if value == nil {
			nulls = append(nulls, field)
		}
	}

	return biz.WithChannelSettingsExplicitNulls(ctx, nulls)
}
