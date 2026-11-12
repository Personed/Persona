import { ChoiceCards } from "@persona/ui/choice";
import { Button } from "@persona/ui/controls";
import { useEffect, useRef, useState } from "react";
import { client } from "../../client.ts";
import { useI18n } from "../i18n/context.tsx";
import { userError } from "../user-error.ts";
import "./style.css";

export function ComputerLocation({
  botId,
  canIsolate,
  deployment,
  onReady,
  onBusyChange,
  onBack,
  initialComputer,
}: {
  botId: number;
  canIsolate: boolean;
  deployment: "native" | "container";
  onReady: () => void;
  onBusyChange?: (busy: boolean) => void;
  onBack?: () => void;
  initialComputer?: Awaited<ReturnType<typeof client.computer.get>>["computer"];
}) {
  const { t } = useI18n();
  const [mode, setMode] = useState<"process" | "shared" | "dedicated">(
    initialComputer && initialComputer.kind !== "process"
      ? (initialComputer.scope?.kind ?? "shared")
      : "process",
  );
  const [local, setLocal] = useState(false);
  const request = useRef(crypto.randomUUID());
  const active = useRef(false);
  useEffect(() => {
    void window.personaDesktop
      ?.state()
      .then((result) => setLocal(result.value?.mode === "local"));
  }, []);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <form
      className="setup-form"
      aria-busy={busy}
      onSubmit={async (event) => {
        event.preventDefault();
        if (active.current) return;
        active.current = true;
        setBusy(true);
        onBusyChange?.(true);
        setError("");
        try {
          if (mode !== "process" && local) {
            const result = await window.personaDesktop?.prepare();
            if (result && !result.ok) throw new Error(result.error);
          }
          const binding = await client.computer.get({ botId });
          const input = {
            botId,
            revision: binding.revision,
            requestId: request.current,
          };
          if (mode === "process") {
            const directory = await client.computer.list();
            const process = directory.items.find(
              (item) => item.kind === "process" && item.authorized,
            );
            if (process)
              await client.computer.assign({
                ...input,
                computerId: process.id,
              });
            else await client.computer.configure({ ...input, mode });
          } else await client.computer.prepare({ ...input, mode });
          request.current = crypto.randomUUID();
          onReady();
        } catch (cause) {
          setError(userError(cause, t("电脑准备失败，请重试。")));
        } finally {
          active.current = false;
          setBusy(false);
          onBusyChange?.(false);
        }
      }}
    >
      <ChoiceCards
        label={t("Bot 的工作环境")}
        value={mode}
        disabled={busy}
        onValueChange={(value) => {
          setMode(value);
          request.current = crypto.randomUUID();
        }}
        options={[
          {
            value: "process",
            label:
              deployment === "container"
                ? t("使用服务容器")
                : local
                  ? t("使用这台电脑")
                  : t("使用服务电脑"),
            description: t("访问 Persona 运行环境中的命令和文件。"),
          },
          {
            value: "shared",
            label: t("共享隔离电脑"),
            description: t("与其他 Bot 共享文件。需要 Docker。"),
            disabled: !canIsolate,
          },
          {
            value: "dedicated",
            label: t("专用隔离电脑"),
            description: t("仅此 Bot 使用。需要 Docker。"),
            disabled: !canIsolate,
          },
        ]}
      />
      <p className="detail-note">
        {mode === "process"
          ? t("确认后，你允许 Bot 在此环境中运行命令并管理文件。")
          : t("确认后，将准备镜像。对话仍保存在 Persona。")}
        {!canIsolate && ` ${t("此服务器暂不支持隔离电脑。")}`}
      </p>
      {busy && <p role="status">{t("正在准备环境并绑定 Bot…")}</p>}
      {error && (
        <p role="alert" className="alert">
          {error}
        </p>
      )}
      <div className={onBack ? "setup-actions" : "form-actions"}>
        {onBack && (
          <Button
            type="button"
            variant="secondary"
            disabled={busy}
            onClick={onBack}
          >
            {t("返回")}
          </Button>
        )}
        <Button
          type="submit"
          pending={busy}
          disabled={busy || (mode !== "process" && !canIsolate)}
        >
          {busy
            ? t("准备中…")
            : mode === "process"
              ? t("授权并继续")
              : t("准备隔离电脑")}
        </Button>
      </div>
    </form>
  );
}
