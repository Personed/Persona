if (window.top === window) {
  const invoke = (command, args) =>
    window.__TAURI_INTERNALS__.invoke(command, args);
  const unsupported = (error) => Promise.resolve({ ok: false, error });
  window.personaDesktop = Object.freeze({
    platform: "__PLATFORM__",
    appearance: (theme) => invoke("desktop_appearance", { theme }),
    menu: (scope) => invoke("desktop_menu", scope ? { scope } : {}),
    prepare: () =>
      unsupported(
        "A preparação de Docker não está disponível neste piloto temporário.",
      ),
    state: () => invoke("desktop_state"),
    maintain: () =>
      unsupported(
        "Backup e restauração não estão disponíveis neste piloto temporário.",
      ),
  });
}
