package operator

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcileCreatesManagedProxyResourcesForIngress(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	ingress.UID = "ingress-uid"
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
		ingress,
	)
	var authKeyTags []string

	reconciler := Reconciler{
		Client: client,
		Headscale: fakeHeadscale{
			authKey:         "tskey-auth",
			nodes:           map[string][]string{"radarr": {"100.64.0.7"}},
			lastAuthKeyTags: &authKeyTags,
		},
		Config: testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceIngresses != 1 || summary.Records != 1 || len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	secret, err := client.CoreV1().Secrets("apps").Get(ctx, "radarr-tailnet-authkey", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["TS_AUTHKEY"]) != "tskey-auth" {
		t.Fatalf("auth secret key = %q", secret.Data["TS_AUTHKEY"])
	}
	if !slices.Equal(authKeyTags, []string{"tag:cluster"}) {
		t.Fatalf("auth key tags = %#v", authKeyTags)
	}

	deployment, err := client.AppsV1().Deployments("apps").Get(ctx, "radarr-tailnet-sidecar", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Labels[managedByLabel] != managedByValue {
		t.Fatalf("deployment managed-by label = %q", deployment.Labels[managedByLabel])
	}
	if deployment.Labels[proxySourceKindLabel] != "Ingress" {
		t.Fatalf("deployment source kind label = %q", deployment.Labels[proxySourceKindLabel])
	}
	if len(deployment.OwnerReferences) != 1 || deployment.OwnerReferences[0].Kind != "Ingress" || deployment.OwnerReferences[0].Name != "radarr" {
		t.Fatalf("deployment owner references = %#v", deployment.OwnerReferences)
	}
	tailscale := deployment.Spec.Template.Spec.Containers[0]
	if tailscale.Image != "tailscale/tailscale:v1.98.3" {
		t.Fatalf("tailscale image = %q", tailscale.Image)
	}
	if !envContains(tailscale.Env, "TS_HOSTNAME", "radarr") {
		t.Fatalf("tailscale env does not contain TS_HOSTNAME=radarr: %#v", tailscale.Env)
	}
	if !envContains(tailscale.Env, "TS_KUBE_SECRET", "tailscale-radarr") {
		t.Fatalf("tailscale env does not contain TS_KUBE_SECRET=tailscale-radarr: %#v", tailscale.Env)
	}
	if !envContains(tailscale.Env, "TS_EXTRA_ARGS", "--login-server=https://headscale.example") {
		t.Fatalf("tailscale env does not contain Headscale server URL: %#v", tailscale.Env)
	}

	configMap, err := client.CoreV1().ConfigMaps("apps").Get(ctx, "radarr-tailnet-sidecar-nginx", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nginxConfig := configMap.Data["nginx.conf"]
	if !strings.Contains(nginxConfig, "server_name radarr.cluster.example;") {
		t.Fatalf("nginx config missing host: %s", nginxConfig)
	}
	if !strings.Contains(nginxConfig, "proxy_pass http://radarr.apps.svc.cluster.local:7878;") {
		t.Fatalf("nginx config missing upstream: %s", nginxConfig)
	}

	role, err := client.RbacV1().Roles("apps").Get(ctx, "radarr-tailnet-sidecar", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(role.Rules[1].ResourceNames, "tailscale-radarr") {
		t.Fatalf("role does not scope state secret access: %#v", role.Rules)
	}

	updatedIngress, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "radarr", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedIngress.Annotations[statusAnnotation] != statusReady {
		t.Fatalf("ingress status annotation = %q", updatedIngress.Annotations[statusAnnotation])
	}
	if len(updatedIngress.Status.LoadBalancer.Ingress) != 1 || updatedIngress.Status.LoadBalancer.Ingress[0].IP != "100.64.0.7" {
		t.Fatalf("ingress load balancer status = %#v", updatedIngress.Status.LoadBalancer.Ingress)
	}

	records := recordsFromConfigMap(t, client, ctx)
	want := []DNSRecord{{Name: "radarr.cluster.example", Type: "A", Value: "100.64.0.7"}}
	if !recordsEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
}

func TestReconcileUsesIngressACLTagsForManagedProxyAuthKey(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	ingress.Annotations[proxyACLTagsAnnotation] = "tag:app, tag:team, tag:app"
	client := fake.NewSimpleClientset(namespace("apps"), namespace("headscale"), service, ingress)
	var authKeyTags []string

	reconciler := Reconciler{
		Client: client,
		Headscale: fakeHeadscale{
			authKey:         "tskey-auth",
			nodes:           map[string][]string{"radarr": {"100.64.0.7"}},
			lastAuthKeyTags: &authKeyTags,
		},
		Config: testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if !slices.Equal(authKeyTags, []string{"tag:app", "tag:team"}) {
		t.Fatalf("auth key tags = %#v", authKeyTags)
	}
}

func TestReconcileRejectsInvalidIngressACLTags(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	ingress.Annotations[proxyACLTagsAnnotation] = "tag:app, Team"
	client := fake.NewSimpleClientset(namespace("apps"), namespace("headscale"), service, ingress)
	var authKeyTags []string

	reconciler := Reconciler{
		Client: client,
		Headscale: fakeHeadscale{
			authKey:         "tskey-auth",
			nodes:           map[string][]string{"radarr": {"100.64.0.7"}},
			lastAuthKeyTags: &authKeyTags,
		},
		Config: testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 0 || len(summary.Skipped) != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(authKeyTags) != 0 {
		t.Fatalf("auth key was minted with tags %#v", authKeyTags)
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "radarr", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("ingress status annotation = %q", updated.Annotations[statusAnnotation])
	}
	reason := updated.Annotations[statusReasonAnnotation]
	if !strings.Contains(reason, proxyACLTagsAnnotation) || !strings.Contains(reason, "must start with tag:") {
		t.Fatalf("ingress status reason = %q", reason)
	}
	if _, err := client.CoreV1().Secrets("apps").Get(ctx, "radarr-tailnet-authkey", metav1.GetOptions{}); err == nil {
		t.Fatal("expected auth secret not to be created")
	}
}

func TestReconcileRepairsStaleManagedRecordsConfigMap(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
		ingress,
		managedRecordsConfigMap(t, []DNSRecord{{Name: "radarr.cluster.example", Type: "A", Value: "100.64.0.6"}}),
	)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{"radarr": {"100.64.0.7"}}},
		Config:    testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.RecordsChanged {
		t.Fatalf("expected stale records ConfigMap to be updated: %+v", summary)
	}

	records := recordsFromConfigMap(t, client, ctx)
	want := []DNSRecord{{Name: "radarr.cluster.example", Type: "A", Value: "100.64.0.7"}}
	if !recordsEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
}

func TestReconcileRemovesStaleRecordWhenProxyNodeIPIsMissing(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
		ingress,
		managedRecordsConfigMap(t, []DNSRecord{{Name: "radarr.cluster.example", Type: "A", Value: "100.64.0.6"}}),
	)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{}},
		Config:    testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 0 || len(summary.Skipped) != 1 || !summary.RecordsChanged {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	records := recordsFromConfigMap(t, client, ctx)
	if len(records) != 0 {
		t.Fatalf("records = %#v, want none", records)
	}

	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "radarr", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusPendingNode {
		t.Fatalf("ingress status annotation = %q", updated.Annotations[statusAnnotation])
	}
}

func TestReconcileResolvesNamedIngressBackendServicePort(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Name: "http"})
	client := fake.NewSimpleClientset(namespace("apps"), namespace("headscale"), service, ingress)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{"radarr": {"100.64.0.7"}}},
		Config:    testConfig(),
	}

	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	configMap, err := client.CoreV1().ConfigMaps("apps").Get(ctx, "radarr-tailnet-sidecar-nginx", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configMap.Data["nginx.conf"], "proxy_pass http://radarr.apps.svc.cluster.local:7878;") {
		t.Fatalf("nginx config missing named backend port: %s", configMap.Data["nginx.conf"])
	}
}

