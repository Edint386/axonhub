package biz

import (
	"context"
	"fmt"
	"slices"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelcalleraclmember"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

// ChannelCallerAccessPolicy is the complete caller API key ACL for one channel.
type ChannelCallerAccessPolicy struct {
	Channel *ent.Channel
	Mode    channel.CallerAccessMode
	Members []*ChannelCallerAPIKeySummary
}

// ChannelCallerAPIKeySummary is the only API key shape exposed by Channel ACL
// queries. In particular it intentionally excludes the secret key value,
// allowed IPs, scopes, and profile contents.
type ChannelCallerAPIKeySummary struct {
	ID          objects.GUID
	Name        string
	Type        apikey.Type
	Status      apikey.Status
	ProjectID   objects.GUID
	ProjectName string
}

// APIKeyChannelCallerAccess is one channel ACL projected from an API key.
type APIKeyChannelCallerAccess struct {
	Channel  *ent.Channel
	Mode     channel.CallerAccessMode
	IsMember bool
	Allowed  bool
}

// SetChannelCallerAccessPolicyInput atomically replaces a channel's caller ACL.
type SetChannelCallerAccessPolicyInput struct {
	ChannelID       int
	Mode            channel.CallerAccessMode
	MemberAPIKeyIDs []int
}

func summarizeCallerAPIKeys(apiKeys []*ent.APIKey) []*ChannelCallerAPIKeySummary {
	return lo.Map(apiKeys, func(apiKey *ent.APIKey, _ int) *ChannelCallerAPIKeySummary {
		project := apiKey.Edges.Project
		projectName := ""
		if project != nil {
			projectName = project.Name
		}
		return &ChannelCallerAPIKeySummary{
			ID:          objects.GUID{Type: ent.TypeAPIKey, ID: apiKey.ID},
			Name:        apiKey.Name,
			Type:        apiKey.Type,
			Status:      apiKey.Status,
			ProjectID:   objects.GUID{Type: ent.TypeProject, ID: apiKey.ProjectID},
			ProjectName: projectName,
		}
	})
}

func requireChannelCallerAccessSystemScopes(ctx context.Context, required ...scopes.ScopeSlug) error {
	for _, requiredScope := range required {
		if err := authz.RequireScope(ctx, requiredScope); err != nil {
			return err
		}
	}

	principal, ok := authz.GetPrincipal(ctx)
	if !ok {
		return fmt.Errorf("channel caller access requires an authenticated principal")
	}
	if principal.IsSystem() || principal.IsTest() {
		return nil
	}
	if !principal.IsUser() {
		return fmt.Errorf("channel caller access requires a user principal")
	}

	user, ok := contexts.GetUser(ctx)
	if !ok || user == nil {
		return fmt.Errorf("user not found in context")
	}
	for _, requiredScope := range required {
		if !scopes.HasSystemScope(user, requiredScope) {
			return fmt.Errorf("channel caller access requires system scope %s", requiredScope)
		}
	}

	return nil
}

func requireChannelCallerAccessRead(ctx context.Context) error {
	return requireChannelCallerAccessSystemScopes(
		ctx,
		scopes.ScopeReadChannels,
		scopes.ScopeReadAPIKeys,
	)
}

func requireChannelCallerAccessWrite(ctx context.Context) error {
	return requireChannelCallerAccessSystemScopes(
		ctx,
		scopes.ScopeReadChannels,
		scopes.ScopeReadAPIKeys,
		scopes.ScopeWriteChannels,
		scopes.ScopeWriteAPIKeys,
	)
}

// ChannelCallerAccessPolicy returns one channel's policy and its API key members.
func (svc *ChannelService) ChannelCallerAccessPolicy(
	ctx context.Context,
	channelID int,
) (*ChannelCallerAccessPolicy, error) {
	if err := requireChannelCallerAccessRead(ctx); err != nil {
		return nil, err
	}

	return svc.loadChannelCallerAccessPolicy(ctx, channelID)
}

func (svc *ChannelService) loadChannelCallerAccessPolicy(
	ctx context.Context,
	channelID int,
) (*ChannelCallerAccessPolicy, error) {
	return authz.RunWithSystemBypass(ctx, "query-channel-caller-access-policy", func(bypassCtx context.Context) (*ChannelCallerAccessPolicy, error) {
		db := svc.entFromContext(bypassCtx)
		entity, err := db.Channel.Query().
			Where(channel.IDEQ(channelID), channel.DeletedAtEQ(0)).
			Only(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to query channel caller access policy: %w", err)
		}

		memberIDs, err := db.ChannelCallerACLMember.Query().
			Where(channelcalleraclmember.ChannelIDEQ(channelID)).
			Select(channelcalleraclmember.FieldAPIKeyID).
			Ints(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to query channel caller access members: %w", err)
		}

		members := []*ChannelCallerAPIKeySummary{}
		if len(memberIDs) > 0 {
			memberEntities, err := db.APIKey.Query().
				Where(apikey.IDIn(memberIDs...), apikey.DeletedAtEQ(0)).
				Order(ent.Asc(apikey.FieldName), ent.Asc(apikey.FieldID)).
				WithProject().
				All(bypassCtx)
			if err != nil {
				return nil, fmt.Errorf("failed to load channel caller access API keys: %w", err)
			}
			members = summarizeCallerAPIKeys(memberEntities)
		}

		return &ChannelCallerAccessPolicy{
			Channel: entity,
			Mode:    entity.CallerAccessMode,
			Members: members,
		}, nil
	})
}

// ChannelCallerAccessCandidates returns the safe, system-scoped caller identity
// list used by the ACL editor. It does not expose API key secrets or profile
// contents and intentionally does not inherit a selected project context.
func (svc *ChannelService) ChannelCallerAccessCandidates(ctx context.Context) ([]*ChannelCallerAPIKeySummary, error) {
	if err := requireChannelCallerAccessRead(ctx); err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "query-channel-caller-access-candidates", func(bypassCtx context.Context) ([]*ChannelCallerAPIKeySummary, error) {
		apiKeys, err := svc.entFromContext(bypassCtx).APIKey.Query().
			Where(apikey.DeletedAtEQ(0)).
			Order(ent.Asc(apikey.FieldName), ent.Asc(apikey.FieldID)).
			WithProject().
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to query Channel caller access candidates: %w", err)
		}

		return summarizeCallerAPIKeys(apiKeys), nil
	})
}

