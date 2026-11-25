import { useState, useEffect } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { X, Save, Loader2 } from './icons/registry';
import { useTranslation } from 'react-i18next';
import { api } from '../api/client';
import type { ExternalLink, ExternalLinkCreate, ExternalLinkUpdate } from '../api/client';
import { Button } from './Button';
import { IconPicker, getIconByName } from './IconPicker';
interface AddExternalLinkModalProps {
  link?: ExternalLink | null;
  onClose: () => void;
}

export function AddExternalLinkModal({ link, onClose }: AddExternalLinkModalProps) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const isEditing = !!link;

  const [name, setName] = useState(link?.name || '');
  const [url, setUrl] = useState(link?.url || '');
  const [icon, setIcon] = useState(link?.icon || 'link');
  const [openInNewTab, setOpenInNewTab] = useState(link?.open_in_new_tab || false);
  const [error, setError] = useState<string | null>(null);

  // Close on Escape key
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [onClose]);

  // Create mutation
  const createMutation = useMutation({
    mutationFn: (data: ExternalLinkCreate) => api.createExternalLink(data),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['external-links'] });
      onClose();
    },
    onError: (err: Error) => {
      setError(err.message);
    },
  });

  // Update mutation
  const updateMutation = useMutation({
    mutationFn: (data: ExternalLinkUpdate) => api.updateExternalLink(link!.id, data),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['external-links'] });
      onClose();
    },
    onError: (err: Error) => {
      setError(err.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);

    if (!name.trim()) {
      setError('Name is required');
      return;
    }

    if (!url.trim()) {
      setError('URL is required');
      return;
    }

    // Validate URL
    if (!url.startsWith('http://') && !url.startsWith('https://')) {
      setError('URL must start with http:// or https://');
      return;
    }

    const data = {
      name: name.trim(),
      url: url.trim(),
      icon,
      open_in_new_tab: openInNewTab,
    };

    if (isEditing) {
      updateMutation.mutate(data);
    } else {
      createMutation.mutate(data);
    }
  };

  const isPending = createMutation.isPending || updateMutation.isPending;
  const PresetIcon = getIconByName(icon);

  return (
    <div
      className="fixed inset-0 bg-black/70 flex items-center justify-center z-50 p-4"
      onClick={onClose}
    >
      <div
        className="bg-bambu-dark-secondary rounded-xl border border-bambu-dark-tertiary w-full max-w-md"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-6 py-4 border-b border-bambu-dark-tertiary">
          <div className="flex items-center gap-3">
            <div className="p-2 rounded-full bg-bambu-green/20 text-bambu-green">
              <PresetIcon className="w-5 h-5" />
            </div>
            <h2 className="text-lg font-semibold text-white">
              {isEditing ? 'Edit Link' : 'Add External Link'}
            </h2>
          </div>
          <button
            onClick={onClose}
            className="text-bambu-gray hover:text-white transition-colors"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Form */}
        <form onSubmit={handleSubmit} className="p-6 space-y-4">
          {error && (
            <div className="p-3 bg-red-100 dark:bg-red-500/20 border border-red-300 dark:border-red-500/50 rounded-lg text-sm text-red-700 dark:text-red-400">
              {error}
            </div>
          )}

          {/* Name */}
          <div>
            <label className="block text-sm text-bambu-gray mb-1">Name *</label>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="My Link"
              maxLength={50}
              className="w-full px-3 py-2 bg-bambu-dark border border-bambu-dark-tertiary rounded-lg text-white focus:border-bambu-green focus:outline-none"
            />
          </div>

          {/* URL */}
          <div>
            <label className="block text-sm text-bambu-gray mb-1">URL *</label>
            <input
              type="text"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com"
              className="w-full px-3 py-2 bg-bambu-dark border border-bambu-dark-tertiary rounded-lg text-white focus:border-bambu-green focus:outline-none"
            />
          </div>

          {/* Open in New Tab */}
          <div className="flex items-center justify-between">
            <label className="text-sm text-bambu-gray">{t('externalLinks.openInNewTab')}</label>
            <button
              type="button"
              onClick={() => setOpenInNewTab(!openInNewTab)}
              className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                openInNewTab ? 'bg-bambu-green' : 'bg-bambu-dark-tertiary'
              }`}
            >
              <span
                className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                  openInNewTab ? 'translate-x-6' : 'translate-x-1'
                }`}
              />
            </button>
          </div>

          {/* Icon Section */}
          <div className="space-y-3">
            <label className="block text-sm text-bambu-gray">Icon</label>

            <IconPicker value={icon} onChange={setIcon} />
          </div>

          {/* Actions */}
          <div className="flex gap-3 pt-2">
            <Button
              type="button"
              variant="secondary"
              onClick={onClose}
              className="flex-1"
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={isPending}
              className="flex-1"
            >
              {isPending ? (
                <Loader2 className="w-4 h-4 animate-spin" />
              ) : (
                <Save className="w-4 h-4" />
              )}
              {isEditing ? 'Save' : 'Add'}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
