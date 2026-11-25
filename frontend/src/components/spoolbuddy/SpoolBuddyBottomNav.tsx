import { Home as OBHome, Layers as OBLayers, Package as OBPackage, Radio as OBRadio, Settings as OBSettings } from '../icons/registry';
import { NavLink } from 'react-router-dom';
import { useTranslation } from 'react-i18next';

const navItems = [
  {
    to: '/spoolbuddy',
    labelKey: 'spoolbuddy.nav.dashboard',
    fallback: 'Dashboard',
    icon: (
      <OBHome className="w-6 h-6" />
    ),
  },
  {
    to: '/spoolbuddy/ams',
    labelKey: 'spoolbuddy.nav.ams',
    fallback: 'AMS',
    icon: (
      <OBLayers className="w-6 h-6" />
    ),
  },
  {
    to: '/spoolbuddy/write-tag',
    labelKey: 'spoolbuddy.nav.writeTag',
    fallback: 'Write',
    icon: (
      <OBRadio className="w-6 h-6" />
    ),
  },
  {
    to: '/spoolbuddy/inventory',
    labelKey: 'spoolbuddy.nav.inventory',
    fallback: 'Inventory',
    icon: (
      <OBPackage className="w-6 h-6" />
    ),
  },
  {
    to: '/spoolbuddy/settings',
    labelKey: 'spoolbuddy.nav.settings',
    fallback: 'Settings',
    icon: (
      <OBSettings className="w-6 h-6" />
    ),
  },
];

export function SpoolBuddyBottomNav() {
  const { t } = useTranslation();

  return (
    <nav className="h-14 bg-bambu-dark-secondary border-t border-bambu-dark-tertiary flex items-stretch shrink-0">
      {navItems.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.to === '/spoolbuddy'}
          className={({ isActive }) =>
            `flex-1 flex flex-col items-center justify-center gap-1 transition-colors ${
              isActive
                ? 'text-bambu-green bg-bambu-dark'
                : 'text-white/50 hover:text-white/70 hover:bg-bambu-dark-tertiary'
            }`
          }
        >
          {item.icon}
          <span className="text-xs font-medium">{t(item.labelKey, item.fallback)}</span>
        </NavLink>
      ))}
    </nav>
  );
}
