import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import postcss from 'postcss';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

const accents = ['blue', 'green', 'teal', 'orange', 'purple', 'red'];
const backgrounds = {
  light: ['cool', 'warm', 'neutral'],
  dark: ['cool', 'warm', 'neutral', 'oled', 'slate', 'forest'],
};

function rgb(hex: string) {
  expect(hex).toMatch(/^#[\da-f]{6}$/i);
  return [1, 3, 5].map(start => parseInt(hex.slice(start, start + 2), 16) / 255);
}

function luminance(channels: number[]) {
  const [r, g, b] = channels.map(c => c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return r * 0.2126 + g * 0.7152 + b * 0.0722;
}

function contrast(label: number[], fill: number[]) {
  const a = luminance(label);
  const b = luminance(fill);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

describe('saved theme palettes', () => {
  const style = document.createElement('style');
  const root = document.documentElement;
  const originalClasses = root.className;

  beforeAll(() => {
    // JSDOM can cascade the theme selectors directly once Tailwind directives
    // have been stripped. Preserve source order, including fallback rules.
    const stylesheet = readFileSync(resolve(__dirname, '../../index.css'), 'utf8');
    style.textContent = postcss.parse(stylesheet).nodes.filter(node => node.type === 'rule').join('\n');
    document.head.append(style);
  });

  afterAll(() => {
    style.remove();
    root.className = originalClasses;
  });

  it.each(['light', 'dark'] as const)('honors each %s palette and keeps normal and hover labels legible', mode => {
    const selectedSurfaces = new Set<string>();
    for (const background of backgrounds[mode]) {
      const selectedAccents = new Set<string>();
      for (const accent of accents) {
        root.className = `${mode === 'dark' ? 'dark ' : ''}style-classic bg-${background} accent-${accent}`;
        const theme = getComputedStyle(root);
        const fill = theme.getPropertyValue('--accent');
        const label = rgb(theme.getPropertyValue('--button-label'));
        const surface = theme.getPropertyValue('--bg-primary');
        selectedAccents.add(fill);
        selectedSurfaces.add(surface);
        for (const token of ['--accent', '--accent-light', '--accent-dark']) {
          expect(contrast(label, rgb(theme.getPropertyValue(token))), `${root.className} ${token}`).toBeGreaterThanOrEqual(4.5);
        }
        // Several primary controls use 80% or 90% accent on hover. The
        // least opaque fill gives the lowest contrast against each surface.
        for (const token of ['--bg-primary', '--bg-secondary', '--bg-tertiary']) {
          const underlay = rgb(theme.getPropertyValue(token));
          const hover = rgb(fill).map((channel, i) => channel * 0.8 + underlay[i] * 0.2);
          expect(contrast(label, hover), `${root.className} 80% on ${token}`).toBeGreaterThanOrEqual(4.5);
        }
      }
      expect(selectedAccents.size).toBe(accents.length);
    }
    expect(selectedSurfaces.size).toBe(backgrounds[mode].length);
  });

  it.each([
    ['', '#075a99', '#ffffff', '#f1f7fb'],
    ['dark', '#78c6ff', '#092237', '#0a2030'],
  ])('uses the default blue palette with only "%s" on the root', (classes, fill, label, canvas) => {
    root.className = classes;
    const theme = getComputedStyle(root);
    expect(theme.getPropertyValue('--accent')).toBe(fill);
    expect(theme.getPropertyValue('--button-label')).toBe(label);
    expect(theme.getPropertyValue('--bg-primary')).toBe(canvas);
  });

  it.each(['light', 'dark'])('keeps the current navigation label and camera readable in %s mode', mode => {
    root.className = `${mode} bg-cool accent-blue`;
    const row = document.createElement('a');
    row.className = 'sidebar-link';
    row.setAttribute('aria-current', 'page');
    const camera = document.createElement('button');
    camera.className = 'sidebar-camera-selected';
    document.body.append(row, camera);
    try {
      const selected = getComputedStyle(row);
      expect(selected.color).toBe('var(--selection-ink)');
      expect(getComputedStyle(camera).color).toBe(selected.color);
      expect(selected.backgroundColor).toBe('var(--selection-bg)');
      const theme = getComputedStyle(root);
      expect(contrast(rgb(theme.getPropertyValue('--selection-ink')), rgb(theme.getPropertyValue('--selection-bg')))).toBeGreaterThanOrEqual(4.5);
    } finally {
      row.remove();
      camera.remove();
    }
  });

  it.each(['button-primary bg-bambu-green', 'bg-bambu-green text-white', 'bg-bambu-green text-black'])(
    'applies the button label to %s', classes => {
      const button = document.createElement('button');
      button.className = classes;
      document.body.append(button);
      const color = getComputedStyle(button).color;
      button.remove();
      expect(color).toBe('var(--button-label)');
    },
  );
});
