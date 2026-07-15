.PHONY: test vet web build check docker

test:
	go test ./...

vet:
	go vet ./...

web:
	cd web && npm ci && npm run build

build: web
	go build -trimpath -ldflags "-s -w" -o jelly ./cmd/cli

check: test vet

docker:
	docker build -t jelly-agent:local .
