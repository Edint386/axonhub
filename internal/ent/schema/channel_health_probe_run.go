package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/scopes"
)

// ChannelHealthProbeRun is a history row for a synthetic channel generation
// probe. A run starts as pending and is completed after the upstream request;
// identity and scheduling fields remain immutable while result fields do not.
type ChannelHealthProbeRun struct {
	ent.Schema
}

func (ChannelHealthProbeRun) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (ChannelHealthProbeRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("channel_id", "created_at").
			StorageKey("channel_health_probe_runs_by_channel_created_at"),
		index.Fields("channel_id", "model_id", "created_at").
			StorageKey("channel_health_probe_runs_by_channel_model_created_at"),
		index.Fields("status", "created_at").
			StorageKey("channel_health_probe_runs_by_status_created_at"),
		index.Fields("schedule_key").
			Unique().
			StorageKey("channel_health_probe_runs_by_schedule_key"),
	}
}

func (ChannelHealthProbeRun) Fields() []ent.Field {
	return []ent.Field{
		field.Int("channel_id").Immutable(),
		field.String("model_id").Immutable(),
		field.Enum("source").Values("manual", "scheduled").Immutable(),
		field.Enum("status").Values("pending", "healthy", "unhealthy", "skipped"),
		field.Bool("stream").Immutable(),
		field.Float("ttfb_ms").Optional().Nillable(),
		field.Float("ttft_ms").Optional().Nillable(),
		field.Float("total_ms").Default(0),
		field.String("error_message").Optional().Nillable(),
		field.String("schedule_key").Optional().Nillable().Immutable(),
		field.Time("started_at").Immutable(),
		field.Time("completed_at").Optional().Nillable(),
	}
}

func (ChannelHealthProbeRun) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("channel", Channel.Type).
			Ref("health_probe_runs").
			Field("channel_id").
			Required().
			Immutable().
			Unique(),
	}
}

func (ChannelHealthProbeRun) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.RelayConnection()}
}

func (ChannelHealthProbeRun) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.APIKeyScopeQueryRule(scopes.ScopeReadChannels),
			scopes.OwnerRule(),
			scopes.UserReadScopeRule(scopes.ScopeReadChannels),
		},
		Mutation: scopes.MutationPolicy{
			scopes.OwnerRule(),
			scopes.UserWriteScopeRule(scopes.ScopeWriteChannels),
		},
	}
}
