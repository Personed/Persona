// Tests the Go service cannot pass yet, by the gap that blocks them.
// playwright.config.ts leaves them out when PERSONA_WEB_SERVER=go. Remove a
// title when its gap closes.
export const goGaps: Record<string, string[]> = {
  // Waits on the worker's prepare/install/mode/settle/view port (another session).
  "computer viewer: the Go worker does not yet stream the work computer's screen or switch browser mode":
    [
      "a failed browser input pauses the viewer and reconnect never replays it",
      "computer viewing uses separate tabs and releases control when closed",
      "continuous wheel input keeps takeover active",
      "takeover card first click opens the browser and resumes after completion",
      "takeover controls the Agent browser and preserves its layout through fullscreen",
    ],
  "parallel tools: Pi runs one response's tool calls concurrently, so attach_file can read report.md before the edit beside it lands; the TypeScript harness runs them in order":
    [
      // The fixture holds the model request after ls, not the tool, so input
      // steered while it waits joins a later turn instead of that request.
      "chat keeps sending available while the Bot is working",
      "file drafts, real tools, and published downloads survive Bot switches and restart",
    ],
  // The spec injects approval requests through the TypeScript worker's
  // computer runtime; go-server.ts has no command that opens one in serve.
  "shared approval cards: the Go fixture cannot open an approval request": [
    "shared question and approval cards persist answers and terminal states",
  ],
  "faux failure: Pi's OpenAI-compatible provider always gives a failed reply an error message, so the fixture cannot script web-server.ts's failure without one":
    ["typing follows the Bot and failures retain their error message"],
  "native questions: Pi's ask_user is not stored as a shared question card": [
    "native questions use the shared card and native replies persist",
  ],
  "native Work records: work.records does not yet project native turns": [
    "Work follows a native turn, opens its record and preserves it after stop and reload",
  ],
};

export const goGapPatterns = Object.values(goGaps)
  .flat()
  .map((title) => new RegExp(title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
