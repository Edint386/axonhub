package biz

import (
	"context"
	"fmt"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelmodelprice"
	"github.com/looplj/axonhub/internal/pkg/xerrors"
)

// DuplicateChannel creates a new channel from input and copies current model prices
// from the source channel in the same transaction.
func (svc *ChannelService) DuplicateChannel(ctx context.Context, sourceID int, input ent.CreateChannelInput) (*ent.Channel, error) {
	var duplicated *ent.Channel

	err := svc.RunInTransaction(ctx, func(ctx context.Context) error {
		db := svc.entFromContext(ctx)

		source, err := db.Channel.Query().
			Where(channel.IDEQ(sourceID)).
			WithCallerACLMembers().
			Only(ctx)
		if err != nil {
			return fmt.Errorf("failed to get source channel: %w", err)
		}

		existing, err := db.Channel.Query().
			Where(channel.Name(input.Name)).
			First(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return fmt.Errorf("failed to check channel name: %w", err)
		}

		if existing != nil {
			return xerrors.DuplicateNameError("channel", input.Name)
		}

		ch, err := svc.createChannel(ctx, input)
		if err != nil {
			return err
		}
		ch, err = db.Channel.UpdateOne(ch).
			SetModelPriceMultiplier(source.ModelPriceMultiplier).
			SetCallerAccessMode(source.CallerAccessMode).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to copy channel model price multiplier: %w", err)
		}

		if source.CallerAccessMode != channel.CallerAccessModePublic && len(source.Edges.CallerACLMembers) > 0 {
			builders := make([]*ent.ChannelCallerACLMemberCreate, 0, len(source.Edges.CallerACLMembers))
			for _, member := range source.Edges.CallerACLMembers {
				builders = append(builders, db.ChannelCallerACLMember.Create().
					SetChannelID(ch.ID).
					SetAPIKeyID(member.APIKeyID))
			}
			if _, err := db.ChannelCallerACLMember.CreateBulk(builders...).Save(ctx); err != nil {
				return fmt.Errorf("failed to copy Channel caller ACL members: %w", err)
			}
		}

		prices, err := db.ChannelModelPrice.Query().
			Where(
				channelmodelprice.ChannelID(sourceID),
			).
			All(ctx)
		if err != nil {
			return fmt.Errorf("failed to query source channel model prices: %w", err)
		}

		now := time.Now()
		for _, price := range prices {
			if _, err := svc.createChannelModelPrice(ctx, ch.ID, price.ModelID, price.Price, now); err != nil {
				return fmt.Errorf("failed to copy channel model price: %w", err)
			}
		}

		if _, err := svc.ensureChannelModelPrices(ctx, ch.ID, ch.SupportedModels); err != nil {
			return err
		}

		duplicated = ch

		return nil
	})
	if err != nil {
		return nil, err
	}
	if ent.TxFromContext(ctx) == nil {
		duplicated.Unwrap()
	}

	svc.reloadChannelsAfterCommit(ctx)

	return duplicated, nil
}
