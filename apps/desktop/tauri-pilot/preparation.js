(() => {
  const bridge = window.personaPreparation;
  const startButton = document.getElementById("start");
  const cancelButton = document.getElementById("cancel");
  const statusText = document.getElementById("status");
  const errorText = document.getElementById("error");
  const track = document.getElementById("progress-track");
  const card = document.querySelector(".progress-card");
  const knownStatuses = new Set([
    "idle",
    "running",
    "cancelling",
    "success",
    "error",
    "cancelled",
  ]);
  let busy = false;
  let polling = false;
  let terminal = false;

  const labels = {
    idle: "Pronto para verificar e preparar as imagens.",
    running: "Preparando imagens. Isso pode levar alguns minutos.",
    cancelling: "Cancelamento solicitado. Aguardando a operação terminar…",
    success: "Preparação concluída.",
    error:
      "Não foi possível preparar as imagens. Verifique se o Docker está disponível e tente novamente.",
    cancelled:
      "Preparação cancelada. Alterações já realizadas no Docker podem permanecer.",
  };
  const phases = {
    checking: "Verificando se o Docker está disponível…",
    downloading: "Baixando imagens necessárias…",
    building: "Construindo imagens necessárias…",
    preparing: labels.running,
  };

  function render(status, phase) {
    card.className = `progress-card${status === "running" || status === "cancelling" ? " is-busy" : status === "success" ? " is-success" : status === "error" ? " is-error" : status === "cancelled" ? " is-cancelled" : ""}`;
    statusText.textContent =
      status === "running" && phase
        ? phases[phase] || labels.running
        : labels[status];
    errorText.hidden = status !== "error";
    errorText.textContent = status === "error" ? labels.error : "";
    track.hidden = status !== "running";
    startButton.disabled = busy || status !== "idle";
    cancelButton.hidden = status !== "running" && status !== "cancelling";
    cancelButton.disabled = status === "cancelling";
  }

  function setFailure() {
    busy = false;
    terminal = true;
    render("error");
  }

  async function readState() {
    if (!bridge || typeof bridge.state !== "function")
      throw new Error("bridge unavailable");
    const result = await bridge.state();
    if (
      result?.ok !== true ||
      !result.value ||
      !knownStatuses.has(result.value.status)
    )
      throw new Error("invalid state");
    return result.value;
  }

  async function syncState() {
    try {
      const value = await readState();
      render(
        value.status,
        value.progress && typeof value.progress.phase === "string"
          ? value.progress.phase
          : "",
      );
      if (value.status === "running" || value.status === "cancelling") {
        busy = true;
        terminal = false;
      }
      if (
        value.status === "success" ||
        value.status === "error" ||
        value.status === "cancelled" ||
        value.status === "idle"
      ) {
        terminal = true;
        busy = false;
      }
      return value.status;
    } catch {
      setFailure();
      return "error";
    }
  }

  async function pollUntilTerminal() {
    if (polling) return;
    polling = true;
    try {
      while (busy && !terminal) {
        const status = await syncState();
        if (status !== "running" && status !== "cancelling") break;
        await new Promise((resolve) => window.setTimeout(resolve, 700));
      }
    } finally {
      polling = false;
    }
  }

  startButton.addEventListener("click", async () => {
    if (busy || !bridge || typeof bridge.start !== "function") return;
    busy = true;
    terminal = false;
    render("running");
    try {
      const result = await bridge.start();
      if (result && result.ok === false) throw new Error("start failed");
      await pollUntilTerminal();
    } catch {
      setFailure();
    }
  });

  cancelButton.addEventListener("click", async () => {
    if (!busy || !bridge || typeof bridge.cancel !== "function") return;
    cancelButton.disabled = true;
    render("cancelling");
    try {
      const result = await bridge.cancel();
      if (result && result.ok === false) throw new Error("cancel failed");
      await pollUntilTerminal();
    } catch {
      setFailure();
    }
  });

  syncState().then((status) => {
    if (status === "running" || status === "cancelling") pollUntilTerminal();
  });
})();
