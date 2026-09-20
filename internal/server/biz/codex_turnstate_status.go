package biz

import (
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/transformer/openai/codex/turnstate"
)

const (
	turnStatePhaseDisabled   = "disabled"
	turnStatePhasePending    = "pending"
	turnStatePhaseHarvesting = "harvesting"
	turnStatePhaseReady      = "ready"
	turnStatePhaseCooldown   = "cooldown"
	turnStatePhaseFailed     = "failed"

	maxTurnStateEvents = 8
)

// CodexTurnStateRuntime is the in-memory harvest snapshot exposed over GraphQL.
type CodexTurnStateRuntime struct {
	Enabled      bool
	Phase        string
	Models       []*CodexTurnStateModelStatus
	RecentEvents []*CodexTurnStateEvent
}

// CodexTurnStateModelStatus is per-model harvest state for a channel.
type CodexTurnStateModelStatus struct {
	Model           string
	Phase           string
	LastError       string
	Attempts        int
	CooldownUntil   *time.Time
	TicketExpiresAt *time.Time
	StateBytes      int
}

// CodexTurnStateEvent is one harvest/validate/invalidate probe record.
type CodexTurnStateEvent struct {
	At         time.Time
	Model      string
	Kind       string
	Reason     string
	StatusCode int
	DurationMs int
	StateBytes int
}

// CodexTurnStateRuntime returns the live harvest snapshot for a channel.
func (svc *ChannelService) CodexTurnStateRuntime(ch *ent.Channel) *CodexTurnStateRuntime {
	if svc == nil || svc.turnState == nil {
		return nil
	}
	return svc.turnState.Runtime(ch)
}

func (m *CodexTurnStateManager) Runtime(entCh *ent.Channel) *CodexTurnStateRuntime {
	if m == nil || entCh == nil {
		return nil
	}
	ch := &Channel{Channel: entCh}
	now := time.Now()
	settingsOn := ch.Settings != nil && ch.Settings.CodexTurnState.IsEnabled()
	cfg, harvestable := turnStateConfigOf(ch)

	out := &CodexTurnStateRuntime{
		Enabled:      settingsOn,
		Phase:        turnStatePhaseDisabled,
		Models:       []*CodexTurnStateModelStatus{},
		RecentEvents: []*CodexTurnStateEvent{},
	}

	m.mu.Lock()
	events := append([]CodexTurnStateEvent(nil), m.events[ch.ID]...)
	if settingsOn && harvestable {
		for _, model := range cfg.Models {
			out.Models = append(out.Models, m.modelStatusLocked(ch, cfg, model, now))
		}
	}
	m.mu.Unlock()

	for i := range events {
		ev := events[i]
		out.RecentEvents = append(out.RecentEvents, &ev)
	}

	if !settingsOn {
		return out
	}
	if !harvestable {
		out.Phase = turnStatePhasePending
		return out
	}
	out.Phase = aggregateTurnStatePhase(out.Models)
	return out
}

func (m *CodexTurnStateManager) modelStatusLocked(ch *Channel, cfg objects.CodexTurnStateSettings, model string, now time.Time) *CodexTurnStateModelStatus {
	key := turnStateKey(ch.ID, model)
	item := &CodexTurnStateModelStatus{Model: model, Phase: turnStatePhasePending}
	if _, running := m.jobs[key]; running {
		item.Phase = turnStatePhaseHarvesting
	}
	if rec := m.records[key]; rec != nil {
		item.LastError = rec.LastError
		item.Attempts = rec.Attempts
		if rec.CooldownUntil.After(now) {
			until := rec.CooldownUntil
			item.CooldownUntil = &until
			if item.Phase != turnStatePhaseHarvesting {
				item.Phase = turnStatePhaseCooldown
			}
		} else if rec.LastError != "" && item.Phase == turnStatePhasePending {
			item.Phase = turnStatePhaseFailed
		}
	}
	stored := m.tickets[key]
	revoked := m.revoked[key]
	fp := turnStateFingerprint(ch, cfg)
	identity := turnStateIdentity(ch)
	if stored != nil && stored.Fingerprint == fp && stored.Identity == identity &&
		stored.Ticket.Version != revoked && stored.Ticket.ExpiresAt.After(now) &&
		turnstate.Valid(stored.Ticket.State, turnstate.TargetLength(cfg.Plan)) {
		expires := stored.Ticket.ExpiresAt
		item.TicketExpiresAt = &expires
		item.StateBytes = len(stored.Ticket.State)
		if item.Phase != turnStatePhaseHarvesting {
			item.Phase = turnStatePhaseReady
		}
	}
	return item
}

func aggregateTurnStatePhase(models []*CodexTurnStateModelStatus) string {
	if len(models) == 0 {
		return turnStatePhasePending
	}
	hasReady, hasHarvesting, hasCooldown, hasFailed := false, false, false, false
	for _, item := range models {
		switch item.Phase {
		case turnStatePhaseHarvesting:
			hasHarvesting = true
		case turnStatePhaseReady:
			hasReady = true
		case turnStatePhaseCooldown:
			hasCooldown = true
		case turnStatePhaseFailed:
			hasFailed = true
		}
	}
	switch {
	case hasHarvesting:
		return turnStatePhaseHarvesting
	case hasReady:
		return turnStatePhaseReady
	case hasCooldown:
		return turnStatePhaseCooldown
	case hasFailed:
		return turnStatePhaseFailed
	default:
		return turnStatePhasePending
	}
}

func (m *CodexTurnStateManager) recordEvent(channelID int, ev CodexTurnStateEvent) {
	if m == nil || channelID <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendEventLocked(channelID, ev)
}

func (m *CodexTurnStateManager) appendEventLocked(channelID int, ev CodexTurnStateEvent) {
	list := m.events[channelID]
	next := make([]CodexTurnStateEvent, 0, len(list)+1)
	next = append(next, ev)
	next = append(next, list...)
	if len(next) > maxTurnStateEvents {
		next = next[:maxTurnStateEvents]
	}
	m.events[channelID] = next
}
