.PHONY: build test vet verify manifests helm-lint helm-template

build:
	go build ./cmd/operator

test:
	go test ./...

vet:
	go vet ./...

manifests:
	kubectl kustomize deploy >/dev/null

helm-lint:
	helm lint charts/headscale-ingress-operator

helm-template:
	helm template headscale-ingress-operator charts/headscale-ingress-operator >/dev/null

verify: test vet build manifests
