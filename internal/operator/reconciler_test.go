package operator

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcilePublishesServiceMagicDNSRecords(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "whoami", "whoami.cluster.example")
	service.Annotations[targetIPAnnotation] = "100.64.0.10"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceServices != 1 || summary.Records != 1 || len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	updatedService, err := client.CoreV1().Services("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedService.Annotations[statusAnnotation] != statusReady {
		t.Fatalf("service status annotation = %q", updatedService.Annotations[statusAnnotation])
	}

	configMap, err := client.CoreV1().ConfigMaps("headscale").Get(ctx, "headscale-extra-records", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var records []DNSRecord
	if err := json.Unmarshal([]byte(configMap.Data["extra-records.json"]), &records); err != nil {
		t.Fatal(err)
	}

	want := []DNSRecord{{Name: "whoami.cluster.example", Type: "A", Value: "100.64.0.10"}}
	if !recordsEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
}

func TestReconcileUsesServiceAddresses(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "api", "api.cluster.example, api-alt.cluster.example")
	service.Spec.ExternalIPs = []string{"100.64.0.10"}
	service.Spec.ClusterIPs = []string{"10.96.0.25"}
	service.Spec.ClusterIP = "10.96.0.25"
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "100.64.0.11"}}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceServices != 1 || summary.Records != 6 || len(summary.Skipped) != 0 {
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

	want := []DNSRecord{
		{Name: "api-alt.cluster.example", Type: "A", Value: "10.96.0.25"},
		{Name: "api-alt.cluster.example", Type: "A", Value: "100.64.0.10"},
		{Name: "api-alt.cluster.example", Type: "A", Value: "100.64.0.11"},
		{Name: "api.cluster.example", Type: "A", Value: "10.96.0.25"},
		{Name: "api.cluster.example", Type: "A", Value: "100.64.0.10"},
		{Name: "api.cluster.example", Type: "A", Value: "100.64.0.11"},
	}
	if !recordsEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
}

func TestReconcileRejectsHostsOutsideAllowedZones(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "bad", "bad.other.example")
	service.Annotations[targetIPAnnotation] = "100.64.0.10"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 1 || summary.SourceServices != 1 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	configMap, err := client.CoreV1().ConfigMaps("headscale").Get(ctx, "headscale-extra-records", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if configMap.Data["extra-records.json"] != "[]\n" {
		t.Fatalf("records = %q", configMap.Data["extra-records.json"])
	}

	updated, err := client.CoreV1().Services("apps").Get(ctx, "bad", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("service status annotation = %q", updated.Annotations[statusAnnotation])
	}
}

func TestReconcileRejectsInvalidDNSHosts(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "bad", "bad_name.cluster.example")
	service.Annotations[targetIPAnnotation] = "100.64.0.10"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
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
}

func TestReconcileRejectsInvalidTargetAnnotation(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "bad-target", "bad-target.cluster.example")
	service.Annotations[targetIPAnnotation] = "not-an-ip"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
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

	updated, err := client.CoreV1().Services("apps").Get(ctx, "bad-target", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("service status annotation = %q", updated.Annotations[statusAnnotation])
	}
}

func TestReconcileMarksServicePendingWhenNoAddressIsAvailable(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "headless", "headless.cluster.example")
	service.Spec.ClusterIP = corev1.ClusterIPNone
	service.Spec.ClusterIPs = []string{corev1.ClusterIPNone}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
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

	updated, err := client.CoreV1().Services("apps").Get(ctx, "headless", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusPending {
		t.Fatalf("service status annotation = %q", updated.Annotations[statusAnnotation])
	}
}

func TestReconcileSkipsServicesWithoutHostnames(t *testing.T) {
	ctx := context.Background()
	ignored := sourceService("apps", "ignored", "")
	ignored.Annotations = map[string]string{}
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		ignored,
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
	if summary.SourceServices != 0 || summary.Records != 0 || len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestReconcileRejectsDuplicateHosts(t *testing.T) {
	ctx := context.Background()
	appA := sourceService("apps", "app-a", "shared.cluster.example")
	appA.Annotations[targetIPAnnotation] = "100.64.0.10"
	appB := sourceService("apps", "app-b", "shared.cluster.example")
	appB.Annotations[targetIPAnnotation] = "100.64.0.11"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		appA,
		appB,
	)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			AllowedZones:         []string{"cluster.example"},
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 2 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	for _, name := range []string{"app-a", "app-b"} {
		updated, err := client.CoreV1().Services("apps").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Annotations[statusAnnotation] != statusRejected {
			t.Fatalf("%s status annotation = %q", name, updated.Annotations[statusAnnotation])
		}
	}
}

func TestReconcileRefusesUnmanagedRecordsConfigMap(t *testing.T) {
	ctx := context.Background()
	service := sourceService("apps", "whoami", "whoami.cluster.example")
	service.Annotations[targetIPAnnotation] = "100.64.0.10"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
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

	updated, err := client.CoreV1().Services("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[statusAnnotation]; got == statusReady {
		t.Fatalf("service was marked ready before records were published: %q", got)
	}
}

func TestReconcileRejectsInvalidConfig(t *testing.T) {
	tests := map[string]Config{
		"invalid headscale namespace": {
			HeadscaleNamespace: "bad.namespace",
		},
		"invalid records key": {
			RecordsConfigMapKey: "../extra-records.json",
		},
		"invalid allowed zone": {
			AllowedZones: []string{"bad_zone.example"},
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

func sourceService(namespace, name, hosts string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				hostnameAnnotation: hosts,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt(8080),
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
