import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { ORPCError } from "@orpc/server";
import type { Bots } from "@persona/agent/bots";
import { z } from "zod";

export const botTarget = z.object({ botId: z.number().int().nonnegative() });

export async function botConversation(bots: Bots, botId: number) {
  const conversation = await bots.conversation(botId, BACKGROUND_CONTEXT);
  if (!conversation) throw new ORPCError("NOT_FOUND");
  return conversation;
}
