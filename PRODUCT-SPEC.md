# Bambuddy product specification

Bambuddy is a self-hosted control room for Bambu Lab printers. An operator can bring a design into a library, turn a selected plate into printable G-code, request production on a particular machine or a suitable member of the fleet, supervise the print, and trace the result back to its file, printer, spools, operator and budget. A print started at the machine or in a desktop slicer can enter the history without passing through the web queue. The local server owns that history and can operate the LAN workflow without a cloud account. [Release overview](../bambuddy/README.md) · [Operator wiki](https://wiki.bambuddy.cool/)

**Baseline.** This specification describes the sibling checkout at tag `v.1.3.2`, commit `b7d894b57c0a373f66dc0de49680865b02b3aae5` (November 20, 2026). The wiki was reviewed November 24, 2026. Where they differ, the tagged implementation defines release parity. The rebuild plan proposes internal module boundaries and a migration sequence. Relative source links assume the original `bambuddy` checkout sits beside this directory. [Pinned source tree](https://github.com/maziggy/bambuddy/tree/b7d894b57c0a373f66dc0de49680865b02b3aae5) · [Runtime version](../bambuddy/backend/app/core/config.py) · [Upstream AGPL-3.0 license](../bambuddy/LICENSE)

## Accepted Bambastic architecture

| Area | Decision |
| --- | --- |
| Browser | React and TypeScript render a single-page app built with Vite. React owns navigation, forms and live screen updates. Preserve Bambuddy's operator workflows, with OpenBridge icons and a blue, white and aqua theme. |
| Backend | Go owns the REST API, WebSocket, first-party MCP server, permissions, storage, scheduling, printer control and accounting. Browser requests and MCP tools use the same Go services and permission checks. |
| Development | Run Vite and Go as two processes. Vite provides fast refresh and proxies API and WebSocket traffic to Go. |
| Production | Build the frontend with Vite and embed the compiled assets in the Go executable. Operators run one application executable serving the UI, API, WebSocket and HTTP MCP endpoint at one origin, defaulting to `http://127.0.0.1:8080`. Node.js is needed only for frontend development and builds. |

This split preserves details such as the print dialog keeping the selected plate while progress updates arrive. Go makes the authoritative decision about whether the submitted job can run. [Frontend and release packaging](spec/bambastic-overlay.md#react-frontend-and-vite-build) · [Release acceptance](spec/bambastic-overlay.md#release-acceptance)

## Read by task

| Question | Specification |
| --- | --- |
| What is specific to Bambastic? How should Go, MCP, the familiar workflows, OpenBridge icons and the new colors work together? | [Bambastic rewrite overlay](spec/bambastic-overlay.md) |
| How does a printer join the fleet? How does a request become a physical print? What happens if it stalls, restarts or needs review? | [Printers, dispatch and the print lifecycle](spec/print-lifecycle.md) |
| Which files can I import, edit, slice, print again or recover? How do projects, pipelines and statistics count work? | [Assets, slicing, projects and reporting](spec/assets-and-slicing.md) |
| Which spool was consumed? What happens at a mid-print swap, a failed plate, a scale reading or a budget limit? | [Materials, SpoolBuddy and finance](spec/materials-and-finance.md) |
| Who may see or control what? What must existing browsers, kiosks, scripts and connected apps be able to do? | [Access, user interfaces and network contracts](spec/access-and-interfaces.md) |
| When may a plug cut power, a sensor hold work or an alert reach someone? How does an administrator recover an installation? | [Automation and operations](spec/automation-and-operations.md) |
| Where should the code split? Who owns each write? How can two implementations be compared and an installation moved? | [Rebuild design and acceptance plan](spec/rebuild.md) |
| What are the exact HTTP paths, methods, fields, defaults and declared responses? What does the fresh SQLite schema store? | [OpenAPI 3.1 contract](spec/openapi.json) · [SQLite schema](spec/storage.sql) |

## People and jobs

| Actor | Job to be done | Evidence that the job succeeded |
| --- | --- | --- |
| Individual maker | Leave a printer running, check it remotely on the LAN, find a successful plate months later and reproduce it. | The archive identifies the source, plate, settings and prior runs. Reprinting creates a new attempt with its own result. |
| Farm operator | Stage work, group it by location, use an available compatible printer, clear beds and intervene on failures. | The queue explains why each held job is waiting. The dashboard distinguishes a live machine, an offline machine, a transfer and an acknowledged print. |
| Submitter and reviewer | Let a student or team member prepare work within their access and budget. Have staff release it. | A request made as a review-required user is visible to its submitter and waits for an authorized reviewer. Unowned slicer uploads follow the configured virtual printer's intake policy. |
| Material or finance manager | Know which physical spool occupies a slot, how much it cost, what stock to order, and whose funds a run consumed. | Material history follows the spool through swaps. The wallet can explain a reservation, a charge or a release for the relevant dispatch. |
| Administrator | Operate the service, control credentials and visibility, monitor health, update machines and recover the database and files. | Restoring a backup recovers usable files and ownership. Printer access follows group membership across fleet lists, printer-scoped aggregates, media and live views. |
| Slicer, external app or hardware companion | Submit work or exchange status through a stable interface. | A slicer can discover a virtual printer, a shop app can reconcile a batch by its external reference, and a kiosk can identify and weigh a spool. |

One person may do all of these jobs in an installation with authentication disabled. In a shared installation, group permissions, printer scope, item ownership and finance membership express these powers independently. [Authorization](spec/access-and-interfaces.md) · [Billing](spec/materials-and-finance.md)

## Follow a two-plate order

1. **Prepare.** A user imports a model into a library folder. A project can organize the files, track parts and represent the whole build. An STL needs slicing. A 3MF can hold source geometry, multiple plates, or executable plate G-code. Inspect the contents to decide which actions the UI offers.
2. **Request.** The user picks the plate, target printer or printer model, quantity, material mapping, calibration options and any required cost center. A batch can request three runs of plate A and two of plate B. It retains those targets if an attempt later fails. A reusable pipeline can prepare and enqueue work with saved choices.
3. **Admit and dispatch.** Permission to create work, authority to run it without review, and the ability to pay for it are separate checks. A future time, occupied printer, empty slot, pending manual release or uncleared plate can keep an item pending. The scheduler selects an eligible device, claims the item, transfers the file and sends the start request. The operator sees upload progress and then a printer acknowledgment or a reason for failure.
4. **Observe.** The printer provides evidence of a running job. The server correlates the start with expected work, captures its actual plate, spool assignments and identifiers, and opens a run. Printers also report jobs started outside Bambuddy. Automatic archiving for those jobs requires both the global and per-printer settings. With capture enabled, a missing printable file can leave a metadata-only archive. Multiple machines can print the same archived file concurrently.
5. **Account.** The terminal report closes the corresponding run. Material attribution, budget settlement, batch progress, archive summaries, energy records and downstream notifications use that run's context. A failed run can consume real filament and money while leaving a plate owed. A power meter or missing 3MF changes which values can be measured.
6. **Review and repeat.** The operator can inspect the result or record a quality verdict, retry owed work and confirm the bed is clear for another start. A human rejecting a finished part, the printer reporting failure, and a reviewer approving a pending request are three different decisions.

The exact guards, calculations and recovery rules belong to their respective workflows. [Queue and physical runs](spec/print-lifecycle.md) · [Batch model](../bambuddy/backend/app/models/print_batch.py) · [Cost and material attribution](spec/materials-and-finance.md)

## Component boundaries for a rebuild

Start with one application and a transactional database. Give the **device adapter** ownership of the printer connection and observed machine state, the **scheduler** ownership of admission, selection and dispatch, and the **run recorder** ownership of what physically happened. The recorder must accept an unexpected start from a printer as readily as an expected start from the queue. A replacement for any of these three components can then be tested against the same recorded sequence of machine reports. [Module map and component contracts](spec/rebuild.md#module-map)

Keep reusable source files, per-plate slicing outputs and production targets with the **library, slicer and batch components**. Let **inventory and finance** consume the result of each physical attempt using identifiers they can reconcile after a restart. A failure on the second of three plates may consume material, settle part of a budget reservation and leave one plate owed. That single case exercises the boundary between machine state, accounting and order fulfillment. [Ordering and failure rules](spec/rebuild.md#ordering-and-failure-rules) · [Worked material and billing cases](spec/materials-and-finance.md#worked-parity-cases)

## Domain vocabulary

| Term | Identity and lifetime |
| --- | --- |
| **Printer** | A configured physical device with its serial, connection credentials, model and location. Online state and model-specific capabilities can change while its historical identity persists. |
| **Library file** | User-managed source or printable asset, in managed storage or an indexed external folder. A variant can be a separately sliced file for another printer. |
| **Archive** | Reusable print-file record with attachments, metadata and a summary of its latest print. It can exist before the first print, survive many prints and have historical aliases. |
| **Plate** | A selectable build plate inside a file. It can contain several objects and have its own G-code, material requirements, preview, quantities and estimates. A plate number has meaning within its source file. |
| **Queue item** | A request carrying a source, execution choices and scheduling constraints. A row awaiting a printer may have no assigned printer ID. The printer can run jobs that never had a queue item. |
| **Run** | One physical attempt on one printer. The print log stores these facts independently of file retention. An open run supports recovery. Completed history and statistics exclude open work. |
| **Batch, project, pipeline** | A batch tracks production quantities owed by plate. A project organizes a build, its files, parts and progress. A pipeline is a reusable slicing and delivery recipe with its own execution record. |
| **AMS slot, spool and preset** | A slot is an address on hardware. A spool is a physical inventory item that can move. A preset describes how to print a material. A firmware tray reading, a slot assignment and a known spool can disagree or be missing. |
| **Reservation and charge** | A reservation holds estimated funds against production. A wallet transaction records a financial movement. The physical dispatch has a billing identity distinct from the printer's subtask number. |

These distinctions affect reporting. Printing one file twice is one reusable archive and two attempts. Printing one plate carrying six objects is one physical run. Counting an order's remaining runs, its completed sets and its number of individual parts requires different inputs. [Archive and run models](../bambuddy/backend/app/models/print_log.py) · [Project and production accounting](spec/assets-and-slicing.md)

## Deployment and product scope

The core ships a browser application, a web/API process, persistent database and file storage, printer connections, and background workers. Local credentials, printer storage access and a reachable LAN serve the core workflow. The installation can add a headless slicer, material system, NFC/scale kiosk, identity provider, camera, power meter, notification destination or cloud account independently. A failure in one of these dependencies must be visible in the operation that uses it. For example, a failed camera grab can leave a run with no photo, while an acknowledged print keeps running. [Runtime composition](../bambuddy/backend/app/main.py) · [Deployment](spec/automation-and-operations.md)

| Surface | Ownership and rebuild scope |
| --- | --- |
| Bambuddy web, API, device adapters and virtual printers | In the reviewed repository. Include them for standalone product parity. |
| SpoolBuddy | Kiosk screens, server routes, Pi daemon and installation material are in the repository. Hardware is optional. [Companion source](../bambuddy/spoolbuddy) |
| Slicer engines | The repository supplies orchestration, integration and sidecar deployment definitions. OrcaSlicer and Bambu Studio perform the slicing through external engine packages. A new server can reuse that interface. [Sidecar definitions](../bambuddy/slicer-api) |
| Bambuddy Orders | Separately distributed application. It connects paid Shopify/Etsy orders to batches, stock, packing and fulfillment. The core owns batch production, status and connected-app integration. Shop credentials, sales channels, shipping and licensing belong to Orders. [Orders guide](https://wiki.bambuddy.cool/orders/) |
| Bambuddy Appliance | Separate Raspberry Pi appliance image and administration experience, including setup, update and recovery. Core has appliance-aware defaults and status. Rebuilding the core does not reproduce the image or its host admin panel. [Appliance guide](https://wiki.bambuddy.cool/appliance/) · [Core appliance interface](../bambuddy/backend/app/api/routes/system.py) |
| MCP | Bambuddy's documented MCP server is a separate community project. Bambastic ships a first-party Go MCP server that shares the application's authorization and print workflows. [Community MCP](https://wiki.bambuddy.cool/community/bambuddy-mcp/) · [Bambastic MCP contract](spec/bambastic-overlay.md#first-party-mcp-server) |
| Community clients and hosted services | Physical controls such as Bambutton, MCP clients, mobile dashboards, external catalogs, identity providers and detection servers consume or supply interfaces. Their implementations have independent lifecycles. [Community add-ons](https://wiki.bambuddy.cool/community/) |

## Where the release and the wiki diverge

| Topic | Parity decision for this release |
| --- | --- |
| Printer list | Include X2D alongside the X1, P1, P2, H2, H3, A1 and A2 families. The wiki home table omits X2D. Model-specific control, transport and sensor capabilities still need separate checks. [Release support table](../bambuddy/README.md) · [Wiki home](https://wiki.bambuddy.cool/) |
| Queue plate clearance | The tagged scheduler reads a global requirement and stores a per-printer waiting flag. The live guide also describes per-printer configuration. Separate a policy override from the actual fact that a particular bed needs clearing. [Release scheduler](../bambuddy/backend/app/services/print_scheduler.py) · [Queue guide](https://wiki.bambuddy.cool/features/print-queue/) |
| Slicing outputs and parameters | A direct library slice produces a library file, a direct archive slice produces an archive, and pipeline output goes to the library. The current wiki also describes a nozzle flow-class slice parameter absent from the release request. Preserve the tagged destinations and request shape. [Slice routes](../bambuddy/backend/app/api/routes/slice_jobs.py) · [Server-side slicing guide](https://wiki.bambuddy.cool/features/slicer-api/) |
| Wallet settlement | The guide describes selectable energy, wear and failure charging. The tagged settlement uses a material-cost baseline and a separate partial-run calculation. Energy can be displayed in analytics without entering a wallet charge. [Billing implementation](../bambuddy/backend/app/services/finance_billing.py) · [Billing guide](https://wiki.bambuddy.cool/features/billing/) |
| Integrations and access | The live wiki includes BIQU Panda Breath, an account-based 3D Filament Profiles integration and group-based filament restrictions. The tagged source has WLED, material/catalog editing and group-based **printer** restrictions. Treat the additional integrations and material restrictions as separate scope if targeting the live guide. [Release routes](../bambuddy/backend/app/api/routes) · [Panda Breath](https://wiki.bambuddy.cool/features/panda-breath/) · [3D Filament Profiles](https://wiki.bambuddy.cool/features/3d-filament-profiles/) · [Filament access](https://wiki.bambuddy.cool/features/authentication/#filament-access) |
| Backup restoration | The tagged ZIP manifest identifies the version and database. Restore replaces the contents of directories actually present in the ZIP. The wiki describes a later manifest distinguishing complete, partial and empty folders. This changes what happens to destination-only files. [Tagged backup writer and restore](../bambuddy/backend/app/api/routes/settings.py) · [Backup guide](https://wiki.bambuddy.cool/features/backup/) |

For a rebuild, start with one recorded printer trace from start through completion, one printable plate, one slot assignment and one terminal result. Require the replacement to reconstruct the same run twice from repeated observations without starting another print or charging it twice. That vertical slice establishes the contracts that the library, UI, integrations and larger farm workflows depend on. [Rebuild design and acceptance plan](spec/rebuild.md)
