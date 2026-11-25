import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { api } from '../api/client';
import { useAuth } from './AuthContext';

type ThemeMode = 'light' | 'dark' | 'system';
type ThemeStyle = 'classic' | 'glow' | 'vibrant';
type DarkBackground = 'neutral' | 'warm' | 'cool' | 'oled' | 'slate' | 'forest';
type LightBackground = 'neutral' | 'warm' | 'cool';
type ThemeAccent = 'green' | 'teal' | 'blue' | 'orange' | 'purple' | 'red';

const styles: readonly ThemeStyle[] = ['classic', 'glow', 'vibrant'];
const lightBackgrounds: readonly LightBackground[] = ['neutral', 'warm', 'cool'];
const darkBackgrounds: readonly DarkBackground[] = [...lightBackgrounds, 'oled', 'slate', 'forest'];
const accents: readonly ThemeAccent[] = ['green', 'teal', 'blue', 'orange', 'purple', 'red'];

function preference<T extends string>(choices: readonly T[], value: unknown, fallback: T): T {
  return choices.find(choice => choice === value) ?? fallback;
}

interface ThemeContextType {
  mode: ThemeMode;
  resolvedMode: 'light' | 'dark';
  // Dark mode settings
  darkStyle: ThemeStyle;
  darkBackground: DarkBackground;
  darkAccent: ThemeAccent;
  // Light mode settings
  lightStyle: ThemeStyle;
  lightBackground: LightBackground;
  lightAccent: ThemeAccent;
  // Show live print progress (% + accent-coloured ring favicon) in the browser tab
  progressInTitle: boolean;
  setProgressInTitle: (v: boolean) => void;
  tabDisplay: 'percentage' | 'time';
  setTabDisplay: (v: 'percentage' | 'time') => void;
  // Actions
  toggleMode: () => void;
  setMode: (mode: ThemeMode) => void;
  setDarkStyle: (style: ThemeStyle) => void;
  setDarkBackground: (background: DarkBackground) => void;
  setDarkAccent: (accent: ThemeAccent) => void;
  setLightStyle: (style: ThemeStyle) => void;
  setLightBackground: (background: LightBackground) => void;
  setLightAccent: (accent: ThemeAccent) => void;
}

const ThemeContext = createContext<ThemeContextType | undefined>(undefined);

