import { runtimeEntry } from "./layout.mjs";
import { installPrivateTransport } from "./service-transport.mjs";

installPrivateTransport();
await import(runtimeEntry.href);
