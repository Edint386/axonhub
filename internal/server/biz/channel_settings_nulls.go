package biz

import "context"

type channelSettingsExplicitNullsKey struct{}

// WithChannelSettingsExplicitNulls records the top-level channel settings
// fields that the client sent as an explicit null. GraphQL decodes an omitted
// field and an explicit null to the same nil pointer, so the update merge needs
// this to tell "keep the stored value" from "clear it".
func WithChannelSettingsExplicitNulls(ctx context.Context, fields []string) context.Context {
	if len(fields) == 0 {
		return ctx
	}

	nulls := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		nulls[field] = struct{}{}
	}

	return context.WithValue(ctx, channelSettingsExplicitNullsKey{}, nulls)
}

func channelSettingsFieldExplicitlyNull(ctx context.Context, field string) bool {
	nulls, _ := ctx.Value(channelSettingsExplicitNullsKey{}).(map[string]struct{})
	_, ok := nulls[field]

	return ok
}
