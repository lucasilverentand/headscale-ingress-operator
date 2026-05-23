# Headscale Ingress Operator

Kubernetes operator that publishes annotated Services into Headscale MagicDNS.
It can also create an opt-in per-Service tailnet proxy workload so applications
do not have to carry their own Tailscale sidecar manifests.

The intended user flow is:

1. An app exposes a normal Kubernetes Service.
2. The Service opts in with
   `headscale-ingress-operator.lucasilverentand.dev/hostname`.
3. The operator either resolves the Service target IPs directly or manages a
   per-Service tailnet proxy.
4. The operator writes A/AAAA records into Headscale's managed
   `dns.extra_records_path` ConfigMap.
5. Headscale serves those names through MagicDNS.

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

See [docs/design.md](docs/design.md) for the Service-to-MagicDNS design,
managed proxy mode, and setup notes.

## Current Status

This repo publishes release images and Helm charts from release-please managed
GitHub releases.

Implemented:

- Go controller loop using `client-go`
- Service reconciliation through a hostname annotation
- Headscale `extra_records_path` JSON written to an operator-owned ConfigMap
- service status annotation updates
- optional per-Service tailnet proxy Deployments
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

## Managed Tailnet Proxies

By default the operator only publishes DNS records. Enable managed proxy support
in the chart before using proxy annotations:

```yaml
headscale:
  serverURL: https://headscale.example

proxy:
  enabled: true
  tailscaleImage: tailscale/tailscale:v1.98.3
  nginxImage: nginx:1.27-alpine
  defaultTLSSecret: wildcard-example-tls
```

Then opt a Service into per-app proxy management:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: whoami
  namespace: apps
  annotations:
    headscale-ingress-operator.lucasilverentand.dev/hostname: whoami.cluster.example
    headscale-ingress-operator.lucasilverentand.dev/target-ip: 100.64.0.10
    headscale-ingress-operator.lucasilverentand.dev/proxy: managed
spec:
  ports:
    - name: http
      port: 80
      targetPort: 8080
  selector:
    app.kubernetes.io/name: whoami
```

In managed mode, the operator creates a per-Service proxy Deployment in the
Service namespace. The proxy joins Headscale as one app-specific node, runs
`tailscale serve` on TCP/443, terminates TLS with nginx, and forwards to the
Kubernetes Service. The `target-ip` annotation remains the DNS target for the
app's stable Headscale IP.

Useful optional annotations:

- `headscale-ingress-operator.lucasilverentand.dev/proxy-tls-secret`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-auth-secret`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-state-secret`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-tailnet-name`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-service-port`

## Headscale `extra_records_path`

Headscale versions that support `dns.extra_records_path` can watch the JSON
records file and reload DNS-only changes without restarting Headscale. The
operator writes that JSON into one ConfigMap key:

```yaml
headscale:
  namespace: headscale
  recordsConfigMap: headscale-extra-records
  recordsKey: extra-records.json
```

If Headscale starts before the operator has written records, enable the chart's
seed ConfigMap so the mounted file already contains valid empty JSON:

```yaml
headscale:
  seedConfigMap:
    create: true
```

The seed ConfigMap is labelled
`app.kubernetes.io/managed-by=headscale-ingress-operator`, which is the label the
operator requires before it will update the resource. Helm renders the current
live records data on upgrades when it can read the cluster, so it does not reset
the key back to `[]`.

Configure Headscale to watch the same mounted key:

```yaml
dns:
  magic_dns: true
  extra_records_path: /etc/headscale/extra-records/extra-records.json
```

Mount the ConfigMap at that path in the Headscale workload:

```yaml
volumeMounts:
  - name: extra-records
    mountPath: /etc/headscale/extra-records/extra-records.json
    subPath: extra-records.json
    readOnly: true
volumes:
  - name: extra-records
    configMap:
      name: headscale-extra-records
      items:
        - key: extra-records.json
          path: extra-records.json
```

Keep DNS-only changes out of restart annotations or checksum rollouts. Headscale
should pick up changes from the watched file.

For GitOps controllers, create the seed ConfigMap once with:

```yaml
data:
  extra-records.json: "[]"
```

Then stop managing that data key or configure the controller to ignore it. A
continuously reconciled seed manifest that keeps writing `[]` will fight the
operator.
