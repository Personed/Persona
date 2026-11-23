// Tests the Go service cannot pass yet, by the gap that blocks them.
// playwright.config.ts leaves them out when PERSONA_WEB_SERVER=go. Remove a
// title when its gap closes.
export const goGaps: Record<string, string[]> = {
  "computer.* beyond the fixture's get/setup/list stub (assign, prepare, services, watch, takeover)":
    [
      "Bot identity stays consistent and image previews work in drafts and history",
      "a failed browser input pauses the viewer and reconnect never replays it",
      "computer connection failures keep a direct retry",
      "computer viewing uses separate tabs and releases control when closed",
      "contact rows keep their density and searchable previews across selections",
      "contacts keep independent histories, drafts, models, and locations after restart",
      "continuous wheel input keeps takeover active",
      "creation retries keep one contact and themed dialogs work on narrow screens",
      "file drafts, real tools, and published downloads survive Bot switches and restart",
      "first use prepares the default Bot with a computer and model before shared chat",
      "switching contacts keeps the first Bot running while another Bot replies",
      "takeover card first click opens the browser and resumes after completion",
      "takeover controls the Agent browser and preserves its layout through fullscreen",
      "working contacts retain green status across switches, concurrent work, reload, and stop",
    ],
  "engine controls: Go lists Pi's local model IDs without display names, efforts or native controls":
    [
      "Bot model selection saves choices and fits both themes",
      "model choices load from the server and selection survives a restart",
      "model search and themed popups support keyboard, dismissal, and narrow screens",
      "model selection updates immediately and restores a failed save",
      "native Harness controls persist and client accounts retain model access",
    ],
  "untriaged: needs web-server.ts scripting (seeded history, tool calls, approvals, files) or an unported Go behavior":
    [
      "Apps completes and cancels browser OAuth with stored state",
      "Apps manages a real MCP connection and Bot grants across reloads and themes",
      "Bot details share the desktop with chat and cover narrow screens",
      "Markdown keeps one message scale across elements, themes, and text enlargement",
      "Work follows a native turn, opens its record and preserves it after stop and reload",
      "Work renders native search, file and terminal details in both themes",
      "a killed server retains uncertain work and permits an explicit retry",
      "a reply arrives complete while typing survives reloads",
      "attachment retry keeps its request and draft after a lost receipt",
      "chat keeps sending available while the Bot is working",
      "chat reads older pages and retains them through live updates",
      "chat reflows within the frame budget and retains its position",
      "chat routine changes require confirmation and link runs to Work",
      "chat suggestions fill the draft and dismissal stays complete on other devices and after restart",
      "composer fades scrolling messages and covers content below its edge",
      "composer menu adds task materials, keeps drafts, and fits both themes",
      "delivered images form a gallery with viewer downloads and keyboard navigation",
      "image thumbnails scroll horizontally and keep the selected image in view",
      "large histories render a bounded window and retain the earlier reading anchor",
      "latest bubbles stay above the composer after typing, resizing, and reloading",
      "message insertion moves typing continuously and completion fades it out",
      "message media leads captions and bubble tails follow both themes",
      "monitoring stays quiet, delivers one result and opens the native Work record",
      "native questions use the shared card and native replies persist",
      "panel transitions preserve the transcript and draft through rapid toggles and reduced motion",
      "published files wait for the complete reply",
      "returning to a Bot shows retained messages and draft while its stream is delayed",
      "shared question and approval cards persist answers and terminal states",
      "structured messages keep Markdown and private tool results across reloads",
      "two windows can submit while a reply is active",
      "typing dots share the avatar color and follow complete messages",
      "typing follows the Bot and failures retain their error message",
    ],
};

export const goGapPatterns = Object.values(goGaps)
  .flat()
  .map((title) => new RegExp(title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
