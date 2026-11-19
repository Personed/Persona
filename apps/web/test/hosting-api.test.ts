import { afterEach, expect, test, vi } from "vitest";
import { hostingRequest } from "../src/features/hosting/api.ts";

afterEach(() => vi.unstubAllGlobals());

function reject(body: unknown) {
  vi.stubGlobal(
    "fetch",
    async () => new Response(JSON.stringify(body), { status: 403 }),
  );
}

test("hosting errors are shown in the interface language", async () => {
  reject({ error: "请求来源不匹配。" });
  await expect(hostingRequest("exchange", {})).rejects.toThrow(
    "A origem da requisição não corresponde.",
  );
});

test("untranslated hosting errors fall back to a translated message", async () => {
  reject({ error: "未知错误。" });
  await expect(hostingRequest("session")).rejects.toThrow(
    "A conexão não foi concluída. Tente novamente.",
  );
});
