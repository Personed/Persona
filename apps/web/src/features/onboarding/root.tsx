import { Button } from "@persona/ui/controls";
import { SetupPage } from "@persona/ui/setup";
import { type ReactNode, useCallback, useEffect, useState } from "react";
import { client } from "../../client.ts";
import { ModelSetup } from "../bots/details/model.tsx";
import { ComputerLocation } from "../computer/location.tsx";
import { useI18n } from "../i18n/context.tsx";
import type { SettingsAccess } from "../settings/sections.tsx";
import { Titlebar } from "../shell/titlebar.tsx";

type Setup = {
  botId: number;
  computer: boolean;
  binding: Awaited<ReturnType<typeof client.computer.get>>;
  canIsolate: boolean;
  deployment: "native" | "container";
  configured: boolean;
};

export function Onboarding({
  children,
  admin,
  standalone,
}: SettingsAccess & { children: ReactNode }) {
  const { t } = useI18n();
  const [setup, setSetup] = useState<Setup>();
  const [error, setError] = useState("");
  const [step, setStep] = useState<0 | 1>();
  const [finished, setFinished] = useState(false);
  const refresh = useCallback(async () => {
    setError("");
    const bots = await client.bots.list();
    const bot = bots[0];
    if (!bot) throw new Error(t("未找到初始 Bot。请重新连接 Persona。"));
    const [binding, location, engine] = await Promise.all([
      client.computer.get({ botId: bot.id }),
      client.computer.setup(),
      client.engines.get({ botId: bot.id }),
    ]);
    const next: Setup = {
      botId: bot.id,
      computer: Boolean(binding.computer?.authorized),
      binding,
      canIsolate: location.canIsolate,
      deployment: location.deployment,
      configured: engine.revision > 0 || engine.prepared,
    };
    setSetup(next);
    return next;
  }, [t]);
  const initialize = useCallback(async () => {
    const next = await refresh();
    setFinished(next.configured && next.computer);
    setStep(next.computer ? 1 : 0);
  }, [refresh]);
  useEffect(() => {
    void initialize().catch((cause) => setError(cause.message));
  }, [initialize]);
  if (finished) return children;
  const computer = step === 0;
  const advance = () =>
    void refresh()
      .then((next) => {
        if (next.configured && next.computer) setFinished(true);
        else setStep(1);
      })
      .catch((cause) => setError(cause.message));
  return (
    <div className="setup-window">
      <Titlebar />
      <SetupPage
        label={t("首次设置")}
        title={
          step === undefined
            ? t("正在加载设置…")
            : computer
              ? t("Bot 在哪里工作？")
              : t("选择对话模型")
        }
        description={
          step === undefined
            ? undefined
            : computer
              ? t("选择工作环境。之后可以更改。")
              : t("连接 AI 账户以开始第一个任务。")
        }
        progress={
          step !== undefined ? { current: step + 1, total: 2 } : undefined
        }
      >
        {setup &&
          step !== undefined &&
          (admin && !standalone ? (
            <>
              <div hidden={!computer}>
                <ComputerLocation
                  botId={setup.botId}
                  canIsolate={setup.canIsolate}
                  deployment={setup.deployment}
                  initialComputer={setup.binding.computer}
                  onReady={advance}
                />
              </div>
              {step === 1 && (
                <ModelSetup
                  botId={setup.botId}
                  onBack={() => setStep(0)}
                  onReady={() =>
                    void refresh()
                      .then((next) => {
                        if (next.configured && next.computer) setFinished(true);
                        else setError(t("请先保存模型配置。"));
                      })
                      .catch((cause) => setError(cause.message))
                  }
                />
              )}
            </>
          ) : (
            <p role="status">{t("请联系管理员完成初始设置。")}</p>
          ))}
        {!setup && !error && <p role="status">{t("正在加载设置…")}</p>}
        {error && (
          <p role="alert" className="alert">
            {error}
          </p>
        )}
        {(error || (setup && (!admin || standalone))) && (
          <Button
            variant="secondary"
            onClick={() =>
              void initialize().catch((cause) => setError(cause.message))
            }
          >
            {t("重试")}
          </Button>
        )}
      </SetupPage>
    </div>
  );
}
