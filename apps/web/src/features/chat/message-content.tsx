import type { ChatMessage } from "@persona/agent/messages";
import { MessageMarkdown } from "@persona/ui/markdown";
import { chatCopy } from "./copy.ts";

export function MessageContent({
  content,
  user = false,
}: {
  content: ChatMessage["content"];
  user?: boolean;
}) {
  return content
    .toSorted((a, b) => Number(b.type === "image") - Number(a.type === "image"))
    .map((part) => {
      switch (part.type) {
        case "text":
          return user ? (
            <div className="user-text" key={part.key}>
              {part.text}
            </div>
          ) : (
            <MessageMarkdown key={part.key}>{part.text}</MessageMarkdown>
          );
        case "image":
          return (
            <img
              className="message-image"
              key={part.key}
              src={`data:${part.mimeType};base64,${part.data}`}
              alt={chatCopy.image}
            />
          );
        case "thinking":
        case "toolCall":
          return null;
      }
      const exhaustive: never = part;
      return exhaustive;
    });
}
