package operator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcileCreatesImplementationIngressAndHeadscaleRecords(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale"),
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			StaticRecords:        []DNSRecord{{Name: "admin.cluster.example", Type: "A", Value: "100.64.0.20"}},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceIngresses != 1 || summary.ImplementationIngresses != 1 || summary.Records != 2 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	implementation, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami-headscale", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := *implementation.Spec.IngressClassName; got != "traefik-internal" {
		t.Fatalf("implementation class = %q", got)
	}
	if implementation.Annotations[externalDNSExclude] != "true" {
		t.Fatalf("generated ingress should exclude external-dns: %#v", implementation.Annotations)
	}

	updatedSource, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedSource.Annotations[statusAnnotation] != statusReady {
		t.Fatalf("source status annotation = %q", updatedSource.Annotations[statusAnnotation])
	}
	if got := updatedSource.Status.LoadBalancer.Ingress[0].IP; got != "100.64.0.10" {
		t.Fatalf("source status target = %q", got)
	}

	configMap, err := client.CoreV1().ConfigMaps("headscale").Get(ctx, "headscale-extra-records", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var records []DNSRecord
	if err := json.Unmarshal([]byte(configMap.Data["extra-records.json"]), &records); err != nil {
		t.Fatal(err)
	}

	want := []DNSRecord{
		{Name: "admin.cluster.example", Type: "A", Value: "100.64.0.20"},
		{Name: "whoami.cluster.example", Type: "A", Value: "100.64.0.10"},
	}
	if !recordsEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
}

func TestReconcileRejectsHostsOutsideAllowedZones(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "bad", "bad.other.example", "headscale")
	source.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "100.64.0.99"}}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		source,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 1 {
		t.Fatalf("expected one skipped ingress, got %+v", summary)
	}

	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad-headscale", metav1.GetOptions{}); err == nil {
		t.Fatal("unexpected implementation ingress for rejected source")
	}

	configMap, err := client.CoreV1().ConfigMaps("headscale").Get(ctx, "headscale-extra-records", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if configMap.Data["extra-records.json"] != "[]\n" {
		t.Fatalf("records = %q", configMap.Data["extra-records.json"])
	}

	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.LoadBalancer.Ingress) != 0 {
		t.Fatalf("expected rejected source status to be cleared, got %#v", updated.Status.LoadBalancer.Ingress)
	}
}

func TestReconcileRejectsInvalidDNSHosts(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		sourceIngress("apps", "bad", "bad_name.cluster.example", "headscale"),
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 1 || summary.ImplementationIngresses != 0 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestReconcileDeletesStaleImplementationIngress(t *testing.T) {
	ctx := context.Background()
	stale := implementationIngressFor(*sourceIngress("apps", "old", "old.cluster.example", "headscale"), Config{}.withDefaults())
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		&stale,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.DeletedImplementations != 1 {
		t.Fatalf("deleted implementations = %d", summary.DeletedImplementations)
	}

	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "old-headscale", metav1.GetOptions{}); err == nil {
		t.Fatal("stale implementation ingress still exists")
	}
}

func TestReconcileDoesNotDeleteIngressWithOnlyManagedByLabel(t *testing.T) {
	ctx := context.Background()
	unrelated := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated",
			Namespace: "apps",
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
	}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		unrelated,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.DeletedImplementations != 0 {
		t.Fatalf("deleted implementations = %d", summary.DeletedImplementations)
	}
	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileDoesNotDeleteManagedLabelsWithoutOwnerReference(t *testing.T) {
	ctx := context.Background()
	spoofed := implementationIngressFor(*sourceIngress("apps", "old", "old.cluster.example", "headscale"), Config{}.withDefaults())
	spoofed.OwnerReferences = nil
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		&spoofed,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.DeletedImplementations != 0 {
		t.Fatalf("deleted implementations = %d", summary.DeletedImplementations)
	}
	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "old-headscale", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileRejectsDuplicateHosts(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		sourceIngress("apps", "app-a", "shared.cluster.example", "headscale"),
		sourceIngress("apps", "app-b", "shared.cluster.example", "headscale"),
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 2 || summary.ImplementationIngresses != 0 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	for _, name := range []string{"app-a", "app-b"} {
		updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Annotations[statusAnnotation] != statusRejected {
			t.Fatalf("%s status = %q", name, updated.Annotations[statusAnnotation])
		}
		if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, name+"-headscale", metav1.GetOptions{}); err == nil {
			t.Fatalf("unexpected implementation ingress for %s", name)
		}
	}
}

