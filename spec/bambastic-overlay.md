# Bambastic rewrite overlay

[Product specification](../PRODUCT-SPEC.md) · [Rebuild design](rebuild.md) · [Bambuddy interface baseline](access-and-interfaces.md)

Bambastic pairs a Go backend with a React and TypeScript frontend built by Vite. A Bambuddy operator should be able to walk up to Bambastic, find the same job, make the same choices and recognize the result. Bambastic ships a first-party Model Context Protocol (MCP) server, uses OpenBridge for every interface icon, and adopts a blue, white and aqua visual identity.

The product specification records the accepted frontend, backend and deployment decisions. This overlay defines how to implement and verify them against Bambuddy `v.1.3.2`. [Accepted Bambastic architecture](../PRODUCT-SPEC.md#accepted-bambastic-architecture) · [Release baseline and wiki differences](../PRODUCT-SPEC.md#where-the-release-and-the-wiki-diverge)

## Go implementation

Use Go for all first-party backend work. This includes serving the compiled frontend, the REST API, WebSocket, MCP, authentication and authorization, databases and migrations, device and virtual-printer adapters, scheduling, physical-run capture, materials and finance, background workers, the CLI and the SpoolBuddy daemon when ported. The HTTP and MCP handlers use the same Go application services. One active scheduler owns dispatch for each printer. OrcaSlicer, Bambu Studio and media codecs can continue as external engines. [Component boundaries](rebuild.md#module-map) · [Slicer behavior](assets-and-slicing.md#server-side-slicing)

## React frontend and Vite build

Bambuddy already renders a React and TypeScript single-page application in the browser. React Router handles navigation, TanStack Query manages API reads and cached server state, and a WebSocket carries live updates. In the tagged release, Vite builds into the repository's `static/` directory and Python's FastAPI serves those files. This gives Bambastic a working frontend against which to test its Go API. [Browser entry](../../bambuddy/frontend/src/main.tsx) · [Router and query client](../../bambuddy/frontend/src/App.tsx) · [Vite configuration](../../bambuddy/frontend/vite.config.ts) · [Release build stages](../../bambuddy/Dockerfile)

Keep that frontend structure for Bambastic. React owns the screens, navigation, form state, keyboard focus, camera wall, 3D previews and kiosk UI. For example, the print dialog keeps the operator's chosen plate and AMS mapping while a printer update arrives. On submission, Go checks permission, material, budget and printer eligibility, then returns the queue item or the reason the request failed. The browser uses the same-origin `/api/v1` API and subscribes to `/api/v1/ws` for progress. [Reference print dialog](../../bambuddy/frontend/src/components/PrintModal/index.tsx) · [Browser API client](../../bambuddy/frontend/src/api/client.ts) · [Live events](access-and-interfaces.md#websocket-and-media-access)

| Environment | How it runs |
| --- | --- |
| Development | Run Vite and the Go backend side by side. Open Vite for fast refresh. Proxy `/api/v1/ws` with WebSocket upgrades and `/api` HTTP requests to Go. Use the same relative URLs in application code in both environments. |
| Production | Run `vite build` during the release build, then embed the generated files in the Go executable with `//go:embed`. The Go process serves the UI, API, WebSocket and MCP at one origin. The native listener defaults to `127.0.0.1:8080` and accepts an override through `BAMBASTIC_ADDR`. The container listens on port 8080. Node is a build and development dependency. The installed application runs with the Go executable and its configured storage and integrations. |

Serve the SPA entry for direct browser navigation to paths such as `/archives/42` and `/spoolbuddy/ams`. Route API, MCP, health, media, WebSocket and static asset requests to their own handlers. Serve the browser UI and its assets with `Cache-Control: no-store` in development and production. Opening or reloading the UI requires a live server. Retire the offline service worker and delete its saved pages and assets when an existing browser returns to the same origin. Ship icons, fonts, viewer workers and PWA files locally so LAN-only installations can render the whole interface. [Bambuddy web routes](../../bambuddy/backend/app/main.py) · [Vite static builds](https://vite.dev/guide/static-deploy.html) · [Go embedded files](https://pkg.go.dev/embed)

## Workflow parity

Keep Bambuddy's task names, entry points, navigation order, dialog choices, defaults, permission checks and feedback. Use the same number of operator decisions for the same job and role. The visual changes apply to desktop, mobile, PWA, camera wall and SpoolBuddy. A new operator can use the entire browser workflow without configuring an MCP client. [Baseline routes](../../bambuddy/frontend/src/App.tsx) · [Sidebar and keyboard shortcuts](../../bambuddy/frontend/src/components/Layout.tsx) · [Shared print dialog](../../bambuddy/frontend/src/components/PrintModal/index.tsx)

| Operator journey | What must match Bambuddy |
| --- | --- |
| Set up and find a printer | The landing page shows Printers. Discovery and manual entry feed the same add-printer flow. A failed connection explains the problem in place. A successful connection opens up the same monitoring, camera and control actions for the caller. |
| Prepare and submit work | Start from File Manager, an archive or a project. Offer slicing for a source model and the print dialog for a printable plate. Preserve plate selection, copies, pinned or eligible printer, AMS routing, print options, timing, project and cost center where applicable. Submitting returns the queue item. |
| Review and dispatch | A submitter who needs review sees their pending request. An authorized reviewer can release it from Queue. Preserve hold reasons, bed clearance, transfer progress, device acknowledgment and the eventual run result. Reloading the page preserves the identity of the job. |
| Inspect and repeat | An archive deep link opens its detail over the list. Preserve the return path, per-run evidence, reprint action, editable print choices and quality verdict. Two printers running the same file produce two independently addressable runs. |
| Manage a farm | Keep Queue, History, Timeline and Pipelines together on the Queue page. Preserve inventory and AMS assignment, SpoolBuddy scan and weigh, restricted finance views, saved sidebar order and numeric navigation shortcuts. At narrow widths the same permitted decisions remain reachable. |

For parity testing, give both applications the same user, preferences, files, inventory and recorded printer reports. Walk the same journey and compare the visible labels, available choices, initial values, holds and return destination. IDs generated on separate installations can differ. Each queue item must still link to the correct plate, owner and resulting physical run. [Print lifecycle and recovery](print-lifecycle.md) · [Browser and display surfaces](access-and-interfaces.md#browser-and-display-surfaces)

## First-party MCP server

Distribute the server with Bambastic and implement it with the official Go MCP SDK. A desktop client can launch `bambastic mcp --stdio` and point it at its configured installation. That process forwards requests under the caller's identity to the running server. A network client can use Streamable HTTP at `/mcp` on the installation. Both transports expose the same operation catalog. Pin the SDK version and test its supported protocol revisions with real clients. The reference community integration establishes a familiar discovery flow for existing Bambuddy operators. [Official Go SDK](https://github.com/modelcontextprotocol/go-sdk) · [MCP transports](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports) · [Bambuddy community MCP](https://wiki.bambuddy.cool/community/bambuddy-mcp/)

Expose `list_categories`, `search_tools` and `execute_tool` as the compact discovery interface. Search returns stable operation names, descriptions, argument schemas and whether an operation reads or changes state. Execute accepts a registered name and schema-validated arguments. Catalog entries map to the same handlers used by the application. Use the release HTTP contract to track operation coverage. The following operations form the first end-to-end acceptance slice. [MCP tools](https://modelcontextprotocol.io/specification/2026-07-28/server/tools) · [Release HTTP contract](openapi.json)

| A user asks the client to… | Required behavior |
| --- | --- |
| “What is printing in my lab?” | List visible printers and read current status. Distinguish offline, idle and active. Use the same printer scope and filename visibility as the corresponding API view. |
| “Find the bracket and show its plates.” | Search accessible archives and return the plate numbers, identities and available metadata for the chosen file. A print request must carry the selected source and plate. |
| “Queue two copies of plate 2 on printer 7.” | Use normal admission, billing and review rules. Return the created queue IDs or batch reference, current state and known hold reason. The client can query those IDs while prints progress. Acceptance into Queue is a different event from a printer acknowledging a start. |
| “Release this job” or “Pause that print.” | Apply the same role, printer scope, current-state checks and dispatch/control path as the equivalent UI action. Report the target and the observed or pending result. A request to resume a paused run refers to that existing run. |
| “Show me the camera.” | Use the scoped snapshot operation and return MCP image content with its actual media type. An unavailable or unauthorized camera yields a useful error and no image. |

Resolve the actor from the configured scoped credential for stdio or the authorized HTTP request. For authenticated HTTP clients, implement MCP authorization against the installation and bind the credential to its intended resource. Apply ownership, key limits, reviewer authority, media visibility and budget rules again when executing a discovered action. Keep printer access codes and account credentials out of tool output. [MCP authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization) · [Bambuddy access rules](access-and-interfaces.md#how-authorization-composes)

Return machine-readable IDs, state and failure reasons alongside a short human-readable result. Preserve the difference between an invalid request, a denied action, a valid job on hold and a device that has yet to answer. For retryable MCP submissions, accept a caller-scoped request key and persist it with the created work. Repeating the same key and arguments returns the original IDs. Reusing the key for different work returns a conflict. If a client disconnects after submission, the operator can still find the work in Queue. [Dispatch recovery rules](rebuild.md#ordering-and-failure-rules)

## OpenBridge icons

Use the Bambastic “B” brand mark for the browser favicon. Use `open-ships/openbridge-icons` for all other icons, including sidebar items, buttons, status symbols, empty states, embedded controls, kiosk screens and PWA launcher artwork. Generate launcher sizes from the chosen OpenBridge glyph. Serve the assets locally so a disconnected farm retains the same controls. Camera frames, model previews, graphs of measured data and filament color swatches carry product data and retain their actual appearance. [Specified OpenBridge repository](https://github.com/open-ships/openbridge-icons)

Pin the icon input to package `@openships/openbridge-icons` version `3.0.0`, verified at commit `52e330eeef7c69cdaf50cf04931adbcb2c80ab1b`. Build a small role-to-icon registry from its manifest and ship the used assets with the upstream LICENSE and NOTICE. Use the package's monochrome category exports where CSS supplies the color. Its raw SVG files retain the original paint. These mappings are verified examples, expressed as category and export. [Pinned manifest](https://github.com/open-ships/openbridge-icons/blob/52e330eeef7c69cdaf50cf04931adbcb2c80ab1b/dist/manifest.json) · [Package usage](https://github.com/open-ships/openbridge-icons/blob/52e330eeef7c69cdaf50cf04931adbcb2c80ab1b/README.md) · [Attribution](https://github.com/open-ships/openbridge-icons/blob/52e330eeef7c69cdaf50cf04931adbcb2c80ab1b/NOTICE)

| UI role | OpenBridge export |
| --- | --- |
| Fleet home | `application/homeIcon` |
| Queue and archives | `ui/listAltCheckGoogleIcon`, `ui/mediaLibraryIcon` |
| Search and camera | `ui/searchIcon`, `ui/cameraIcon` |
| Start, pause, resume and stop | `ui/mediaPlayIcon`, `ui/mediaPauseIcon`, `ui/mediaResumeIcon`, `ui/mediaStopIcon` |
| Settings, warning and external link | `ui/settingsIecIcon`, `alert/warningGoogleIcon`, `ui/linkIcon` |

Keep action labels and accessible names. An icon beside a visible label is decorative to assistive technology. An icon-only control needs an accessible name and keyboard focus. If no catalog symbol communicates a specialized action, use its text label. For external sidebar links, replace Bambuddy's custom icon upload with an OpenBridge picker in the same settings form. Map known legacy names to catalog entries and use the OpenBridge link glyph for unmatched names or uploaded images. Preserve each link's title, destination, ordering and stored original asset on migration. [Bambuddy link editor](../../bambuddy/frontend/src/components/AddExternalLinkModal.tsx)

## Blue, white and aqua theme

Use white for working surfaces, deep blue for primary actions and aqua for selections, live progress and focus. The new-install default is light. Honor an existing light, dark or system preference and retain the theme controls. Dark mode uses navy surfaces with light blue controls and bright aqua indicators. On a printer card, for example, the primary action reads in blue, a labeled aqua bar shows progress, and an amber “Paused” or red “Failed” state stays distinguishable alongside it. [Bambuddy theme preferences](../../bambuddy/frontend/src/contexts/ThemeContext.tsx)

| Role | Light value | Dark value |
| --- | --- | --- |
| Page canvas / reading surface | `#F1F7FB` / `#FFFFFF` | `#0A2030` / `#102C40` |
| Primary / secondary text | `#102C43` / `#48647A` | `#EAF6FC` / `#AEC6D5` |
| Primary button label / fill | `#FFFFFF` / `#075A99` | `#092237` / `#78C6FF` |
| Selected text / aqua surface | `#005F63` / `#D6F5F2` | `#78ECE3` / `#103C44` |
| Focus ring and progress indicator | `#007D85` | `#53DAD2` |
| Required control boundary | `#5D7E91` | `#7CA2B7` |
| Completed text / surface | `#176441` / `#E4F6EC` | `#8EE4B2` / `#17392D` |
| Warning or paused text / surface | `#7A4300` / `#FFF1D7` | `#FFD28A` / `#3F3019` |
| Fault or destructive text / surface | `#9E2C3B` / `#FDE9EC` | `#FFABB5` / `#47242C` |

Pair state colors with text or a labeled OpenBridge symbol. Keep AMS swatches true to spool color and chart series identifiable by label or pattern. Verify at least 4.5:1 for normal text and 3:1 for large text and meaningful control boundaries against adjacent colors. Use a surface-colored gap around an outside focus ring so it remains visible on filled buttons. The specified primary-button pair measures 7.16:1 in light mode and 8.74:1 in dark mode. [WCAG text contrast](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html) · [WCAG non-text contrast](https://www.w3.org/WAI/WCAG22/Understanding/non-text-contrast.html)

## Release acceptance

1. Run the same printer-status read and live update through the Vite development proxy and through the packaged Go release on a host without Node.js. Both paths must return the same visible state for the same user. On the packaged release, hard-refresh an archive deep link and a kiosk route, load their local assets, and check that a missing asset and an unknown API endpoint return 404 with the appropriate content type. Submit work through the UI and MCP against that instance and verify that each dispatch has one owner.
2. Walk the five operator journeys against Bambuddy and Bambastic with identical fixtures. Check desktop and narrow layouts, keyboard navigation, archive deep links, review holds and the existing printer and budget scope.
3. With a real MCP client on each transport, discover a printer, choose a plate, enqueue work as a review-required user, and inspect the same IDs in the UI. An authorized reviewer releases it once. Repeating the submission after a lost reply leaves one set of queue items and one intended budget commitment. A narrower grant immediately limits subsequent tool calls.
4. Check every rendered icon role against the pinned manifest, including PWA assets and migrated external links. In both themes, inspect idle, printing, held, paused, failed and offline cards, dialogs and kiosks. Verify contrast, focus, accessible names and real filament colors.
