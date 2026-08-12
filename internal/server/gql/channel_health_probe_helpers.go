package gql

import "github.com/looplj/axonhub/internal/server/biz"

func activeChannelHealthProbeRun(record *biz.ChannelHealthProbeRunRecord) *ActiveChannelHealthProbeRun {
	if record == nil {
		return nil
	}
	return &ActiveChannelHealthProbeRun{
		ID:           record.ID,
		ChannelID:    record.ChannelID,
		ModelID:      record.ModelID,
		Source:       record.Source,
		Status:       record.Status,
		Stream:       record.Stream,
		TtfbMs:       record.TTFBMs,
		TtftMs:       record.TTFTMs,
		TotalMs:      record.TotalMs,
		ErrorMessage: record.ErrorMessage,
		StartedAt:    record.StartedAt,
		CompletedAt:  record.CompletedAt,
		CreatedAt:    record.CreatedAt,
	}
}

func activeChannelHealthProbeChannel(item *biz.ChannelHealthProbeChannelOverview) *ChannelHealthProbeChannel {
	if item == nil {
		return nil
	}
	return &ChannelHealthProbeChannel{
		ChannelID:       item.ChannelID,
		ChannelName:     item.ChannelName,
		ChannelStatus:   item.ChannelStatus,
		Priority:        item.Priority,
		Enabled:         item.Enabled,
		IntervalMinutes: item.IntervalMinutes,
		Models:          item.Models,
	}
}
