import { Button } from "@persona/ui/controls";
import { Dialog } from "@persona/ui/dialog";
import { SettingRow } from "@persona/ui/setting-row";
import { useState } from "react";
import { AppearancePicker } from "../appearance/picker.tsx";
import { DataBackup } from "../hosting/backup.tsx";
import { Devices } from "../hosting/devices.tsx";
import { useI18n } from "../i18n/context.tsx";
import { LanguagePicker } from "../i18n/picker.tsx";
import { useSettings } from "./context.ts";
import type { SettingsAccess } from "./sections.tsx";

export function GeneralSettings({ admin, deviceId }: SettingsAccess) {
  const settings = useSettings();
  const { t } = useI18n();
  const [flow, setFlow] = useState<"access" | "backup" | null>(null);
  return (
    <div className="settings-sections">
      <section className="settings-group">
        <h3>{t("显示")}</h3>
        <div className="settings-card">
          <SettingRow title={t("语言")} description={t("选择界面的显示语言。")}>
            <LanguagePicker />
          </SettingRow>
          <SettingRow
            title={t("主题")}
            description={t("选择浅色、深色或跟随系统。")}
          >
            <AppearancePicker />
          </SettingRow>
        </div>
      </section>
      {admin && deviceId && (
        <section className="settings-group">
          <h3>{t("访问")}</h3>
          <div className="settings-card">
            <SettingRow
              title={t("在其他设备上使用")}
              description={t("连接手机或另一台电脑，继续使用这些 Bot。")}
            >
              <Button
                variant="secondary"
                size="small"
                onClick={() => setFlow("access")}
              >
                {t("连接设备")}
              </Button>
            </SettingRow>
          </div>
        </section>
      )}
      {admin && (
        <section className="settings-group">
          <h3>{t("管理")}</h3>
          <div className="settings-card">
            <SettingRow
              title={t("电脑高级管理")}
              description={t(
                "管理镜像、远程连接和容器。Bot 的工作环境可在对话中选择。",
              )}
            >
              <Button
                variant="secondary"
                size="small"
                onClick={() => settings.open("computers")}
              >
                {t("打开")}
              </Button>
            </SettingRow>
            {window.personaDesktop && (
              <SettingRow
                title={t("数据与备份")}
                description={t("创建加密备份，或从备份恢复。")}
              >
                <Button
                  variant="secondary"
                  size="small"
                  onClick={() => setFlow("backup")}
                >
                  {t("管理备份")}
                </Button>
              </SettingRow>
            )}
          </div>
        </section>
      )}
      <Dialog
        open={flow === "access"}
        onOpenChange={(open) => {
          if (!open) setFlow(null);
        }}
        title={t("在其他设备上使用")}
        description="Autorize dispositivos de confiança. Ao revogar, o acesso será interrompido."
        footer={
          <div className="dialog-actions">
            <Button variant="secondary" onClick={() => setFlow(null)}>
              {t("完成")}
            </Button>
          </div>
        }
      >
        {deviceId && <Devices current={deviceId} />}
      </Dialog>
      <Dialog
        open={flow === "backup"}
        onOpenChange={(open) => {
          if (!open) setFlow(null);
        }}
        title={t("数据与备份")}
        description="Gerencie os dados e backups do Persona neste computador."
        footer={
          <div className="dialog-actions">
            <Button variant="secondary" onClick={() => setFlow(null)}>
              Fechar
            </Button>
          </div>
        }
      >
        <DataBackup />
      </Dialog>
    </div>
  );
}
