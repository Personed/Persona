const listeners = new Set();
window.personaSetup = Object.freeze({
  platform: "__PLATFORM__",
  state: () => window.__TAURI_INTERNALS__.invoke("setup_state"),
  submit: (value) =>
    window.__TAURI_INTERNALS__.invoke("setup_submit", { value }),
  cancel: () => window.__TAURI_INTERNALS__.invoke("setup_cancel"),
  docker: () => window.__TAURI_INTERNALS__.invoke("setup_docker"),
  progress: (listener) => listeners.add(listener),
});
window.addEventListener("persona-setup-progress", (event) => {
  for (const listener of listeners) listener(event.detail);
});
