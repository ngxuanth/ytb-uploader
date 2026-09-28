import type { IncomingMessage } from "node:http";

import { WebSocketServer } from "ws";

import { mcpConfig } from "@repo/config/mcp.config";
import { wait } from "@repo/utils";

import { isPortInUse, killProcessOnPort } from "@/utils/port";

/**
 * Only accept connections from a Chrome extension (web pages cannot forge
 * `Origin`). Set `BMCP_EXTENSION_ID` to restrict this to one extension.
 */
function isAuthorizedClient(req: IncomingMessage): boolean {
  const origin = req.headers.origin ?? "";
  const extensionId = process.env.BMCP_EXTENSION_ID;
  return extensionId
    ? origin === `chrome-extension://${extensionId}`
    : origin.startsWith("chrome-extension://");
}

/**
 * `BMCP_WS_PORT` lets several servers run side by side, one per Chrome
 * profile, each profile's extension pointing at its own port.
 */
export function configuredPort(): number {
  const raw = process.env.BMCP_WS_PORT;
  if (!raw) return mcpConfig.defaultWsPort;
  const port = Number(raw);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`Invalid BMCP_WS_PORT: ${raw}`);
  }
  return port;
}

export async function createWebSocketServer(
  port: number = configuredPort(),
): Promise<WebSocketServer> {
  // With BMCP_NO_KILL set, a busy port is an error instead of a reason to
  // kill whatever holds it.
  if (process.env.BMCP_NO_KILL) {
    if (await isPortInUse(port)) {
      throw new Error(`Port ${port} is already in use`);
    }
  } else {
    killProcessOnPort(port);
    // Wait until the port is free
    while (await isPortInUse(port)) {
      await wait(100);
    }
  }
  return new WebSocketServer({
    host: "127.0.0.1",
    port,
    verifyClient: ({ req }: { req: IncomingMessage }) => isAuthorizedClient(req),
  });
}
