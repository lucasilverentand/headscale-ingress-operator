.PHONY: build test vet verify manifests

build:
	go build ./cmd/operator

test:
	go test ./...

vet:
	go vet ./...

manifests:
	kubectl kustomize deploy >/dev/null

verify: test vet build manifests
