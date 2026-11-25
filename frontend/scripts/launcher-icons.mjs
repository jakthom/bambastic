import { readFile, writeFile } from 'node:fs/promises';
import { Resvg } from '@resvg/resvg-js';
import { homeIcon } from '@openships/openbridge-icons/application';

const mark = await readFile(new URL('../src/assets/brand/bambastic-mark.svg', import.meta.url), 'utf8');
// The mark's own viewBox centers it in a square and preserves its proportions.
const faviconSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">${mark}</svg>`;
const body = homeIcon.slice(homeIcon.indexOf('>') + 1, homeIcon.lastIndexOf('</svg>'));
const launcherSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="9" fill="#075A99"/><g transform="translate(9 9) scale(1.25)" color="#D6F5F2">${body}</g></svg>`;
const assets = {
  'favicon.png': 64,
  'favicon-16x16.png': 16,
  'favicon-32x32.png': 32,
  'apple-touch-icon.png': 180,
  'android-chrome-192x192.png': 192,
  'android-chrome-512x512.png': 512,
  'maskable-192x192.png': 192,
  'maskable-512x512.png': 512,
};
for (const [file, width] of Object.entries(assets)) {
  const svg = file.startsWith('favicon') ? faviconSvg : launcherSvg;
  const bytes = new Resvg(svg, { fitTo: { mode: 'width', value: width } }).render().asPng();
  await writeFile(new URL(`../public/img/${file}`, import.meta.url), bytes);
}

for (const name of ['Bambastic', 'SpoolBuddy']) {
  const width = name === 'Bambastic' ? 159 : 172;
  const wordmark = `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="38" viewBox="0 0 ${width} 38"><rect x="1" y="2" width="34" height="34" rx="8" fill="#075A99"/><g transform="translate(7 8) scale(.9)" color="#D6F5F2">${body}</g><text x="42" y="27" font-size="23" font-weight="650" font-family="system-ui,sans-serif" fill="#EDF9FF">${name}</text></svg>`;
  await writeFile(new URL(`../public/img/${name.toLowerCase()}-wordmark.svg`, import.meta.url), wordmark);
}
