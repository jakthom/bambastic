/**
 * Tests for the VirtualPrinterCard component.
 *
 * Tests the auto-dispatch toggle behavior:
 * - Visibility based on mode (print_queue only)
 * - Default state (on)
 * - API mutation on toggle click
 */

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { render } from '../utils';
import { VirtualPrinterCard } from '../../components/VirtualPrinterCard';
import { VirtualPrinterAddDialog } from '../../components/VirtualPrinterAddDialog';
import type { VirtualPrinterConfig } from '../../api/client';

// Mock the API client
vi.mock('../../api/client', () => ({
  multiVirtualPrinterApi: {
    create: vi.fn(),
    update: vi.fn().mockResolvedValue({}),
    remove: vi.fn().mockResolvedValue({}),
    getTailscaleStatus: vi.fn().mockResolvedValue({
      available: false,
      fqdn: '',
      hostname: '',
      tailnet_name: '',
      tailscale_ips: [],
      error: null,
    }),
  },
  api: {
    getSettings: vi.fn().mockResolvedValue({}),
    getPrinters: vi.fn().mockResolvedValue([]),
    updatePrinter: vi.fn(),
    getNetworkInterfaces: vi.fn().mockResolvedValue({ interfaces: [] }),
  },
}));

import { multiVirtualPrinterApi, api } from '../../api/client';

const models: Record<string, string> = {
  'BL-P001': 'X1C',
  'C12': 'P1S',
};

const createMockPrinter = (overrides: Partial<VirtualPrinterConfig> = {}): VirtualPrinterConfig => ({
  id: 1,
  name: 'Test VP',
  enabled: false,
  mode: 'archive',
  model: 'BL-P001',
  model_name: 'X1C',
  access_code_set: false,
  serial: '00M00A391800001',
  target_printer_id: null,
  auto_dispatch: true,
  queue_force_color_match: false,
  save_ams_mapping: false,
  gcode_injection: false,
  tailscale_disabled: true,
  bind_ip: null,
  remote_interface_ip: null,
  position: 0,
  status: { running: false, pending_files: 0 },
  ...overrides,
});

describe('VirtualPrinterCard - auto-dispatch toggle', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(createMockPrinter());
  });

  it('renders auto-dispatch toggle when mode is print_queue', async () => {
    const printer = createMockPrinter({ mode: 'queue' });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Auto-dispatch')).toBeInTheDocument();
    });
  });

  it('does not render auto-dispatch toggle when mode is immediate', async () => {
    const printer = createMockPrinter({ mode: 'archive' });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    // Wait for the card to render fully (check for something that should be there)
    await waitFor(() => {
      expect(screen.getByText('Test VP')).toBeInTheDocument();
    });

    expect(screen.queryByText('Auto-dispatch')).not.toBeInTheDocument();
  });

  it('does not render auto-dispatch toggle when mode is proxy', async () => {
    const printer = createMockPrinter({ mode: 'proxy' });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Test VP')).toBeInTheDocument();
    });

    expect(screen.queryByText('Auto-dispatch')).not.toBeInTheDocument();
  });

  it('auto-dispatch toggle defaults to on', async () => {
    const printer = createMockPrinter({ mode: 'queue', auto_dispatch: true });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Auto-dispatch')).toBeInTheDocument();
    });

    // The auto-dispatch section container has the toggle button as a sibling of the text div
    const title = screen.getByText('Auto-dispatch');
    const section = title.closest('.flex.items-center.justify-between');
    expect(section).toBeTruthy();
    const toggleButton = section!.querySelector('button');
    expect(toggleButton).toBeTruthy();
    expect(toggleButton!.className).toContain('bg-bambu-green');
  });

  it('clicking auto-dispatch toggle calls update API', async () => {
    const user = userEvent.setup();
    const printer = createMockPrinter({ mode: 'queue', auto_dispatch: true });
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(
      createMockPrinter({ mode: 'queue', auto_dispatch: false })
    );

    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Auto-dispatch')).toBeInTheDocument();
    });

    // Find the auto-dispatch toggle via the section container
    const title = screen.getByText('Auto-dispatch');
    const section = title.closest('.flex.items-center.justify-between');
    expect(section).toBeTruthy();
    const toggleButton = section!.querySelector('button');
    expect(toggleButton).toBeTruthy();

    await user.click(toggleButton!);

    await waitFor(() => {
      expect(multiVirtualPrinterApi.update).toHaveBeenCalledWith(1, { auto_dispatch: false });
    });
  });
});

