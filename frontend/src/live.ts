import { useEffect, useState } from 'react';
import { RequestError, submit } from './http';

export interface DeviceState {
  connected: boolean;
  state: string;
  progress: number;
  current_print?: string;
}
export function useFleetFeed(active: boolean) {
  const [states, setStates] = useState<Record<number, DeviceState>>({});
  useEffect(() => {
    if (!active) return;
    let stopped = false;
    let socket: WebSocket | undefined;
    let timer: ReturnType<typeof setTimeout>;
    function retry() {
      if (!stopped) {
        setStates({});
        timer = setTimeout(connect, 5000);
      }
    }
    async function connect() {
      try {
        const { token } = await submit<{ token: string }>(
          '/api/v1/auth/ws-token',
          {},
        );
        if (stopped) return;
        const url = new URL('/api/v1/ws', location.origin);
        url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
        url.searchParams.set('token', token);
        socket = new WebSocket(url);
        socket.onmessage = (event) => {
          let update;
          try {
            update = JSON.parse(event.data);
          } catch {
            return;
          }
          if (
            update.type !== 'printer_status' ||
            !update.data ||
            !Number.isInteger(update.printer_id)
          )
            return;
          setStates((previous) => ({
            ...previous,
            [update.printer_id]: update.data,
          }));
          if (import.meta.env.DEV)
            window.dispatchEvent(
              new CustomEvent('farm:diagnostic-event', {
                detail: {
                  kind: 'websocket.message',
                  fields: {
                    type: update.type,
                    printerId: update.printer_id,
                    connected: update.data.connected,
                    state: update.data.state,
                  },
                },
              }),
            );
        };
        socket.onclose = retry;
      } catch (e) {
        if (!(e instanceof RequestError && [401, 403].includes(e.status)))
          retry();
      }
    }
    void connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      if (socket) {
        socket.onclose = null;
        socket.close();
      }
    };
  }, [active]);
  return states;
}
