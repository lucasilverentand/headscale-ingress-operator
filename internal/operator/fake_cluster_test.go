package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestReconcileThroughFakeKubernetesAPIServer(t *testing.T) {
	cluster := newFakeCluster(t)
	defer cluster.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{
		Host: cluster.URL,
		ContentConfig: rest.ContentConfig{
			ContentType:        "application/json",
			AcceptContentTypes: "application/json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

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

	summary, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceIngresses != 1 || summary.Records != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	configMap := cluster.configMaps["headscale/headscale-extra-records"]
	if configMap == nil {
		t.Fatal("expected records ConfigMap to be created through fake API server")
	}

	var records []DNSRecord
	if err := json.Unmarshal([]byte(configMap.Data["extra-records.json"]), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "whoami.cluster.example" {
		t.Fatalf("unexpected records: %#v", records)
	}
}

type fakeCluster struct {
	*httptest.Server
	ingresses  map[string]*networkingv1.Ingress
	configMaps map[string]*corev1.ConfigMap
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()

	cluster := &fakeCluster{
		ingresses: map[string]*networkingv1.Ingress{
			"apps/whoami": sourceIngress("apps", "whoami", "whoami.cluster.example", "headscale"),
		},
		configMaps: map[string]*corev1.ConfigMap{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/apis/networking.k8s.io/v1/ingresses", cluster.handleIngressList)
	mux.HandleFunc("/apis/networking.k8s.io/v1/namespaces/apps/ingresses", cluster.handleImplementationIngressCreate)
	mux.HandleFunc("/apis/networking.k8s.io/v1/namespaces/apps/ingresses/whoami-headscale", cluster.handleImplementationIngress)
	mux.HandleFunc("/apis/networking.k8s.io/v1/namespaces/apps/ingresses/whoami", cluster.handleSourceIngress)
	mux.HandleFunc("/apis/networking.k8s.io/v1/namespaces/apps/ingresses/whoami/status", cluster.handleSourceIngressStatus)
	mux.HandleFunc("/api/v1/namespaces/headscale/configmaps/headscale-extra-records", cluster.handleConfigMap)
	mux.HandleFunc("/api/v1/namespaces/headscale/configmaps", cluster.handleConfigMapCreate)
	cluster.Server = httptest.NewServer(mux)
	return cluster
}

func (cluster *fakeCluster) handleIngressList(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}

	list := networkingv1.IngressList{}
	for _, ingress := range cluster.ingresses {
		list.Items = append(list.Items, *ingress.DeepCopy())
	}
	writeJSON(writer, &list)
}

func (cluster *fakeCluster) handleSourceIngress(writer http.ResponseWriter, request *http.Request) {
	ingress := cluster.ingresses["apps/whoami"]
	if ingress == nil {
		http.NotFound(writer, request)
		return
	}

	switch request.Method {
	case http.MethodGet:
		writeJSON(writer, ingress)
	case http.MethodPut:
		var updated networkingv1.Ingress
		readJSON(request, &updated)
		cluster.ingresses["apps/whoami"] = &updated
		writeJSON(writer, &updated)
	default:
		methodNotAllowed(writer)
	}
}

func (cluster *fakeCluster) handleSourceIngressStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		methodNotAllowed(writer)
		return
	}

	var updated networkingv1.Ingress
	readJSON(request, &updated)
	existing := cluster.ingresses["apps/whoami"].DeepCopy()
	existing.Status = updated.Status
	cluster.ingresses["apps/whoami"] = existing
	writeJSON(writer, existing)
}

func (cluster *fakeCluster) handleImplementationIngress(writer http.ResponseWriter, request *http.Request) {
	key := "apps/whoami-headscale"

	switch request.Method {
	case http.MethodGet:
		ingress := cluster.ingresses[key]
		if ingress == nil {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, ingress)
	case http.MethodPut:
		var updated networkingv1.Ingress
		readJSON(request, &updated)
		cluster.ingresses[key] = &updated
		writeJSON(writer, &updated)
	default:
		methodNotAllowed(writer)
	}
}

func (cluster *fakeCluster) handleImplementationIngressCreate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}

	var created networkingv1.Ingress
	readJSON(request, &created)
	cluster.ingresses["apps/"+created.Name] = &created
	writeJSON(writer, &created)
}

func (cluster *fakeCluster) handleConfigMap(writer http.ResponseWriter, request *http.Request) {
	key := "headscale/headscale-extra-records"

	switch request.Method {
	case http.MethodGet:
		configMap := cluster.configMaps[key]
		if configMap == nil {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, configMap)
	case http.MethodPut:
		var updated corev1.ConfigMap
		readJSON(request, &updated)
		cluster.configMaps[key] = &updated
		writeJSON(writer, &updated)
	default:
		methodNotAllowed(writer)
	}
}

func (cluster *fakeCluster) handleConfigMapCreate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}

	var created corev1.ConfigMap
	readJSON(request, &created)
	cluster.configMaps["headscale/"+created.Name] = &created
	writeJSON(writer, &created)
}

func writeJSON(writer http.ResponseWriter, object runtime.Object) {
	writer.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(writer).Encode(object)
}

func readJSON(request *http.Request, object any) {
	defer request.Body.Close()
	if err := json.NewDecoder(request.Body).Decode(object); err != nil {
		panic(err)
	}
}

func methodNotAllowed(writer http.ResponseWriter) {
	writer.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = writer.Write([]byte(`{"kind":"Status","status":"Failure","reason":"MethodNotAllowed"}`))
}
