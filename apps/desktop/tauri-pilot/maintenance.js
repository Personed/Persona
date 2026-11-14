(() => {
  const bridge = window.personaMaintenance;
  const startButton = document.getElementById("start");
  const cancelButton = document.getElementById("cancel");
  const passwordInput = document.getElementById("password");
  const confirmation = document.getElementById("confirmation");
  const choices = document.getElementById("choices");
  const statusText = document.getElementById("status");
  const errorText = document.getElementById("error");
  const track = document.getElementById("progress-track");
  const card = document.querySelector(".progress-card");
  let busy = false;
  let polling = false;
  let terminal = false;
  let currentStatus = "idle";

  function render(value) {
    currentStatus = value.status;
    card.className = `progress-card${busy ? " is-busy" : ""}${value.status === "error" ? " is-error" : value.status === "cancelled" ? " is-cancelled" : ""}`;
    statusText.textContent =
      typeof value.message === "string" && value.message
        ? value.message
        : {
            idle: "Pronto para iniciar.",
            running: "Operação em andamento…",
            awaiting_file: "Aguardando a escolha do arquivo no diálogo nativo…",
            success: "Operação concluída.",
            error: "Não foi possível concluir a operação.",
            cancelled: "Operação cancelada.",
            cancelling: "Solicitação de cancelamento enviada…",
          }[value.status] || "A operação está em andamento.";
    errorText.hidden = value.status !== "error";
    errorText.textContent =
      value.status === "error"
        ? value.message || "Tente novamente ou verifique o arquivo selecionado."
        : "";
    track.hidden = !busy;
    startButton.disabled =
      busy ||
      !["idle", "error", "cancelled"].includes(value.status) ||
      passwordInput.value.length < 12 ||
      !confirmation.checked;
    passwordInput.disabled = busy;
    confirmation.disabled = busy;
    choices.disabled = busy;
    cancelButton.hidden = !busy || value.status === "cancelling";
    cancelButton.disabled = !busy;
    if (["success", "error", "cancelled", "idle"].includes(value.status)) {
      busy = false;
      terminal = true;
    }
  }

  async function readState() {
    if (!bridge || typeof bridge.state !== "function")
      throw new Error("bridge unavailable");
    const result = await bridge.state();
    if (
      result?.ok !== true ||
      !result.value ||
      typeof result.value.status !== "string"
    )
      throw new Error("invalid state");
    return result.value;
  }

  function fail(message) {
    busy = false;
    terminal = true;
    render({ status: "error", message });
  }

  async function syncState() {
    try {
      const value = await readState();
      render(value);
      if (["running", "awaiting_file", "cancelling"].includes(value.status)) {
        busy = true;
        terminal = false;
        render(value);
      }
      return value.status;
    } catch {
      fail("Não foi possível consultar o estado da operação.");
      return "error";
    }
  }

  async function pollUntilTerminal() {
    if (polling) return;
    polling = true;
    try {
      while (busy && !terminal) {
        const status = await syncState();
        if (!["running", "awaiting_file", "cancelling"].includes(status)) break;
        await new Promise((resolve) => window.setTimeout(resolve, 700));
      }
    } finally {
      polling = false;
    }
  }

  function updateReady() {
    startButton.disabled =
      busy ||
      !["idle", "error", "cancelled"].includes(currentStatus) ||
      passwordInput.value.length < 12 ||
      !confirmation.checked;
  }
  passwordInput.addEventListener("input", updateReady);
  confirmation.addEventListener("change", updateReady);

  startButton.addEventListener("click", async () => {
    if (
      busy ||
      passwordInput.value.length < 12 ||
      !confirmation.checked ||
      !bridge ||
      typeof bridge.start !== "function"
    )
      return;
    let password = passwordInput.value;
    passwordInput.value = "";
    busy = true;
    terminal = false;
    render({ status: "running", message: "Preparando a operação…" });
    try {
      const starting = bridge.start(password);
      password = "";
      const result = await starting;
      if (result?.ok === false) throw new Error(result.error || "start failed");
      await pollUntilTerminal();
    } catch (error) {
      fail(error?.message || "Não foi possível iniciar a operação.");
    }
  });

  cancelButton.addEventListener("click", async () => {
    if (!busy || !bridge || typeof bridge.cancel !== "function") return;
    cancelButton.disabled = true;
    statusText.textContent = "Enviando solicitação de cancelamento…";
    try {
      const result = await bridge.cancel();
      if (result?.ok === false) throw new Error("cancel failed");
      await pollUntilTerminal();
    } catch {
      fail("Não foi possível solicitar o cancelamento.");
    }
  });

  syncState().then((status) => {
    updateReady();
    if (["running", "awaiting_file", "cancelling"].includes(status)) {
      busy = true;
      terminal = false;
      pollUntilTerminal();
    }
  });
})();
