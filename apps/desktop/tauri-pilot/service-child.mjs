import { installPrivateTransport } from "./service-transport.mjs";

installPrivateTransport();
await import("../../api/src/main.ts");