// #1188 — VP queue mode now pins per-slot type+color so the scheduler refuses
// to dispatch onto a printer with the wrong filament loaded. The toggle is
// mode-gated to print_queue (mirroring the auto-dispatch toggle), defaults
// off (preserves pre-fix behaviour for upgraders), and the click both flips
// the local state and POSTs the new value to the backend.
describe('VirtualPrinterCard - force color match toggle (#1188)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(createMockPrinter());
  });

  it('renders force-color-match toggle when mode is print_queue', async () => {
    const printer = createMockPrinter({ mode: 'queue' });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Force color match')).toBeInTheDocument();
    });
  });

  it('does not render force-color-match toggle when mode is immediate', async () => {
    const printer = createMockPrinter({ mode: 'archive' });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Test VP')).toBeInTheDocument();
    });
    expect(screen.queryByText('Force color match')).not.toBeInTheDocument();
  });

  it('does not render force-color-match toggle when mode is proxy', async () => {
    const printer = createMockPrinter({ mode: 'proxy' });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Test VP')).toBeInTheDocument();
    });
    expect(screen.queryByText('Force color match')).not.toBeInTheDocument();
  });

  it('force-color-match toggle defaults off (not green) — preserves pre-fix behaviour', async () => {
    const printer = createMockPrinter({ mode: 'queue', queue_force_color_match: false });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Force color match')).toBeInTheDocument();
    });

    const title = screen.getByText('Force color match');
    const section = title.closest('.flex.items-center.justify-between');
    expect(section).toBeTruthy();
    const toggleButton = section!.querySelector('button');
    expect(toggleButton).toBeTruthy();
    expect(toggleButton!.className).not.toContain('bg-bambu-green');
  });

  it('force-color-match toggle renders enabled (green) when queue_force_color_match is true', async () => {
    const printer = createMockPrinter({ mode: 'queue', queue_force_color_match: true });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Force color match')).toBeInTheDocument();
    });

    const title = screen.getByText('Force color match');
    const section = title.closest('.flex.items-center.justify-between');
    const toggleButton = section!.querySelector('button');
    expect(toggleButton!.className).toContain('bg-bambu-green');
  });

  it('clicking force-color-match toggle posts queue_force_color_match in update body', async () => {
    const user = userEvent.setup();
    const printer = createMockPrinter({ mode: 'queue', queue_force_color_match: false });
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(
      createMockPrinter({ mode: 'queue', queue_force_color_match: true })
    );

    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Force color match')).toBeInTheDocument();
    });

    const title = screen.getByText('Force color match');
    const section = title.closest('.flex.items-center.justify-between');
    const toggleButton = section!.querySelector('button');

    await user.click(toggleButton!);

    await waitFor(() => {
      expect(multiVirtualPrinterApi.update).toHaveBeenCalledWith(1, { queue_force_color_match: true });
    });
  });
});

describe('VirtualPrinterCard - tailscale toggle', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(createMockPrinter());
  });

  it('renders tailscale toggle as enabled (green) when tailscale_disabled is false', async () => {
    const printer = createMockPrinter({ tailscale_disabled: false });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Tailscale integration')).toBeInTheDocument();
    });

    const title = screen.getByText('Tailscale integration');
    const section = title.closest('.flex.items-center.justify-between');
    expect(section).toBeTruthy();
    const toggleButton = section!.querySelector('button');
    expect(toggleButton).toBeTruthy();
    expect(toggleButton!.className).toContain('bg-bambu-green');
  });

  it('renders tailscale toggle as disabled (not green) when tailscale_disabled is true', async () => {
    const printer = createMockPrinter({ tailscale_disabled: true });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Tailscale integration')).toBeInTheDocument();
    });

    const title = screen.getByText('Tailscale integration');
    const section = title.closest('.flex.items-center.justify-between');
    expect(section).toBeTruthy();
    const toggleButton = section!.querySelector('button');
    expect(toggleButton).toBeTruthy();
    expect(toggleButton!.className).not.toContain('bg-bambu-green');
  });

  it('clicking tailscale toggle calls update API with tailscale_disabled: true', async () => {
    const user = userEvent.setup();
    const printer = createMockPrinter({ tailscale_disabled: false });
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(
      createMockPrinter({ tailscale_disabled: true })
    );

    render(<VirtualPrinterCard printer={printer} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('Tailscale integration')).toBeInTheDocument();
    });

    const title = screen.getByText('Tailscale integration');
    const section = title.closest('.flex.items-center.justify-between');
    expect(section).toBeTruthy();
    const toggleButton = section!.querySelector('button');
    expect(toggleButton).toBeTruthy();

    await user.click(toggleButton!);

    await waitFor(() => {
      expect(multiVirtualPrinterApi.update).toHaveBeenCalledWith(1, { tailscale_disabled: true });
    });
  });

});

