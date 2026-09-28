// Command launcher runs LLM upload sessions on the upload machine.
//
//	launcher try -video ./a.mp4 -profile Default [-channel UC...] [-harness claude|cursor|codex|hermes] [-port 9009]
//	launcher run -server ws://host/ws [-harness hermes]
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "try":
		err = try(os.Args[2:])
	case "run":
		err = runAgent(os.Args[2:])
	case "mcp-chrome":
		err = mcpChrome()
	case "chrome-check":
		err = chromeCheck(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  launcher try -video FILE -profile DIR [-channel UC...] [flags]  run one upload locally, no server (see: launcher try -h)
  launcher run -server ws://HOST/ws [flags]                     stay up and start a harness per assigned task
  launcher chrome-check -profile DIR [-port N]                  open Chrome, set up and verify the extension, no LLM
  launcher mcp-chrome                                          chrome MCP server (stdio), started by the LLM CLI`)
	os.Exit(2)
}
