import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useState,
} from "react";
import { translations } from "./translations.ts";
import type { Language } from "./types.ts";

const storageKey = "persona:language:v1";

export function getStoredLanguage(): Language {
  try {
    const saved = localStorage.getItem(storageKey);
    if (saved === "zh-CN") return saved;
  } catch {}
  return "pt-BR";
}

let currentLanguage: Language = getStoredLanguage();

export function getLanguage(): Language {
  return currentLanguage;
}

export function translate(key: string, lang = currentLanguage): string {
  if (lang === "zh-CN") return key;
  const dict = translations[lang];
  return dict?.[key] ?? key;
}

const I18nContext = createContext<{
  language: Language;
  setLanguage: (lang: Language) => void;
  t: (key: string) => string;
}>({
  language: "pt-BR",
  setLanguage: () => {},
  t: (key: string) => translate(key, "pt-BR"),
});

export function I18nProvider({ children }: { children: ReactNode }) {
  const [language, setLanguageState] = useState<Language>(getStoredLanguage);

  const setLanguage = useCallback((lang: Language) => {
    const selected = lang === "en-US" ? "pt-BR" : lang;
    currentLanguage = selected;
    setLanguageState(selected);
    try {
      localStorage.setItem(storageKey, selected);
    } catch {}
    document.documentElement.lang = selected;
  }, []);

  useEffect(() => {
    currentLanguage = language;
    document.documentElement.lang = language;
  }, [language]);

  const t = useCallback((key: string) => translate(key, language), [language]);

  return (
    <I18nContext value={{ language, setLanguage, t }}>{children}</I18nContext>
  );
}

export function useI18n() {
  return useContext(I18nContext);
}
