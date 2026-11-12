import { SegmentedControl } from "@persona/ui/choice";
import { Button, Switch } from "@persona/ui/controls";
import { TextField } from "@persona/ui/field";
import { SettingRow } from "@persona/ui/setting-row";
import { useEffect, useState } from "react";
import { client } from "../../client.ts";
import { useI18n } from "../i18n/context.tsx";
import { userError } from "../user-error.ts";

export function ComputerDecision({
  botId,
  admin,
}: {
  botId: number;
  admin: boolean;
}) {
  const { t } = useI18n();
  const [state, setState] =
    useState<Awaited<ReturnType<typeof client.computer.assistant>>>();
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    void client.computer
      .assistant({ botId }, { signal: controller.signal })
      .then(setState)
      .catch((cause) => {
        if (!controller.signal.aborted)
          setError(userError(cause, t("无法读取辅助决策设置。")));
      });
    return () => controller.abort();
  }, [botId, t]);
  const provider = state?.provider ?? "typesafe";
  const configured = state?.configured[provider] ?? false;
  const disabled = !admin || busy || !state?.available;
  async function run(action: () => Promise<void>) {
    setBusy(true);
    setError("");
    setResult("");
    try {
      await action();
    } catch (cause) {
      setError(userError(cause, t("辅助决策设置未完成。")));
    } finally {
      setBusy(false);
    }
  }
  async function configure(enabled: boolean, source = provider) {
    if (!state?.computerId) return;
    setState(
      await client.computer.assistantConfigure({
        botId,
        computerId: state.computerId,
        revision: state.revision,
        enabled,
        provider: source,
      }),
    );
    setKey("");
  }
  return (
    <section
      className="detail-stack"
      aria-label={t("辅助决策")}
      aria-busy={busy}
    >
      <div className="panel-card computer-services">
        <SettingRow title={t("Jev 协作模式")}>
          <Switch
            label={t("启用辅助决策")}
            checked={state?.enabled ?? false}
            disabled={disabled || (!configured && !state?.enabled)}
            onCheckedChange={(enabled) => void run(() => configure(enabled))}
          />
        </SettingRow>
        <SettingRow title={t("来源")}>
          <SegmentedControl
            label={t("Jev 来源")}
            value={provider}
            disabled={disabled}
            options={[
              { value: "typesafe", label: t("官方") },
              { value: "openrouter", label: "OpenRouter" },
            ]}
            onValueChange={(value) => void run(() => configure(false, value))}
          />
        </SettingRow>
      </div>
      {admin && state?.available && (
        <form
          className="detail-stack"
          onSubmit={(event) => {
            event.preventDefault();
            const value = key.trim();
            setKey("");
            void run(async () =>
              setState(
                await client.computer.assistantKey({
                  botId,
                  provider,
                  key: value,
                }),
              ),
            );
          }}
        >
          <TextField
            label={`${provider === "typesafe" ? "TypeSafe" : "OpenRouter"} ${t("API 密钥")}`}
            type="password"
            autoComplete="off"
            maxLength={512}
            value={key}
            onValueChange={setKey}
            placeholder={configured ? t("已保存") : t("输入密钥")}
            disabled={busy}
          />
          <div className="form-actions">
            <Button
              size="small"
              variant="secondary"
              type="submit"
              disabled={busy || !key.trim()}
            >
              {t("保存")}
            </Button>
            <Button
              size="small"
              variant="secondary"
              type="button"
              disabled={busy || !configured}
              onClick={() =>
                void run(async () => {
                  const test = await client.computer.assistantTest({
                    botId,
                    provider,
                  });
                  setResult(`${t("已连接")} · ${test.milliseconds} ms`);
                })
              }
            >
              {t("测试连接")}
            </Button>
            {configured && (
              <Button
                size="small"
                variant="ghost"
                type="button"
                disabled={busy}
                onClick={() =>
                  void run(async () =>
                    setState(
                      await client.computer.assistantKey({
                        botId,
                        provider,
                        key: "",
                      }),
                    ),
                  )
                }
              >
                {t("移除")}
              </Button>
            )}
          </div>
        </form>
      )}
      <p className="detail-note">
        {t(
          "启用后，必要的界面文字或截图会发送至所选服务。密钥加密保存在此 Persona 服务中。",
        )}
      </p>
      {state && !state.available && (
        <p className="detail-note">{t("当前服务未配置凭据存储。")}</p>
      )}
      {result && (
        <p role="status" className="detail-note">
          {result}
        </p>
      )}
      {error && (
        <p role="alert" className="alert">
          {error}
        </p>
      )}
    </section>
  );
}
