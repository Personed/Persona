// Tests the Go service cannot pass yet, by the gap that blocks them.
// playwright.config.ts leaves them out when PERSONA_WEB_SERVER=go. Remove a
// title when its gap closes.
export const goGaps: Record<string, string[]> = {
  // These drive a real browser page: the fixture needs the takeover page,
  // state and metrics endpoints on the service origin, scripted browser tool
  // calls (web-server.ts "takeover" mode) and an open-takeover-page command
  // that reaches the Bot's agent-browser — none of which exist on the Go
  // fixture yet.
  "computer viewer: the Go fixture cannot script the takeover browser flow": [
    "a failed browser input pauses the viewer and reconnect never replays it",
    "continuous wheel input keeps takeover active",
    "takeover card first click opens the browser and resumes after completion",
    "takeover controls the Agent browser and preserves its layout through fullscreen",
  ],
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
