import { getApiProvider } from "@earendil-works/pi-ai/compat";

// web-server.ts scripts failures through pi-ai's faux provider, which streams
// an empty text block and then fails, with no error message when the script
// gives none. Pi's OpenAI-compatible provider always names the HTTP error, so
// go-server.ts marks such a failure with PERSONA_FAUX_NO_MESSAGE and this
// stream replays it the faux way.
const noMessage = process.env.PERSONA_FAUX_NO_MESSAGE;

export default function (pi) {
  pi.registerProvider("faux", {
    api: "openai-completions",
    async *streamSimple(model, context, options) {
      const events = getApiProvider("openai-completions").streamSimple(
        model,
        context,
        options,
      );
      for await (const event of events) {
        if (
          !noMessage ||
          event.type !== "error" ||
          !event.error.errorMessage?.includes(noMessage)
        ) {
          yield event;
          continue;
        }
        const { errorMessage: _, ...failed } = event.error;
        const partial = {
          ...failed,
          stopReason: "stop",
          content: [{ type: "text", text: "" }],
        };
        yield { type: "start", partial };
        yield { type: "text_start", contentIndex: 0, partial };
        yield { type: "text_delta", contentIndex: 0, delta: "", partial };
        yield { type: "text_end", contentIndex: 0, content: "", partial };
        yield {
          type: "error",
          reason: "error",
          error: { ...partial, stopReason: "error" },
        };
        return;
      }
    },
  });
}
