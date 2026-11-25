import { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';

type Verdict = 'good' | 'reject';
interface Confirmation {
  print_name: string;
  plate_id: number | null;
  verdict: Verdict;
  answered: boolean;
  recorded_verdict: Verdict | null;
}

async function confirmation(url: string, method: 'GET' | 'POST', signal?: AbortSignal): Promise<Confirmation> {
  const res = await fetch(url, { method, signal, headers: { Accept: 'application/json' }, referrerPolicy: 'no-referrer' });
  const body = await res.json();
  if (!res.ok) throw new Error(body.detail || res.statusText);
  return body;
}

export function PrintConfirmationPage() {
  const { t } = useTranslation();
  const { token, verdict } = useParams();
  const url = '/api/v1/archives/confirm/' + encodeURIComponent(token ?? '') + '/' + encodeURIComponent(verdict ?? '');
  const [result, setResult] = useState<Confirmation | null>(null);
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    const request = new AbortController();
    setResult(null);
    setError('');
    confirmation(url, 'GET', request.signal).then(setResult).catch((e: Error) => {
      if (!request.signal.aborted) setError(e.message);
    });
    return () => request.abort();
  }, [url]);

  async function save() {
    setSaving(true);
    setError('');
    try { setResult(await confirmation(url, 'POST')); }
    catch (e) { setError(e instanceof Error ? e.message : t('confirmOutcome.saveFailed')); }
    finally { setSaving(false); }
  }

  return <main className="min-h-screen p-6 grid place-items-center bg-bambu-dark">
    <section className="w-full max-w-md space-y-5 rounded-xl border border-bambu-dark-tertiary bg-bambu-dark-secondary p-7 shadow-sm">
      <h1 className="text-2xl font-semibold text-white">{t('confirmOutcome.title')}</h1>
      {error && <p role="alert" className="text-red-400">{error}</p>}
      {result && <>
        <p className="font-medium text-white">{result.print_name}</p>
        {result.answered ? <p role="status" className="text-bambu-gray-light">
          {result.recorded_verdict === 'good' ? t('confirmOutcome.alreadyGood')
            : result.recorded_verdict === 'reject' ? t('confirmOutcome.alreadyRejected') : t('common.done', { defaultValue: 'Done' })}
        </p> : <button type="button" disabled={saving} onClick={save} className="rounded-lg bg-bambu-green px-5 py-2 font-medium text-white disabled:opacity-50">
          {t(result.verdict === 'good' ? 'confirmOutcome.good' : 'confirmOutcome.reject')}
        </button>}
      </>}
      {!result && !error && <p role="status">{t('common.loading')}</p>}
    </section>
  </main>;
}
