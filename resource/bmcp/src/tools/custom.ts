import fs from "fs";
import path from "path";
import { z } from "zod";
import { zodToJsonSchema } from "zod-to-json-schema";

import { GetConsoleLogsTool, ScreenshotTool } from "@repo/types/mcp/tool";

import { Tool } from "./tool";

export const getConsoleLogs: Tool = {
  schema: {
    name: GetConsoleLogsTool.shape.name.value,
    description: GetConsoleLogsTool.shape.description.value,
    inputSchema: zodToJsonSchema(GetConsoleLogsTool.shape.arguments),
  },
  handle: async (context, _params) => {
    const consoleLogs = await context.sendSocketMessage(
      "browser_get_console_logs",
      {},
    );
    const text: string = consoleLogs
      .map((log) => JSON.stringify(log))
      .join("\n");
    return {
      content: [{ type: "text", text }],
    };
  },
};

export const screenshot: Tool = {
  schema: {
    name: ScreenshotTool.shape.name.value,
    description: ScreenshotTool.shape.description.value,
    inputSchema: zodToJsonSchema(ScreenshotTool.shape.arguments),
  },
  handle: async (context, _params) => {
    const screenshot = await context.sendSocketMessage(
      "browser_screenshot",
      {},
    );
    return {
      content: [
        {
          type: "image",
          data: screenshot,
          mimeType: "image/png",
        },
      ],
    };
  },
};

// Mirrors the extension's `browser_evaluate` schema.
const EvaluateTool = z.object({
  name: z.literal("browser_evaluate"),
  description: z.literal(
    "Evaluate a JavaScript expression in the page and return its JSON-serialisable value. Promises are awaited. Use it to read state (text, attributes, upload progress) without taking a full snapshot.",
  ),
  arguments: z.object({
    expression: z
      .string()
      .describe("JavaScript expression, e.g. document.title or (() => { ... })()"),
  }),
});

export const evaluate: Tool = {
  schema: {
    name: EvaluateTool.shape.name.value,
    description: EvaluateTool.shape.description.value,
    inputSchema: zodToJsonSchema(EvaluateTool.shape.arguments),
  },
  handle: async (context, params) => {
    const { expression } = EvaluateTool.shape.arguments.parse(params);
    const value = await context.sendSocketMessage("browser_evaluate", {
      expression,
    });
    return {
      content: [
        {
          type: "text",
          text: value === undefined ? "undefined" : JSON.stringify(value),
        },
      ],
    };
  },
};

// Mirrors the extension's `browser_upload_file` schema.
const UploadFileTool = z.object({
  name: z.literal("browser_upload_file"),
  description: z.literal(
    "Attach a local file to an input[type=file]. Only files inside the directory set by BMCP_UPLOAD_DIR can be attached.",
  ),
  arguments: z.object({
    selector: z
      .string()
      .describe("CSS selector of the file input, e.g. input[type=file]"),
    filePath: z
      .string()
      .describe("Path of the file to attach, relative to BMCP_UPLOAD_DIR"),
  }),
});

/**
 * Resolves `filePath` against the upload directory and rejects anything that
 * escapes it (via `..`, absolute paths or symlinks), hidden files, and
 * non-regular files. Returns the real absolute path to hand to the browser.
 */
function resolveUploadPath(filePath: string): string {
  const uploadDir = process.env.BMCP_UPLOAD_DIR;
  if (!uploadDir) {
    throw new Error(
      "File upload is disabled. Set BMCP_UPLOAD_DIR to the directory whose files may be uploaded.",
    );
  }

  let realDir: string;
  try {
    realDir = fs.realpathSync(uploadDir);
  } catch {
    throw new Error(`BMCP_UPLOAD_DIR does not exist: ${uploadDir}`);
  }
  if (!fs.statSync(realDir).isDirectory()) {
    throw new Error(`BMCP_UPLOAD_DIR is not a directory: ${uploadDir}`);
  }

  let realFile: string;
  try {
    realFile = fs.realpathSync(path.resolve(realDir, filePath));
  } catch {
    throw new Error(`File not found in upload directory: ${filePath}`);
  }

  const relative = path.relative(realDir, realFile);
  if (!relative || relative.startsWith("..") || path.isAbsolute(relative)) {
    throw new Error(`File is outside the upload directory: ${filePath}`);
  }
  if (relative.split(path.sep).some((segment) => segment.startsWith("."))) {
    throw new Error(`Hidden files cannot be uploaded: ${filePath}`);
  }
  if (!fs.statSync(realFile).isFile()) {
    throw new Error(`Not a regular file: ${filePath}`);
  }

  return realFile;
}

