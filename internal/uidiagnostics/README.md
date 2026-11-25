# Investigating frontend crashes

Run `just dev` or `just run` from the repository root. Both run Vite and a Go development build at the same origin. The two-terminal workflow uses `just dev-backend` and `just dev-frontend`. Open the Vite URL for that workflow. Production builds omit browser instrumentation and the two POST handlers.

Use the installation directory selected by `BAMBASTIC_DATA_DIR` (default `./data`). The server writes two private, append-only JSON Lines streams under `ui-diagnostics/`. Each stream rotates independently at 16 MiB and keeps its previous file with a `.1` suffix. Each line has a server UTC receipt time and a browser page-session ID.

* `crashes.jsonl` receives `POST /crashes`. Start here. Look at the source, message, JavaScript and React stacks, current route, theme, auth state, environment and recent events. The session and sequence number locate the failure in the event log.  Use the crash ID to recognize a repeated delivery.
* `uie.jsonl` receives `POST /uie`. Expand each batch's `events` array. Events carry their own client time, sequence, route, kind and fields. Use sequence numbers within one session for ordering. The batch's `dropped` count shows queue overflow during a slow or unavailable connection.

Inspect both logs, including the rotated files when the incident spans a rotation. For example:

```sh
logs="${BAMBASTIC_DATA_DIR:-data}/ui-diagnostics"
tail -n 1 "$logs/crashes.jsonl" | jq '.crash | {id,source,message,stack,componentStack,context,recent}'
session=$(tail -n 1 "$logs/crashes.jsonl" | jq -r '.session')
jq -c --arg session "$session" 'select(.session==$session) | .events[]' "$logs/uie.jsonl"
```

To compare a saved printer's reported state with its connection checks, substitute its database ID in this query. Each diagnostic check has its own outcome and reason. Checks run before registration have a null printer ID.

```sh
jq -c --argjson id 1 '.events[] | select(.fields.printerId == $id and (.fields.type == "printer_status" or .kind == "printer.diagnostic")) | {time,kind,fields}' "$logs/uie.jsonl"
```

The replacement console records page navigation, control interactions, live printer status and operator-requested connection checks. Window errors and unhandled promise rejections include the recent event sequence. The logger omits query strings, input values and request bodies.

For an agent using local MCP, start `go run -tags dev . mcp --stdio` from the checkout with the same data-directory setting as the server. Read `bambastic://ui-diagnostics/guide`, `bambastic://ui-diagnostics/crashes`, and `bambastic://ui-diagnostics/uie`. Each log resource returns the latest complete records from up to 512 KiB of the current file. Use the files directly for older events. Local resources can be read after the app stops.
