import assert from 'node:assert/strict';
import test from 'node:test';
import { plotMoves } from '../src/toolpath.ts';

test('groups extrusions by height and excludes travel and retractions', () => {
  const drawing = plotMoves(
    'G90\nM83\nG1 Z0.2\nG1 X6 Y2 E1\nG1 X10 E-1\nG1 Z0.4\nG1 X12 Y3 E1',
  );
  assert.deepEqual(
    drawing.layers.map((l) => [l.z, ...l.lines]),
    [
      [0.2, 0, 0, 6, 2],
      [0.4, 10, 2, 12, 3],
    ],
  );
  assert.deepEqual(drawing.bounds, [0, 0, 12, 3]);
});
test('applies coordinate resets and relative motion', () => {
  const drawing = plotMoves(
    'G92 X3 Y4 E20\nG91\nG1 X2 Y3 E1\nG90\nM82\nG1 X6 Y8 E22\n',
  );
  assert.deepEqual([...drawing.layers[0].lines], [3, 4, 5, 7, 5, 7, 6, 8]);
});
