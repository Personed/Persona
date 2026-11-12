import { ChoicePicker } from "@persona/ui/picker";
import { useTheme } from "@persona/ui/theme";
import { themePreference } from "@persona/ui/tokens";
import { useI18n } from "../i18n/context.tsx";

export function AppearancePicker() {
  const theme = useTheme();
  const { t } = useI18n();
  return (
    <div className="appearance-picker">
      <ChoicePicker
        label={t("外观")}
        size="small"
        value={theme.preference}
        onValueChange={(value) => theme.select(themePreference(value))}
        items={[
          { value: "system", label: t("跟随系统") },
          { value: "light", label: t("浅色") },
          { value: "dark", label: t("深色") },
        ]}
      />
      {theme.error && (
        <p className="alert" role="alert">
          {t("外观已应用，但未能保存。")}
        </p>
      )}
    </div>
  );
}
