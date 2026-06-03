import { useCallback, useEffect, useState } from 'react';

const STORAGE_KEY = 'axonhub.channels.skipStatusConfirmation';
const CHANGE_EVENT = 'axonhub:channels:skip-status-confirmation-changed';

function readSkipStatusConfirmation() {
  if (typeof window === 'undefined') {
    return false;
  }

  return window.localStorage.getItem(STORAGE_KEY) === 'true';
}

export function useSkipChannelStatusConfirmation() {
  const [skipStatusConfirmation, setSkipStatusConfirmationState] = useState(readSkipStatusConfirmation);

  useEffect(() => {
    const syncPreference = () => {
      setSkipStatusConfirmationState(readSkipStatusConfirmation());
    };

    window.addEventListener(CHANGE_EVENT, syncPreference);
    window.addEventListener('storage', syncPreference);

    return () => {
      window.removeEventListener(CHANGE_EVENT, syncPreference);
      window.removeEventListener('storage', syncPreference);
    };
  }, []);

  const setSkipStatusConfirmation = useCallback((enabled: boolean) => {
    if (typeof window === 'undefined') {
      return;
    }

    window.localStorage.setItem(STORAGE_KEY, String(enabled));
    setSkipStatusConfirmationState(enabled);
    window.dispatchEvent(new Event(CHANGE_EVENT));
  }, []);

  return [skipStatusConfirmation, setSkipStatusConfirmation] as const;
}
