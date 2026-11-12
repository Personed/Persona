import { ActivityDetails } from "@persona/ui/activity";
import { Button } from "@persona/ui/controls";
import { useState } from "react";
import { client } from "../../../client.ts";
import { useI18n } from "../../i18n/context.tsx";
import type { WorkAction } from "./use-bot-work.ts";

export function PromptPanel({
  botId,
  action,
}: {
  botId: number;
  action: WorkAction;
}) {
  const { t } = useI18n();
  const [prompt, setPrompt] = useState<Awaited<
    ReturnType<typeof client.work.prompt>
  > | null>(null);
  return (
    <section className="detail-page" aria-label={t("提示词")}>
      <div className="detail-card detail-prompt-action">
        <div>
          <strong>{t("实际提示词")}</strong>
        </div>
        <Button
          variant="secondary"
          size="small"
          aria-label={t("读取实际提示词")}
          disabled={action.busy}
          pending={action.busy}
          onClick={() =>
            void action.run(
              ["prompt", botId],
              () => client.work.prompt({ botId }),
              setPrompt,
            )
          }
        >
          {t("读取")}
        </Button>
      </div>

      {prompt ? (
        prompt.prepared ? (
          <>
            <div className="item-meta prompt-tools">
              <span>{t("工具")}</span>
              {prompt.tools.length ? (
                prompt.tools.map((tool) => (
                  <span key={tool} className="item-tag">
                    {tool}
                  </span>
                ))
              ) : (
                <span>{t("无")}</span>
              )}
            </div>
            <div className="prompt-sections">
              {Object.entries(prompt.sections).map(([key, text]) => (
                <ActivityDetails key={key} label={key} status="">
                  <pre className="detail-prompt">{text}</pre>
                </ActivityDetails>
              ))}
              <ActivityDetails label={t("完整提示词")} status="">
                <pre className="detail-prompt">{prompt.text}</pre>
              </ActivityDetails>
            </div>
          </>
        ) : (
          <p className="detail-empty">
            {t("尚无提示词请求记录。先与此 Bot 对话。")}
          </p>
        )
      ) : null}
    </section>
  );
}
