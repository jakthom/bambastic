import { Lightbulb } from './registry';

export function ChamberLight({ on, className = 'w-5 h-5' }: { on: boolean; className?: string }) {
  return <Lightbulb className={`${className} ${on ? 'text-amber-500' : 'text-bambu-gray'}`} />;
}
