package realtime

import (
	"errors"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/internal/server/realtime/dialect/bailian"
)

// OpenAI realtime translation sessions (GET /v1/realtime/translations) are relayed to Bailian
// Qwen-LiveTranslate realtime models only, and those models serve nothing else.
var (
	// ErrNotTranslationModel answers a translation session whose model is not served by a
	// Bailian Qwen-LiveTranslate channel.
	ErrNotTranslationModel = errors.New("realtime translation sessions are only served by Bailian Qwen-LiveTranslate realtime models " +
		"(e.g. qwen3.8-livetranslate-flash-realtime)")
	// ErrTranslationOnlyModel answers a /v1/realtime session for a Qwen-LiveTranslate model.
	ErrTranslationOnlyModel = errors.New("this is a Bailian Qwen-LiveTranslate realtime model, which only serves realtime translation sessions; " +
		"connect to /v1/realtime/translations instead")
)

// translationSelectionError maps the selector's translation errors to the client answers; it
// returns nil for every other error.
func translationSelectionError(err error) error {
	switch {
	case errors.Is(err, orchestrator.ErrRealtimeNotTranslationModel):
		return ErrNotTranslationModel
	case errors.Is(err, orchestrator.ErrRealtimeTranslationOnly):
		return ErrTranslationOnlyModel
	default:
		return nil
	}
}

// checkTranslationTarget re-checks the selector's translation rule for one target. decided is
// true when the rule settles the target: a translation session or a livetranslate model.
func checkTranslationTarget(translation bool, target biz.RealtimeTarget, actual string) (decided bool, err error) {
	liveTranslate := target.Dialect == biz.RealtimeDialectBailian && bailian.SupportedLiveTranslate(actual)

	switch {
	case translation && !liveTranslate:
		return true, ErrNotTranslationModel
	case !translation && liveTranslate:
		return true, ErrTranslationOnlyModel
	default:
		return translation, nil
	}
}

// newTranslationDialect builds the dialect of a translation session.
func (r *Relay) newTranslationDialect(requested, actual string, ids *dialect.EventIDs) (dialect.Dialect, error) {
	d, err := bailian.NewLiveTranslate(bailian.LiveTranslateOptions{
		RequestedModel:  requested,
		ActualModel:     actual,
		IDs:             ids,
		Voice:           r.Config.LiveTranslateVoice,
		SessionDuration: r.Config.SessionDuration(bailian.LiveTranslateDialectName),
	})
	if err != nil {
		return nil, err
	}

	return d, nil
}
