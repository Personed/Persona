import { getLanguage, translate } from "../i18n/context.tsx";

const failed = "连接未完成。请重试。";

function hostingError(message: unknown): string {
  const text = typeof message === "string" && message.trim() ? message : failed;
  const translated = translate(text);
  return getLanguage() !== "zh-CN" && /[\u3400-\u9fff]/.test(translated)
    ? translate(failed)
    : translated;
}

export async function hostingRequest<T>(
  path: string,
  input?: unknown,
  method = input === undefined ? "GET" : "POST",
  signal?: AbortSignal,
): Promise<T> {
  const response = await fetch(`/hosting/${path}`, {
    method,
    signal,
    credentials: "same-origin",
    ...(input === undefined
      ? {}
      : {
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(input),
        }),
  });
  if (!response.ok) {
    if (response.status === 401)
      window.dispatchEvent(new Event("persona:pair"));
    const body = await response.json().catch(() => ({}));
    throw new Error(hostingError(body.error));
  }
  return response.json();
}
