# Implementation status

Bambastic builds as a Go host and a Go SpoolBuddy companion. The host embeds the original MIT browser console. The workflow table below describes backend capabilities and the earlier interface. The [frontend status](../frontend/README.md) identifies the workflows implemented by this rewrite. The reference specification remains the acceptance target. The executable can report the current route coverage for this checkout.

```sh
just build
./bin/bambastic coverage
./bin/bambastic coverage --missing
./bin/bambastic coverage --json
```

**841 of 841 reference HTTP operations have registered handlers.** The comparator builds a fresh installation and checks the HTTP methods and route patterns against the 841 operations in the pinned reference. The installation publishes its registered operations at `/openapi.json`. A handler’s presence establishes routing coverage. Field-level compatibility, device behavior and complete operator journeys require additional checks.

## Working through the application

| Operator workflow | Implementation and evidence |
| --- | --- |
| Set up an installation and delegate access | First-run setup, local credentials, user and group grants, printer scopes, scoped API keys, TOTP, email verification and reset, OIDC, LDAP and OAuth. Tests use a local signed identity provider, SMTP peer, WebSocket and real MCP client. |
| Add a printer and submit a job | Go manages MQTT sessions, certificate pinning and encrypted file transfer. The queue uses persisted claims and distinguishes transfer, a published start request, an acknowledged physical run and terminal completion. Fixtures cover competing jobs, review, sensor holds, per-plate preheat and soak, disconnects and duplicate terminal reports. |
| Use a slicer with the farm | Operators can send through a virtual printer, review uploads, import models, choose a plate, start a slicing pipeline and follow resulting queue items in the same interface. Target-backed virtual printers mirror device and AMS information and match calibration replies to the requesting slicer. Rendering toolpaths requires the selected external slicer engine. |
| Manage jobs and files | Managed uploads, writable and read-only host folders, versions, plates, tags, attachments, model merging, per-run history, quality verdicts and project ZIP export/import operate on persisted records and actual file bytes. |
| Monitor hardware | Live status, camera snapshots and camera-wall tokens, external camera feeds, timelapse attachments, FFmpeg editing and printer storage use the same principal and printer scope as the web UI. Failure detection and plate calibration depend on configured cameras or providers. |
| Manage materials and costs | Native spool inventory, AMS and station observations, local and Spoolman-backed material workflows, projects, cost centers, wallets and print settlement use the Go service. Material and finance fixtures exercise RFID assignment, mid-run spool changes, partial extrusion, fallback measurements and cost settlement on SQLite and PostgreSQL. Manual assignment reports whether the printer accepted the configuration or the slot is pending. Saved K-profile recovery checks the current spool, printer, extruder and nozzle before applying a calibration. The Linux SpoolBuddy binary includes PN5180 and NAU7802 drivers. Quick Add can use a readable OpenTag3D record, an enrolled printer’s AMS report, or issuer-authorized MIFARE spool blocks. |
| Automate and recover | Smart plugs, sensor holds, low-stock and reorder notifications, webhooks, selective remote configuration backups and scheduled local backups run in Go. A local restore validates the snapshot, invalidates credentials carried over from it and switches to the restored generation. |
| Operate from MCP | The official Go SDK serves stdio and Streamable HTTP. Both transports support the same named catalog, with 16 operations, including printer and archive discovery, plate lookup, queue submission and review, printer control and camera snapshots. Caller-scoped submission keys protect retries. |
| Use the browser on a LAN | React owns navigation and interactions. Go serves history fallbacks and the compiled local assets. The frontend uses a blue/white/aqua theme and a bundled OpenBridge interface icon set. The notices and licenses accompany the build. |

## Verification

- A contract test validates API response bodies for 86 operator reads against the pinned release schemas on an installation with a project, folder, spool, archive, queue item and scoped API key.
- Go integration tests exercise first-run setup, sign-in, printer access, queue review, project data and portable backup on SQLite and PostgreSQL 16. The migration fixture starts with the tagged Bambuddy database schema, legacy Fernet and plaintext authenticators, a passlib-hashed API key and an archived print file.
- The MCP integration test submits two copies of a selected plate as a review-required user, retries through the other transport, checks the same IDs through the browser API and verifies reviewer authority and subsequent narrowing of the submitting key.
- The portable database tests exercise deleted-ID sequence high-water marks, foreign-key cycles, trigger behavior, rollback on invalid records and rejection of a populated destination. Online restore checks the installation’s active generation and removes old sessions. A rejected PostgreSQL restore discards its staged schema and leaves the serving generation available. A second host using the same database and base schema is denied the scheduler lease.
- The adapted browser suite passed 6,016 tests across 440 suites and checked translation keys for 17 locales. A packaged executable running without Node on its path and the Vite development proxy returned the same printer state over REST and WebSocket. Direct archive and kiosk navigation served the SPA. The primary-button text and fill tokens measure 7.16:1 contrast in light mode and 8.74:1 in dark mode.
- Signed application updates, station binary delivery, provider notifications, failure detection, remote imports, virtual printers and printer protocols have local automated fixtures. Both the host and the SpoolBuddy companion cross-compile as static Linux ARM64 executables.

## Acceptance still required

- Verify discovery, transfer, print acknowledgement, AMS changes, camera feeds, drying and recovery on the Bambu models and firmware used by the installation. Attribution uses toolpath and telemetry evidence. Unsupported toolpath segments can leave some material unattributed. Firmware staging ends with activation on the printer panel.
- Test a physical SpoolBuddy station with its reader and scale, supported desktop slicers, the deployed FFmpeg binary and live integrations with your farm’s accounts. Provider tests use local peers.
- Run the five operator journeys side by side in a live browser with the same fixtures. Visual acceptance of desktop, phone, camera wall and kiosk layouts remains open. The in-app browser session was unavailable for this run.
- Test the backup from the installation being migrated, including its environment-supplied keys and mounted external files. The automated migration fixture uses representative tagged data. Exhaustive field-by-field parity and production fleet load have not been established.
- The native build was exercised. The multi-stage Dockerfile is present, and a Docker daemon was unavailable for an image build in this environment.

The [product spec](../PRODUCT-SPEC.md), [rewrite overlay](bambastic-overlay.md), [rebuild rules](rebuild.md) and [pinned HTTP contract](openapi.json) define the acceptance criteria.