// APIKeyChannelCallerAccess returns all channel ACLs from one API key's point of view.
func (svc *ChannelService) APIKeyChannelCallerAccess(
	ctx context.Context,
	apiKeyID int,
) ([]*APIKeyChannelCallerAccess, error) {
	if err := requireChannelCallerAccessRead(ctx); err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "query-api-key-channel-caller-access", func(bypassCtx context.Context) ([]*APIKeyChannelCallerAccess, error) {
		db := svc.entFromContext(bypassCtx)
		if _, err := db.APIKey.Query().
			Where(apikey.IDEQ(apiKeyID), apikey.DeletedAtEQ(0)).
			Only(bypassCtx); err != nil {
			return nil, fmt.Errorf("failed to query API key channel caller access: %w", err)
		}

		channels, err := db.Channel.Query().
			Where(channel.DeletedAtEQ(0)).
			Order(ent.Asc(channel.FieldName), ent.Asc(channel.FieldID)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to query channels for API key caller access: %w", err)
		}

		memberChannelIDs, err := db.ChannelCallerACLMember.Query().
			Where(channelcalleraclmember.APIKeyIDEQ(apiKeyID)).
			Select(channelcalleraclmember.FieldChannelID).
			Ints(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to query API key caller access memberships: %w", err)
		}

		memberChannelSet := lo.SliceToMap(memberChannelIDs, func(id int) (int, struct{}) {
			return id, struct{}{}
		})
		result := make([]*APIKeyChannelCallerAccess, 0, len(channels))
		for _, entity := range channels {
			_, isMember := memberChannelSet[entity.ID]
			allowed := entity.CallerAccessMode == channel.CallerAccessModePublic ||
				(entity.CallerAccessMode == channel.CallerAccessModeAllowlist && isMember) ||
				(entity.CallerAccessMode == channel.CallerAccessModeDenylist && !isMember)

			result = append(result, &APIKeyChannelCallerAccess{
				Channel:  entity,
				Mode:     entity.CallerAccessMode,
				IsMember: isMember,
				Allowed:  allowed,
			})
		}

		return result, nil
	})
}

