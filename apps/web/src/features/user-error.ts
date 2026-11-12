import { ORPCError } from "@orpc/client";
import { getLanguage } from "./i18n/context.tsx";
import { translations } from "./i18n/translations.ts";

const zhErrors: Record<string, string> = {
  UNAUTHORIZED: "登录已过期，请重新登录。",
  FORBIDDEN: "当前账号没有这项权限。",
  NOT_FOUND: "内容不存在，或当前服务尚不支持此操作。",
  CONFLICT: "内容已在别处更新，请刷新后重试。",
  PRECONDITION_FAILED: "当前状态不允许此操作。",
  BAD_REQUEST: "提交的内容不完整，请检查后重试。",
  SERVICE_UNAVAILABLE: "服务暂时不可用，请稍后重试。",
  NOT_IMPLEMENTED: "当前服务版本不支持此操作。",
  TOO_MANY_REQUESTS: "操作太频繁，请稍后再试。",
  TIMEOUT: "服务响应超时，请重试。",
};

const ptErrors: Record<string, string> = {
  UNAUTHORIZED: "Sessão expirada. Faça login novamente.",
  FORBIDDEN: "Sua conta não tem permissão para esta ação.",
  NOT_FOUND: "Conteúdo não encontrado ou operação não suportada.",
  CONFLICT:
    "O conteúdo foi atualizado em outro local. Atualize e tente novamente.",
  PRECONDITION_FAILED: "O estado atual não permite esta operação.",
  BAD_REQUEST: "Dados incompletos ou inválidos. Verifique e tente novamente.",
  SERVICE_UNAVAILABLE:
    "Serviço temporariamente indisponível. Tente mais tarde.",
  NOT_IMPLEMENTED: "A versão atual do serviço não suporta esta ação.",
  TOO_MANY_REQUESTS:
    "Muitas requisições. Aguarde um momento e tente novamente.",
  TIMEOUT: "Tempo limite esgotado. Tente novamente.",
};

export function userError(cause: unknown, fallback: string): string {
  if (cause instanceof ORPCError) {
    const isZh = getLanguage() === "zh-CN";
    const dict = isZh ? zhErrors : ptErrors;
    const knownError = cause.code ? dict[cause.code] : undefined;
    if (knownError) return knownError;
    const message = cause.message.trim();
    if (!isZh && /[\u3400-\u9fff]/.test(message)) {
      return translations["pt-BR"][message] || fallback;
    }
    if (/[\u3400-\u9fff]/.test(message)) return message;
    return message || fallback;
  }
  if (cause instanceof TypeError) return fallback;
  if (cause instanceof Error && cause.message.trim()) {
    const isZh = getLanguage() === "zh-CN";
    if (!isZh && /[\u3400-\u9fff]/.test(cause.message)) {
      return translations["pt-BR"][cause.message] || fallback;
    }
    return cause.message;
  }
  return fallback;
}