func TestReconcileRejectsHostsOutsideAllowedZones(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "bad", 80)
	ingress := sourceIngress("apps", "bad", "bad.other.example", "bad", networkingv1.ServiceBackendPort{Number: 80})
	client := fake.NewSimpleClientset(namespace("apps"), namespace("headscale"), service, ingress)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{"bad": {"100.64.0.10"}}},
		Config:    testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 1 || summary.SourceIngresses != 1 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "bad", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusRejected {
		t.Fatalf("ingress status annotation = %q", updated.Annotations[statusAnnotation])
	}
}

func TestReconcileRejectsDuplicateIngressHosts(t *testing.T) {
	ctx := context.Background()
	appA := sourceIngress("apps", "app-a", "shared.cluster.example", "app-a", networkingv1.ServiceBackendPort{Number: 80})
	appB := sourceIngress("apps", "app-b", "shared.cluster.example", "app-b", networkingv1.ServiceBackendPort{Number: 80})
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		plainService("apps", "app-a", 80),
		plainService("apps", "app-b", 80),
		appA,
		appB,
	)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{"app-a": {"100.64.0.10"}, "app-b": {"100.64.0.11"}}},
		Config:    testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 2 || summary.Records != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestReconcileIgnoresIngressForOtherClass(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	otherClass := "traefik"
	ingress.Spec.IngressClassName = &otherClass
	client := fake.NewSimpleClientset(namespace("apps"), namespace("headscale"), service, ingress)

	reconciler := Reconciler{
		Client: client,
		Config: Config{
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
			IngressClassName:     "headscale",
		},
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceIngresses != 0 || summary.Records != 0 || len(summary.Skipped) != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestReconcileMarksIngressPendingUntilNodeIPExists(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "radarr", 7878)
	ingress := sourceIngress("apps", "radarr", "radarr.cluster.example", "radarr", networkingv1.ServiceBackendPort{Number: 7878})
	client := fake.NewSimpleClientset(namespace("apps"), namespace("headscale"), service, ingress)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{}},
		Config:    testConfig(),
	}

	summary, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 0 || len(summary.Skipped) != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "radarr", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[statusAnnotation] != statusPendingNode {
		t.Fatalf("ingress status annotation = %q", updated.Annotations[statusAnnotation])
	}
	if _, err := client.AppsV1().Deployments("apps").Get(ctx, "radarr-tailnet-sidecar", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileCleansUpProxyWhenIngressIsRemoved(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		desiredProxyDeployment(Config{Proxy: ProxyConfig{TailscaleImage: "tailscale", NginxImage: "nginx"}}, testProxySpec("radarr"), metav1.OwnerReference{Kind: "Ingress", Name: "radarr"}),
		desiredProxyConfigMap(testProxySpec("radarr"), metav1.OwnerReference{Kind: "Ingress", Name: "radarr"}),
		desiredProxyServiceAccount(testProxySpec("radarr"), metav1.OwnerReference{Kind: "Ingress", Name: "radarr"}),
		desiredProxyRole(testProxySpec("radarr"), metav1.OwnerReference{Kind: "Ingress", Name: "radarr"}),
		desiredProxyRoleBinding(testProxySpec("radarr"), metav1.OwnerReference{Kind: "Ingress", Name: "radarr"}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "radarr-tailnet-authkey",
				Namespace: "apps",
				Labels:    proxyLabelsForSpec(testProxySpec("radarr")),
			},
			Data: map[string][]byte{"TS_AUTHKEY": []byte("old-key")},
		},
	)

	reconciler := Reconciler{
		Client: client,
		Config: testConfig(),
	}

	if _, err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments("apps").Get(ctx, "radarr-tailnet-sidecar", metav1.GetOptions{}); err == nil {
		t.Fatal("expected stale proxy deployment to be deleted")
	}
	if _, err := client.CoreV1().Secrets("apps").Get(ctx, "radarr-tailnet-authkey", metav1.GetOptions{}); err == nil {
		t.Fatal("expected stale proxy auth secret to be deleted")
	}
}