describe('VirtualPrinterCard - Tailscale FQDN copy', () => {
  const fqdn = 'test-host.tail1234.ts.net';

  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(createMockPrinter());
    // FQDN now comes from the host-level Tailscale status endpoint, not VP status.
    // Tests in this block need the toggle to be ON (tailscale_disabled=false) so the
    // useQuery actually fires and the FQDN row renders.
    vi.mocked(multiVirtualPrinterApi.getTailscaleStatus).mockResolvedValue({
      available: true,
      fqdn,
      hostname: 'test-host',
      tailnet_name: 'tail1234.ts.net',
      tailscale_ips: ['100.64.0.1'],
      error: null,
    });
  });

  function getCopyButton() {
    // The copy button is a <button> with a title attribute. Use title to locate it.
    const candidates = screen.getAllByRole('button');
    return candidates.find(btn => /copy/i.test(btn.getAttribute('title') || '')) as HTMLButtonElement;
  }

  it('uses navigator.clipboard.writeText in a secure context', async () => {
    const user = userEvent.setup();
    const writeTextMock = vi.fn().mockResolvedValue(undefined);
    // JSDOM defaults isSecureContext to true; confirm and stub clipboard.
    Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true });
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: writeTextMock },
      configurable: true,
    });

    const printer = createMockPrinter({ tailscale_disabled: false });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    const copyBtn = await waitFor(() => {
      const btn = getCopyButton();
      if (!btn) throw new Error('copy button not yet rendered');
      return btn;
    });
    await user.click(copyBtn);

    await waitFor(() => {
      expect(writeTextMock).toHaveBeenCalledWith(fqdn);
    });
  });

  it('falls back to execCommand("copy") when clipboard API is unavailable (HTTP)', async () => {
    const user = userEvent.setup();
    // Simulate non-secure context: no clipboard API available.
    Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true });
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });

    const execCommandMock = vi.fn().mockReturnValue(true);
    document.execCommand = execCommandMock;

    const printer = createMockPrinter({ tailscale_disabled: false });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    const copyBtn = await waitFor(() => {
      const btn = getCopyButton();
      if (!btn) throw new Error('copy button not yet rendered');
      return btn;
    });
    await user.click(copyBtn);

    await waitFor(() => {
      expect(execCommandMock).toHaveBeenCalledWith('copy');
    });
    // Fallback path: textarea is appended, used, then removed in `finally`.
    // After the click resolves, no stray textareas should remain in the DOM.
    expect(document.querySelectorAll('textarea').length).toBe(0);
  });

  it('always cleans up the hidden textarea even if execCommand throws', async () => {
    const user = userEvent.setup();
    Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true });
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });

    document.execCommand = vi.fn().mockImplementation(() => {
      throw new Error('synthetic execCommand failure');
    });

    const printer = createMockPrinter({ tailscale_disabled: false });
    render(<VirtualPrinterCard printer={printer} models={models} />);

    const copyBtn = await waitFor(() => {
      const btn = getCopyButton();
      if (!btn) throw new Error('copy button not yet rendered');
      return btn;
    });
    await user.click(copyBtn);

    // The `finally` block must remove the textarea regardless of the exception.
    await waitFor(() => {
      expect(document.querySelectorAll('textarea').length).toBe(0);
    });
  });
});

