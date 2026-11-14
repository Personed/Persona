if (window.top === window) {
  const invoke = (command, args) =>
    window.__TAURI_INTERNALS__.invoke(command, args);
  const unsupported = (error) => Promise.resolve({ ok: false, error });
  window.personaDesktop = Object.freeze({
    platform: "__PLATFORM__",
    nativeMaintenanceConfirmation: true,
    appearance: (theme) => invoke("desktop_appearance", { theme }),
    menu: (scope) => invoke("desktop_menu", scope ? { scope } : {}),
    prepare: () =>
      invoke("desktop_prepare").catch((error) =>
        unsupported(
          error instanceof Error ? error.message : "Preparação indisponível.",
        ),
      ),
    state: () => invoke("desktop_state"),
    maintain: () =>
      invoke("desktop_maintain").catch((error) =>
        unsupported(
          typeof error === "string"
            ? error
            : error instanceof Error
              ? error.message
              : "Não foi possível abrir a confirmação de manutenção.",
        ),
      ),
  });
}
