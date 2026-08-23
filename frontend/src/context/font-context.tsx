import React, { createContext, useContext, useEffect, useState } from 'react';
import { sansFonts, serifFonts, monoFonts, fontStacks, systemStacks } from '@/config/fonts';

type SansFont = (typeof sansFonts)[number];
type SerifFont = (typeof serifFonts)[number];
type MonoFont = (typeof monoFonts)[number];

type FontCategory = 'sans' | 'serif' | 'mono';

interface FontContextType {
  sansFont: SansFont;
  serifFont: SerifFont;
  monoFont: MonoFont;
  setSansFont: (font: SansFont) => void;
  setSerifFont: (font: SerifFont) => void;
  setMonoFont: (font: MonoFont) => void;
}

const FONT_VAR: Record<FontCategory, string> = {
  sans: '--font-sans',
  serif: '--font-serif',
  mono: '--font-mono',
};

const FONT_KEY: Record<FontCategory, string> = {
  sans: 'font-sans',
  serif: 'font-serif',
  mono: 'font-mono',
};

const LEGACY_FONT_KEY = 'font';
const LEGACY_SANS_FONTS = ['inter', 'manrope', 'system'] as const;

function isFontOption<T extends string>(value: string | null, options: readonly T[]): value is T {
  return value !== null && options.includes(value as T);
}

function readStoredFont<T extends string>(category: FontCategory, options: readonly T[]): T | 'theme' {
  if (typeof window === 'undefined') return 'theme';

  try {
    const storage = window.localStorage;
    const saved = storage.getItem(FONT_KEY[category]);
    if (isFontOption(saved, options)) return saved;

    if (category === 'sans') {
      const legacyFont = storage.getItem(LEGACY_FONT_KEY);
      if (isFontOption(legacyFont, LEGACY_SANS_FONTS)) {
        // Persist the migrated value when possible. The in-memory value still
        // takes effect if storage is unavailable or rejects writes.
        try {
          storage.setItem(FONT_KEY.sans, legacyFont);
          storage.removeItem(LEGACY_FONT_KEY);
        } catch {
          // Ignore storage failures and continue with the migrated value.
        }
        return legacyFont as T;
      }
    }
  } catch {
    // Accessing localStorage itself can throw (for example in privacy mode).
  }

  return 'theme';
}

function persistFont(category: FontCategory, font: string) {
  if (typeof window === 'undefined') return;

  try {
    window.localStorage.setItem(FONT_KEY[category], font);
  } catch {
    // Storage is optional; the provider's in-memory state remains authoritative.
  }
}

const FontContext = createContext<FontContextType | undefined>(undefined);

export const FontProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [sansFont, _setSansFont] = useState<SansFont>(() => readStoredFont('sans', sansFonts) as SansFont);
  const [serifFont, _setSerifFont] = useState<SerifFont>(() => readStoredFont('serif', serifFonts) as SerifFont);
  const [monoFont, _setMonoFont] = useState<MonoFont>(() => readStoredFont('mono', monoFonts) as MonoFont);

  useEffect(() => {
    if (typeof document === 'undefined') return;

    const root = document.documentElement;

    const applyFont = (category: FontCategory, font: string) => {
      const cssVar = FONT_VAR[category];
      if (font === 'theme') {
        // 'theme'：跟随主题默认字体，移除覆盖让主题类生效
        root.style.removeProperty(cssVar);
      } else if (font === 'system') {
        // 'system'：直接使用操作系统默认字体，不跟随主题
        root.style.setProperty(cssVar, systemStacks[category]);
      } else {
        root.style.setProperty(cssVar, fontStacks[font] ?? font);
      }
    };

    applyFont('sans', sansFont);
    applyFont('serif', serifFont);
    applyFont('mono', monoFont);
  }, [sansFont, serifFont, monoFont]);

  const setSansFont = (font: SansFont) => {
    _setSansFont(font);
    persistFont('sans', font);
  };
  const setSerifFont = (font: SerifFont) => {
    _setSerifFont(font);
    persistFont('serif', font);
  };
  const setMonoFont = (font: MonoFont) => {
    _setMonoFont(font);
    persistFont('mono', font);
  };

  return (
    <FontContext value={{ sansFont, serifFont, monoFont, setSansFont, setSerifFont, setMonoFont }}>
      {children}
    </FontContext>
  );
};

// eslint-disable-next-line react-refresh/only-export-components
export const useFont = () => {
  const context = useContext(FontContext);
  if (!context) {
    throw new Error('useFont must be used within a FontProvider');
  }
  return context;
};
