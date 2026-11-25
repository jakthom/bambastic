import { useEffect, useState } from 'react';
import { message, read, submit } from './http';

interface Camera {
  id: number;
  name: string;
  model: string;
  state: string;
  progress: number;
}
export function Cameras({
  id,
  displayToken = '',
  secured,
}: {
  id?: number;
  displayToken?: string;
  secured: boolean;
}) {
  const [cameras, setCameras] = useState<Camera[]>();
  const [media, setMedia] = useState('');
  const [error, setError] = useState('');
  const [attempt, retry] = useState(0);
  useEffect(() => {
    let current = true;
    const stop = new AbortController();
    async function connect() {
      setError('');
      const list = id
        ? [{ id, name: 'Printer ' + id, model: '', state: '', progress: 0 }]
        : await read<Camera[]>(
            '/api/v1/camwall/printers' +
              (displayToken
                ? '?token=' + encodeURIComponent(displayToken)
                : ''),
            { signal: stop.signal },
          );
      const token =
        displayToken ||
        (secured && list.length
          ? (
              await submit<{ token: string }>(
                '/api/v1/printers/camera/stream-token',
                {},
              )
            ).token
          : '');
      if (current) {
        setMedia(token);
        setCameras(list);
      }
    }
    connect().catch((e) => {
      if (current) setError(message(e));
    });
    return () => {
      current = false;
      stop.abort();
    };
  }, [id, displayToken, secured, attempt]);
  return (
    <>
      <header className="pagehead">
        <h1>{id ? 'Printer camera' : 'Camera wall'}</h1>
        <button onClick={() => retry((n) => n + 1)}>Reconnect</button>
      </header>
      {error && <p role="alert">{error}</p>}
      {!error &&
        (!cameras ? (
          <p role="status">Connecting…</p>
        ) : !cameras.length ? (
          <section className="sheet">
            No cameras are available for this account.
          </section>
        ) : (
          <div className="feeds">
            {cameras.map((c) => (
              <CameraFeed key={c.id + '-' + attempt} camera={c} token={media} />
            ))}
          </div>
        ))}
    </>
  );
}
function CameraFeed({ camera, token }: { camera: Camera; token: string }) {
  const [failed, setFailed] = useState(false);
  return (
    <figure>
      <figcaption>
        <strong>{camera.name}</strong> {camera.model}
      </figcaption>
      {failed ? (
        <p role="status">The camera stream is unavailable.</p>
      ) : (
        <img
          alt={'Live view from ' + camera.name}
          src={`/api/v1/printers/${camera.id}/camera/stream${token ? '?token=' + encodeURIComponent(token) : ''}`}
          onError={() => setFailed(true)}
        />
      )}
    </figure>
  );
}
