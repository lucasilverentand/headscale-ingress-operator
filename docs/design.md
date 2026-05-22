# Headscale-Compatible Ingress Operator Design

## Goal

Build a Kubernetes operator that gives Headscale users the ergonomic shape of a
tailnet Ingress controller without depending on Tailscale SaaS APIs.

Application authors should be able to declare a normal Kubernetes Ingress:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: app
  namespace: app
spec:
  ingressClassName: headscale
  tls:
    - hosts:
        - app.cluster.example
      secretName: wildcard-tls
  rules:
    - host: app.cluster.example
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: app
                port:
                  number: 8080
```

The operator should then:

- create the concrete internal Ingress resource used by the cluster's ingress
  controller
- suppress public DNS publishing for the implementation resource
- publish `app.cluster.example` into Headscale MagicDNS
- remove the implementation resource and DNS record when the source Ingress is
  deleted or no longer uses the `headscale` class
- report useful Kubernetes status conditions instead of requiring humans to
  inspect generated resources

## Research Summary

Headscale's current documented dynamic DNS integration is
`dns.extra_records_path`: Headscale watches a JSON file of DNS records and
processes changes without restarting. Inline `dns.extra_records` is for static
records and needs a restart when changed. Current Headscale/Tailscale clients
only process A and AAAA records for these extra records, so the operator must
not emit CNAMEs or wildcard records.

Headscale exposes REST and gRPC APIs, but the documented REST examples cover
server operations such as users and node registration, not a DNS-record CRUD API.
For this operator, the clean integration is a single Kubernetes-owned dynamic
records file mounted into Headscale, not direct mutation through an undocumented
endpoint.

Kubernetes Ingress is stable, but frozen. It remains the right first API because
the cluster already uses ingress-style host routing and because users expect
IngressClass-based ownership. Gateway API support can be added later without
changing the Headscale DNS writer.

The Tailscale Kubernetes Operator is a useful reference for UX: users set
`ingressClassName: tailscale`, and the operator creates tailnet-facing proxy
resources. It is not a drop-in fit for Headscale because its production path
depends on Tailscale control-plane features, OAuth scopes, SaaS MagicDNS names,
and Tailscale certificate behavior. This operator should copy the IngressClass
ergonomics, not the SaaS control-plane dependency.

ExternalDNS is the closest DNS reconciliation model: it reads Kubernetes
Services and Ingresses, computes desired DNS records, and writes them through a
provider. The important lesson is record ownership. This operator needs one
clear owner for the Headscale dynamic records file and a domain allowlist so it
does not take over unrelated names.

## Cluster Patterns To Preserve

The existing cluster repo points to these matching patterns:

- templates are rendered through Kustodian and deployed by Flux, so the operator
  should be installable as a template with cluster-owned substitutions
- Traefik is the ingress controller, with explicit public and internal
  entrypoints
- private routes are marked so public external-dns does not publish them
- Headscale already pushes DNS to mesh clients through MagicDNS and uses
  extra-records for exact host overrides
- the current manual step is capturing a tailnet IP and copying it back into
  cluster values
- future app migrations prefer keeping tailnet access behavior close to the app
  boundary and avoiding broad shared privilege

This design keeps those lessons but removes the manual DNS/IP copy step.

## Architecture

### Controllers

The operator has four reconcilers.

`IngressClass` bootstrap reconciler:

- ensures an `IngressClass` named `headscale` exists
- sets `spec.controller` to `headscale.silverswarm.io/ingress-controller`
- optionally marks it non-default so services must opt in deliberately

Source Ingress reconciler:

- watches `networking.k8s.io/v1` Ingresses with `spec.ingressClassName:
  headscale`
- validates hosts, TLS hosts, paths, backend service refs, and domain allowlist
- creates or updates an implementation resource for the real ingress controller
- adds a finalizer so DNS records are removed before the source object
  disappears
- writes status conditions on the source Ingress

Implementation renderer:

- default renderer creates a second `networking.k8s.io/v1` Ingress with the
  configured downstream class, for example `traefik-internal`
- Traefik renderer can create `traefik.io/v1alpha1` `IngressRoute` resources to
  match clusters that already standardize on Traefik CRDs
- all generated implementation resources include
  `external-dns.alpha.kubernetes.io/exclude: "true"` by default
- generated resources are owned by the source Ingress

Headscale DNS reconciler:

- watches source Ingresses and implementation resource status
- computes the desired A/AAAA record set
- merges managed ingress records with configured static managed records
- writes a sorted, stable JSON list to a Kubernetes ConfigMap key
- exposes metrics and Events when a record cannot be published

### DNS Write Path

Use a dedicated ConfigMap as the dynamic Headscale records source:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: headscale-extra-records
  namespace: headscale
data:
  extra-records.json: |
    [
      {
        "name": "app.cluster.example",
        "type": "A",
        "value": "100.64.0.10"
      }
    ]
```

