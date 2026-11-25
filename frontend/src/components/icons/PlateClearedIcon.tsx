import { CheckSquare } from './registry';

export function PlateClearedIcon({ className = 'w-4 h-4' }: { className?: string }) {
  return <CheckSquare className={className} />;
}
