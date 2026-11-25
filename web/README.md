# Bambastic browser console

This independent React and TypeScript implementation uses the Bambastic Go API directly. Its source is licensed under MIT. The production bundle includes license notices for its browser dependencies.

From the repository root, run `just build-console`, then start `./bin/bambastic-console serve`. To try it with a separate installation, pass `--addr 127.0.0.1:8090 --data /path/to/new-directory`. A development session uses `npm ci` and `npm run dev` in this directory. Set `BAMBASTIC_URL` to the backend origin if it differs from the default local port 8080.

The console includes account setup, sign-in, second-factor verification, fleet registration and status, camera views, file and archive uploads, sliced G-code and 3MF layer previews, manual queue submission and release, spools, projects, maintenance records, print statistics, cost centers, SpoolBuddy station listings, and MCP client setup and consent. More workflows from the existing operator UI are still being migrated. The default release continues to use the existing interface during this transition.