Mount that ConfigMap into the Headscale pod as a directory, not a `subPath`, and
point Headscale at it:

```yaml
dns:
  magic_dns: true
  base_domain: tailnet.example
  extra_records_path: /etc/headscale/extra-records/extra-records.json
```

The operator owns this ConfigMap. Static records that should live in the same
dynamic file should be represented as `HeadscaleDNSRecord` custom resources or
as `staticRecords` in the operator configuration. Do not let humans and the
operator edit the same JSON list by hand.

Clusters that still run a Headscale version without documented
`extra_records_path` support can use a compatibility mode:

- render inline `dns.extra_records` from the operator-owned ConfigMap
- trigger a Headscale rollout after the ConfigMap changes
- treat this as temporary because it restarts the control plane for DNS changes

### DNS Target Strategies

The operator should support two target strategies.

`IngressStatus` target:

- reads the generated implementation Ingress status or a configured ingress
  controller Service address
- publishes that IP as the Headscale record value
- requires tailnet clients to have a route to the internal ingress address,
  usually through a Headscale subnet router
- fits the current cluster model best because Traefik already owns host routing

`TailnetGateway` target:

- registers one or more managed Tailscale/Headscale gateway pods as stable
  tailnet nodes
- publishes app hostnames to the gateway tailnet IPs
- the gateway forwards TCP/443 to the internal ingress controller and preserves
  SNI/Host routing
- works when clients cannot route to the cluster's internal load-balancer IPs

`IngressStatus` should be the MVP because it is simpler and matches the existing
Traefik plus subnet-router pattern. `TailnetGateway` is the escape hatch for
clusters without routed access to the internal ingress endpoint.

### API Surface

Use standard Ingress as the primary API. Keep custom resources small and
operator-focused.

Operator configuration:

```yaml
apiVersion: headscale.silverswarm.io/v1alpha1
kind: HeadscaleIngressConfig
metadata:
  name: default
spec:
  sourceClassName: headscale
  implementation:
    kind: Ingress
    ingressClassName: traefik-internal
    tlsSecretName: wildcard-tls
    addExternalDNSExclude: true
  dns:
    headscaleNamespace: headscale
    recordsConfigMapName: headscale-extra-records
    recordsConfigMapKey: extra-records.json
    allowedZones:
      - cluster.example
    targetStrategy: IngressStatus
    defaultTargetIPs:
      - 192.0.2.10
```

Optional static record CRD:

```yaml
apiVersion: headscale.silverswarm.io/v1alpha1
kind: HeadscaleDNSRecord
metadata:
  name: static-admin
  namespace: headscale
spec:
  name: admin.cluster.example
  type: A
  value: 100.64.0.20
```

Useful annotations on source Ingresses:

| Annotation | Purpose |
| --- | --- |
| `headscale.silverswarm.io/implementation-kind` | Override renderer, such as `Ingress` or `TraefikIngressRoute`. |
| `headscale.silverswarm.io/target-ip` | Explicit DNS target for unusual migrations. Must be an IP. |
| `headscale.silverswarm.io/publish: "false"` | Create implementation ingress but skip Headscale DNS. |
| `headscale.silverswarm.io/tls-secret` | Override default TLS secret for the implementation resource. |

Avoid annotations for credentials. API keys, auth keys, and Headscale connection
settings belong in Secrets or the operator config.

### Status

Write conditions on each source Ingress:

| Condition | Meaning |
| --- | --- |
| `Accepted` | The Ingress uses the managed class and passed basic validation. |
| `Implemented` | The downstream Ingress or IngressRoute has been applied. |
| `TargetResolved` | The operator found an A/AAAA-compatible target address. |
| `DNSPublished` | The Headscale records ConfigMap contains the desired records. |
| `Ready` | Implementation and DNS are both current. |

Also set `.status.loadBalancer.ingress` on the source Ingress to the published
address when possible. That keeps `kubectl get ingress` useful.

## Security Model

The operator should have narrow Kubernetes RBAC:

- watch source Ingresses, Services, Endpoints or EndpointSlices as needed
- create/update/delete only generated implementation resources
- update status on source Ingresses
- update one named ConfigMap in the Headscale namespace
- read one optional Secret for Headscale API health checks if enabled

It should not need `pods/exec` against Headscale. The current cluster uses
`pods/exec` for some operational Headscale tasks; this operator can avoid that
for DNS by writing the dynamic records file through Kubernetes.

