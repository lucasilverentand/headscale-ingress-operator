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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcileCreatesImplementationIngressAndHeadscaleRecords(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	source.Annotations[targetIPAnnotation] = "100.64.0.10"
	source.Annotations[publishAnnotation] = "true"
	source.Annotations[statusAnnotation] = "Old"
	source.Annotations[sourceNamespaceAnnotation] = "wrong"
	source.Annotations[sourceNameAnnotation] = "wrong"
	source.Annotations[sourceUIDAnnotation] = "spoofed"
	source.Annotations["example.com/backend-protocol"] = "HTTPS"
	source.Annotations["external-dns.alpha.kubernetes.io/hostname"] = "public.example"
	source.Annotations["kubectl.kubernetes.io/last-applied-configuration"] = "{}"
	source.Annotations["meta.helm.sh/release-name"] = "app"
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
	if _, ok := implementation.Annotations[targetIPAnnotation]; ok {
		t.Fatalf("generated ingress should not copy operator target annotations: %#v", implementation.Annotations)
	}
	if _, ok := implementation.Annotations[publishAnnotation]; ok {
		t.Fatalf("generated ingress should not copy operator publish annotations: %#v", implementation.Annotations)
	}
	if _, ok := implementation.Annotations[statusAnnotation]; ok {
		t.Fatalf("generated ingress should not copy operator status annotations: %#v", implementation.Annotations)
	}
	if implementation.Annotations[sourceNamespaceAnnotation] != source.Namespace {
		t.Fatalf("generated ingress source namespace annotation = %q, want %q", implementation.Annotations[sourceNamespaceAnnotation], source.Namespace)
	}
	if implementation.Annotations[sourceNameAnnotation] != source.Name {
		t.Fatalf("generated ingress source name annotation = %q, want %q", implementation.Annotations[sourceNameAnnotation], source.Name)
	}
	if implementation.Annotations[sourceUIDAnnotation] != string(source.UID) {
		t.Fatalf("generated ingress source UID annotation = %q, want %q", implementation.Annotations[sourceUIDAnnotation], source.UID)
	}
	if implementation.Annotations["example.com/backend-protocol"] != "HTTPS" {
		t.Fatalf("generated ingress should keep implementation annotations: %#v", implementation.Annotations)
	}
	if _, ok := implementation.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Fatalf("generated ingress should not copy kubectl management annotations: %#v", implementation.Annotations)
	}
	if _, ok := implementation.Annotations["meta.helm.sh/release-name"]; ok {
		t.Fatalf("generated ingress should not copy Helm management annotations: %#v", implementation.Annotations)
	}
	if _, ok := implementation.Annotations["external-dns.alpha.kubernetes.io/hostname"]; ok {
		t.Fatalf("generated ingress should not copy ExternalDNS annotations: %#v", implementation.Annotations)
	}
	if implementation.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Fatal("generated ingress owner reference should not set blockOwnerDeletion")
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

func TestReconcileRejectsInvalidTargetAnnotation(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "bad-target", "bad-target.cluster.example", "headscale")
	source.Annotations[targetIPAnnotation] = "not-an-ip"
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
	if len(summary.Skipped) != 1 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad-target", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("status = %q", updated.Annotations[statusAnnotation])
	}
	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad-target-headscale", metav1.GetOptions{}); err == nil {
		t.Fatal("unexpected implementation ingress for invalid target annotation")
	}
}

func TestReconcileRejectsEmptyTargetAnnotation(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "empty-target", "empty-target.cluster.example", "headscale")
	source.Annotations[targetIPAnnotation] = ","
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
	if len(summary.Skipped) != 1 || summary.ImplementationIngresses != 0 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "empty-target-headscale", metav1.GetOptions{}); err == nil {
		t.Fatal("unexpected implementation ingress for empty target annotation")
	}
}

func TestReconcileIgnoresInvalidImplementationStatusTargets(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	implementation := implementationIngressFor(*source, Config{}.withDefaults())
	implementation.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{
		{IP: "0.0.0.0"},
		{IP: "not-an-ip"},
	}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		source,
		&implementation,
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
	if summary.Records != 1 || len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	updatedSource, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedSource.Status.LoadBalancer.Ingress[0].IP; got != "100.64.0.10" {
		t.Fatalf("source status target = %q", got)
	}
}

func TestReconcileUsesImplementationStatusBeforeDefaults(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	implementation := implementationIngressFor(*source, Config{}.withDefaults())
	implementation.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "100.64.0.11"}}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		source,
		&implementation,
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
	if summary.Records != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	configMap, err := client.CoreV1().ConfigMaps("headscale").Get(ctx, "headscale-extra-records", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var records []DNSRecord
	if err := json.Unmarshal([]byte(configMap.Data["extra-records.json"]), &records); err != nil {
		t.Fatal(err)
	}
	want := []DNSRecord{{Name: "whoami.cluster.example", Type: "A", Value: "100.64.0.11"}}
	if !recordsEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
}

