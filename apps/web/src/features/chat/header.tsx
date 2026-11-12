import type { Bot } from "@persona/agent/bots";
import { BotAvatar } from "@persona/ui/avatar";
import { Button } from "@persona/ui/controls";

export function ChatHeader({
  bot,
  busy,
  detailsOpen,
  onDetails,
}: {
  bot: Bot;
  busy: boolean;
  detailsOpen: boolean;
  onDetails: () => void;
}) {
  return (
    <header className="chat-header">
      <Button
        id="details-trigger"
        variant="ghost"
        size="small"
        className="chat-identity"
        aria-label={bot.name}
        title={bot.name}
        aria-expanded={detailsOpen}
        aria-controls={detailsOpen ? "bot-details" : undefined}
        onClick={onDetails}
      >
        <BotAvatar
          seed={bot.avatar}
          size="panel"
          state={busy ? "running" : "idle"}
        />
      </Button>
    </header>
  );
}
