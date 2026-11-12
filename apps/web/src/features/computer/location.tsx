import { ChoiceCards } from "@persona/ui/choice";
import { Button } from "@persona/ui/controls";
import { useEffect, useRef, useState } from "react";
import { client } from "../../client.ts";
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
          setError(
            userError(
              cause,
              "Falha na preparação do computador. Tente novamente.",
            ),
          );
        } finally {
          active.current = false;
          setBusy(false);
          onBusyChange?.(false);
        }
      }}
    >
      <ChoiceCards
        label="Ambiente de trabalho deste Bot"
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
                ? "Usar contêiner do serviço"
                : local
                  ? "Usar este computador"
                  : "Usar computador do serviço",
            description:
              "Acessa os comandos e arquivos do ambiente onde o Persona está rodando.",
          },
          {
            value: "shared",
            label: "Computador isolado compartilhado",
            description: "Compartilha arquivos com outros Bots. Requer Docker.",
            disabled: !canIsolate,
          },
          {
            value: "dedicated",
            label: "Computador isolado exclusivo",
            description: "Dedicado apenas para este Bot. Requer Docker.",
            disabled: !canIsolate,
          },
        ]}
      />
      <p className="detail-note">
        {mode === "process"
          ? "Ao confirmar, você permite que o Bot execute comandos e gerencie arquivos neste ambiente."
          : "Ao confirmar, a imagem será preparada. As conversas permanecem salvas no Persona."}
        {!canIsolate &&
          " Este servidor ainda não possui suporte a computadores isolados."}
      </p>
      {busy && <p role="status">Preparando ambiente e vinculando o Bot…</p>}
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
            Voltar
          </Button>
        )}
        <Button
          type="submit"
          pending={busy}
          disabled={busy || (mode !== "process" && !canIsolate)}
        >
          {busy
            ? "Preparando…"
            : mode === "process"
              ? "Autorizar e Continuar"
              : "Preparar computador isolado"}
        </Button>
      </div>
    </form>
  );
}