// SetChannelCallerAccessPolicy atomically replaces one channel's mode and member set.
// PUBLIC policies are canonicalized to an empty member set because membership has
// no meaning while every API key is allowed.
func (svc *ChannelService) SetChannelCallerAccessPolicy(
	ctx context.Context,
	input SetChannelCallerAccessPolicyInput,
) (*ChannelCallerAccessPolicy, error) {
	if err := requireChannelCallerAccessWrite(ctx); err != nil {
		return nil, err
	}
	if input.ChannelID <= 0 {
		return nil, fmt.Errorf("channel ID must be positive")
	}
	if err := channel.CallerAccessModeValidator(input.Mode); err != nil {
		return nil, fmt.Errorf("invalid channel caller access mode: %w", err)
	}

	memberAPIKeyIDs := lo.Uniq(input.MemberAPIKeyIDs)
	for _, id := range memberAPIKeyIDs {
		if id <= 0 {
			return nil, fmt.Errorf("API key IDs must be positive")
		}
	}
	if input.Mode == channel.CallerAccessModePublic {
		memberAPIKeyIDs = nil
	}
	slices.Sort(memberAPIKeyIDs)

	err := svc.RunInTransaction(ctx, func(txCtx context.Context) error {
		return authz.RunWithSystemBypassVoid(txCtx, "set-channel-caller-access-policy", func(bypassCtx context.Context) error {
			db := svc.entFromContext(bypassCtx)
			if len(memberAPIKeyIDs) > 0 {
				count, err := db.APIKey.Query().
					Where(apikey.IDIn(memberAPIKeyIDs...), apikey.DeletedAtEQ(0)).
					Count(bypassCtx)
				if err != nil {
					return fmt.Errorf("failed to validate channel caller access API keys: %w", err)
				}
				if count != len(memberAPIKeyIDs) {
					return fmt.Errorf("one or more channel caller access API keys were not found")
				}
			}

			_, err := db.Channel.UpdateOneID(input.ChannelID).
				Where(channel.DeletedAtEQ(0)).
				SetCallerAccessMode(input.Mode).
				Save(bypassCtx)
			if err != nil {
				return fmt.Errorf("failed to update channel caller access policy: %w", err)
			}

			if _, err := db.ChannelCallerACLMember.Delete().
				Where(channelcalleraclmember.ChannelIDEQ(input.ChannelID)).
				Exec(bypassCtx); err != nil {
				return fmt.Errorf("failed to replace channel caller access members: %w", err)
			}
			if len(memberAPIKeyIDs) > 0 {
				builders := lo.Map(memberAPIKeyIDs, func(apiKeyID int, _ int) *ent.ChannelCallerACLMemberCreate {
					return db.ChannelCallerACLMember.Create().
						SetChannelID(input.ChannelID).
						SetAPIKeyID(apiKeyID)
				})
				if _, err := db.ChannelCallerACLMember.CreateBulk(builders...).Save(bypassCtx); err != nil {
					return fmt.Errorf("failed to save channel caller access members: %w", err)
				}
			}

			svc.reloadChannelsAfterCommit(bypassCtx)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return svc.loadChannelCallerAccessPolicy(ctx, input.ChannelID)
}
