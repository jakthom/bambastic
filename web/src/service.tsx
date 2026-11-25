import { useEffect, useState } from 'react';
import { read, message, submit } from './http';

interface Task {
  id: number;
  maintenance_type_name: string;
  is_due: boolean;
  is_warning: boolean;
  enabled: boolean;
}
interface ServicePlan {
  printer_id: number;
  printer_name: string;
  total_print_hours: number;
  maintenance_items: Task[];
}
export function Maintenance({ canRecord }: { canRecord: boolean }) {
  const [plans, setPlans] = useState<ServicePlan[]>();
  const [error, setError] = useState('');
  const [revision, reload] = useState(0);
  const [working, setWorking] = useState(false);
  useEffect(() => {
    const stop = new AbortController();
    read<ServicePlan[]>('/api/v1/maintenance/overview', { signal: stop.signal })
      .then(setPlans)
      .catch((e) => {
        if (!stop.signal.aborted) setError(message(e));
      });
    return () => stop.abort();
  }, [revision]);
  async function record(task: Task, printer: string) {
    const notes = window.prompt(
      `Record completed ${task.maintenance_type_name} on ${printer}. Optional notes:`,
    );
    if (notes === null) return;
    setWorking(true);
    setError('');
    try {
      await submit(`/api/v1/maintenance/items/${task.id}/perform`, { notes });
      reload((n) => n + 1);
    } catch (e) {
      setError(message(e));
    } finally {
      setWorking(false);
    }
  }
  return (
    <>
      <h1>Maintenance</h1>
      {error && <p role="alert">{error}</p>}
      {!plans && !error && <p>Loading maintenance tasks…</p>}
      {plans?.length === 0 && (
        <section className="sheet">
          No active printers have maintenance schedules.
        </section>
      )}
      {plans?.map((plan) => (
        <section key={plan.printer_id} className="sheet">
          <h2>{plan.printer_name}</h2>
          <p>{plan.total_print_hours.toFixed(1)} print hours</p>
          <table>
            <thead>
              <tr>
                <th>Task</th>
                <th>Status</th>
                {canRecord && <th>Service log</th>}
              </tr>
            </thead>
            <tbody>
              {plan.maintenance_items
                .filter((t) => t.enabled)
                .map((t) => (
                  <tr key={t.id}>
                    <td>{t.maintenance_type_name}</td>
                    <td>
                      {t.is_due
                        ? 'Due'
                        : t.is_warning
                          ? 'Due soon'
                          : 'Scheduled'}
                    </td>
                    {canRecord && (
                      <td>
                        <button
                          disabled={working}
                          onClick={() => record(t, plan.printer_name)}
                        >
                          Record service
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
            </tbody>
          </table>
        </section>
      ))}
    </>
  );
}

interface Counts {
  total_prints: number;
  successful_prints: number;
  failed_prints: number;
  cancelled_prints: number;
  total_print_time_hours: number;
  total_filament_grams: number;
  total_energy_kwh: number;
  prints_by_filament_type: Record<string, number>;
}
export function Statistics() {
  const [range, setRange] = useState('');
  const [stats, setStats] = useState<Counts>();
  const [error, setError] = useState('');
  useEffect(() => {
    const stop = new AbortController();
    setError('');
    setStats(undefined);
    read<Counts>('/api/v1/archives/stats?' + range, { signal: stop.signal })
      .then(setStats)
      .catch((e) => {
        if (!stop.signal.aborted) setError(message(e));
      });
    return () => stop.abort();
  }, [range]);
  return (
    <>
      <h1>Print statistics</h1>
      <section className="sheet">
        <form
          className="actions"
          onSubmit={(e) => {
            e.preventDefault();
            const form = new FormData(e.currentTarget);
            const q = new URLSearchParams();
            for (const [name, value] of form)
              if (value) q.set(name, String(value));
            q.set('tz', Intl.DateTimeFormat().resolvedOptions().timeZone);
            setRange(q.toString());
          }}
        >
          <label>
            From
            <input type="date" name="date_from" />
          </label>
          <label>
            Through
            <input type="date" name="date_to" />
          </label>
          <button>Apply dates</button>
        </form>
        {error && <p role="alert">{error}</p>}
        {stats ? (
          <>
            <h2>Production totals</h2>
            <dl className="totals">
              {Object.entries({
                Prints: stats.total_prints,
                Completed: stats.successful_prints,
                Failed: stats.failed_prints,
                Cancelled: stats.cancelled_prints,
                'Print hours': stats.total_print_time_hours,
                'Filament (g)': stats.total_filament_grams,
                'Energy (kWh)': stats.total_energy_kwh,
              }).map(([k, v]) => (
                <div key={k}>
                  <dt>{k}</dt>
                  <dd>{v.toLocaleString()}</dd>
                </div>
              ))}
            </dl>
            {Object.keys(stats.prints_by_filament_type).length > 0 && (
              <>
                <h2>Prints by filament</h2>
                <dl>
                  {Object.entries(stats.prints_by_filament_type).map(
                    ([material, count]) => (
                      <div key={material}>
                        <dt>{material}</dt>
                        <dd>{count}</dd>
                      </div>
                    ),
                  )}
                </dl>
              </>
            )}
          </>
        ) : (
          !error && <p role="status">Loading statistics…</p>
        )}
      </section>
    </>
  );
}
