import { Button } from "@persona/ui/controls";
import { TextField } from "@persona/ui/field";
import { InlineNotice } from "@persona/ui/message";
import { SettingRow } from "@persona/ui/setting-row";
import { useEffect, useState } from "react";
import { useI18n } from "../i18n/context.tsx";
import type { DesktopState } from "./desktop.ts";

export function DataBackup() {
  const { t } = useI18n();
  const [state, setState] = useState<DesktopState>();
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const desktop = window.personaDesktop;
  useEffect(() => {
    void desktop
      ?.state()
      .then((result) => {
        if (!result.ok) setError(result.error ?? t("无法读取本机后台状态。"));
        else setState(result.value);
      })
      .catch(() => setError(t("无法读取本机后台状态。")));
  }, [t]);
  async function run(action: "backup" | "restore") {
    if (!desktop || busy) return;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const result = await desktop.maintain(action, password);
      if (!result.ok) throw new Error(result.error);
      setMessage(result.value?.message ?? t("操作已取消。"));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("备份操作未完成。"));
    } finally {
      setBusy(false);
      setPassword("");
    }
  }
  return (
    <section
      className="detail-stack"
      aria-label={t("数据与备份")}
      aria-busy={busy}
      data-dirty={Boolean(password)}
    >
      <p className="detail-note">
        {t(
          "操作会停止本机后台。完成后，从应用菜单打开本机 Persona。恢复会使用新目录。",
        )}
      </p>
      {state?.mode === "local" && (
        <>
          <TextField
            label={t("备份密码")}
            type="password"
            value={password}
            minLength={12}
            placeholder={t("至少 12 个字符")}
            autoComplete="new-password"
            disabled={busy}
            onValueChange={setPassword}
          />
          <div className="settings-card">
            <SettingRow
              title={t("加密备份")}
              description={t("选择保存位置，备份本机 Persona 的数据。")}
            >
              <Button
                variant="secondary"
                size="small"
                disabled={busy || password.length < 12}
                onClick={() => void run("backup")}
              >
                {t("创建备份")}
              </Button>
            </SettingRow>
            <SettingRow
              title={t("从备份恢复")}
              description={t("选择备份文件，并输入创建备份时的密码。")}
            >
              <Button
                variant="secondary"
                size="small"
                disabled={busy || password.length < 12}
                onClick={() => void run("restore")}
              >
                {t("选择备份")}
              </Button>
            </SettingRow>
          </div>
        </>
      )}
      {state?.mode === "existing" && (
        <p role="status">{t("请在运行此 Persona 的电脑上管理备份。")}</p>
      )}
      {!state && !error && <p role="status">{t("正在读取本机后台状态…")}</p>}
      {message && (
        <div role="status">
          <InlineNotice>{message}</InlineNotice>
        </div>
      )}
      {error && (
        <p role="alert" className="alert">
          {error}
        </p>
      )}
    </section>
  );
}
