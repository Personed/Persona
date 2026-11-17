import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai/models";
import {
  fauxAssistantMessage,
  fauxProvider,
} from "@earendil-works/pi-ai/providers/faux";
import { getOrThrow } from "@earendil-works/pi-durable/env";
import { createORPCClient } from "@orpc/client";
import { RPCLink } from "@orpc/client/fetch";
import type { RouterClient } from "@orpc/server";
import { openChat } from "@persona/agent/chat";
import { storedComputers } from "@persona/agent/computer/storage";
import { parseConfig } from "@persona/agent/config";
import { messageText } from "@persona/agent/messages";
import { expect, onTestFinished, test } from "vitest";
import {
  prepareLocalInstallation,
  type Release,
} from "../../../packages/hosting/src/install.ts";
import {
  backupInstallation,
  restoreInstallation,
} from "../../../packages/hosting/src/maintenance.ts";
import { createApp } from "../src/app.ts";
import type { AppRouter } from "../src/router.ts";

const release: Release = {
  version: "test",
  workImage: "test-image:1",
  controllerEntry: "controller.ts",
  serverEntry: "server.ts",
  bridgeEntry: "bridge.ts",
  staticDir: "web",
};

function apiClient(runtime: Awaited<ReturnType<typeof openChat>>) {
  const app = createApp(
    runtime.bots,
    runtime.models,
    runtime.files,
    runtime.work,
    undefined,
    process.cwd(),
  );
  const client: RouterClient<AppRouter> = createORPCClient(
    new RPCLink({
      url: "http://localhost/rpc",
      fetch: async (request, init) => app.fetch(new Request(request, init)),
    }),
  );
  return client;
}

test("encrypted backups preserve computer permissions, chat, memory, attachments and native workspaces", async () => {
  const directory = await mkdtemp(join(tmpdir(), "persona-backup-test-"));
  onTestFinished(() => rm(directory, { recursive: true, force: true }));
  const root = join(directory, "host");
  const installation = await prepareLocalInstallation(root, release);
  const context = BACKGROUND_CONTEXT;
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  const runtime = await openChat(
    context,
    parseConfig(
      {
        ...installation.config,
        model: { provider: "faux", modelId: "faux-1" },
      },
      installation.configFile,
    ),
    { models },
  );
  let botId: number;
  let attachmentId: string;
  let memory: Awaited<ReturnType<typeof runtime.work.memory.save>>;
  try {
    await runtime.computer.authorize("process", true, context);
    const bot = await runtime.bots.create(
      {
        name: "Writer",
        model: { provider: "faux", modelId: "faux-1" },
        computerId: "process",
        requestId: crypto.randomUUID(),
      },
      context,
    );
    botId = bot.id;
    const client = apiClient(runtime);
    const uploaded = await client.files.upload({
      botId,
      file: new File(["A source file that must survive restore"], "source.txt"),
    });
    attachmentId = uploaded.id;
    memory = await client.work.memory.save({
      botId,
      id: crypto.randomUUID(),
      text: "Keep the backup test preference",
      shared: false,
      revision: 0,
      requestId: crypto.randomUUID(),
    });
    faux.setResponses([fauxAssistantMessage("The attached source is saved.")]);
    await client.send({
      botId,
      text: "Read the attached source",
      attachments: [uploaded.id],
      requestId: crypto.randomUUID(),
    });
    const conversation = await runtime.bots.conversation(botId, context);
    if (!conversation) throw new Error("Bot conversation missing");
    await conversation.waitForIdle(context);

    const chat = await client.get({ botId });
    expect(chat.messages).toContainEqual(
      expect.objectContaining({
        role: "user",
        content: [
          expect.objectContaining({
            type: "text",
            text: "Read the attached source",
          }),
        ],
        attachments: [
          expect.objectContaining({ id: uploaded.id, name: "source.txt" }),
        ],
      }),
    );
    expect(chat.messages.map(messageText)).toContain(
      "The attached source is saved.",
    );
    expect((await client.work.get({ botId })).memory).toContainEqual(memory);
    expect(
      await (await client.files.download({ botId, id: uploaded.id })).text(),
    ).toBe("A source file that must survive restore");

    const workspace = await runtime.computer.workspace(bot.id, context);
    getOrThrow(
      await workspace.writeFile(
        await workspace.path("draft.txt"),
        new TextEncoder().encode("A saved draft"),
        context,
      ),
    );
  } finally {
    await runtime.close();
  }
  const records = await storedComputers(join(root, "data", "agent.sqlite"));
  await writeFile(
    installation.configFile,
    JSON.stringify({ ...installation.config, workComputer: undefined }),
  );
  const archive = join(directory, "backup.persona");
  const password = crypto.randomUUID();
  await backupInstallation(root, archive, password);
  const target = join(directory, "restored");
  await restoreInstallation(archive, target, password);
  expect(await storedComputers(join(target, "data", "agent.sqlite"))).toEqual(
    records,
  );
  const restored = await openChat(
    context,
    parseConfig(
      { dataDir: join(target, "data") },
      join(target, "persona.config.json"),
    ),
    { models },
  );
  try {
    expect((await restored.computer.binding(botId, context)).computerId).toBe(
      "process",
    );
    const restoredClient = apiClient(restored);
    const chat = await restoredClient.get({ botId });
    expect(chat.messages).toContainEqual(
      expect.objectContaining({
        role: "user",
        content: [
          expect.objectContaining({
            type: "text",
            text: "Read the attached source",
          }),
        ],
        attachments: [
          expect.objectContaining({ id: attachmentId, name: "source.txt" }),
        ],
      }),
    );
    expect(chat.messages.map(messageText)).toContain(
      "The attached source is saved.",
    );
    expect((await restoredClient.work.get({ botId })).memory).toContainEqual(
      memory,
    );
    expect(
      await (
        await restoredClient.files.download({ botId, id: attachmentId })
      ).text(),
    ).toBe("A source file that must survive restore");

    const workspace = await restored.computer.workspace(botId, context);
    expect(
      getOrThrow(
        await workspace.readTextFile(
          await workspace.path("draft.txt"),
          context,
        ),
      ),
    ).toBe("A saved draft");
  } finally {
    await restored.close();
  }
});
