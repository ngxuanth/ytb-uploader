import { execSync } from "node:child_process";
import net from "node:net";

export async function isPortInUse(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.once("error", () => resolve(true)); // Port is still in use
    server.once("listening", () => {
      server.close(() => resolve(false)); // Port is free
    });
    server.listen(port);
  });
}

// Only target the process *listening* on the port: clients connected to it
// (e.g. Chrome, via the extension) must not be killed.
export function killProcessOnPort(port: number) {
  try {
    if (process.platform === "win32") {
      execSync(
        `FOR /F "tokens=5" %a in ('netstat -ano ^| findstr LISTENING ^| findstr :${port}') do taskkill /F /PID %a`,
      );
    } else {
      execSync(`lsof -ti tcp:${port} -sTCP:LISTEN | xargs -r kill -9`);
    }
  } catch (error) {
    console.error(`Failed to kill process on port ${port}:`, error);
  }
}
