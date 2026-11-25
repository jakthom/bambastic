When interacting with Cloudflare, use the cf CLI unless the project has a Wrangler configuration file.

## Investigating frontend issues

For a UI crash, find the matching report in `data/ui-diagnostics/crashes.jsonl` by time and route. Use its session ID and sequence number to find the preceding events in `uie.jsonl`. For issues without a crash, such as an incorrect theme or an unresponsive control, start with `uie.jsonl` and trace the affected interaction.

For an offline printer, compare `websocket.message` events with `fields.type == "printer_status"` against `printer.diagnostic` events for the same `printerId`. The status events record connectivity and print state. Each diagnostic event records the check, outcome and reason.

If the logs do not cover the incident, reproduce it with `just dev` or `just run`. After a fix, repeat the interaction and check the new events for the expected behavior and any recurring error. If `BAMBASTIC_DATA_DIR` is set, use that directory in place of `data/`.

The [UI diagnostics guide](internal/uidiagnostics/README.md) includes log queries, rotation details and local MCP resource URIs.
