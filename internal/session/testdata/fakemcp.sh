#!/bin/sh
# Minimal MCP stdio server for tests: answers the handshake, advertises one tool,
# ignores everything else. A script rather than a Go binary so the test needs
# neither a compiler nor an ad-hoc process spawn of its own.
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue

  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fakemcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
  esac
done
