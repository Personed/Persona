import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { expect, test } from "vitest";
import {
  readConversationHistory,
  readConversationPage,
} from "../src/chat-view.ts";
import { messageText } from "../src/messages.ts";
import { runtimeFixture } from "./fixtures/runtime.ts";

test("durable chat views page older entries without losing full history reads", async () => {
  const { harness, conversation } = await runtimeFixture();
  await harness.commit(async (tx) => {
    for (let index = 0; index < 750; index++)
      await tx.appendEntry(conversation.id, {
        kind: "test.message",
        model: [
          {
            role: "user",
            content: [{ type: "text", text: `message ${index}` }],
            timestamp: index,
          },
        ],
      });
  }, context);

  const latest = await readConversationPage(conversation, context);
  expect(latest.messages).toHaveLength(50);
  expect(latest.before).toBeDefined();

  const earlier = await readConversationPage(
    conversation,
    context,
    latest.before,
  );
  expect(earlier.messages).toHaveLength(200);
  expect(earlier.before).toBeDefined();
  expect(earlier.messages.map(messageText)[0]).toBe("message 500");

  const oldestPage = await readConversationPage(
    conversation,
    context,
    earlier.before,
  );
  expect(oldestPage.messages).toHaveLength(200);
  expect(oldestPage.messages.map(messageText)[0]).toBe("message 300");

  expect(
    (await readConversationHistory(conversation, context)).entries,
  ).toHaveLength(750);
});