func TestReconcileSkipsDNSWhenPublishFalse(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "private", "private.cluster.example", "headscale")
	source.Annotations[publishAnnotation] = " FALSE "
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
	if summary.ImplementationIngresses != 1 || summary.Records != 0 || len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "private-headscale", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "private", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusImplemented {
		t.Fatalf("source status = %q", updated.Annotations[statusAnnotation])
	}
	if len(updated.Status.LoadBalancer.Ingress) != 0 {
		t.Fatalf("expected source status targets to be cleared, got %#v", updated.Status.LoadBalancer.Ingress)
	}
}

func TestReconcileRejectsInvalidPublishAnnotation(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "bad-publish", "bad-publish.cluster.example", "headscale")
	source.Annotations[publishAnnotation] = "no"
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
	if len(summary.Skipped) != 1 || summary.ImplementationIngresses != 0 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad-publish", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("source status = %q", updated.Annotations[statusAnnotation])
	}
	if _, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad-publish-headscale", metav1.GetOptions{}); err == nil {
		t.Fatal("unexpected implementation ingress for invalid publish annotation")
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

func TestReconcileDoesNotDeleteManagedLabelsWithoutSourceUIDAnnotation(t *testing.T) {
	ctx := context.Background()
	spoofed := implementationIngressFor(*sourceIngress("apps", "old", "old.cluster.example", "headscale"), Config{}.withDefaults())
	delete(spoofed.Annotations, sourceUIDAnnotation)
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

func TestReconcileRefusesManagedImplementationWithoutSourceUIDProof(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	existing := implementationIngressFor(*source, Config{}.withDefaults())
	delete(existing.Annotations, sourceUIDAnnotation)
	existing.Annotations["example.com/sticky"] = "keep"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		source,
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
	if len(summary.Skipped) != 1 || summary.ImplementationIngresses != 0 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	updatedSource, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedSource.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("source status = %q", updatedSource.Annotations[statusAnnotation])
	}

	updatedImplementation, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami-headscale", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updatedImplementation.Annotations[sourceUIDAnnotation]; ok {
		t.Fatalf("ambiguous implementation was overwritten: %#v", updatedImplementation.Annotations)
	}
	if updatedImplementation.Annotations["example.com/sticky"] != "keep" {
		t.Fatalf("ambiguous implementation annotations changed: %#v", updatedImplementation.Annotations)
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
	if got := updated.Annotations[sourceUIDAnnotation]; got != string(newSource.UID) {
		t.Fatalf("source UID annotation = %q, want %q", got, newSource.UID)
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

func TestReconcileDoesNotMarkReadyBeforeRecordsPublish(t *testing.T) {
	ctx := context.Background()
	source := sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale")
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		source,
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
			AllowedZones:         []string{"cluster.example"},
			DefaultTargetIPs:     []string{"100.64.0.10"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	if _, err := reconciler.Reconcile(ctx); err == nil {
		t.Fatal("expected unmanaged records ConfigMap to be rejected")
	}

	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[statusAnnotation]; got == statusReady {
		t.Fatalf("source was marked ready before records were published: %q", got)
	}
}

func TestReconcileRejectsInvalidConfig(t *testing.T) {
	tests := map[string]Config{
		"invalid default target": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			DefaultTargetIPs:               []string{"not-an-ip"},
		},
		"invalid source class": {
			SourceClassName:                "Bad_Class",
			ImplementationIngressClassName: "traefik-internal",
		},
		"invalid implementation suffix": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			ImplementationNameSuffix:       "-",
		},
		"invalid headscale namespace": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			HeadscaleNamespace:             "bad.namespace",
		},
		"invalid records key": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			RecordsConfigMapKey:            "../extra-records.json",
		},
		"invalid static record": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			StaticRecords:                  []DNSRecord{{Name: "bad.cluster.example", Type: "CNAME", Value: "target.cluster.example"}},
		},
		"unusable static record target": {
			SourceClassName:                "headscale",
			ImplementationIngressClassName: "traefik-internal",
			StaticRecords:                  []DNSRecord{{Name: "bad.cluster.example", Type: "A", Value: "0.0.0.0"}},
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
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Fatalf("generated name %q is invalid: %v", name, errs)
	}
	if name != generatedIngressName(sourceName, "-headscale") {
		t.Fatal("generated name should be deterministic")
	}
	if name == sourceName+"-headscale" {
		t.Fatal("generated name should be shortened")
	}

	dottedSourceName := strings.Repeat("a", 49) + "." + strings.Repeat("b", 63)
	name = generatedIngressName(dottedSourceName, "-headscale")
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Fatalf("generated dotted name %q is invalid: %v", name, errs)
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
