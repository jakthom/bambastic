import type { HMSError } from '../api/client';
import { filterKnownHMSErrors } from '../components/HMSErrorModal';

export type PrinterState = 'printing' | 'paused' | 'finished' | 'idle' | 'error' | 'unknown' | 'offline';

/** Classify a printer into one of the UI status buckets. */
export function classifyPrinterStatus(
  status: { connected: boolean; state: string | null; hms_errors?: HMSError[] } | undefined,
): PrinterState {
  if (!status) return 'unknown';
  if (!status.connected) return 'offline';
  const hmsErrors = status.hms_errors ? filterKnownHMSErrors(status.hms_errors) : [];
  if (hmsErrors.length > 0) return 'error';
  switch (status.state) {
    case null:
    case 'UNKNOWN': return 'unknown';
    case 'RUNNING': return 'printing';
    case 'PAUSE':   return 'paused';
    case 'FINISH':  return 'finished';
    // FAILED without an active HMS error is the printer's terminal state after
    // any unsuccessful end — including user-cancellations. Treat the same as
    // FINISH for grouping/badging purposes; only escalate to "error" when an
    // HMS code is actually attached (handled by the early-return above).
    case 'FAILED':  return 'finished';
    default:        return 'idle';
  }
}
