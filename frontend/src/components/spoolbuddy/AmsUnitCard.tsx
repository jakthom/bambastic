import { Droplet as OBDroplet, Thermometer as OBThermometer } from '../icons/registry';
import { useTranslation } from 'react-i18next';
import type { AMSUnit, AMSTray } from '../../api/client';
import { amsUnitSlotCount, getEmptySlotKind, getFillBarColor, isAms3hUnit, spoolBuddyAmsName } from '../../utils/amsHelpers';

function trayColorToCSS(color: string | null): string {
  if (!color) return '#808080';
  return `#${color.slice(0, 6)}`;
}

function isTrayEmpty(tray: AMSTray): boolean {
  return !tray.tray_type || tray.tray_type === '';
}

// --- SVG Icons (matching PrintersPage Bambu Lab style) ---

function WaterDropEmpty({ className }: { className?: string }) {
  return (
    <OBDroplet className={className} />
  );
}

function WaterDropHalf({ className }: { className?: string }) {
  return (
    <OBDroplet className={className} />
  );
}

function WaterDropFull({ className }: { className?: string }) {
  return (
    <OBDroplet className={className} />
  );
}

function ThermometerEmpty({ className }: { className?: string }) {
  return (
    <OBThermometer className={className} />
  );
}

function ThermometerHalf({ className }: { className?: string }) {
  return (
    <OBThermometer className={className} />
  );
}

function ThermometerFull({ className }: { className?: string }) {
  return (
    <OBThermometer className={className} />
  );
}

// --- Threshold-colored indicators ---

function HumidityIndicator({ humidity, goodThreshold = 40, fairThreshold = 60 }: { humidity: number; goodThreshold?: number; fairThreshold?: number }) {
  let textColor: string;
  let DropComponent: React.FC<{ className?: string }>;

  if (humidity <= goodThreshold) {
    textColor = '#22a352';
    DropComponent = WaterDropEmpty;
  } else if (humidity <= fairThreshold) {
    textColor = '#d4a017';
    DropComponent = WaterDropHalf;
  } else {
    textColor = '#c62828';
    DropComponent = WaterDropFull;
  }

  return (
    <div className="flex items-center gap-0.5">
      <DropComponent className="w-3 h-3.5" />
      <span className="font-medium tabular-nums text-xs" style={{ color: textColor }}>{humidity}%</span>
    </div>
  );
}

function TemperatureIndicator({ temp, goodThreshold = 28, fairThreshold = 35 }: { temp: number; goodThreshold?: number; fairThreshold?: number }) {
  let textColor: string;
  let ThermoComponent: React.FC<{ className?: string }>;

  if (temp <= goodThreshold) {
    textColor = '#22a352';
    ThermoComponent = ThermometerEmpty;
  } else if (temp <= fairThreshold) {
    textColor = '#d4a017';
    ThermoComponent = ThermometerHalf;
  } else {
    textColor = '#c62828';
    ThermoComponent = ThermometerFull;
  }

  return (
    <div className="flex items-center gap-0.5">
      <ThermoComponent className="w-3 h-3.5" />
      <span className="font-medium tabular-nums text-xs" style={{ color: textColor }}>{temp}°C</span>
    </div>
  );
}

// --- Nozzle badge ---

function NozzleBadge({ side }: { side: 'L' | 'R' }) {
  return (
    <span
      className="inline-flex items-center justify-center w-4 h-4 text-[9px] font-bold rounded"
      style={{ backgroundColor: '#1a4d2e', color: '#00ae42' }}
    >
      {side}
    </span>
  );
}

// --- Components ---

interface SpoolSlotProps {
  tray: AMSTray;
  slotIndex: number;
  isActive: boolean;
  fillOverride?: number | null;
  spoolmanFill?: number | null;
  accessibleLabel?: string;
  onClick?: () => void;
}

