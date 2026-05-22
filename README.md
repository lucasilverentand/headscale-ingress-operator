# Headscale Ingress Operator

Kubernetes operator that exposes ordinary Ingress-backed HTTP(S) services
through a Headscale tailnet and publishes the matching Headscale MagicDNS
records.

The intended user flow is:

1. An app declares an Ingress with `ingressClassName: headscale`.
2. The operator creates the implementation Ingress or Traefik `IngressRoute`
   used by the cluster ingress controller.
3. The operator publishes A/AAAA records into Headscale's managed
   `dns.extra_records_path` file.
4. Tailnet clients resolve the app hostname through Headscale and reach the
   internal ingress endpoint through an advertised route or a managed tailnet
   gateway.

The design is intentionally private-data-free. Examples use placeholder domains
and addresses.

See [docs/design.md](docs/design.md) for the proposed architecture, setup
steps, CRD/API shape, and migration notes for the existing cluster patterns.

## Current Status

This repo publishes release images and Helm charts from release-please managed
GitHub releases.

Implemented:

- Go controller loop using `client-go`
- `Ingress` reconciliation for `ingressClassName: headscale`
- generated downstream `Ingress` resources using a configurable implementation
  class
- public DNS suppression on generated resources
- Headscale `extra_records_path` JSON written to an operator-owned ConfigMap
- source Ingress status annotation and load balancer status updates
- ownership checks that refuse ambiguous generated resources and unmanaged
  records ConfigMaps
- local fake Kubernetes API tests
- deployable Kubernetes YAML under `deploy/`
- Helm chart under `charts/headscale-ingress-operator`
- container image publishing to `ghcr.io/lucasilverentand/headscale-ingress-operator`

## Local Development

Install the pinned toolchain:

```bash
mise install
```

Run the checks:

```bash
mise exec -- make verify
```

The test suite includes:

- reconciliation tests using `k8s.io/client-go/kubernetes/fake`
- a local `httptest` fake Kubernetes API server that the real client-go client
  talks to over HTTP

Build the operator binary:

```bash
mise exec -- go build ./cmd/operator
```

Render the install manifests:

```bash
kubectl kustomize deploy
```

The default deployment image is `headscale-ingress-operator:local`. Build and
load that image into your local test cluster, or patch the image to a private
registry before applying the manifests.

Install the published Helm chart:

```bash
helm install headscale-ingress-operator \
  oci://ghcr.io/lucasilverentand/charts/headscale-ingress-operator \
  --namespace headscale-ingress-operator \
  --create-namespace
```

The chart defaults to `ghcr.io/lucasilverentand/headscale-ingress-operator`
and uses the chart `appVersion` as the image tag unless `image.tag` is set.
