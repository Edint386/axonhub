import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import ChannelHealthPage from '@/features/channel-health';

function ProtectedChannelHealth() {
  return (
    <RouteGuard requiredScopes={['read_channels']} scopeLevel='system'>
      <ChannelHealthPage />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/channel-health/')({
  component: ProtectedChannelHealth,
});
