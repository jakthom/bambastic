// Rebuild the Go server's compatibility icon responses using precisely the
// same semantic roles, geometry and package version as the Vite icon picker.
import fs from 'node:fs/promises';
const read = path => fs.readFile(new URL(path, import.meta.url), 'utf8');
const picker = await read('../src/components/IconPicker.tsx');
const registry = await read('../src/components/icons/registry.tsx');
const roles = new Map([...registry.matchAll(/export const (\w+) = .*?makeIcon\('[^']+', (\w+)\)/g)].map(match => match.slice(1, 3)));
const icons = {};
for (const [, name, component] of picker.matchAll(/\{ name: '([^']+)', icon: (\w+) \}/g)) {
  const symbol = roles.get(component);
  const category = [...registry.matchAll(/import \{([^{}]+)\} from "@openships\/openbridge-icons\/([^"]+)"/g)].find(match => match[1].split(',').map(s => s.trim()).includes(symbol));
  if (!category) throw new Error('No registered OpenBridge glyph for '+component);
  const glyph = (await import('@openships/openbridge-icons/'+category[2]))[symbol];
  if (!glyph.startsWith('<svg')) throw new Error('Invalid SVG '+name);
  icons[name] = glyph;
}
if (Object.keys(icons).length < 20) throw new Error('Incomplete OpenBridge picker registry');
await fs.writeFile(new URL('../../internal/openbridge/glyphs.json',import.meta.url),JSON.stringify(icons,null,2)+'\n');
