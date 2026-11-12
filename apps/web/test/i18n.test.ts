import { afterEach, expect, test, vi } from "vitest";
import { getStoredLanguage } from "../src/features/i18n/context.tsx";
import { languages } from "../src/features/i18n/types.ts";

afterEach(() => vi.unstubAllGlobals());

test("legacy English preference falls back to Portuguese", () => {
  vi.stubGlobal("localStorage", { getItem: () => "en-US" });
  expect(getStoredLanguage()).toBe("pt-BR");
});

test("the language picker only offers complete translations", () => {
  expect(languages.map(({ value }) => value)).toEqual(["pt-BR", "zh-CN"]);
});