func TestReconcileRefusesUnmanagedRecordsConfigMap(t *testing.T) {
	ctx := context.Background()
	service := plainService("apps", "whoami", 80)
	ingress := sourceIngress("apps", "whoami", "whoami.cluster.example", "whoami", networkingv1.ServiceBackendPort{Number: 80})
	client := fake.NewSimpleClientset(
		namespace("apps"),
		namespace("headscale"),
		service,
		ingress,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "headscale-extra-records",
				Namespace: "headscale",
			},
			Data: map[string]string{"extra-records.json": "[]\n"},
		},
	)

	reconciler := Reconciler{
		Client:    client,
		Headscale: fakeHeadscale{authKey: "tskey-auth", nodes: map[string][]string{"whoami": {"100.64.0.10"}}},
		Config:    testConfig(),
	}

	if _, err := reconciler.Reconcile(ctx); err == nil {
		t.Fatal("expected unmanaged records ConfigMap to be rejected")
	}
	updated, err := client.NetworkingV1().Ingresses("apps").Get(ctx, "whoami", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[statusAnnotation]; got == statusReady {
		t.Fatalf("ingress was marked ready before records were published: %q", got)
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
		"proxy enabled without headscale server URL": {
			Proxy: ProxyConfig{Enabled: true},
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

func recordsFromConfigMap(t *testing.T, client *fake.Clientset, ctx context.Context) []DNSRecord {
	t.Helper()
	recordsConfigMap, err := client.CoreV1().ConfigMaps("headscale").Get(ctx, "headscale-extra-records", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var records []DNSRecord
	if err := json.Unmarshal([]byte(recordsConfigMap.Data["extra-records.json"]), &records); err != nil {
		t.Fatal(err)
	}
	return records
}

func managedRecordsConfigMap(t *testing.T, records []DNSRecord) *corev1.ConfigMap {
	t.Helper()
	payload, err := stableRecordsJSON(records)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "headscale-extra-records",
			Namespace: "headscale",
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
		Data: map[string]string{"extra-records.json": payload},
	}
}

func envContains(values []corev1.EnvVar, name string, value string) bool {
	for _, item := range values {
		if item.Name == name && item.Value == value {
			return true
		}
	}
	return false
}

func plainService(namespace, name string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       port,
				TargetPort: intstr.FromInt(8080),
			}},
		},
	}
}

