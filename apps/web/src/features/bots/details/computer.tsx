import { Button } from "@persona/ui/controls";
import { Dialog } from "@persona/ui/dialog";
import { ChoicePicker } from "@persona/ui/picker";
import { SettingRow } from "@persona/ui/setting-row";
import { useState } from "react";
import { computerLabels } from "../../computer/copy.ts";
import { ComputerLocation } from "../../computer/location.tsx";
import { ComputerServices } from "../../computer/services.tsx";
import type { useComputer } from "../../computer/use-computer.ts";
import { useI18n } from "../../i18n/context.tsx";
import { useSettings } from "../../settings/context.ts";

export function ComputerSettings({
  computer,
  botId,
}: {
  computer: ReturnType<typeof useComputer>;
  botId: number;
}) {
  const { status, busy, error, setup, refresh, assign, choices } = computer;
  const { t } = useI18n();
  const settings = useSettings();
  const [location, setLocation] = useState(false);
  const [preparing, setPreparing] = useState(false);
  const ready = () => {
    setLocation(false);
    void refresh();
  };
  return (
    <section
      className="detail-page"
      aria-label={t("工作电脑")}
      aria-busy={busy}
    >
      <div className="panel-card computer-binding">
        <SettingRow title={t("工作位置")}>
          <ChoicePicker
            label={t("此 Bot 使用的电脑")}
            value={status?.id ?? ""}
            disabled={busy}
            onValueChange={(value) => void assign(value || null)}
            items={[
              { value: "", label: t("仅聊天") },
              ...choices
                .filter((item) => item.authorized)
                .map((item) => ({ value: item.id, label: item.name })),
            ]}
          />
        </SettingRow>
        <div className="computer-binding-status">
          <span
            className="status-tag"
            data-tone={status?.authorized ? "ready" : "attention"}
          >
            {status
              ? `${computerLabels.state[status.state]} · ${status.authorized ? t("已授权") : t("待授权")}`
              : t("仅聊天")}
          </span>
          {status && (
            <div className="computer-capabilities">
              <span data-on={status.capabilities.commands}>{t("命令")}</span>
              <span data-on={status.capabilities.files}>{t("文件")}</span>
            </div>
          )}
        </div>
      </div>
      {busy && <p role="status">{t("正在准备并连接电脑…")}</p>}
      {status?.error && (
        <p className="alert" role="alert">
          {status.error}
        </p>
      )}
      {status?.authorized && (
        <ComputerServices
          key={status.id}
          botId={botId}
          admin={settings.admin}
        />
      )}
      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}

      {settings.admin && (
        <div className="detail-page-head">
          {setup && (
            <Button
              variant="secondary"
              size="small"
              disabled={busy || status?.busy}
              onClick={() => setLocation(true)}
            >
              {t("准备新电脑")}
            </Button>
          )}
          <Button
            variant="ghost"
            size="small"
            onClick={() => settings.open("computers")}
          >
            {t("管理所有电脑")}
          </Button>
        </div>
      )}

      <Dialog
        open={location}
        onOpenChange={(open) => {
          if (!preparing) setLocation(open);
        }}
        title={t("Bot 在哪里工作？")}
        description={t("选择只应用到此 Bot。")}
        footer={
          <div className="dialog-actions">
            <Button
              variant="secondary"
              disabled={preparing}
              onClick={() => setLocation(false)}
            >
              {t("取消")}
            </Button>
          </div>
        }
      >
        {setup && (
          <ComputerLocation
            botId={botId}
            canIsolate={setup.canIsolate}
            deployment={setup.deployment}
            onReady={ready}
            onBusyChange={setPreparing}
          />
        )}
      </Dialog>
    </section>
  );
}