export function ThemeProvider({ children }: { children: ReactNode }) {
  // Auth-aware: read state from AuthContext so the initial getSettings()
  // sync waits until we know whether (a) auth is enabled and (b) there
  // is a logged-in user. Without this the fetch fires on the login page
  // and returns 401 — harmless (the catch swallows it) but noisy in the
  // network panel and wasteful. ThemeProvider is mounted inside
  // AuthProvider in App.tsx specifically so this hook is callable.
  const { authEnabled, user, loading: authLoading, hasPermission } = useAuth();
  const readsAllSettings = hasPermission('settings:read');

  // Mode
  const [mode, setModeState] = useState<ThemeMode>(() => {
    for (const key of ['theme-mode', 'theme']) {
      const value = localStorage.getItem(key);
      if (value === 'light' || value === 'dark' || value === 'system') return value;
    }
    return 'light';
  });

  // System preference detection
  const [systemPreference, setSystemPreference] = useState<'light' | 'dark'>(() => {
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  });

  useEffect(() => {
    const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => {
      setSystemPreference(e.matches ? 'dark' : 'light');
    };
    mediaQuery.addEventListener('change', handler);
    return () => mediaQuery.removeEventListener('change', handler);
  }, []);

  // Resolved mode: what's actually applied (always 'light' or 'dark')
  const resolvedMode: 'light' | 'dark' = mode === 'system' ? systemPreference : mode;

  // Dark mode settings
  const [darkStyle, setDarkStyleState] = useState<ThemeStyle>(() => {
    return preference(styles, localStorage.getItem('dark-style'), 'classic');
  });
  const [darkBackground, setDarkBackgroundState] = useState<DarkBackground>(() => {
    return preference(darkBackgrounds, localStorage.getItem('dark-background'), 'cool');
  });
  const [darkAccent, setDarkAccentState] = useState<ThemeAccent>(() => {
    return preference(accents, localStorage.getItem('dark-accent'), 'blue');
  });

  // Light mode settings
  const [lightStyle, setLightStyleState] = useState<ThemeStyle>(() => {
    return preference(styles, localStorage.getItem('light-style'), 'classic');
  });
  const [lightBackground, setLightBackgroundState] = useState<LightBackground>(() => {
    return preference(lightBackgrounds, localStorage.getItem('light-background'), 'cool');
  });
  const [lightAccent, setLightAccentState] = useState<ThemeAccent>(() => {
    return preference(accents, localStorage.getItem('light-accent'), 'blue');
  });

  // Client-only pref (localStorage), no api.updateSettings sync — the tab
  // title/favicon is per-browser behaviour. Move to server settings if it
  // ever needs to follow the user across devices. Default off.
  const [progressInTitle, setProgressInTitleState] = useState<boolean>(() => {
    return localStorage.getItem('progress-in-title') === 'true';
  });
  const setProgressInTitle = (v: boolean) => {
    setProgressInTitleState(v);
    localStorage.setItem('progress-in-title', String(v));
  };

  const [tabDisplay, setTabDisplayState] = useState<'percentage' | 'time'>(() =>
    localStorage.getItem('tab-display') === 'time' ? 'time' : 'percentage'
  );
  const setTabDisplay = (v: 'percentage' | 'time') => {
    setTabDisplayState(v);
    localStorage.setItem('tab-display', v);
  };

  // Sync from API once auth state is known. Same gate shape as
  // useStreamTokenSync / ColorCatalogProvider: wait for AuthContext to
  // settle, then only fetch when we can actually expect a 200 (auth
  // disabled, or auth enabled with a logged-in user).
  useEffect(() => {
    if (authLoading) return;
    if (authEnabled && user === null) return;
    // The theme is part of what any signed-in user may read (useUiSettings).
    (readsAllSettings ? api.getSettings() : api.getUiSettings()).then((settings) => {
      // Dark settings
      if (settings.dark_style) {
        const value = preference(styles, settings.dark_style, 'classic');
        setDarkStyleState(value);
        localStorage.setItem('dark-style', value);
      }
      if (settings.dark_background) {
        const value = preference(darkBackgrounds, settings.dark_background, 'cool');
        setDarkBackgroundState(value);
        localStorage.setItem('dark-background', value);
      }
      if (settings.dark_accent) {
        const value = preference(accents, settings.dark_accent, 'blue');
        setDarkAccentState(value);
        localStorage.setItem('dark-accent', value);
      }
      // Light settings
      if (settings.light_style) {
        const value = preference(styles, settings.light_style, 'classic');
        setLightStyleState(value);
        localStorage.setItem('light-style', value);
      }
      if (settings.light_background) {
        const value = preference(lightBackgrounds, settings.light_background, 'cool');
        setLightBackgroundState(value);
        localStorage.setItem('light-background', value);
      }
      if (settings.light_accent) {
        const value = preference(accents, settings.light_accent, 'blue');
        setLightAccentState(value);
        localStorage.setItem('light-accent', value);
      }
    }).catch(() => {});
    // Re-fetch when auth state transitions (e.g. login completes); the
    // gate above short-circuits subsequent calls once we already have a
    // valid sync.
  }, [authLoading, authEnabled, user, readsAllSettings]);

  // Apply theme classes based on current mode
  useEffect(() => {
    const root = document.documentElement;

    // Remove all theme classes
    root.classList.remove(
      'dark',
      'style-classic', 'style-glow', 'style-vibrant',
      'bg-neutral', 'bg-warm', 'bg-cool', 'bg-oled', 'bg-slate', 'bg-forest',
      'accent-green', 'accent-teal', 'accent-blue', 'accent-orange', 'accent-purple', 'accent-red'
    );

    // Apply based on resolved mode
    if (resolvedMode === 'dark') {
      root.classList.add('dark');
      root.classList.add(`style-${darkStyle}`);
      root.classList.add(`bg-${darkBackground}`);
      root.classList.add(`accent-${darkAccent}`);
    } else {
      root.classList.add(`style-${lightStyle}`);
      root.classList.add(`bg-${lightBackground}`);
      root.classList.add(`accent-${lightAccent}`);
    }

    localStorage.setItem('theme-mode', mode);
    localStorage.removeItem('theme');
  }, [mode, resolvedMode, darkStyle, darkBackground, darkAccent, lightStyle, lightBackground, lightAccent]);

  const toggleMode = () => setModeState(prev => {
    if (prev === 'dark') return 'light';
    if (prev === 'light') return 'system';
    return 'dark';
  });
  const setMode = (m: ThemeMode) => setModeState(m);

  // Dark setters
  const setDarkStyle = (v: ThemeStyle) => {
    setDarkStyleState(v);
    localStorage.setItem('dark-style', v);
    api.updateSettings({ dark_style: v }).catch(() => {});
  };
  const setDarkBackground = (v: DarkBackground) => {
    setDarkBackgroundState(v);
    localStorage.setItem('dark-background', v);
    api.updateSettings({ dark_background: v }).catch(() => {});
  };
  const setDarkAccent = (v: ThemeAccent) => {
    setDarkAccentState(v);
    localStorage.setItem('dark-accent', v);
    api.updateSettings({ dark_accent: v }).catch(() => {});
  };

  // Light setters
  const setLightStyle = (v: ThemeStyle) => {
    setLightStyleState(v);
    localStorage.setItem('light-style', v);
    api.updateSettings({ light_style: v }).catch(() => {});
  };
  const setLightBackground = (v: LightBackground) => {
    setLightBackgroundState(v);
    localStorage.setItem('light-background', v);
    api.updateSettings({ light_background: v }).catch(() => {});
  };
  const setLightAccent = (v: ThemeAccent) => {
    setLightAccentState(v);
    localStorage.setItem('light-accent', v);
    api.updateSettings({ light_accent: v }).catch(() => {});
  };

  return (
    <ThemeContext.Provider value={{
      mode,
      resolvedMode,
      darkStyle, darkBackground, darkAccent,
      lightStyle, lightBackground, lightAccent,
      progressInTitle, setProgressInTitle, tabDisplay, setTabDisplay,
      toggleMode, setMode,
      setDarkStyle, setDarkBackground, setDarkAccent,
      setLightStyle, setLightBackground, setLightAccent,
    }}>
      {children}
    </ThemeContext.Provider>
  );
}

export function useTheme() {
  const context = useContext(ThemeContext);
  if (!context) throw new Error('useTheme must be used within ThemeProvider');
  return context;
}

export type { ThemeMode, ThemeStyle, DarkBackground, LightBackground, ThemeAccent };
