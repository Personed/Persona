import type { EngineControls } from "@persona/agent/harness/types";
import { SegmentedControl } from "@persona/ui/choice";
import { useI18n } from "../i18n/context.tsx";

export function Effort({
  control,
  disabled,
  onChange,
}: {
  control: EngineControls["options"][number];
  disabled: boolean;
  onChange: (value: string) => void;
}) {
  const { t } = useI18n();
  if (control.options.length < 2) return null;
  const labels: Record<string, string> = {
    off: t("关闭"),
    minimal: t("最少"),
    low: t("低"),
    medium: t("中"),
    high: t("高"),
    xhigh: t("极高"),
    max: t("最高"),
  };
  return (
    <div className="model-effort">
      <SegmentedControl
        label={control.label}
        variant="compact"
        value={control.value ?? ""}
        disabled={disabled}
        onValueChange={onChange}
        options={[
          { value: "", label: t("默认") },
          ...control.options.map((item) => ({
            value: item.id,
            label: labels[item.id] ?? item.label,
          })),
        ]}
      />
    </div>
  );
}
