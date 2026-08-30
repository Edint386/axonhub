package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ChannelCallerACLMember stores one API key membership in a channel caller
// access allowlist or denylist. The channel's caller_access_mode determines how
// membership is interpreted.
type ChannelCallerACLMember struct {
	ent.Schema
}

func (ChannelCallerACLMember) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (ChannelCallerACLMember) Fields() []ent.Field {
	return []ent.Field{
		field.Int("channel_id").Immutable(),
		field.Int("api_key_id").Immutable(),
	}
}

func (ChannelCallerACLMember) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("channel", Channel.Type).
			Ref("caller_acl_members").
			Field("channel_id").
			Required().
			Immutable().
			Unique(),
		edge.From("api_key", APIKey.Type).
			Ref("channel_caller_acl_members").
			Field("api_key_id").
			Required().
			Immutable().
			Unique(),
	}
}

func (ChannelCallerACLMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("channel_id", "api_key_id").
			StorageKey("channel_caller_acl_members_by_channel_id_api_key_id").
			Unique(),
		index.Fields("api_key_id").
			StorageKey("channel_caller_acl_members_by_api_key_id"),
	}
}

func (ChannelCallerACLMember) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
	}
}
