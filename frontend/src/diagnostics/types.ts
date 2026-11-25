import type { QueryClient } from '@tanstack/react-query';

export type Fields = Record<string, string | number | boolean | null>;
export interface Diagnostics {
  event(kind: string, fields?: Fields): void;
  context(name: string, fields: Fields): void;
  crash(source: string, error: unknown, componentStack?: string | null, fields?: Fields): void;
  queries(client: QueryClient): void;
  socket(socket: WebSocket): void;
}

declare global {
  interface Window { bambasticDiagnostics?: Diagnostics }
}
