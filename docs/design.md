# Service MagicDNS and Tailnet Proxy Design

The operator has one job: publish annotated Kubernetes Services into Headscale
MagicDNS. It can optionally own the per-Service tailnet proxy workload that
backs the published Headscale address.

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

In DNS-only mode that annotation is just DNS target selection. In managed proxy
mode it is optional: when omitted, the operator discovers the app-specific
Headscale node by tailnet name and publishes the node's assigned IPs.

## Managed Proxy Mode

Managed proxy mode is opt-in at two levels:

1. The chart must enable proxy management.
2. The Service must set
   `headscale-ingress-operator.lucasilverentand.dev/proxy: managed`.

When both are true, the operator creates per-Service resources in the Service
namespace:

- ServiceAccount
- Role and RoleBinding for the proxy's Tailscale state Secret
- Secret containing a Headscale preauth key for the proxy
- nginx ConfigMap
- Deployment with `tailscale/tailscale` and nginx containers

The proxy joins Headscale with an app-specific node name, runs
`tailscale serve --tcp 443`, terminates TLS on localhost with nginx, and
forwards traffic to the Kubernetes Service DNS name. The operator mints the
preauth key by executing the Headscale CLI in the configured Headscale pod,
then writes the key into the Service namespace.

Per-Service annotations override defaults:

| Annotation | Purpose |
| --- | --- |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-tls-secret` | TLS Secret mounted into nginx. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-auth-secret` | Secret containing `TS_AUTHKEY`. Defaults to `<service>-tailnet-authkey`. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-state-secret` | Secret used by `TS_KUBE_SECRET`. Defaults to `tailscale-<service>`. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-tailnet-name` | Headscale/Tailscale node name. Defaults to the Service name. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-service-port` | Service port forwarded by nginx. Defaults to the first Service port. |

This keeps the Headscale identity per application. It does not create one shared
gateway node for all applications.

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

The Helm chart can optionally seed that ConfigMap with an empty records array so
the file exists before Headscale starts:

```yaml
headscale:
  seedConfigMap:
    create: true
```

That seed is only the bootstrap handoff. After startup, the operator owns the
records key and writes the full desired DNS set on every reconciliation.

Example Headscale mount:

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

GitOps tools should not keep reconciling the records data back to `[]`. Create
the seed once, ignore the records key after bootstrap, or let the Helm chart
seed it while preserving the live value on upgrades.

## Reconciliation

Each loop:

1. List Services across namespaces.
2. Keep only Services with
   `headscale-ingress-operator.lucasilverentand.dev/hostname`.
3. Validate hostnames and reject duplicate claims.
4. Resolve Service target IPs.
5. Mint an app auth key and reconcile an app-specific proxy workload when the
   Service opts into managed proxy mode.
6. Discover the app Headscale node IPs unless explicit target IPs are set.
7. Render deterministic Headscale `extra_records_path` JSON.
8. Patch each participating Service with a small status annotation.
9. Delete generated proxy resources for Services that no longer opt in.

Status values:

| Value | Meaning |
| --- | --- |
| `Ready` | The Service has valid hostnames and at least one published A/AAAA target. |
| `PendingTarget` | The Service opted in but has no usable A/AAAA target yet. |
| `PendingAuthKey` | Managed proxy mode is waiting for a Headscale preauth key. |
| `PendingNodeIP` | Managed proxy mode is waiting for the app node to appear in Headscale. |
| `Rejected` | The Service has invalid hostnames, conflicting hostnames, or invalid target IPs. |

## Permissions

The operator needs:

- cluster-wide `get`, `list`, `watch`, and `patch` on Services
- `get`, `update`, and `patch` on the managed records ConfigMap
- `create` on ConfigMaps in the Headscale namespace
- `get`, `list`, `watch`, `create`, `update`, and `patch` on generated
  ServiceAccounts, ConfigMaps, Deployments, Roles, and RoleBindings when managed
  proxy mode is enabled
- `get` and `list` on Headscale pods plus `create` on `pods/exec` in the
  Headscale namespace when managed proxy mode mints auth keys or discovers node
  IPs

Generated proxy resources are owned by the source Service, so normal Kubernetes
garbage collection removes them when the Service is deleted.

## Safety Rules

- Empty hostname annotation means the Service is ignored.
- Wildcard hostnames are rejected.
- Hostnames outside `--allowed-zones` are rejected when zones are configured.
- Two Services may not claim the same hostname.
- DNS records are only A/AAAA records.
- Unspecified and multicast IPs are rejected.
- Unmanaged records ConfigMaps are never overwritten.
