# Bambastic

Bambastic is a self-hosted Bambu printer farm application. One Go process serves the React operator interface, the REST API, live WebSocket updates and an MCP endpoint. SQLite is the default database. PostgreSQL is also supported. Both use a local installation directory for managed files, encryption keys and device state. The browser application adapts the Bambuddy v.1.3.2 interface, uses OpenBridge icons and defaults to a blue, white and aqua light theme. Existing users can select dark or system themes.


An independent MIT frontend is available in the [Bambastic browser console](web/README.md). Build it with `just build-console` to run its implemented workflows against the same Go backend.

## Run locally

Building from source requires `just`, Go 1.25.2 or later, and Node.js 22.12 or later with npm. Run `just --list` from the repository root to see the available recipes.

```sh
just run
```

Open [Bambastic on localhost](http://127.0.0.1:8080). `just run` starts the app in development mode. Frontend edits appear in the browser as you save, and Go server edits trigger a rebuild and restart. The first run installs the frontend dependencies and the Air file watcher. The app stores your farm in `./data`. Set `BAMBASTIC_ADDR` or `BAMBASTIC_DATA_DIR` to override those defaults. Press Ctrl+C to stop.

Development builds record UI activity and crash reports in separate files under `$BAMBASTIC_DATA_DIR/ui-diagnostics/` (default `./data/ui-diagnostics/`). See the [UI diagnostics guide](internal/uidiagnostics/README.md) for agent resources and investigation commands.

Complete setup, then add a printer from **Printers** using its LAN address, serial number and access code. The backend verifies a connection at registration. Use an empty data directory for a new installation. Stopping and restarting the executable with the same directory preserves your farm. A lock prevents two schedulers from using that directory.

To run the servers separately, use `just dev-backend` in one terminal and `just dev-frontend` in another. Open the Vite URL shown in the terminal. Vite proxies API calls and WebSocket upgrades to the Go process. Set `BACKEND_PORT` when running the development API on a port other than 8080. For production, run `just build` followed by `./bin/bambastic serve`. The executable includes the frontend and runs without Node or a Vite server.

A multi-stage container build is also included.

```sh
docker build -t bambastic .
docker run --rm -p 127.0.0.1:8080:8080 -v bambastic-data:/data bambastic
```

This port mapping serves the web and MCP interfaces. Virtual printer TCP listeners and LAN discovery also need network access from the slicer to the configured addresses and ports. Printer connections, streaming and slicing require the corresponding device or media/slicer engine.

### Use PostgreSQL

Set `BAMBASTIC_DATABASE_URL` to a PostgreSQL connection URI and start the app with `just run`. `BAMBASTIC_POSTGRES_SCHEMA` selects the installation’s schema and defaults to `bambastic`. The role must be able to create schemas in the selected database and manage the objects it creates. Keep the schema name and data directory with the installation across restarts. A PostgreSQL advisory lock prevents a second host from dispatching against the same installation. The service exits if it loses that lease.

A PostgreSQL `DATABASE_URL` is also recognized. Use `--database-url` and `--postgres-schema` to override the environment for one invocation. The database integration suite was exercised on PostgreSQL 16.

## Connect an MCP client

Open **MCP Guide** in the sidebar for setup instructions for ChatGPT, Claude, Grok, Codex CLI, LM Studio, Goose with Ollama, and other stdio clients. The guide includes the current operation catalog and a read-only prompt to test the connection. [Open the guide on a local installation](http://127.0.0.1:8080/mcp-guide)

**Streamable HTTP.** Append `/mcp` to the installation’s URL. Protected installations support OAuth with PKCE or a scoped Bambastic API key. ChatGPT, Claude remote connectors, and Grok make requests from their providers’ servers, so configure an authenticated HTTPS endpoint reachable from the provider. ChatGPT also supports Secure MCP Tunnel from a machine that can reach a private installation. [ChatGPT setup](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt) · [OpenAI Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) · [Claude remote connectors](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp) · [Grok connectors](https://docs.x.ai/grok/connectors)

**Private tailnet.** For a client running on a tailnet device, leave Bambastic on its default loopback port. On the Bambastic host, run `tailscale serve --bg http://127.0.0.1:8080`. Copy the HTTPS origin from its output. Start Bambastic with `BAMBASTIC_PUBLIC_URL` set to that exact origin so OAuth advertises the same address and the MCP server accepts the proxy’s Host header. In this example, the device is named `farm` on the example tailnet.

```sh
just build
BAMBASTIC_PUBLIC_URL=https://farm.example.ts.net ./bin/bambastic serve
```

The client on the tailnet would connect to `https://farm.example.ts.net/mcp`. Tailscale Serve limits this route to authorized tailnet devices. A hosted connector needs its own reachable endpoint or supported tunnel. For other reverse proxies, preserve the external Host header and forward WebSocket upgrades for the UI. [Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve)

**Stdio.** Configure the client to launch the Bambastic executable using its absolute path and pass the installation’s base URL to `--server`. Set `BAMBASTIC_API_KEY` in the client's environment when authentication is enabled.

```sh
./bin/bambastic mcp --stdio --server http://127.0.0.1:8080
```

Use `list_categories` and `search_tools` to discover operations and their argument schemas. Call `execute_tool` with the operation name and arguments. For example, `{"name":"list_printers","arguments":{}}` returns the printers visible to that identity. Device actions and queue submissions use the same handlers as the operator UI.

## Storage and backup

Download the backup ZIP from the installation’s `GET /api/v1/settings/backup` endpoint as an administrator. Both database backends produce the same portable ZIP. It contains a consistent SQLite snapshot, managed files and installation secrets. Store it with the same access restrictions as the data directory. Restore to a new directory, then start Bambastic with that directory.

```sh
./bin/bambastic restore --data ./restored ./backup.zip
./bin/bambastic serve --data ./restored
```

The restore checks the database and clears cached sign-in and display credentials. It requires a destination path that does not already exist. To move between SQLite and PostgreSQL, quiesce the source farm, download the ZIP, restore it into a fresh directory, and start it with the destination database settings. PostgreSQL imports the portable data into a new generation before the service begins dispatching. Preserve any installation keys supplied through the environment. The tagged Bambuddy `v.1.3.2` restore fixture covers archived bytes and re-encryption of an existing authenticator secret. Test a copy of your installation before switching an existing farm.

The administration UI also supports scheduled local snapshots, retention, downloading a saved snapshot and restoring an uploaded or saved snapshot. A running restore validates the new data, switches the active generation and restarts the service against that generation using the same installation directory. An administrator can also back up selected configuration to a private GitHub, GitLab or Gitea repository. That export has a different scope from the full installation ZIP.

Administrators can mount read-only or writable host directories in the File Manager. Set `BAMBASTIC_EXTERNAL_ROOTS` to the allowed absolute parent directories using your operating system’s path-list separator. Back up those external directories separately. The portable ZIP includes the library index and managed files.

## Slicing, cameras and SpoolBuddy

Configure the slicer under Settings. Bambastic sends work to the selected Bambu Studio or OrcaSlicer engine. Set `BAMBU_STUDIO_API_URL` or `SLICER_API_URL` to override the default sidecar addresses (`localhost:3001` and `localhost:3003`). Install FFmpeg and ffprobe on the Go host for timelapse editing and supported camera transports. The container image includes both executables. Compose configurations for the two slicer services are available in the [tagged engine deployment guide](https://github.com/maziggy/bambuddy/tree/b7d894b57c0a373f66dc0de49680865b02b3aae5/slicer-api).

Build the companion on the station with `just spoolbuddy`, or cross-compile it on the application host.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/spoolbuddy-linux-arm64 ./cmd/spoolbuddy
```

Run that binary on the Raspberry Pi with `SPOOLBUDDY_BACKEND_URL` pointing to Bambastic. Set `SPOOLBUDDY_API_KEY` when the installation uses authentication. The daemon handles station registration, heartbeats, scale readings and NFC operations. Its Linux hardware drivers use SPI, GPIO and I²C device access. The [station installation guide](cmd/spoolbuddy/README.md) covers pairing, permissions and remote updates.

## Software updates and issue reports

Set `BAMBASTIC_RELEASE_FEED` to the distribution’s signed manifest endpoint and `BAMBASTIC_RELEASE_KEY` to its base64-encoded Ed25519 public key to enable updates in Settings. Bambastic checks the signature, target OS, architecture, byte count and SHA-256 digest before replacing the executable. Restart the service to run the new version. For a container installation, rebuild or replace the deployed image.

The issue-report form can submit to a receiver configured with `BAMBASTIC_REPORT_URL` and an optional `BAMBASTIC_REPORT_TOKEN`. The operator chooses which diagnostic material to submit.

## Verify and inspect coverage

```sh
just test
go test -race . ./internal/... ./cmd/...
go vet . ./internal/... ./cmd/...
npm --prefix frontend run typecheck
npm --prefix frontend run test:run
./bin/bambastic coverage --missing
```

Set `BAMBASTIC_TEST_DATABASE_URL` to a disposable PostgreSQL database to exercise the server integration suite on PostgreSQL. The tests create and remove their own schemas. The database adapter and transfer packages can also start an ephemeral local PostgreSQL cluster when the server tools are installed.

The Go integration suite exercises the embedded app, setup, WebSocket authentication, both MCP transports, OAuth code replay protection, and portable backup. Package tests exercise device and slicer protocols against local test peers. Physical hardware, external accounts and desktop slicer interoperability require separate acceptance testing.

The [product specification](PRODUCT-SPEC.md) is the target. The [overlay](spec/bambastic-overlay.md) records the rewrite decisions. The [release OpenAPI document](spec/openapi.json) indexes the reference service. The running service generates `/openapi.json` from its registered compatibility routes. A registered operation can still differ in accepted options or device support. The [implementation report](spec/implementation-status.md) describes the verified workflows, dependencies and outstanding parity work. The coverage command lists each missing method and path. Use `coverage --json` for a machine-readable report.

## Licensing

Original work by Bambastic contributors is available under the [MIT License](LICENSE).

The browser application adapts Bambuddy v1.3.2, which carries the GNU Affero General Public License v3 (AGPL). The standard Bambastic executable embeds that application, so distributions of the combined build must also satisfy the AGPL. Bambuddy-derived material elsewhere in the repository, including the reference API and database schema, retains its upstream terms. See the [AGPL text](frontend/LICENSE) and [upstream notice](frontend/NOTICE).

OpenBridge artwork and bundled label fonts carry their own licenses. See the [OpenBridge license](frontend/public/OpenBridge-LICENSE.txt), [artwork notice](frontend/public/OpenBridge-NOTICE.txt), and [font licenses](internal/management/labelfonts/).
