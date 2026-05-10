// Mock MCP server used by launcher integration tests.
//
// Reads exactly one line from stdin. If that line looks like a JSON-RPC
// `initialize` request, prints a minimal JSON-RPC response on stdout and
// exits 0. Any other input is treated as a test failure.
//
// The mock is cross-compiled per-OS by the launcher test harness and dropped
// into place by a stub `curl`, simulating the first-install download path.
// Running the real launcher against this mock proves that the launcher:
//
//   1. Reaches the download code path (not the stdio fast-fail),
//   2. Places the downloaded artefact where it will be exec'd,
//   3. Sets the artefact executable,
//   4. Passes stdin/stdout straight through to the artefact.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const initializeResponse = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"mock-lumen","version":"0.0.0"}}}`

func main() {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "mock_mcp_server: stdin closed before a request arrived:", err)
		os.Exit(2)
	}
	if !strings.Contains(line, `"method":"initialize"`) {
		fmt.Fprintln(os.Stderr, "mock_mcp_server: expected initialize request, got:", strings.TrimSpace(line))
		os.Exit(3)
	}
	fmt.Println(initializeResponse)
}
