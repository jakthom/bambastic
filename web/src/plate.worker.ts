import { unzipSync } from 'fflate';
import { plotMoves } from './toolpath';
let archive: Uint8Array | undefined;
const limit = 96 * 1024 * 1024;
self.onmessage = (
  event: MessageEvent<{ data?: ArrayBuffer; zipped?: boolean; plate?: string }>,
) => {
  try {
    const request = event.data;
    if (request.data) {
      archive = new Uint8Array(request.data);
    }
    if (!archive) throw new Error('Missing print file.');
    let program = archive;
    if (request.zipped) {
      const plates: string[] = [];
      const files = unzipSync(archive, {
        filter: (entry) => {
          if (!/^Metadata\/plate_\d+\.gcode$/i.test(entry.name)) return false;
          plates.push(entry.name);
          if (entry.name !== request.plate) return false;
          if (entry.originalSize > limit)
            throw new Error('The plate is too large to preview.');
          return true;
        },
      });
      if (!request.plate) {
        if (!plates.length)
          throw new Error('This 3MF contains no sliced plates.');
        plates.sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
        self.postMessage({ plates });
        return;
      }
      program = files[request.plate];
      if (!program) throw new Error('The requested plate is missing.');
    }
    const drawing = plotMoves(new TextDecoder().decode(program));
    self.postMessage(
      { drawing },
      { transfer: drawing.layers.map((l) => l.lines.buffer) },
    );
  } catch (error) {
    self.postMessage({
      error: error instanceof Error ? error.message : 'Preview failed.',
    });
  }
};
