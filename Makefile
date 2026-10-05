.PHONY: setup test lint vuln dev

## One-time: secret-scanning git hook + resolve dependencies.
setup:
	git config core.hooksPath .githooks
	go mod tidy
	go mod verify

## Unit tests; offline (saved copies of the rink's page).
test:
	go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .) || (echo "gofmt needed"; exit 1)
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## The site on http://<this machine>:8081, fetching the real schedule.
## HOCKEY_DEV_BIND=127.0.0.1 keeps it local.
dev:
	HOCKEY_ADDR="$${HOCKEY_DEV_BIND:-0.0.0.0}:8081" go run ./cmd/hockey
