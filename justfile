# Show the available commands.
_default:
    @just --list

# Install frontend dependencies and build the production Vite bundle.
frontend:
    @go run ./tools/buildlock _frontend

# Build the Go server with the frontend embedded.
build:
    @go run ./tools/buildlock _build

_frontend:
    cd frontend && npm ci && npm run build

_build: _frontend
    mkdir -p bin
    go build -trimpath -o bin/bambastic .

air_version := "v1.64.5"

# Serve the app with Vite hot reload and automatic Go rebuilds.
run:
    @cd frontend && { test -x node_modules/.bin/vite && test node_modules/.package-lock.json -nt package-lock.json && test node_modules/.package-lock.json -nt package.json || npm ci; }
    @test -x bin/air-{{air_version}}/air || GOBIN="$PWD/bin/air-{{air_version}}" go install github.com/air-verse/air@{{air_version}}
    @node frontend/scripts/run-dev.mjs bin/air-{{air_version}}/air

# Start the integrated development server.
alias dev := run

_ensure-frontend:
    @test -f frontend/dist/index.html || just frontend

# Run the Go test suite.
test: _ensure-frontend
    go test . ./internal/... ./cmd/...

# Build the SpoolBuddy companion for this machine.
spoolbuddy:
    mkdir -p bin
    go build -trimpath -o bin/spoolbuddy ./cmd/spoolbuddy

# Start the Go backend for local development.
dev-backend: _ensure-frontend
    go run -tags dev . serve

# Start the Vite development server.
dev-frontend:
    cd frontend && npm run dev
