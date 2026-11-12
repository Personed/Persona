import { Button } from "@persona/ui/controls";
import { TextField } from "@persona/ui/field";
import { SetupPage } from "@persona/ui/setup";
import { type ReactNode, useCallback, useEffect, useState } from "react";
import { Onboarding } from "../onboarding/root.tsx";
import { SettingsRoot } from "../settings/root.tsx";
import { Titlebar } from "../shell/titlebar.tsx";
import { hostingRequest } from "./api.ts";
import "./style.css";

type Session = { device: { id: string; scope: string } };

export function HostingRoot({ children }: { children: ReactNode }) {
  const [state, setState] = useState<
    "loading" | "pair" | "ready" | "standalone"
  >("loading");
  const [session, setSession] = useState<Session>();
  const [error, setError] = useState("");
  const [code, setCode] = useState("");
  const [label, setLabel] = useState("Meu dispositivo");
  const [busy, setBusy] = useState(false);
  const refresh = useCallback(async () => {
    const info = await fetch("/hosting/info");
    if (info.status === 404) {
      setState("standalone");
      return;
    }
    if (!info.ok)
      throw new Error("Temporariamente incapaz de conectar ao Persona.");
    const response = await fetch("/hosting/session");
    if (response.status === 401) {
      setState("pair");
      return;
    }
    if (!response.ok)
      throw new Error("Não foi possível carregar o status do dispositivo.");
    const value: Session = await response.json();
    setSession(value);
    setState("ready");
  }, []);
  useEffect(() => {
    void refresh().catch((error) => setError(error.message));
    const unauthorized = () => {
      setSession(undefined);
      setState("pair");
    };
    window.addEventListener("persona:pair", unauthorized);
    return () => window.removeEventListener("persona:pair", unauthorized);
  }, [refresh]);
  if (state === "ready" || state === "standalone")
    return (
      <SettingsRoot
        admin={session?.device.scope === "admin" || state === "standalone"}
        deviceId={session?.device.id}
        standalone={state === "standalone"}
      >
        <Onboarding
          admin={session?.device.scope === "admin" || state === "standalone"}
          deviceId={session?.device.id}
          standalone={state === "standalone"}
        >
          {children}
        </Onboarding>
      </SettingsRoot>
    );
  return (
    <div className="setup-window">
      <Titlebar />
      <SetupPage
        title={
          state === "pair"
            ? "Conectar a este Persona"
            : "Conectando ao Persona…"
        }
        description={
          state === "pair"
            ? "Digite o código fornecido para continuar."
            : undefined
        }
      >
        {state === "loading" && (
          <p role="status" className="detail-note">
            Carregando informações da conexão…
          </p>
        )}
        {state === "pair" && (
          <form
            className="setup-form"
            aria-busy={busy}
            onSubmit={async (event) => {
              event.preventDefault();
              if (busy) return;
              setBusy(true);
              setError("");
              try {
                await hostingRequest("exchange", { code, label });
                setCode("");
                await refresh();
              } catch (error) {
                setError((error as Error).message);
              } finally {
                setBusy(false);
              }
            }}
          >
            <TextField
              label="Nome do dispositivo"
              value={label}
              maxLength={80}
              onValueChange={setLabel}
              required
              disabled={busy}
            />
            <TextField
              label="Código"
              value={code}
              autoComplete="off"
              onValueChange={setCode}
              required
              disabled={busy}
            />
            {error && (
              <p className="alert" role="alert">
                {error}
              </p>
            )}
            <div className="setup-actions">
              <Button type="submit" pending={busy}>
                {busy ? "Conectando…" : "Conectar"}
              </Button>
            </div>
          </form>
        )}
        {state === "loading" && error && (
          <p className="alert" role="alert">
            {error}
          </p>
        )}
        {state === "loading" && error && (
          <Button
            variant="secondary"
            onClick={() =>
              void refresh().catch((error) => setError(error.message))
            }
          >
            Tentar novamente
          </Button>
        )}
      </SetupPage>
    </div>
  );
}
