import { createSocketMessageSender } from "@r2r/messaging/ws/sender";
import { WebSocket } from "ws";

import { mcpConfig } from "@repo/config/mcp.config";
import { MessagePayload, MessageType } from "@repo/messaging/types";
import { SocketMessageMap } from "@repo/types/messages/ws";

const noConnectionMessage = `No connection to browser extension. In order to proceed, you must first connect a tab by clicking the Browser MCP extension icon in the browser toolbar and clicking the 'Connect' button.`;
const noTabMessage = `The browser extension is connected, but no tab is attached to it (the tab may have been closed or switched). Click the Browser MCP extension icon on the tab you want to automate and click the 'Connect' button again.`;
const staleTabMessage = `The connected tab no longer exists (it was closed, discarded or replaced by the browser). Click the Browser MCP extension icon on the tab you want to automate and click the 'Connect' button again.`;

/**
 * How long a request waits for the extension to (re)connect. The extension
 * retries every second, e.g. after Chrome restarted its service worker.
 */
const reconnectGraceMs = 5000;

export class Context {
  private _ws: WebSocket | undefined;
  private _connectionWaiters = new Set<(ws: WebSocket) => void>();

  get ws(): WebSocket {
    if (!this._ws) {
      throw new Error(noConnectionMessage);
    }
    return this._ws;
  }

  set ws(ws: WebSocket) {
    this._ws = ws;
    this._connectionWaiters.forEach((resolve) => resolve(ws));
    this._connectionWaiters.clear();
  }

  /** Forget `ws` if it is still the current connection (e.g. after it closed). */
  clearWs(ws: WebSocket) {
    if (this._ws === ws) {
      this._ws = undefined;
    }
  }

  hasWs(): boolean {
    return !!this._ws;
  }

  /** Returns an open connection, waiting briefly for the extension if needed. */
  private async openWs(): Promise<WebSocket> {
    if (this._ws?.readyState === WebSocket.OPEN) {
      return this._ws;
    }
    return new Promise((resolve, reject) => {
      const onConnect = (ws: WebSocket) => {
        clearTimeout(timeoutId);
        resolve(ws);
      };
      const timeoutId = setTimeout(() => {
        this._connectionWaiters.delete(onConnect);
        reject(new Error(noConnectionMessage));
      }, reconnectGraceMs);
      this._connectionWaiters.add(onConnect);
    });
  }

  async sendSocketMessage<T extends MessageType<SocketMessageMap>>(
    type: T,
    payload: MessagePayload<SocketMessageMap, T>,
    options: { timeoutMs?: number } = { timeoutMs: 30000 },
  ) {
    const { sendSocketMessage } = createSocketMessageSender<SocketMessageMap>(
      await this.openWs(),
    );
    try {
      return await sendSocketMessage(type, payload, options);
    } catch (e) {
      if (e instanceof Error) {
        // The extension is connected here, so don't report a missing connection.
        if (e.message === mcpConfig.errors.noConnectedTab) {
          throw new Error(noTabMessage);
        }
        // Chrome's error for a tab ID that no longer exists.
        if (e.message.includes("No tab with given id")) {
          throw new Error(staleTabMessage);
        }
      }
      throw e;
    }
  }

  async close() {
    if (!this._ws) {
      return;
    }
    await this._ws.close();
  }
}
