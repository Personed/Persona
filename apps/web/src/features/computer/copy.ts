import { getLanguage } from "../i18n/context.tsx";

const zhCN = {
  state: {
    running: "运行中",
    stopped: "已停止",
    missing: "尚未创建",
    unavailable: "连接不可用",
  },
  boundary: {
    host: "当前电脑",
    application: "应用容器",
    sandbox: "独立工作沙箱",
  },
  network: {
    host: "服务环境网络",
    none: "网络已关闭",
    restricted: "公网出网，阻止内部地址",
    web: "HTTP(S) 代理出网，阻止内部地址",
  },
};

const ptBR = {
  state: {
    running: "Em execução",
    stopped: "Parado",
    missing: "Não criado",
    unavailable: "Conexão indisponível",
  },
  boundary: {
    host: "Computador atual",
    application: "Contêiner da aplicação",
    sandbox: "Sandbox de trabalho isolada",
  },
  network: {
    host: "Rede do ambiente do serviço",
    none: "Rede desativada",
    restricted: "Internet pública liberada, bloqueando rede interna",
    web: "Proxy HTTP(S) para internet, bloqueando rede interna",
  },
};

export const computerLabels = new Proxy(ptBR, {
  get(_target, prop: keyof typeof ptBR) {
    const lang = getLanguage();
    if (lang === "zh-CN") return zhCN[prop];
    return ptBR[prop];
  },
});
