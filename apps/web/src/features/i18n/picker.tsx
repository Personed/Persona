import { ChoicePicker } from "@persona/ui/picker";
import { useI18n } from "./context.tsx";
import { type Language, languages } from "./types.ts";

export function LanguagePicker() {
  const { language, setLanguage } = useI18n();

  return (
    <div className="language-picker">
      <ChoicePicker
        label="Idioma"
        size="small"
        value={language}
        onValueChange={(value) => setLanguage(value as Language)}
        items={languages}
      />
    </div>
  );
}
