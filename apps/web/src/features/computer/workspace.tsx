import type { Bot } from "@persona/agent/bots";
import { Button } from "@persona/ui/controls";
import { DesktopIcon } from "@phosphor-icons/react/dist/csr/Desktop";
import { GearSixIcon } from "@phosphor-icons/react/dist/csr/GearSix";
import { GlobeIcon } from "@phosphor-icons/react/dist/csr/Globe";
import { useI18n } from "../i18n/context.tsx";
import { computerLabels } from "./copy.ts";
import type { useComputer } from "./use-computer.ts";
import {
  type ComputerService,
  serviceLabels,
  useComputerServices,
} from "./use-services.ts";
import { ComputerViewer } from "./viewer.tsx";
import "./style.css";

export function ComputerWorkspace({
  bot,
  computer,
  admin,
  service,
  expanded,
  onExpand,
  onSettings,
  takeoverId,
}: {
  bot: Bot;
  computer: ReturnType<typeof useComputer>;
  admin: boolean;
  service: ComputerService;
  expanded: boolean;
  onExpand: () => void;
  onSettings: () => void;
  takeoverId?: string;
}) {
  const { t } = useI18n();
  const status = computer.status;
  const { state, busy, error, configure, reload } = useComputerServices(
    bot.id,
    Boolean(status?.authorized),
  );
  const item = state?.[service];
  const label = serviceLabels[service];
  return (
    <section
      className="computer-workspace"
      aria-label={`${t("Computador de")} ${bot.name}`}
    >
      <div className="computer-workspace-stage">
        {status?.authorized && item?.enabled && item.available ? (
          <ComputerViewer
            takeoverId={takeoverId}
            botId={bot.id}
            service={service}
            expanded={expanded}
            onExpand={onExpand}
            onSettings={onSettings}
          />
        ) : (
          <div className="computer-workspace-empty">
            {(!state || !status?.authorized || item?.enabled) && (
              <>
                {service === "browser" ? (
                  <GlobeIcon size={28} />
                ) : (
                  <DesktopIcon size={28} />
                )}
                <h3>
                  {!status
                    ? t("为 Bot 选择一台电脑")
                    : !status.authorized
                      ? t("电脑需要授权")
                      : !state
                        ? error
                          ? t("电脑暂时无法连接")
                          : t("正在连接工作电脑…")
                        : item?.enabled
                          ? `${label} ${t("尚未就绪")}`
                          : `${label} ${t("尚未启用")}`}
                </h3>
              </>
            )}
            {item?.error && <p>{item.error}</p>}
            {error && (
              <p role="alert" className="alert">
                {error}
              </p>
            )}
            {status && !status.authorized && (
              <p>{computerLabels.state[status.state]}</p>
            )}
            {!status?.authorized ? (
              <Button size="small" variant="secondary" onClick={onSettings}>
                {t("配置工作电脑")}
              </Button>
            ) : (
              state &&
              admin && (
                <Button
                  size="small"
                  variant="secondary"
                  pending={busy === service}
                  disabled={Boolean(busy)}
                  onClick={() =>
                    void configure(
                      service,
                      true,
                      service === "browser" &&
                        Boolean(item?.enabled) &&
                        !item?.available,
                    )
                  }
                >
                  {item?.enabled
                    ? service === "browser"
                      ? t("安装并连接浏览器")
                      : t("重新检查桌面")
                    : `${t("启用")}${label}`}
                </Button>
              )
            )}
            {state && !admin && (
              <p>
                {t("请联系管理员启用")}
                {label}。
              </p>
            )}
            {error && !state && (
              <Button size="small" variant="secondary" onClick={reload}>
                {t("重新连接")}
              </Button>
            )}
          </div>
        )}
        {!(status?.authorized && item?.enabled && item.available) && (
          <div className="computer-empty-controls">
            <Button
              variant="ghost"
              size="icon-small"
              aria-label={t("电脑设置")}
              title={t("电脑设置")}
              onClick={onSettings}
            >
              <GearSixIcon />
            </Button>
          </div>
        )}
      </div>
    </section>
  );
}
