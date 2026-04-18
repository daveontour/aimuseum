.PHONY: build build-exe build-exe-electron build-linux build-launcher test generate lint run clean tidy

MODULE := github.com/daveontour/aimuseum
BINARY := digitalmuseum
CMD     := ./cmd/server

build:
	go build -o bin/$(BINARY) $(CMD)

build-exe:
	go build -o bin/$(BINARY).exe $(CMD)

# Windowsgui subsystem build — no console window when launched by Electron.
build-exe-electron:
	go build -ldflags="-H windowsgui" -o bin/$(BINARY).exe $(CMD)

build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/$(BINARY)-linux-amd64 $(CMD)

build-launcher:
	go build -buildvcs=false -ldflags="-H windowsgui" -o launcher.exe ./cmd/launcher

run:
	go run $(CMD)

test:
	go test ./...

test-verbose:
	go test -v ./...

generate:
	sqlc generate

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	rm -f bin/$(BINARY)

# Run with race detector
race:
	go run -race $(CMD)

# Build and run (convenience)
dev: build
	./bin/$(BINARY)

# ── Electron targets ──────────────────────────────────────────────────────────

# Run Electron in dev mode against the local source tree (no packaging).
# Requires Go binary at bin/digitalmuseum.exe.
electron-dev: build-exe-electron
	cd electron && npm install --prefer-offline && npx electron .

# Package the Electron app into a distributable installer.
# Produces dist/electron/Digital Museum Setup *.exe
electron-dist: build-exe-electron
	cd electron && npm install --prefer-offline && npx electron-builder
