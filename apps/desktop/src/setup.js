const bridge = window.personaSetup;
const form = document.querySelector("#setup");
const choices = document.querySelector("#choices");
const remote = document.querySelector("#remote");
const localOptions = document.querySelector("#local");
const address = document.querySelector("#url");
const name = document.querySelector("#name");
const saved = document.querySelector("#saved");
const progress = document.querySelector("#progress");
const phase = document.querySelector("#phase");
const error = document.querySelector("#error");
const docker = document.querySelector("#docker");
const cancel = document.querySelector("#cancel");
const next = document.querySelector("#continue");
let connections = [];
let stage;
document.documentElement.dataset.platform = bridge.platform;

function setNext(label, pending = false) {
  next.textContent = label;
  next.setAttribute("aria-busy", String(pending));
  if (pending) {
    const spinner = document.createElement("span");
    spinner.className = "button-spinner";
    spinner.setAttribute("aria-hidden", "true");
    next.prepend(spinner);
  }
}

function updateMode() {
  const local = new FormData(form).get("mode") === "local";
  remote.hidden = local;
  localOptions.hidden = !local;
  address.required = !local;
  name.required = !local;
  error.hidden = true;
  docker.hidden = true;
  setNext("Continuar");
}
const errorTranslations = {
  "无法连接 Docker。请安装并启动 Docker Desktop，然后重试。":
    "Não foi possível conectar ao Docker. Certifique-se de que o Docker Desktop está em execução e tente novamente.",
  "请将 Docker 切换为 Linux 容器模式。":
    "Alterne o Docker para o modo de contêineres Linux.",
  "安装文件缺少 Docker 运行包。请重新安装 Persona。":
    "Arquivos de instalação não contêm o pacote Docker. Reinstale o Persona.",
  "首次准备可能需要几分钟…": "A preparação inicial pode levar alguns minutos…",
  "正在启动 Persona…": "Iniciando Persona…",
  "正在检查 Docker 和目录权限…":
    "Verificando o Docker e permissões de diretório…",
  "正在准备 Linux 工作镜像…": "Preparando imagens de trabalho Linux…",
  "Persona 已启动。": "Persona iniciado.",
  "无法执行安装命令。请检查运行环境和权限。":
    "Não foi possível executar o comando de instalação. Verifique o ambiente e as permissões.",
  "安装步骤未完成。请检查网络和运行环境后重试。":
    "Etapa de instalação não concluída. Verifique a rede e o ambiente e tente novamente.",
};

form.addEventListener("change", updateMode);
bridge.progress((value) => {
  stage = value.phase;
  phase.textContent = errorTranslations[value.message] || value.message;
});
cancel.addEventListener("click", () => bridge.cancel());
docker.addEventListener("click", () => bridge.docker());
form.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (next.disabled) return;
  const data = new FormData(form);
  const local = data.get("mode") === "local";
  choices.disabled = true;
  form.setAttribute("aria-busy", "true");
  setNext(local ? "Iniciando…" : "Conectando…", true);
  next.disabled = true;
  remote.inert = true;
  localOptions.disabled = true;
  progress.hidden = false;
  cancel.hidden = false;
  error.hidden = true;
  docker.hidden = true;
  stage = undefined;
  phase.textContent = local
    ? "Preparando serviço local…"
    : "Verificando servidor…";
  try {
    const result = await bridge.submit(
      local
        ? { mode: "local", runtime: data.get("runtime") }
        : { mode: "existing", url: address.value, name: name.value },
    );
    if (result.cancelled) {
      error.textContent = "Operação cancelada. Você pode tentar novamente.";
      error.hidden = false;
      return;
    }
    if (!result.ok) throw new Error(result.error);
  } catch (cause) {
    const message = errorTranslations[cause.message] || cause.message;
    error.textContent = message;
    error.hidden = false;
    docker.hidden = !local || stage !== "checking";
    error.focus();
  } finally {
    choices.disabled = false;
    form.setAttribute("aria-busy", "false");
    setNext(error.hidden ? "Continuar" : "Tentar novamente");
    next.disabled = false;
    remote.inert = false;
    localOptions.disabled = false;
    progress.hidden = true;
    cancel.hidden = true;
  }
});
bridge.state().then((state) => {
  connections = state.instances.filter((item) => item.mode === "existing");
  for (const connection of connections) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "button";
    button.dataset.variant = "secondary";
    button.textContent = connection.name;
    button.addEventListener("click", () => {
      address.value = connection.url;
      name.value = connection.name;
      address.focus();
    });
    saved.append(button);
  }
  document.querySelector("#saved-label").hidden = !connections.length;
  if (state.selected?.mode === "local")
    form.querySelector(
      `[name="runtime"][value="${state.selected.runtime ?? "native"}"]`,
    ).checked = true;
  if (!state.prepare && state.selected?.mode === "existing") {
    form.querySelector('[value="existing"]').checked = true;
    address.value = state.selected.url;
    name.value = state.selected.name;
    updateMode();
  }
  if (state.error) {
    error.textContent = state.error;
    error.hidden = false;
  }
  if (state.prepare) form.requestSubmit();
});
