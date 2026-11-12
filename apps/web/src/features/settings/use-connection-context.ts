import { useEffect, useState } from "react";
import { client } from "../../client.ts";
import { userError } from "../user-error.ts";

export function useConnectionContext(botId?: number) {
  const [target, setTarget] = useState<{
    id: number;
    name: string;
    computer: string;
  }>();
  const [error, setError] = useState("");
  useEffect(() => {
    let current = true;
    void Promise.all([client.bots.list(), client.computer.list()]).then(
      ([bots, computers]) => {
        if (!current) return;
        const bot =
          botId === undefined
            ? bots[0]
            : bots.find((item) => item.id === botId);
        if (!bot) {
          setError("Crie um Bot primeiro.");
          return;
        }
        const binding = computers.bindings[String(bot.id)];
        setTarget({
          id: bot.id,
          name: bot.name,
          computer:
            computers.items.find((item) => item.id === binding?.computerId)
              ?.name ?? "Nenhum computador atribuído",
        });
      },
      (cause) => {
        if (current)
          setError(userError(cause, "Falha ao ler informações de conexão."));
      },
    );
    return () => {
      current = false;
    };
  }, [botId]);
  return { target, error };
}
