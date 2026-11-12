import { Button } from "@persona/ui/controls";
import { ChoicePicker } from "@persona/ui/picker";
import { SettingRow } from "@persona/ui/setting-row";
import { useCallback, useEffect, useState } from "react";
import { useI18n } from "../i18n/context.tsx";
import { hostingRequest } from "./api.ts";

type Device = {
  id: string;
  label: string;
  scope: string;
  created: number;
  expires: number;
};

export function Devices({ current }: { current: string }) {
  const { t } = useI18n();
  const [items, setItems] = useState<Device[]>([]);
  const [pairing, setPairing] = useState<{ code: string; expires: number }>();
  const [scope, setScope] = useState("client");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const refresh = useCallback(
    () => hostingRequest<Device[]>("devices").then(setItems),
    [],
  );
  useEffect(() => {
    void refresh().catch((error) => setError(error.message));
  }, [refresh]);
  const run = async (action: () => Promise<unknown>) => {
    setBusy(true);
    try {
      setError("");
      await action();
    } catch (error) {
      setError((error as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <section
      className="settings-sections"
      aria-label={t("访问授权")}
      aria-busy={busy}
    >
      <section className="settings-group">
        <h3>{t("连接其他设备")}</h3>
        <div className="settings-card">
          <SettingRow
            title={t("设备权限")}
            description={t("选择新设备可以访问的功能。")}
          >
            <ChoicePicker
              label={t("设备权限")}
              size="small"
              value={scope}
              disabled={busy}
              onValueChange={setScope}
              items={[
                { value: "client", label: t("使用 Bot") },
                { value: "admin", label: t("管理 Persona") },
              ]}
            />
          </SettingRow>
          <SettingRow
            title={t("一次性配对码")}
            description={t(
              "在另一台设备打开这个 Persona 的地址，再输入配对码。远程连接需要可访问的 HTTPS 地址。",
            )}
          >
            <Button
              size="small"
              disabled={busy}
              onClick={() =>
                void run(async () =>
                  setPairing(await hostingRequest("pairing", { scope })),
                )
              }
            >
              {t("生成配对码")}
            </Button>
          </SettingRow>
          {pairing && (
            <div className="pairing-result" role="status">
              <code className="pairing-code">{pairing.code}</code>
              <p className="detail-note">
                {t("有效至 ")}
                {new Date(pairing.expires).toLocaleTimeString()}
                {t("，仅可使用一次。")}
              </p>
            </div>
          )}
        </div>
      </section>
      <section className="settings-group">
        <h3>{t("已授权访问")}</h3>
        <div className="settings-card">
          {items.map((item) => (
            <SettingRow
              title={item.label}
              description={item.scope === "admin" ? t("管理员") : t("使用 Bot")}
              key={item.id}
            >
              {item.id === current ? (
                <span className="status-label">{t("当前设备")}</span>
              ) : (
                <Button
                  variant="secondary"
                  size="small"
                  disabled={busy}
                  aria-label={`${t("撤销")}${item.label}`}
                  onClick={() =>
                    void run(async () => {
                      await hostingRequest(
                        `devices/${item.id}`,
                        undefined,
                        "DELETE",
                      );
                      await refresh();
                    })
                  }
                >
                  {t("撤销")}
                </Button>
              )}
            </SettingRow>
          ))}
        </div>
      </section>
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
