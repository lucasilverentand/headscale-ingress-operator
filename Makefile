.PHONY: build test verify manifests

build:
	go build ./cmd/operator

test:
	go test ./...

manifests:
	kubectl kustomize deploy >/dev/null

verify: test build manifests
