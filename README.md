# Headscale Ingress Operator

Kubernetes operator that publishes Kubernetes Ingresses into Headscale MagicDNS
and creates the per-app tailnet proxy workloads behind them.

The intended user flow is:

1. An app exposes a normal Kubernetes Service.
2. An Ingress with `ingressClassName: headscale` declares the host and backend.
3. The operator creates a per-Ingress tailnet proxy, mints/adopts its Headscale
   auth key, and discovers the proxy node IPs.
4. The operator writes A/AAAA records into Headscale's managed
   `dns.extra_records_path` ConfigMap.
5. Headscale serves those names through MagicDNS.

Example:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: whoami
  namespace: apps
spec:
  ingressClassName: headscale
  rules:
    - host: whoami.cluster.example
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: whoami
                port:
                  number: 80
```

The design is intentionally private-data-free. Examples use placeholder domains
and addresses.

See [docs/design.md](docs/design.md) for the Ingress-to-MagicDNS design,
managed proxy mode, and setup notes.

## Current Status

This repo publishes release images and Helm charts from release-please managed
GitHub releases.

Implemented:

- Go controller loop using `client-go`
- Ingress reconciliation through `ingressClassName: headscale`
- Headscale `extra_records_path` JSON written to an operator-owned ConfigMap
- Ingress status annotation updates and load balancer status updates
- per-Ingress tailnet proxy Deployments
- ownership checks that refuse unmanaged records ConfigMaps
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

## Managed Tailnet Ingresses

Enable managed proxy support in the chart before using Headscale Ingresses:

```yaml
headscale:
  serverURL: https://headscale.example

proxy:
  enabled: true
  tailscaleImage: tailscale/tailscale:v1.98.3
  nginxImage: nginx:1.27-alpine
  defaultTLSSecret: wildcard-example-tls
```

Then declare a normal Ingress:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: whoami
  namespace: apps
spec:
  ingressClassName: headscale
  rules:
    - host: whoami.cluster.example
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: whoami
                port:
                  name: http
```

The operator creates a per-Ingress proxy Deployment in the Ingress namespace.
The proxy joins Headscale as one app-specific node, runs
`tailscale serve` on TCP/443, terminates TLS with nginx, and forwards to the
Ingress backend Service. The operator discovers the app node's Headscale IPs and
publishes those as DNS targets.

The app-side ideal is:

1. Deploy the app Pod or Deployment.
2. Expose the app with a normal Kubernetes Service port.
3. Declare a `headscale` Ingress.

Everything else is created and reconciled by the operator.

Useful optional annotations:

- `headscale-ingress-operator.lucasilverentand.dev/proxy-tls-secret`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-auth-secret`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-state-secret`
- `headscale-ingress-operator.lucasilverentand.dev/proxy-tailnet-name`

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
