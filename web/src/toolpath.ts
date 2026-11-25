export interface Layer {
  z: number;
  lines: Float32Array;
}
export interface Drawing {
  layers: Layer[];
  bounds: [number, number, number, number];
  skippedArcs: number;
}

export function plotMoves(program: string): Drawing {
  const position: Record<string, number> = { X: 0, Y: 0, Z: 0, E: 0 };
  let relative = false,
    relativeE = false,
    unit = 1,
    skippedArcs = 0,
    count = 0;
  const byHeight = new Map<number, number[]>();
  let left = Infinity,
    right = -Infinity,
    bottom = Infinity,
    top = -Infinity;
  // Parse one line at a time to avoid allocating an array of every input line.
  for (const line of program.matchAll(/[^\r\n]+/g)) {
    const words = line[0]
      .split(';', 1)[0]
      .trim()
      .toUpperCase()
      .replace(/^N\d+\s*/, '');
    const op = /^([GM]\d+)\s*/.exec(words);
    if (!op) continue;
    const code = op[1];
    if (code === 'G20' || code === 'G21') {
      unit = code === 'G20' ? 25.4 : 1;
      continue;
    }
    if (code === 'G90' || code === 'G91') {
      relative = code === 'G91';
      relativeE = relative;
      continue;
    }
    if (code === 'M82' || code === 'M83') {
      relativeE = code === 'M83';
      continue;
    }
    if (!['G0', 'G1', 'G2', 'G3', 'G92'].includes(code)) continue;
    const x = position.X,
      y = position.Y,
      e = position.E;
    for (const m of words
      .slice(op[0].length)
      .matchAll(/([XYZE])\s*([-+]?(?:\d+(?:\.\d*)?|\.\d+))/g)) {
      const axis = m[1];
      const delta = Number(m[2]) * unit;
      position[axis] =
        code !== 'G92' && (axis === 'E' ? relativeE : relative)
          ? position[axis] + delta
          : delta;
    }
    if (code === 'G2' || code === 'G3') {
      skippedArcs++;
      continue;
    }
    if (
      code === 'G92' ||
      position.E <= e ||
      (position.X === x && position.Y === y)
    )
      continue;
    if (++count > 2_000_000)
      throw new Error(
        'This plate exceeds the preview limit of two million moves.',
      );
    const z = Math.round(position.Z * 1000) / 1000;
    let layer = byHeight.get(z);
    if (!layer) {
      layer = [];
      byHeight.set(z, layer);
    }
    layer.push(x, y, position.X, position.Y);
    left = Math.min(left, x, position.X);
    right = Math.max(right, x, position.X);
    bottom = Math.min(bottom, y, position.Y);
    top = Math.max(top, y, position.Y);
  }
  if (!byHeight.size)
    throw new Error('No linear extrusion moves were found on this plate.');
  return {
    layers: [...byHeight.entries()]
      .sort(([a], [b]) => a - b)
      .map(([z, data]) => ({ z, lines: Float32Array.from(data) })),
    bounds: [left, bottom, right, top],
    skippedArcs,
  };
}
