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
  // Owned by the memory/delivery tool registration session.
  "product tools: Go registers no approved, file-publishing product tool for scripted calls (attach_file delivery)":
    [
      "Bot identity stays consistent and image previews work in drafts and history",
      "attachment retry keeps its request and draft after a lost receipt",
      "composer menu adds task materials, keeps drafts, and fits both themes",
      "delivered images form a gallery with viewer downloads and keyboard navigation",
      "file drafts, real tools, and published downloads survive Bot switches and restart",
      "image thumbnails scroll horizontally and keep the selected image in view",
      "message media leads captions and bubble tails follow both themes",
      "published files wait for the complete reply",
      "structured messages keep Markdown and private tool results across reloads",
      "typing follows the Bot and failures retain their error message",
    ],
  "routine runner: persona.routine tasks and work.routines.test are unported (Go only runs engine/chat turns)":
    [
      "Bot details manage identity, memory, shared skills, routines and actual prompts",
    ],
  "chat window: Go reads a fixed 50-entry page; pi-durable's view window grows to 100 entries and pages older ones":
    [
      "chat reads older pages and retains them through live updates",
      "large histories render a bounded window and retain the earlier reading anchor",
      "panel transitions preserve the transcript and draft through rapid toggles and reduced motion",
    ],
  "routine tools: serve does not offer routine_* to Pi's native MCP catalog":
    ["chat routine changes require confirmation and link runs to Work"],
  "native questions: Pi's ask_user is not stored as a shared question card":
    [
      "native questions use the shared card and native replies persist",
      "shared question and approval cards persist answers and terminal states",
    ],
  "native Work records: work.records does not yet project native turns":
    [
      "Work follows a native turn, opens its record and preserves it after stop and reload",
      "monitoring stays quiet, delivers one result and opens the native Work record",
    ],
  "MCP apps: Go has no application catalog, connection runner, or OAuth callback composed into serve":
    [
      "Apps completes and cancels browser OAuth with stored state",
      "Apps manages a real MCP connection and Bot grants across reloads and themes",
    ],
  "native Harness controls: unhosted persona serve offers no device pairing":
    ["native Harness controls persist and client accounts retain model access"],
};

export const goGapPatterns = Object.values(goGaps)
  .flat()
  .map((title) => new RegExp(title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