func TestReconcileRejectsExistingImplementationItDoesNotOwn(t *testing.T) {
	ctx := context.Background()
	conflicting := sourceIngress("apps", "whoami-headscale", "other.cluster.example", "traefik-internal")
	conflicting.Labels = map[string]string{"app.kubernetes.io/name": "someone-else"}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale"),
		conflicting,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 1 || summary.ImplementationIngresses != 0 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("source status = %q", updated.Annotations[statusAnnotation])
	}

	existing, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami-headscale", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if existing.Labels["app.kubernetes.io/name"] != "someone-else" {
		t.Fatalf("conflicting ingress was overwritten: %#v", existing.Labels)
	}
}

func TestReconcileAdoptsImplementationAfterSourceRecreate(t *testing.T) {
	ctx := context.Background()
	oldSource := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	oldSource.UID = types.UID("old-source-uid")
	existing := implementationIngressFor(*oldSource, Config{}.withDefaults())
	newSource := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	newSource.UID = types.UID("new-source-uid")

	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		newSource,
		&existing,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ImplementationIngresses != 1 || summary.Records != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami-headscale", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.OwnerReferences[0].UID; got != newSource.UID {
		t.Fatalf("owner UID = %q, want %q", got, newSource.UID)
	}
}

func TestReconcileRefusesUnmanagedRecordsConfigMap(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		namespace("headscale"),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "headscale-extra-records",
				Namespace: "headscale",
			},
			Data: map[string]string{"extra-records.json": "[]\n"},
		},
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	if _, err := reconciler.Reconcile(ctx); err == nil {
		t.Fatal("expected unmanaged records ConfigMap to be rejected")
	}
}

func TestReconcileRejectsInvalidConfig(t *testing.T) {
	tests := map[string]Config{
		"invalid default target": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			DefaultTargetIPs:               []string{"not-an-ip"},
		},
		"invalid static record": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			StaticRecords:                  []DNSRecord{{Name: "bad.cluster.example", Type: "CNAME", Value: "target.cluster.example"}},
		},
		"static record outside allowed zone": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			AllowedZones:                   []string{"cluster.example"},
			StaticRecords:                  []DNSRecord{{Name: "bad.other.example", Type: "A", Value: "100.64.0.10"}},
		},
		"invalid allowed zone": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			AllowedZones:                   []string{"bad_zone.example"},
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			reconciler := Reconciler{
				Client: fake.NewSimpleClientset(),
				Config: config,
			}
			if _, err := reconciler.Reconcile(context.Background()); err == nil {
				t.Fatal("expected config validation error")
			}
		})
	}
}

func TestGeneratedIngressNameIsBounded(t *testing.T) {
	sourceName := strings.Repeat("a", 63)
	name := generatedIngressName(sourceName, "-headscale")
	if len(name) > 63 {
		t.Fatalf("generated name length = %d", len(name))
	}
	if name != generatedIngressName(sourceName, "-headscale") {
		t.Fatal("generated name should be deterministic")
	}
	if name == sourceName+"-headscale" {
		t.Fatal("generated name should be shortened")
	}
}

func TestReconcileRejectsSelfReferentialIngressClassConfig(t *testing.T) {
	reconciler := Reconciler{
		Client: fake.NewSimpleClientset(),
		Config: Config{
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "headscale",
		},
	}

	if _, err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("expected an error when source and implementation classes match")
	}
}

func sourceIngress(namespace, name, host, class string) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			UID:         types.UID(namespace + "-" + name),
			Annotations: map[string]string{},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{host},
				SecretName: "wildcard-tls",
			}},
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "whoami",
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func recordsEqual(left, right []DNSRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
