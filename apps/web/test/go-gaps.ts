// Tests the Go service cannot pass yet, by the gap that blocks them.
// playwright.config.ts leaves them out when PERSONA_WEB_SERVER=go. Remove a
// title when its gap closes.
export const goGaps: Record<string, string[]> = {
  // The spec injects approval requests through the TypeScript runtime; the Go
  // approver keeps pending requests in memory, so the fixture cannot open one
  // over the wire.
  "shared approval cards: the Go fixture cannot open an approval request": [
    "shared question and approval cards persist answers and terminal states",
  ],
};

export const goGapPatterns = Object.values(goGaps)
  .flat()
  .map((title) => new RegExp(title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
