import { useEffect, useRef, useState } from 'react';
import { message, request } from './http';
import type { Drawing } from './toolpath';

export function Preview({
  id,
  source,
}: {
  id: number;
  source: 'archives' | 'library/files';
}) {
  const [drawing, setDrawing] = useState<Drawing>();
  const [error, setError] = useState('');
  const [plates, setPlates] = useState<string[]>([]);
  const [plate, setPlate] = useState('');
  const [layer, setLayer] = useState(0);
  const [name, setName] = useState('Print preview');
  const canvas = useRef<HTMLCanvasElement>(null);
  const worker = useRef<Worker | null>(null);
  useEffect(() => {
    const stop = new AbortController();
    const parser = new Worker(new URL('./plate.worker.ts', import.meta.url), {
      type: 'module',
    });
    worker.current = parser;
    parser.onmessage = ({ data }) => {
      if (data.error) {
        setError(data.error);
        setDrawing(undefined);
        return;
      }
      if (data.plates) {
        setPlates(data.plates);
        setPlate(data.plates[0]);
        parser.postMessage({ plate: data.plates[0], zipped: true });
        return;
      }
      setLayer(0);
      setDrawing(data.drawing);
    };
    parser.onerror = () => setError('The file preview could not be generated.');
    async function load() {
      const path = `/api/v1/${source}/${id}`;
      const meta = await (await request(path, { signal: stop.signal })).json();
      const filename = String(meta.filename || '');
      setName(filename || 'Print preview');
      const limit = 96 * 1024 * 1024;
      if (Number(meta.file_size) > limit)
        throw Error('The file exceeds the 96 MiB preview limit.');
      const response = await request(path + '/download', {
        signal: stop.signal,
      });
      if (Number(response.headers.get('Content-Length')) > limit)
        throw Error('The file exceeds the 96 MiB preview limit.');
      const data = await response.arrayBuffer();
      if (data.byteLength > limit)
        throw Error('The file exceeds the 96 MiB preview limit.');
      if (!stop.signal.aborted)
        parser.postMessage({ data, zipped: /\.3mf$/i.test(filename) }, [data]);
    }
    load().catch((e) => {
      if (!stop.signal.aborted) setError(message(e));
    });
    return () => {
      stop.abort();
      parser.terminate();
      worker.current = null;
    };
  }, [id, source]);
  useEffect(() => {
    const node = canvas.current;
    if (!node || !drawing) return;
    const render = () => {
      const width = Math.max(1, node.clientWidth),
        height = Math.max(1, node.clientHeight),
        ratio = devicePixelRatio || 1;
      node.width = Math.round(width * ratio);
      node.height = Math.round(height * ratio);
      const ctx = node.getContext('2d');
      if (!ctx) return;
      ctx.scale(ratio, ratio);
      const [xmin, ymin, xmax, ymax] = drawing.bounds;
      const scale = Math.min(
        (width - 32) / Math.max(1, xmax - xmin),
        (height - 32) / Math.max(1, ymax - ymin),
      );
      ctx.translate(width / 2, height / 2);
      ctx.scale(scale, -scale);
      ctx.translate(-(xmin + xmax) / 2, -(ymin + ymax) / 2);
      ctx.lineWidth = 1.2 / scale;
      ctx.strokeStyle = '#00727d';
      ctx.beginPath();
      const lines = drawing.layers[layer].lines;
      for (let i = 0; i < lines.length; i += 4) {
        ctx.moveTo(lines[i], lines[i + 1]);
        ctx.lineTo(lines[i + 2], lines[i + 3]);
      }
      ctx.stroke();
    };
    const observer = new ResizeObserver(render);
    observer.observe(node);
    render();
    return () => observer.disconnect();
  }, [drawing, layer]);
  return (
    <>
      <p>
        <a href={source === 'archives' ? '/archives' : '/files'}>
          Back to {source === 'archives' ? 'archives' : 'file library'}
        </a>
      </p>
      <h1>{name}</h1>
      <section className="sheet">
        <h2>Layer preview</h2>
        {plates.length > 0 && (
          <label>
            Plate
            <select
              value={plate}
              onChange={(e) => {
                const next = e.target.value;
                setPlate(next);
                setDrawing(undefined);
                setError('');
                worker.current?.postMessage({ plate: next, zipped: true });
              }}
            >
              {plates.map((p) => (
                <option key={p}>{p}</option>
              ))}
            </select>
          </label>
        )}
        {error && <p role="alert">{error}</p>}
        {drawing ? (
          <>
            <canvas
              ref={canvas}
              className="plot"
              aria-label={
                'Extrusion paths at ' + drawing.layers[layer].z + ' mm'
              }
            />
            <label>
              Layer {layer + 1} of {drawing.layers.length} · Z{' '}
              {drawing.layers[layer].z} mm
              <input
                type="range"
                min="0"
                max={drawing.layers.length - 1}
                value={layer}
                onChange={(e) => setLayer(Number(e.target.value))}
              />
            </label>
            {drawing.skippedArcs > 0 && (
              <p>
                {drawing.skippedArcs} curved moves are omitted from this linear
                preview.
              </p>
            )}
          </>
        ) : (
          !error && <p role="status">Reading print moves…</p>
        )}
      </section>
    </>
  );
}
