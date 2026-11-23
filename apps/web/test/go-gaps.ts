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
  "chat steering: the Go fixture holds the model request after ls, not the tool, so input steered while it waits joins a later turn":
    ["chat keeps sending available while the Bot is working"],
  "routine tools: Pi's scripted routine_create call shows no routine approval card in chat":
    ["chat routine changes require confirmation and link runs to Work"],
  "native questions: Pi's ask_user is not stored as a shared question card": [
    "native questions use the shared card and native replies persist",
    "shared question and approval cards persist answers and terminal states",
  ],
  "native Work records: work.records does not yet project native turns": [
    "Work follows a native turn, opens its record and preserves it after stop and reload",
    "monitoring stays quiet, delivers one result and opens the native Work record",
  ],
  "native Harness controls: unhosted persona serve offers no device pairing": [
    "native Harness controls persist and client accounts retain model access",
  ],
};

export const goGapPatterns = Object.values(goGaps)
  .flat()
  .map((title) => new RegExp(title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