DNS publication must be restricted:

- require an allowlist of zones
- reject wildcard hosts
- reject CNAMEs and hostnames as values
- lowercase and normalize names before writing JSON
- refuse to publish an Ingress with no host
- emit a clear condition when a host is outside policy

## Setup Plan

1. Upgrade or configure Headscale for dynamic records.

   Set `dns.magic_dns: true` and configure
   `dns.extra_records_path: /etc/headscale/extra-records/extra-records.json`.
   Mount the operator-owned records ConfigMap at that path. Keep any legacy
   inline records static or migrate them into `HeadscaleDNSRecord` resources.

2. Make routing explicit.

   For `IngressStatus`, ensure tailnet clients can route to the internal ingress
   target IP through a Headscale subnet router. For `TailnetGateway`, deploy the
   managed gateway pod and verify its stable tailnet identity before publishing
   app records.

3. Install the operator through the cluster template system.

   Add a template for the controller Deployment, RBAC, `IngressClass`, CRDs,
   metrics Service, and the Headscale records ConfigMap. Expose substitutions
   for downstream ingress class, allowed DNS zones, target strategy, target IPs,
   image version, and resource requests.

4. Convert one low-risk app first.

   Replace the hand-written internal ingress route with a source Ingress using
   `ingressClassName: headscale`, or add the source Ingress next to the existing
   route with `headscale.silverswarm.io/publish: "false"` until the generated
   resource is confirmed. Then turn DNS publishing on.

5. Verify from both Kubernetes and a tailnet client.

   ```bash
   kubectl get ingress -A \
     -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,CLASS:.spec.ingressClassName,HOSTS:.spec.rules[*].host'
   kubectl -n headscale get configmap headscale-extra-records -o yaml
   dig +short app.cluster.example
   curl -vk https://app.cluster.example/
   ```

6. Remove old manual DNS entries.

   Once the operator-published record works, delete the matching old
   `headscale_extra_records` value from cluster configuration so there is a
   single source of truth.

## Failure Handling

If the implementation ingress has no address:

- keep the source Ingress `Ready=False`
- do not publish DNS unless a configured static target exists
- record an Event with the missing target reason

If the records ConfigMap update fails:

- retry with exponential backoff
- keep the finalizer on the source Ingress
- leave the previous DNS file untouched

If an Ingress is deleted:

- remove its generated implementation resource
- remove its records from the desired set
- write the new sorted JSON file
- then remove the finalizer

If two Ingresses claim the same host:

- allow it only when they resolve to the exact same target and rules are part of
  the same source object
- otherwise mark both conflicting records `Ready=False` and publish neither

## Validation

Unit tests:

- host normalization and zone allowlist
- A/AAAA target validation
- deterministic JSON sorting
- deletion/finalizer behavior
- duplicate-host conflict handling
- renderer output for standard Ingress and Traefik IngressRoute

Integration tests:

- envtest reconciliation from source Ingress to implementation resource
- ConfigMap write and record removal
- status condition transitions
- compatibility mode restart trigger

Cluster smoke test:

- deploy a tiny HTTP service
- create a `headscale` Ingress with a synthetic hostname
- verify the implementation resource exists
- verify Headscale records JSON contains the host
- verify DNS resolves from a tailnet client
- verify HTTPS reaches the service through the internal ingress path

For the cluster repo, use its existing Kustodian validation commands after adding
the template:

```bash
bunx kustodian validate --cluster <cluster-a>
bunx kustodian validate --cluster <cluster-b>
```

## Open Decisions

- Whether MVP should generate standard Kubernetes Ingress resources only, or also
  generate Traefik `IngressRoute` resources immediately for the current cluster.
- Whether the first release should include `TailnetGateway`, or keep it behind a
  follow-up issue after `IngressStatus` mode works.
- Whether static records should be configured in one cluster-scoped config
  object or as individual `HeadscaleDNSRecord` resources.
- Whether the source Ingress should require TLS hosts to exactly match rule
  hosts, or whether the operator should copy rule hosts into generated TLS when
  a default wildcard secret is configured.

## References

- [Headscale DNS extra records](https://headscale.net/stable/ref/dns/)
- [Headscale API](https://headscale.net/stable/ref/api/)
- [Kubernetes Ingress](https://kubernetes.io/docs/concepts/services-networking/ingress/)
- [Kubernetes finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)
- [ExternalDNS](https://kubernetes-sigs.github.io/external-dns/latest/)
- [Tailscale Kubernetes Operator cluster ingress](https://tailscale.com/docs/features/kubernetes-operator/how-to/cluster-ingress)
