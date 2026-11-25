import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Loader2, ChevronDown, ArrowRightLeft } from './icons/registry';
import { api, multiVirtualPrinterApi } from '../api/client';
import { Card, CardContent } from './Card';
import { Button } from './Button';
import { useToast } from '../contexts/ToastContext';
import { VirtualPrinterAccessCodeField } from './VirtualPrinterAccessCode';

type Mode = 'archive' | 'review' | 'queue' | 'proxy';

const MODE_LABELS: Record<string, string> = {
  archive: 'archive',
  review: 'review',
  queue: 'queue',
  proxy: 'proxy',
};

interface VirtualPrinterAddDialogProps {
  onClose: () => void;
}

export function VirtualPrinterAddDialog({ onClose }: VirtualPrinterAddDialogProps) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const { showToast } = useToast();

  const [name, setName] = useState('');
  const [mode, setMode] = useState<Mode>('archive');
  const [targetPrinterId, setTargetPrinterId] = useState<number | null>(null);
  const [accessCode, setAccessCode] = useState('');

  const { data: printers } = useQuery({
    queryKey: ['printers'],
    queryFn: api.getPrinters,
  });

  const createMutation = useMutation({
    mutationFn: () =>
      multiVirtualPrinterApi.create({
        name: name.trim() || 'Bambuddy',
        mode,
        target_printer_id: targetPrinterId ?? undefined,
        access_code: targetPrinterId === null && accessCode ? accessCode : undefined,
        target_access_code: targetPrinterId !== null && accessCode ? accessCode : undefined,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['virtual-printers'] });
      if (targetPrinterId !== null && accessCode) queryClient.invalidateQueries({ queryKey: ['printers'] });
      showToast(t('virtualPrinter.toast.created'));
      onClose();
    },
    onError: (error: Error) => {
      showToast(error.message || t('virtualPrinter.toast.failedToCreate'), 'error');
    },
  });

  const canCreate = !createMutation.isPending && (mode !== 'proxy' || targetPrinterId !== null)
    && (!accessCode || accessCode.length === 8);

  return (
    <div
      className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4"
      onClick={() => { if (!createMutation.isPending) onClose(); }}
    >
      <Card
        role="dialog"
        aria-modal="true"
        aria-labelledby="vp-add-title"
        className="w-full max-w-md max-h-full overflow-y-auto"
        onClick={(e: React.MouseEvent) => e.stopPropagation()}
      >
        <CardContent className="p-6">
          <form className="space-y-4" onSubmit={(e) => {
            e.preventDefault();
            if (canCreate) createMutation.mutate();
          }}>
            <h3 id="vp-add-title" className="text-lg font-semibold text-white">{t('virtualPrinter.addDialog.title')}</h3>

            {/* Name */}
            <div>
              <label htmlFor="vp-add-name" className="text-sm text-white font-medium block mb-1">{t('virtualPrinter.addDialog.name')}</label>
              <input
                id="vp-add-name"
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Bambuddy"
                className="w-full bg-bambu-dark-secondary border border-bambu-dark-tertiary rounded-md px-3 py-2 text-white text-sm placeholder-bambu-gray"
                disabled={createMutation.isPending}
                autoFocus
              />
            </div>

            {/* Mode */}
            <div>
              <label className="text-sm text-white font-medium block mb-1">{t('virtualPrinter.mode.title')}</label>
              <div className="grid grid-cols-2 gap-2">
                {(['archive', 'review', 'queue', 'proxy'] as const).map((m) => (
                  <button
                    key={m}
                    type="button"
                    aria-pressed={mode === m}
                    disabled={createMutation.isPending}
                    onClick={() => {
                      if (m === 'proxy' && targetPrinterId === null) setAccessCode('');
                      setMode(m);
                    }}
                    className={`p-2 rounded-lg border text-left transition-colors ${
                      mode === m
                        ? m === 'proxy'
                          ? 'border-blue-500 bg-blue-500/10'
                          : 'border-bambu-green bg-bambu-green/10'
                        : 'border-bambu-dark-tertiary hover:border-bambu-gray'
                    }`}
                  >
                    <div className="flex items-center gap-1.5 text-white text-xs font-medium">
                      {m === 'proxy' && <ArrowRightLeft className="w-3 h-3" />}
                      {t(`virtualPrinter.mode.${MODE_LABELS[m]}`)}
                    </div>
                    <div className="text-[10px] text-bambu-gray">
                      {t(`virtualPrinter.mode.${MODE_LABELS[m]}Desc`)}
                    </div>
                  </button>
                ))}
              </div>
            </div>

            <div>
              <label htmlFor="vp-add-target" className="text-sm text-white font-medium block mb-1">
                {t('virtualPrinter.targetPrinter.title')}
                <span className="ml-2 text-xs font-normal text-bambu-gray">{t(mode === 'proxy' ? 'common.required' : 'common.optional')}</span>
              </label>
              <div className="relative">
                <select
                  id="vp-add-target"
                  value={targetPrinterId ?? ''}
                  disabled={createMutation.isPending}
                  onChange={(e) => {
                    setTargetPrinterId(e.target.value ? Number(e.target.value) : null);
                    setAccessCode('');
                  }}
                  className="w-full bg-bambu-dark-secondary border border-bambu-dark-tertiary rounded-md px-3 py-2 text-white text-sm appearance-none cursor-pointer pr-10"
                >
                  <option value="">{t('virtualPrinter.targetPrinter.placeholder')}</option>
                  {printers?.map((p) => (
                    <option key={p.id} value={p.id}>{p.name} ({p.ip_address})</option>
                  ))}
                </select>
                <ChevronDown className="absolute right-3 top-1/2 -translate-y-1/2 w-4 h-4 text-bambu-gray pointer-events-none" />
              </div>
            </div>

            {(mode !== 'proxy' || targetPrinterId !== null) && (
              <VirtualPrinterAccessCodeField
                key={targetPrinterId ?? 'standalone'}
                value={accessCode}
                onChange={setAccessCode}
                hasTarget={targetPrinterId !== null}
                disabled={createMutation.isPending}
              />
            )}

            <p className="text-xs text-bambu-gray">
              {t('virtualPrinter.addDialog.hint')}
            </p>

            {/* Actions */}
            <div className="flex gap-3 pt-2">
              <Button type="button" variant="secondary" onClick={onClose} className="flex-1" disabled={createMutation.isPending}>
                {t('common.cancel')}
              </Button>
              <Button
                variant="primary"
                type="submit"
                className="flex-1"
                disabled={!canCreate}
              >
                {createMutation.isPending ? (
                  <Loader2 className="w-4 h-4 animate-spin" />
                ) : (
                  t('virtualPrinter.addDialog.create')
                )}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
