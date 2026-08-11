package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/scheduler"
	"go.uber.org/fx"
)

var Module = fx.Module("orchestrator",
	fx.Provide(NewDefaultSelector),
	fx.Provide(NewCandidateSelectorDiagnostics),
	fx.Provide(NewChannelLimiterManager),
	fx.Provide(func(svc *biz.ProviderQuotaService) ProviderQuotaStatusProvider { return svc }),
	fx.Provide(NewTestChannelOrchestrator),
	fx.Provide(NewChannelHealthProbeRunner),
	fx.Invoke(func(lc fx.Lifecycle, runner *ChannelHealthProbeRunner, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return runner.RegisterScheduledTasks(ctx, s)
			},
		})
	}),
)
