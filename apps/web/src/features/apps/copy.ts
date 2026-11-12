import { getLanguage } from "../i18n/context.tsx";

const zhCN = {
  status: {
    configured: "已配置",
    ready: "可用",
    authorization_required: "待授权",
    failed: "检查失败",
  },
  readError: "连接读取失败，请重试。",
  nativeReadError: "原生连接读取失败，请重试。",
  secretHint: "留空保留已有配置和凭据。填写新配置会替换它们。",
  nativeHint:
    "原生配置只读。权限和登录由 Harness 管理。配置存在不表示本轮已加载。",
  capabilitiesHint:
    "此接入保留工具、资源和提示词。目前不声明 sampling、elicitation 或 roots 能力。",
};

const ptBR = {
  status: {
    configured: "Configurado",
    ready: "Disponível",
    authorization_required: "Autorização necessária",
    failed: "Falha na verificação",
  },
  readError: "Falha ao ler conexão, tente novamente.",
  nativeReadError: "Falha ao ler conexão nativa, tente novamente.",
  secretHint:
    "Deixe em branco para manter a configuração existente. Preencher irá substituí-la.",
  nativeHint:
    "Configuração nativa somente leitura. Permissões e login gerenciados pelo Harness.",
  capabilitiesHint:
    "Esta conexão preserva ferramentas, recursos e prompts. Não declara recursos de sampling ou roots no momento.",
};

export const appsCopy = new Proxy(ptBR, {
  get(_target, prop: keyof typeof ptBR) {
    const lang = getLanguage();
    if (lang === "zh-CN") return zhCN[prop];
    return ptBR[prop];
  },
});
