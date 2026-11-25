import { CopyButton } from './CopyButton';

export function McpSample({ text, label, prose = false }: { text: string; label: string; prose?: boolean }) {
  return (
    <div role="group" aria-label={label} className="min-w-0">
      <CopyButton key={text} value={text}
        className="flex w-full cursor-pointer items-baseline gap-2 rounded-lg border border-bambu-dark-tertiary bg-bambu-dark p-3 text-start text-bambu-gray-light hover:border-bambu-green focus-visible:outline-2 focus-visible:outline-bambu-green"
        iconClassName="size-4 shrink-0 self-start">
        {prose
          ? <span className="min-w-0 flex-1 leading-relaxed text-white">{text}</span>
          : <code className="min-w-0 flex-1 whitespace-pre-wrap wrap-anywhere text-left text-sm leading-relaxed text-white" dir="ltr">{text}</code>}
      </CopyButton>
    </div>
  );
}
