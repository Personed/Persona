const invoke = (command, args) =>
  window.__TAURI_INTERNALS__.invoke(command, args);
window.personaMaintenance = Object.freeze({
  state: () => invoke("maintenance_state"),
  start: (password) =>
    invoke("maintenance_start", {
      password,
      action: document.querySelector('input[name="action"]:checked')?.value,
    }),
  cancel: () => invoke("maintenance_cancel"),
});
