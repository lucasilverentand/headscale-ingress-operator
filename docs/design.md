# Headscale Ingress and Tailnet Proxy Design

The operator has one job: turn a normal Kubernetes Ingress into a Headscale
reachable HTTP endpoint. It owns the per-app tailnet proxy workload and the
Headscale DNS records behind that endpoint.

## Source Resource

Applications expose normal Services. A Headscale route is declared with a
standard Ingress using the operator's IngressClass:

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

That is the preferred app-side API. The app declares a Pod or Deployment, exposes
a Service port, and declares an Ingress. The operator creates everything else:
auth key Secret, tailnet proxy Deployment, nginx config, RBAC, DNS records, and
Ingress status.

The operator does not publish annotated Services directly. Services are only
backend targets referenced by Ingress rules.

The operator validates each hostname, rejects wildcards, and applies the
configured allowed-zone list before writing anything to Headscale.

## DNS Targets

For Ingress sources, DNS targets are the Headscale node IPs assigned to the
operator-managed proxy. The operator discovers those IPs from Headscale by the
proxy tailnet name.

## Managed Proxy Mode

Ingress sources always use managed proxy mode. The chart must enable proxy
management before Ingress sources can reconcile successfully.

When managed proxy mode is active, the operator creates per-source resources in
the source namespace:

- ServiceAccount
- Role and RoleBinding for the proxy's Tailscale state Secret
- Secret containing a Headscale preauth key for the proxy
- nginx ConfigMap
- Deployment with `tailscale/tailscale` and nginx containers

The proxy joins Headscale with an app-specific node name, runs
`tailscale serve --tcp 443`, terminates TLS on localhost with nginx, and
forwards traffic to the backend Service DNS name declared by the Ingress. The
operator mints the preauth key by executing the Headscale CLI in the configured
Headscale pod, then writes the key into the source namespace.

Ingress annotations override defaults:

| Annotation | Purpose |
| --- | --- |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-tls-secret` | TLS Secret mounted into nginx. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-auth-secret` | Secret containing `TS_AUTHKEY`. Defaults to `<source>-tailnet-authkey`. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-state-secret` | Secret used by `TS_KUBE_SECRET`. Defaults to `tailscale-<source>`. |
| `headscale-ingress-operator.lucasilverentand.dev/proxy-tailnet-name` | Headscale/Tailscale node name. Defaults to the source name. |

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

1. List Ingresses across namespaces.
2. Keep Ingresses with `ingressClassName: headscale`.
3. Validate hostnames and reject duplicate claims.
4. Resolve Ingress backend Service routes.
5. Mint an app auth key and reconcile an app-specific proxy workload.
6. Discover the app Headscale node IPs.
7. Render deterministic Headscale `extra_records_path` JSON.
8. Patch each participating Ingress with a small status annotation and update
   Ingress load balancer status with the Headscale node IPs.
9. Delete generated proxy resources for Ingresses that no longer opt in.

Status values:

| Value | Meaning |
| --- | --- |
| `Ready` | The Ingress has valid hostnames and at least one published A/AAAA target. |
| `PendingAuthKey` | Managed proxy mode is waiting for a Headscale preauth key. |
| `PendingNodeIP` | Managed proxy mode is waiting for the app node to appear in Headscale. |
| `Rejected` | The Ingress has invalid hostnames, conflicting hostnames, or invalid backends. |

## Permissions

The operator needs:

- `get` on backend Services
- cluster-wide `get`, `list`, `watch`, and `patch` on Ingresses
- `get`, `update`, and `patch` on Ingress status
- `get`, `update`, and `patch` on the managed records ConfigMap
- `create` on ConfigMaps in the Headscale namespace
- `get`, `list`, `watch`, `create`, `update`, and `patch` on generated
  ServiceAccounts, ConfigMaps, Deployments, Roles, and RoleBindings when managed
  proxy mode is enabled
- `get` and `list` on Headscale pods plus `create` on `pods/exec` in the
  Headscale namespace when managed proxy mode mints auth keys or discovers node
  IPs

Generated proxy resources are owned by the Ingress, so normal Kubernetes
garbage collection removes them when the Ingress is deleted.

## Safety Rules

- Ingresses are ignored unless they use the configured IngressClass.
- Wildcard hostnames are rejected.
- Hostnames outside `--allowed-zones` are rejected when zones are configured.
- Two Ingresses may not claim the same hostname.
- DNS records are only A/AAAA records.
- Unspecified and multicast IPs are rejected.
- Unmanaged records ConfigMaps are never overwritten.
