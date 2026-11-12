import type { ChatMessage } from "@persona/agent/messages";
import { Button } from "@persona/ui/controls";
import { useWorkAction } from "../bots/details/use-bot-work.ts";
import { useI18n } from "../i18n/context.tsx";
import { MessageDetail } from "./message-detail.tsx";

export function TakeoverCard({
  botId,
  message,
}: {
  botId: number;
  message: ChatMessage;
}) {
  const { language } = useI18n();
  const isPt = language === "pt-BR";
  const action = useWorkAction();
  const request = message.takeover;
  if (!request) return null;
  const controlling = request.state === "controlling";
  const waiting = request.state === "waiting" || controlling;
  return (
    <section
      className="interaction-card takeover-card"
      aria-label={isPt ? "Solicitação de controle" : "接管请求"}
    >
      <header>
        <strong>
          {request.service === "browser"
            ? isPt
              ? "Navegador"
              : "浏览器"
            : isPt
              ? "Computador"
              : "电脑"}
        </strong>
        <span className="takeover-badge" data-state={request.state}>
          {controlling
            ? isPt
              ? "Em controle"
              : "控制中"
            : waiting
              ? isPt
                ? "Aguardando controle"
                : "等待接管"
              : request.state === "done"
                ? isPt
                  ? "Devolvido"
                  : "已归还"
                : isPt
                  ? "Expirado"
                  : "已失效"}
        </span>
      </header>
      <MessageDetail botId={botId} message={message} />
      {request.url && (
        <p className="takeover-url" title={request.url}>
          {isPt
            ? `Página solicitada: ${request.url}`
            : `请求页面：${request.url}`}
        </p>
      )}
      {waiting && (
        <div className="interaction-actions">
          <Button
            size="small"
            onClick={() =>
              window.dispatchEvent(
                new CustomEvent("persona:takeover-open", {
                  detail: { botId, ...request },
                }),
              )
            }
          >
            {controlling
              ? isPt
                ? "Ver tela"
                : "查看画面"
              : isPt
                ? "Assumir controle"
                : "接管"}
          </Button>
          {controlling && (
            <Button
              size="small"
              variant="secondary"
              pending={action.busy}
              onClick={() =>
                void action.run(
                  request.id,
                  () =>
                    new Promise<void>((resolve, reject) => {
                      const detail = {
                        botId,
                        id: request.id,
                        handled: false,
                        finish: (ok: boolean) =>
                          ok
                            ? resolve()
                            : reject(
                                new Error(
                                  isPt
                                    ? "Operação não confirmada. Verifique a tela de controle."
                                    : "操作尚未确认。请检查接管画面。",
                                ),
                              ),
                      };
                      window.dispatchEvent(
                        new CustomEvent("persona:takeover-complete", {
                          detail,
                        }),
                      );
                      if (!detail.handled)
                        reject(
                          new Error(
                            isPt
                              ? "Assuma o controle primeiro antes de prosseguir com a tarefa."
                              : "请先点击接管，完成后再继续任务。",
                          ),
                        );
                    }),
                )
              }
            >
              {isPt ? "Concluir" : "我已完成"}
            </Button>
          )}
        </div>
      )}
      {action.error && waiting && <p role="alert">{action.error}</p>}
    </section>
  );
}