// Refs from browser_snapshot look like s12e685.
const ARIA_REF = /^s\d+e\d+$/;

// Mirrors the extension's `browser_scroll` schema.
const ScrollTool = z.object({
  name: z.literal("browser_scroll"),
  description: z.literal(
    "Scroll an element into the middle of the view, or wheel-scroll by deltaY pixels. Give ref (from browser_snapshot) or a CSS selector. Call this before clicking a control hidden under a sticky footer.",
  ),
  arguments: z.object({
    element: z
      .string()
      .optional()
      .describe("Human-readable element description, as for browser_click"),
    ref: z
      .string()
      .optional()
      .describe("Exact target element reference from the page snapshot, e.g. s12e685"),
    selector: z
      .string()
      .optional()
      .describe(
        "CSS selector to scroll into view, e.g. tp-yt-paper-radio-button[name=VIDEO_MADE_FOR_KIDS_NOT_MFK]",
      ),
    deltaY: z
      .number()
      .optional()
      .describe("Wheel delta in pixels. Positive scrolls down. Used when selector is omitted."),
    x: z.number().optional().describe("Viewport X for the wheel. Default is the centre."),
    y: z.number().optional().describe("Viewport Y for the wheel. Default is the centre."),
  }),
});

export const scroll: Tool = {
  schema: {
    name: ScrollTool.shape.name.value,
    description: ScrollTool.shape.description.value,
    inputSchema: zodToJsonSchema(ScrollTool.shape.arguments),
  },
  handle: async (context, params) => {
    const { element: _element, ...args } = ScrollTool.shape.arguments.parse(params);
    // A snapshot ref passed as selector matches nothing, and the extension
    // would wait for it until the socket times out.
    if (args.selector && !args.ref && ARIA_REF.test(args.selector.trim())) {
      args.ref = args.selector.trim();
      delete args.selector;
    }
    if (!args.ref && !args.selector && args.deltaY === undefined) {
      throw new Error("browser_scroll needs ref, selector or deltaY");
    }
    const result = await context.sendSocketMessage("browser_scroll", args);
    return {
      content: [
        {
          type: "text",
          text: JSON.stringify(result),
        },
      ],
    };
  },
};

// Files already attached by this server. One bmcp process serves one upload
// session, so a second attach of the same file would start a duplicate video.
const attached = new Set<string>();

export const uploadFile: Tool = {
  schema: {
    name: UploadFileTool.shape.name.value,
    description: UploadFileTool.shape.description.value,
    inputSchema: zodToJsonSchema(UploadFileTool.shape.arguments),
  },
  handle: async (context, params) => {
    const { selector, filePath } = UploadFileTool.shape.arguments.parse(params);
    const absPath = resolveUploadPath(filePath);
    if (attached.has(absPath)) {
      throw new Error(
        `"${filePath}" was already attached in this session; attaching it again would upload a duplicate video. Continue with the video already created (open https://studio.youtube.com/video/<video_id>/edit) instead.`,
      );
    }
    await context.sendSocketMessage("browser_upload_file", {
      selector,
      filePath: absPath,
    });
    attached.add(absPath);
    return {
      content: [
        {
          type: "text",
          text: `Attached "${filePath}" to "${selector}"`,
        },
      ],
    };
  },
};