describe('virtual printer access code setup', () => {
  // The printer API omits the stored LAN code even when the user can update it.
  const target = { id: 7, name: 'Workshop P1S', ip_address: '192.0.2.7', model: 'P1S' };

  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(api.getPrinters).mockResolvedValue([target] as Awaited<ReturnType<typeof api.getPrinters>>);
    vi.mocked(multiVirtualPrinterApi.create).mockResolvedValue(createMockPrinter());
  });

  it.each(['archive', 'review', 'queue', 'proxy'] as const)('saves the LAN code on the physical printer in %s mode', async (mode) => {
    const user = userEvent.setup();
    render(<VirtualPrinterCard printer={createMockPrinter({ mode, target_printer_id: target.id, access_code_set: true })} models={models} />);

    const input = screen.getByLabelText('Access Code');
    const save = screen.getByRole('button', { name: 'Save' });
    expect(input).toHaveValue('');
    expect(input).toHaveAttribute('type', 'password');
    expect(screen.getByText('Inherited from target')).toBeInTheDocument();
    await user.type(input, '01Ab');
    expect(save).toBeDisabled();
    await user.type(input, '2345');
    await user.click(screen.getByRole('button', { name: 'Show access code' }));
    expect(input).toHaveAttribute('type', 'text');
    await user.click(save);

    expect(api.updatePrinter).toHaveBeenCalledWith(target.id, { access_code: '01Ab2345' });
    expect(multiVirtualPrinterApi.update).not.toHaveBeenCalled();
    await waitFor(() => expect(input).toHaveValue(''));
    expect(input).toHaveAttribute('type', 'password');
  });

  it('keeps a rejected LAN code editable for retry', async () => {
    const user = userEvent.setup();
    vi.mocked(api.updatePrinter).mockRejectedValueOnce(new Error('Printer refused the LAN code'));
    render(<VirtualPrinterCard printer={createMockPrinter({ target_printer_id: target.id })} models={models} />);

    const input = screen.getByLabelText('Access Code');
    await user.type(input, 'wrong123{enter}');
    expect(await screen.findByText('Printer refused the LAN code')).toBeInTheDocument();
    expect(input).toHaveValue('wrong123');
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
  });

  it('saves a standalone code on the virtual printer', async () => {
    const user = userEvent.setup();
    render(<VirtualPrinterCard printer={createMockPrinter()} models={models} />);
    await user.type(screen.getByLabelText('Access Code'), 'vpCode01{enter}');
    expect(multiVirtualPrinterApi.update).toHaveBeenCalledWith(1, { access_code: 'vpCode01' });
    expect(api.updatePrinter).not.toHaveBeenCalled();
  });

  it('creates a virtual printer with an optional standalone code', async () => {
    const user = userEvent.setup();
    render(<VirtualPrinterAddDialog onClose={vi.fn()} />);
    const create = screen.getByRole('button', { name: 'Create' });
    expect(create).toBeEnabled();
    await user.type(screen.getByLabelText('Access Code'), '0A');
    expect(create).toBeDisabled();
    await user.type(screen.getByLabelText('Access Code'), 'b12345');
    await user.click(create);
    expect(multiVirtualPrinterApi.create).toHaveBeenCalledWith(expect.objectContaining({ access_code: '0Ab12345', target_access_code: undefined }));
  });

  it('clears the standalone draft before entering a physical LAN code during creation', async () => {
    const user = userEvent.setup();
    const close = vi.fn();
    render(<VirtualPrinterAddDialog onClose={close} />);
    await user.type(screen.getByLabelText('Access Code'), 'vpCode01');
    await user.selectOptions(screen.getByRole('combobox', { name: /Target Printer/ }), String(target.id));
    const input = screen.getByLabelText('Access Code');
    expect(input).toHaveValue('');
    expect(screen.getByRole('button', { name: 'Create' })).toBeEnabled();
    await user.type(input, '01Ab2345{enter}');
    expect(multiVirtualPrinterApi.create).toHaveBeenCalledWith(expect.objectContaining({ target_printer_id: 7, target_access_code: '01Ab2345', access_code: undefined }));
    await waitFor(() => expect(close).toHaveBeenCalledOnce());
  });

  it('requires a target in proxy mode and reuses its saved code when left blank', async () => {
    const user = userEvent.setup();
    render(<VirtualPrinterAddDialog onClose={vi.fn()} />);
    await user.type(screen.getByLabelText('Access Code'), 'vpCode01');
    await user.click(screen.getByRole('button', { name: /Proxy/ }));
    const create = screen.getByRole('button', { name: 'Create' });
    expect(create).toBeDisabled();
    await user.selectOptions(screen.getByRole('combobox', { name: /Target Printer/ }), String(target.id));
    expect(screen.getByLabelText('Access Code')).toHaveValue('');
    await user.click(create);
    expect(multiVirtualPrinterApi.create).toHaveBeenCalledWith(expect.objectContaining({ mode: 'proxy', target_printer_id: 7, target_access_code: undefined, access_code: undefined }));
  });
});

