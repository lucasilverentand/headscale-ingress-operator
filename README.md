# Headscale Service Publisher

Kubernetes operator that publishes annotated Services into Headscale MagicDNS.
It does not create routing, proxy, TLS, or generated application resources.

The intended user flow is:

1. An app exposes a normal Kubernetes Service.
2. The Service opts in with
   `headscale-ingress-operator.lucasilverentand.dev/hostname`.
3. The operator resolves the Service target IPs and writes A/AAAA records into
   Headscale's managed `dns.extra_records_path` ConfigMap.
4. Headscale serves those names through MagicDNS.

Example:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: whoami
  namespace: apps
  annotations:
    headscale-ingress-operator.lucasilverentand.dev/hostname: whoami.cluster.example
spec:
  ports:
    - name: http
      port: 80
      targetPort: 8080
  selector:
    app.kubernetes.io/name: whoami
```

By default the operator publishes IPs from, in order:

- `.status.loadBalancer.ingress[].ip`
- `.spec.externalIPs`
- `.spec.clusterIPs`

For unusual cases, set
`headscale-ingress-operator.lucasilverentand.dev/target-ip` to a comma-separated
list of explicit A/AAAA targets.

The design is intentionally private-data-free. Examples use placeholder domains
and addresses.

See [docs/design.md](docs/design.md) for the direct Service-to-MagicDNS design
and setup notes.

## Current Status

This repo publishes release images and Helm charts from release-please managed
GitHub releases.

Implemented:

- Go controller loop using `client-go`
- Service reconciliation through a hostname annotation
- Headscale `extra_records_path` JSON written to an operator-owned ConfigMap
- service status annotation updates
- ownership checks that refuse unmanaged records ConfigMaps
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
