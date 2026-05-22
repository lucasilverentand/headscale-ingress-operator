package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
			HeadscaleNamespace:   "headscale",
			RecordsConfigMapName: "headscale-extra-records",
			RecordsConfigMapKey:  "extra-records.json",
		},
	}

	summary, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.SourceServices != 1 || summary.Records != 1 {
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
	if len(records) != 1 || records[0].Name != "whoami.cluster.example" || records[0].Value != "100.64.0.10" {
		t.Fatalf("unexpected records: %#v", records)
	}
}

type fakeCluster struct {
	*httptest.Server
	services   map[string]*corev1.Service
	configMaps map[string]*corev1.ConfigMap
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()

	service := sourceService("apps", "whoami", "whoami.cluster.example")
	service.Annotations[targetIPAnnotation] = "100.64.0.10"
	cluster := &fakeCluster{
		services: map[string]*corev1.Service{
			"apps/whoami": service,
		},
		configMaps: map[string]*corev1.ConfigMap{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/services", cluster.handleServiceList)
	mux.HandleFunc("/api/v1/namespaces/apps/services/whoami", cluster.handleService)
	mux.HandleFunc("/api/v1/namespaces/headscale/configmaps/headscale-extra-records", cluster.handleConfigMap)
	mux.HandleFunc("/api/v1/namespaces/headscale/configmaps", cluster.handleConfigMapCreate)
	cluster.Server = httptest.NewServer(mux)
	return cluster
}

func (cluster *fakeCluster) handleServiceList(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}

	list := corev1.ServiceList{}
	for _, service := range cluster.services {
		list.Items = append(list.Items, *service.DeepCopy())
	}
	writeJSON(writer, &list)
}

func (cluster *fakeCluster) handleService(writer http.ResponseWriter, request *http.Request) {
	service := cluster.services["apps/whoami"]
	if service == nil {
		http.NotFound(writer, request)
		return
	}

	switch request.Method {
	case http.MethodGet:
		writeJSON(writer, service)
	case http.MethodPatch:
		var patch struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		readJSON(request, &patch)
		updated := service.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		for key, value := range patch.Metadata.Annotations {
			updated.Annotations[key] = value
		}
		cluster.services["apps/whoami"] = updated
		writeJSON(writer, updated)
	default:
		methodNotAllowed(writer)
	}
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
