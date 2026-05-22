# Direct Service MagicDNS Design

The operator has one job: publish annotated Kubernetes Services into Headscale
MagicDNS. It should not own routing, proxying, TLS, or any generated workload.

## Source Resource

Applications expose normal Services. A Service is published only when it has a
hostname annotation:

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

The hostname annotation may contain a comma-separated list of DNS names. The
operator validates each name, rejects wildcards, and applies the configured
allowed-zone list before writing anything to Headscale.

## DNS Targets

The operator publishes A/AAAA records using these Service addresses:

1. `.status.loadBalancer.ingress[].ip`
2. `.spec.externalIPs`
3. `.spec.clusterIPs`

Headless Services without an explicit target stay pending because there is no
single A/AAAA target to publish.

For the small number of cases where the Service address is not the address that
Headscale clients should use, a Service may set:

```yaml
headscale-ingress-operator.lucasilverentand.dev/target-ip: 100.64.0.10,fd7a:115c:a1e0::10
```

That annotation is still just DNS target selection. It does not create a proxy
or route.

## Headscale Write Path

Headscale watches a JSON file through `dns.extra_records_path`. In Kubernetes,
the operator writes that JSON into a ConfigMap owned by the operator:

```yaml
dns:
  magic_dns: true
  extra_records_path: /etc/headscale/extra-records.json
```

The Headscale Deployment should mount the operator-managed ConfigMap at that
path. The operator refuses to update an existing ConfigMap unless it has
`app.kubernetes.io/managed-by=headscale-ingress-operator`, which keeps it from
overwriting hand-maintained DNS state.

## Reconciliation

Each loop:

1. List Services across namespaces.
2. Keep only Services with
   `headscale-ingress-operator.lucasilverentand.dev/hostname`.
3. Validate hostnames and reject duplicate claims.
4. Resolve Service target IPs.
5. Render deterministic Headscale `extra_records_path` JSON.
6. Patch each participating Service with a small status annotation.

Status values:

| Value | Meaning |
| --- | --- |
| `Ready` | The Service has valid hostnames and at least one published A/AAAA target. |
| `PendingTarget` | The Service opted in but has no usable A/AAAA target yet. |
| `Rejected` | The Service has invalid hostnames, conflicting hostnames, or invalid target IPs. |

## Permissions

The operator needs only:

- cluster-wide `get`, `list`, `watch`, and `patch` on Services
- `get`, `update`, and `patch` on the managed records ConfigMap
- `create` on ConfigMaps in the Headscale namespace

It does not need permission to create application resources.

## Safety Rules

- Empty hostname annotation means the Service is ignored.
- Wildcard hostnames are rejected.
- Hostnames outside `--allowed-zones` are rejected when zones are configured.
- Two Services may not claim the same hostname.
- DNS records are only A/AAAA records.
- Unspecified and multicast IPs are rejected.
- Unmanaged records ConfigMaps are never overwritten.
