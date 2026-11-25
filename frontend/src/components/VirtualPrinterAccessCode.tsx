import { useId, useState, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api, multiVirtualPrinterApi } from '../api/client';
import { useAuth } from '../contexts/AuthContext';
import { useToast } from '../contexts/ToastContext';
import { Button } from './Button';
import { AlertTriangle, Check, Eye, EyeOff, Info, Loader2 } from './icons/registry';

interface AccessCodeFieldProps {
  value: string;
  onChange: (code: string) => void;
  hasTarget: boolean;
  codeSet?: boolean;
  disabled?: boolean;
  children?: ReactNode;
}

export function VirtualPrinterAccessCodeField({ value, onChange, hasTarget, codeSet, disabled, children }: AccessCodeFieldProps) {
  const { t } = useTranslation();
  const { hasPermission } = useAuth();
  const id = useId();
  const [revealed, setRevealed] = useState(false);
  const editable = hasPermission('settings:update') && (!hasTarget || hasPermission('printers:update'));
  const showDraft = revealed && value.length > 0;
  const placeholder = hasTarget && codeSet === undefined ? t('printers.accessCodePlaceholder') : t('virtualPrinter.accessCode.placeholder');

  return (
    <div>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 mb-2">
        <label htmlFor={editable ? id : undefined} className="text-white text-sm font-medium">{t('virtualPrinter.accessCode.title')}</label>
        {hasTarget && codeSet !== false ? (
          <span className="flex items-center gap-1 text-xs text-blue-700 dark:text-blue-400">
            <Info className="w-3 h-3" />{t('virtualPrinter.accessCode.inheritedFromTarget')}
          </span>
        ) : codeSet === undefined ? (
          <span className="text-xs text-bambu-gray">{t('common.optional')}</span>
        ) : codeSet ? (
          <span className="flex items-center gap-1 text-xs text-green-700 dark:text-green-400">
            <Check className="w-3 h-3" />{t('virtualPrinter.accessCode.isSet')}
          </span>
        ) : (
          <span className="flex items-center gap-1 text-xs text-yellow-700 dark:text-yellow-400">
            <AlertTriangle className="w-3 h-3" />{t('virtualPrinter.accessCode.notSet')}
          </span>
        )}
      </div>
      {editable && (
        <div className="flex gap-2">
          <div className="relative min-w-0 flex-1">
            <input
              id={id}
              type={showDraft ? 'text' : 'password'}
              value={value}
              onChange={(e) => {
                if (!value) setRevealed(false);
                onChange(e.target.value);
              }}
              placeholder={codeSet ? '••••••••' : placeholder}
              aria-describedby={hasTarget ? `${id}-hint` : undefined}
              maxLength={8}
              autoComplete="new-password"
              autoCapitalize="none"
              spellCheck={false}
              disabled={disabled}
              className={`w-full bg-bambu-dark-secondary border border-bambu-dark-tertiary rounded-md px-3 py-1.5 text-white text-sm font-mono placeholder-bambu-gray ${value ? 'pr-10' : ''}`}
            />
            {value && (
              <button
                type="button"
                aria-label={t(showDraft ? 'virtualPrinter.accessCode.hide' : 'virtualPrinter.accessCode.reveal')}
                onClick={() => setRevealed(!revealed)}
                disabled={disabled}
                className="absolute right-2 top-1/2 -translate-y-1/2 text-bambu-gray hover:text-white disabled:opacity-50"
              >
                {showDraft ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
              </button>
            )}
          </div>
          {children}
        </div>
      )}
      {hasTarget && (
        <p id={`${id}-hint`} className="text-xs text-bambu-gray mt-1">
          {t(editable ? 'virtualPrinter.accessCode.derivedFromTargetHint' : 'printers.permission.noEdit')}
        </p>
      )}
      {value && editable && (
        <p className="text-xs text-bambu-gray mt-1">
          {t('virtualPrinter.accessCode.charCount', { count: value.length })}
        </p>
      )}
    </div>
  );
}

export function VirtualPrinterAccessCodeEditor({ printerId, targetId, codeSet, disabled }: {
  printerId: number;
  targetId: number | null;
  codeSet: boolean;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const queryClient = useQueryClient();
  const [code, setCode] = useState('');
  const save = useMutation({
    mutationFn: async () => {
      if (targetId !== null) {
        await api.updatePrinter(targetId, { access_code: code });
      } else {
        await multiVirtualPrinterApi.update(printerId, { access_code: code });
      }
    },
    onSuccess: () => {
      setCode('');
      if (targetId !== null) queryClient.invalidateQueries({ queryKey: ['printers'] });
      queryClient.invalidateQueries({ queryKey: ['virtual-printers'] });
      showToast(t('virtualPrinter.toast.updated'));
    },
    onError: (err: Error) => showToast(err.message || t('virtualPrinter.toast.failedToUpdate'), 'error'),
  });
  const canSave = code.length === 8 && !disabled && !save.isPending;

  return (
    <form onSubmit={(e) => {
      e.preventDefault();
      if (canSave) save.mutate();
    }}>
      <VirtualPrinterAccessCodeField value={code} onChange={setCode} hasTarget={targetId !== null} codeSet={codeSet} disabled={disabled || save.isPending}>
        <Button type="submit" disabled={!canSave}>
          {save.isPending ? <Loader2 className="w-4 h-4 animate-spin" /> : t('common.save')}
        </Button>
      </VirtualPrinterAccessCodeField>
    </form>
  );
}