function SpoolSlot({ tray, slotIndex, isActive, fillOverride, spoolmanFill, accessibleLabel, onClick }: SpoolSlotProps) {
  const isEmpty = isTrayEmpty(tray);
  const emptyKind = getEmptySlotKind(tray);
  const color = trayColorToCSS(tray.tray_color);
  const amsFill = tray.remain !== null && tray.remain !== undefined && tray.remain >= 0 ? tray.remain : null;
  // If inventory says 0% but AMS reports positive remain, prefer AMS (#676)
  const resolvedOverride = (fillOverride === 0 && amsFill !== null && amsFill > 0) ? null : fillOverride;
  // Fill level fallback chain: Spoolman → Inventory → AMS remain
  const effectiveFill = spoolmanFill ?? resolvedOverride ?? amsFill;

  return (
    <div
      className={`relative flex flex-col items-center p-2.5 rounded-lg border transition-all ${
        isActive
          ? 'ring-2 ring-bambu-green border-bambu-green bg-bambu-green/10'
          : 'border-transparent'
      } ${onClick ? 'cursor-pointer hover:border-bambu-gray hover:bg-bambu-dark-tertiary' : ''}`}
      onClick={onClick}
      onKeyDown={onClick ? (event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          onClick();
        }
      } : undefined}
      role={onClick ? 'button' : undefined}
      tabIndex={onClick ? 0 : undefined}
      aria-pressed={onClick ? isActive : undefined}
      aria-label={onClick ? accessibleLabel : undefined}
      title={onClick ? accessibleLabel : undefined}
      data-testid={onClick ? 'ams-slot' : undefined}
    >
      {/* Spool visualization */}
      <div className="relative w-16 h-16 mb-1">
        {isEmpty ? (
          <div className="w-full h-full rounded-full border-2 border-dashed border-gray-500 flex items-center justify-center">
            <div className="w-3 h-3 rounded-full bg-gray-600" />
          </div>
        ) : (
          <svg viewBox="0 0 56 56" className="w-full h-full">
            <circle cx="28" cy="28" r="26" fill={color} />
            <circle cx="28" cy="28" r="20" fill={color} style={{ filter: 'brightness(0.85)' }} />
            <ellipse cx="20" cy="20" rx="6" ry="4" fill="white" opacity="0.3" />
            <circle cx="28" cy="28" r="8" fill="#2d2d2d" />
            <circle cx="28" cy="28" r="5" fill="#1a1a1a" />
          </svg>
        )}
        {isActive && (
          <div className="absolute -bottom-1 left-1/2 -translate-x-1/2 w-2.5 h-2.5 bg-bambu-green rounded-full" />
        )}
      </div>

      {/* Material type */}
      <span
        className="text-sm text-bambu-gray-light truncate max-w-full"
        title={emptyKind === 'reset' ? 'Spool loaded — slot not configured' : undefined}
      >
        {isEmpty ? (emptyKind === 'reset' ? '?' : 'Empty') : tray.tray_type || 'Unknown'}
      </span>

      {/* Fill level bar */}
      {!isEmpty && effectiveFill !== null && effectiveFill >= 0 && (
        <div className="w-full h-1 bg-bambu-dark-tertiary rounded-full overflow-hidden mt-1">
          <div
            className="h-full rounded-full transition-all"
            style={{
              width: `${effectiveFill}%`,
              backgroundColor: getFillBarColor(effectiveFill),
            }}
          />
        </div>
      )}

      {/* Slot number */}
      <span className="absolute top-1 right-1 rounded bg-bambu-dark-tertiary px-1.5 py-0.5 text-[10px] font-semibold text-bambu-gray-light">
        {slotIndex + 1}
      </span>
    </div>
  );
}

export interface AmsThresholds {
  humidityGood: number;
  humidityFair: number;
  tempGood: number;
  tempFair: number;
}

interface AmsUnitCardProps {
  unit: AMSUnit;
  activeSlot: number | null;
  onConfigureSlot?: (amsId: number, trayId: number, tray: AMSTray | null) => void;
  isDualNozzle?: boolean;
  nozzleSide?: 'L' | 'R' | null;
  thresholds?: AmsThresholds;
  fillOverrides?: Record<string, number>;
  spoolmanFillOverrides?: Record<string, number>;
}

export function AmsUnitCard({ unit, activeSlot, onConfigureSlot, isDualNozzle, nozzleSide, thresholds, fillOverrides, spoolmanFillOverrides }: AmsUnitCardProps) {
  const { t } = useTranslation();
  const trays = unit.tray || [];
  const isHt = unit.is_ams_ht;
  const isAms3h = isAms3hUnit(unit);
  const slotCount = isHt ? 1 : amsUnitSlotCount(unit);
  const amsName = spoolBuddyAmsName(unit.id, unit.ams_type);

  return (
    <div className="bg-bambu-dark-secondary rounded-xl border border-[var(--border-color)] p-3 shadow-sm">
      {/* Header */}
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-1.5">
          <span className="text-white font-medium text-base">{amsName}</span>
          {isDualNozzle && nozzleSide && (
            <NozzleBadge side={nozzleSide} />
          )}
        </div>
        <div className="flex items-center gap-2">
          {unit.temp != null && (
            <TemperatureIndicator
              temp={unit.temp}
              goodThreshold={thresholds?.tempGood}
              fairThreshold={thresholds?.tempFair}
            />
          )}
          {unit.humidity != null && (
            <HumidityIndicator
              humidity={unit.humidity}
              goodThreshold={thresholds?.humidityGood}
              fairThreshold={thresholds?.humidityFair}
            />
          )}
        </div>
      </div>

      {/* Slots grid */}
      <div className={`grid ${isHt ? 'grid-cols-1 max-w-[100px] mx-auto' : isAms3h ? 'grid-cols-5 md:grid-cols-9' : 'grid-cols-4'} gap-2`}>
        {Array.from({ length: slotCount }).map((_, i) => {
          const tray = trays[i] || {
            id: i,
            tray_color: null,
            tray_type: '',
            tray_sub_brands: null,
            tray_id_name: null,
            tray_info_idx: null,
            remain: -1,
            k: null,
            cali_idx: null,
            tag_uid: null,
            tray_uuid: null,
            nozzle_temp_min: null,
            nozzle_temp_max: null,
          };
          return (
            <SpoolSlot
              key={i}
              tray={tray}
              slotIndex={i}
              isActive={activeSlot === i}
              fillOverride={fillOverrides?.[`${unit.id}-${i}`] ?? null}
              spoolmanFill={spoolmanFillOverrides?.[`${unit.id}-${i}`] ?? null}
              accessibleLabel={t('configureAmsSlot.slotLabel', { ams: amsName, slot: i + 1 })}
              onClick={onConfigureSlot ? () => onConfigureSlot(unit.id, i, isTrayEmpty(tray) ? null : tray) : undefined}
            />
          );
        })}
      </div>
    </div>
  );
}

// Exported for use in SpoolBuddyAmsPage compact cards
export { HumidityIndicator, TemperatureIndicator, NozzleBadge };
