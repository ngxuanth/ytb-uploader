// Messages the MCP server sends to the extension over the WebSocket.
type Msg<P, R = unknown> = { payload: P; result: R };

type ElementArgs = { element: string; ref: string };

export type SocketMessageMap = {
  getUrl: Msg<undefined, string>;
  getTitle: Msg<undefined, string>;
  browser_navigate: Msg<{ url: string }>;
  browser_go_back: Msg<{}>;
  browser_go_forward: Msg<{}>;
  browser_wait: Msg<{ time: number }>;
  browser_press_key: Msg<{ key: string }>;
  browser_snapshot: Msg<{}, string>;
  browser_click: Msg<ElementArgs>;
  browser_drag: Msg<{
    startElement: string;
    startRef: string;
    endElement: string;
    endRef: string;
  }>;
  browser_hover: Msg<ElementArgs>;
  browser_type: Msg<ElementArgs & { text: string; submit: boolean }>;
  browser_select_option: Msg<ElementArgs & { values: string[] }>;
  browser_screenshot: Msg<{}, string>;
  browser_get_console_logs: Msg<{}, unknown[]>;
  browser_upload_file: Msg<{ selector: string; filePath: string }>;
  browser_evaluate: Msg<{ expression: string; awaitPromise?: boolean }, unknown>;
  browser_scroll: Msg<
    { ref?: string; selector?: string; deltaY?: number; x?: number; y?: number },
    { scrolled: string }
  >;
};