func sourceIngress(namespace, name, host, serviceName string, servicePort networkingv1.ServiceBackendPort) *networkingv1.Ingress {
	ingressClass := "headscale"
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: serviceName,
									Port: servicePort,
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func testProxySpec(name string) proxySpec {
	return proxySpec{
		name:            name + "-tailnet-sidecar",
		authSecretName:  name + "-tailnet-authkey",
		stateSecret:     "tailscale-" + name,
		tailnetName:     name,
		tlsSecretName:   "cluster-tls",
		servicePort:     80,
		hosts:           []string{name + ".cluster.example"},
		sourceName:      name,
		sourceNamespace: "apps",
		routes: []proxyRoute{{
			Path:             "/",
			PathType:         networkingv1.PathTypePrefix,
			ServiceName:      name,
			ServiceNamespace: "apps",
			ServicePort:      80,
		}},
	}
}

func testConfig() Config {
	return Config{
		AllowedZones:         []string{"cluster.example"},
		HeadscaleNamespace:   "headscale",
		RecordsConfigMapName: "headscale-extra-records",
		RecordsConfigMapKey:  "extra-records.json",
		IngressClassName:     "headscale",
		Proxy: ProxyConfig{
			Enabled:              true,
			HeadscaleServerURL:   "https://headscale.example",
			TailscaleImage:       "tailscale/tailscale:v1.98.3",
			NginxImage:           "nginx:1.27-alpine",
			DefaultTLSSecretName: "cluster-tls",
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

type fakeHeadscale struct {
	authKey         string
	nodes           map[string][]string
	lastAuthKeyTags *[]string
}

func (fake fakeHeadscale) MintReusableAuthKey(_ context.Context, tags []string) (string, error) {
	if fake.lastAuthKeyTags != nil {
		*fake.lastAuthKeyTags = append((*fake.lastAuthKeyTags)[:0], tags...)
	}
	return fake.authKey, nil
}

func (fake fakeHeadscale) NodeIPs(_ context.Context, nodeName string) ([]string, error) {
	return fake.nodes[nodeName], nil
}
