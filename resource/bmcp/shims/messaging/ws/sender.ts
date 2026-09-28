import { WebSocket } from "ws";

import type {
  MessageMap,
  MessagePayload,
  MessageResult,
  MessageType,
} from "../types";

const MESSAGE_RESPONSE_TYPE = "messageResponse";

type ResponseMessage = {
  type: typeof MESSAGE_RESPONSE_TYPE;
  payload: { requestId: string; result?: unknown; error?: string };
};

export function createSocketMessageSender<M extends MessageMap>(ws: WebSocket) {
  async function sendSocketMessage<T extends MessageType<M>>(
    type: T,
    payload: MessagePayload<M, T>,
    options: { timeoutMs?: number } = { timeoutMs: 30000 },
  ): Promise<MessageResult<M, T>> {
    const { timeoutMs } = options;
    const id = crypto.randomUUID();
    const message = { id, type, payload };

    return new Promise((resolve, reject) => {
      let timeoutId: ReturnType<typeof setTimeout> | undefined;

      const onMessage = (event: WebSocket.MessageEvent) => {
        const response = JSON.parse(event.data.toString()) as ResponseMessage;
        if (
          response.type !== MESSAGE_RESPONSE_TYPE ||
          response.payload.requestId !== id
        ) {
          return;
        }
        cleanup();
        const { result, error } = response.payload;
        if (error) {
          reject(new Error(error));
        } else {
          resolve(result as MessageResult<M, T>);
        }
      };
      const onError = () => {
        cleanup();
        reject(new Error("WebSocket error occurred"));
      };
      const onClose = () => {
        cleanup();
        reject(new Error("WebSocket closed before a response was received"));
      };
      const cleanup = () => {
        ws.removeEventListener("message", onMessage);
        ws.removeEventListener("error", onError);
        ws.removeEventListener("close", onClose);
        clearTimeout(timeoutId);
      };

      if (timeoutMs) {
        timeoutId = setTimeout(() => {
          cleanup();
          reject(new Error(`WebSocket response timeout after ${timeoutMs}ms`));
        }, timeoutMs);
      }
      ws.addEventListener("message", onMessage);
      ws.addEventListener("error", onError);
      ws.addEventListener("close", onClose);

      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(message));
      } else {
        cleanup();
        reject(new Error("WebSocket is not open"));
      }
    });
  }

  return { sendSocketMessage };
}
