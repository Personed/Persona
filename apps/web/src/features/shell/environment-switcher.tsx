import { Button } from "@persona/ui/controls";
import { CaretDownIcon } from "@phosphor-icons/react/dist/csr/CaretDown";
import { CloudIcon } from "@phosphor-icons/react/dist/csr/Cloud";
import { MonitorIcon } from "@phosphor-icons/react/dist/csr/Monitor";
import { useEffect, useState } from "react";
import type { DesktopState } from "../hosting/desktop.ts";
import { useI18n } from "../i18n/context.tsx";

export function EnvironmentSwitcher() {
  const { t } = useI18n();
  const desktop = window.personaDesktop;
  const [current, setCurrent] = useState<DesktopState["instances"][number]>();
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    void desktop
      ?.state()
      .then((result) => {
        if (!active) return;
        if (result.ok && result.value)
          setCurrent(
            result.value.instances.find(
              (item) => item.id === result.value?.selected,
            ),
          );
        else setError(t("无法读取当前环境"));
      })
      .catch(() => {
        if (active) setError(t("无法读取当前环境"));
      });
    return () => {
      active = false;
    };
  }, [t]);
  if (!desktop) return null;
  const Icon = current?.mode === "existing" ? CloudIcon : MonitorIcon;
  return (
    <div className="environment-switcher">
      <Button
        variant="ghost"
        size="small"
        className="environment-trigger"
        aria-label={`${t("选择环境")}：${current?.name ?? t("选择环境")}`}
        aria-haspopup="menu"
        aria-expanded={open}
        title={current?.url}
        onClick={async () => {
          if (open) return;
          setError("");
          setOpen(true);
          try {
            const result = await desktop.menu("environment");
            if (!result.ok) setError(t("无法打开环境菜单"));
          } catch {
            setError(t("无法打开环境菜单"));
          } finally {
            setOpen(false);
          }
        }}
      >
        <Icon aria-hidden="true" />
        <span>{current?.name ?? t("选择环境")}</span>
        <CaretDownIcon aria-hidden="true" />
      </Button>
      {error && (
        <p className="environment-error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
