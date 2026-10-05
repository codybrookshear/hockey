.PHONY: setup test lint vuln dev dev-schedule dev-rhl

## One-time: secret-scanning git hook + resolve dependencies.
setup:
	git config core.hooksPath .githooks
	go mod tidy
	go mod verify

## Unit tests; offline (saved copies of the rink's page and GameSheet's API).
test:
	go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .) || (echo "gofmt needed"; exit 1)
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## Both sites, reading the real sources: the schedule on http://<this
## machine>:8081, the RHL on :8082. HOCKEY_DEV_BIND=127.0.0.1 keeps them local.
dev:
	$(MAKE) -j2 dev-schedule dev-rhl

dev-schedule:
	SCHEDULE_ADDR="$${HOCKEY_DEV_BIND:-0.0.0.0}:8081" go run ./cmd/schedule

dev-rhl:
	RHL_ADDR="$${HOCKEY_DEV_BIND:-0.0.0.0}:8082" go run ./cmd/rhl
