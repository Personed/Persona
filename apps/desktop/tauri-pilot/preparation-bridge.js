const invoke = (command, args) =>
  window.__TAURI_INTERNALS__.invoke(command, args);
window.personaPreparation = Object.freeze({
  state: () => invoke("preparation_state"),
  start: () => invoke("preparation_start"),
  cancel: () => invoke("preparation_cancel"),
});
