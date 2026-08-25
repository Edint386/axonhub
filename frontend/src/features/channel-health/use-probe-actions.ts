import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useRunChannelHealthProbe, type ChannelHealthProbeChannel } from './data/channel-health';

function targetKey(channelID: string, modelID: string) {
  return `${channelID}:${modelID}`;
}

export function useProbeActions() {
  const { t } = useTranslation();
  const runProbe = useRunChannelHealthProbe();
  const [probing, setProbing] = useState<Set<string>>(new Set());
  const probingRef = useRef(new Set<string>());

  const isChannelProbing = (channelID: string) => {
    const prefix = `${channelID}:`;
    for (const key of probingRef.current) {
      if (key.startsWith(prefix)) {
        return true;
      }
    }
    return false;
  };

  const reserveTargets = (keys: string[]) => {
    if (keys.some((key) => probingRef.current.has(key))) {
      return false;
    }
    for (const key of keys) {
      probingRef.current.add(key);
    }
    setProbing(new Set(probingRef.current));
    return true;
  };

  const releaseTargets = (keys: string[]) => {
    for (const key of keys) {
      probingRef.current.delete(key);
    }
    setProbing(new Set(probingRef.current));
  };

  const probeChannel = async (channel: ChannelHealthProbeChannel) => {
    const targets = channel.models.filter((model) => model.enabled);
    if (!channel.enabled || targets.length === 0 || isChannelProbing(channel.channelID)) {
      return;
    }
    const keys = targets.map((model) => targetKey(channel.channelID, model.modelID));
    if (!reserveTargets(keys)) {
      return;
    }
    try {
      const results = await Promise.allSettled(
        targets.map((model) => runProbe.mutateAsync({ channelID: channel.channelID, modelID: model.modelID, stream: model.stream }))
      );
      const failed = results.filter((result) => result.status === 'rejected').length;
      if (failed === 0) {
        toast.success(t('channelHealth.messages.probeFinished', { name: channel.channelName }));
      } else {
        toast.error(t('channelHealth.messages.probePartialFailed', { failed, total: targets.length }));
      }
    } finally {
      releaseTargets(keys);
    }
  };

  const probeModel = async (channel: ChannelHealthProbeChannel, modelID: string, stream: boolean) => {
    const key = targetKey(channel.channelID, modelID);
    if (!channel.enabled || isChannelProbing(channel.channelID)) {
      return;
    }
    if (!reserveTargets([key])) {
      return;
    }
    try {
      const result = await Promise.allSettled([runProbe.mutateAsync({ channelID: channel.channelID, modelID, stream })]);
      if (result[0]?.status === 'fulfilled') {
        toast.success(t('channelHealth.messages.probeFinished', { name: modelID }));
      } else {
        const reason = result[0]?.reason;
        toast.error(reason instanceof Error ? reason.message : t('channelHealth.messages.runFailed'));
      }
    } finally {
      releaseTargets([key]);
    }
  };

  return { probing, isChannelProbing, probeChannel, probeModel };
}