// The collapsed header used to be a single flex row of flex-shrink-0 items,
// so it was as wide as its contents and spilled past the card's border --
// the reporter of #2808 saw an IP address and the enable toggle floating on
// the page background. jsdom does no layout, so these pin the CSS contract
// that makes wrapping possible rather than the pixels: a flexible, wrappable
// metadata group, and no item that both refuses to shrink and claims it will
// truncate.
describe('VirtualPrinterCard - header does not overflow the card', () => {
  const printers = [
    {
      id: 9,
      // The name a printer adopted without one gets (discovery.py) -- 25
      // unbreakable characters, and half of why this overflowed.
      name: 'Printer at 192.168.30.210',
      ip_address: '192.168.30.210',
      access_code: 'TGTCODE1',
      serial_number: '01P00A391800001',
      model: 'H2C',
      is_active: true,
    },
  ];

  const twoVlanPrinter = () =>
    createMockPrinter({
      name: 'H2C',
      mode: 'proxy',
      model_name: 'H2C',
      target_printer_id: 9,
      // Both are populated only when Bambuddy and the printer sit on
      // different subnets -- the reporter's case.
      bind_ip: '192.168.20.175',
      remote_interface_ip: '192.168.30.210',
    });

  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(multiVirtualPrinterApi.update).mockResolvedValue(createMockPrinter());
    vi.mocked(api.getPrinters).mockResolvedValue(printers as unknown as Awaited<ReturnType<typeof api.getPrinters>>);
  });

  it('keeps both IPs visible instead of truncating them away', async () => {
    render(<VirtualPrinterCard printer={twoVlanPrinter()} models={models} />);

    await waitFor(() => {
      expect(screen.getByText('192.168.20.175')).toBeInTheDocument();
    });
    // An operator opens this page to check exactly these two values, so the
    // fix has to wrap them, not hide them.
    expect(screen.getByText('192.168.30.210')).toBeInTheDocument();
  });

  it('puts the metadata in a shrinkable, wrapping group', async () => {
    render(<VirtualPrinterCard printer={twoVlanPrinter()} models={models} />);

    const bindIp = await screen.findByText('192.168.20.175');
    const group = bindIp.parentElement as HTMLElement;

    expect(group.className).toContain('flex-wrap');
    // Without min-w-0 a flex item cannot shrink below its content, which is
    // what defeated the name's `truncate` before.
    expect(group.className).toContain('min-w-0');
    expect(group.className).toContain('flex-1');
  });

  it('has no header item that is both unshrinkable and truncating', async () => {
    render(<VirtualPrinterCard printer={twoVlanPrinter()} models={models} />);

    const bindIp = await screen.findByText('192.168.20.175');
    const group = bindIp.parentElement as HTMLElement;

    const selfCancelling = Array.from(group.children).filter(
      (el) => el.className.includes('flex-shrink-0') && el.className.includes('truncate')
    );
    expect(selfCancelling).toHaveLength(0);
  });

  it('clips at the card border as a backstop', async () => {
    const { container } = render(<VirtualPrinterCard printer={twoVlanPrinter()} models={models} />);

    await screen.findByText('192.168.20.175');
    // Scoped to this card on purpose: cards elsewhere render menus that paint
    // outside their own bounds, so this must not migrate into `Card`.
    const card = container.querySelector('.rounded-xl') as HTMLElement;
    expect(card.className).toContain('overflow-hidden');
  });
});
