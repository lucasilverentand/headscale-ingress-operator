package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeHeadscaleAPI struct {
	mu        sync.Mutex
	key       string
	users     map[string]string
	requests  []map[string]any
	nodesJSON string
}

func (api *fakeHeadscaleAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+api.key {
		http.Error(w, `{"code":16,"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		name := r.URL.Query().Get("name")
		users := []map[string]string{}
		if id, ok := api.users[name]; ok {
			users = append(users, map[string]string{"id": id, "name": name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": users})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/user":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := "7"
		api.users[body["name"]] = id
		_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"id": id, "name": body["name"]}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/preauthkey":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		api.requests = append(api.requests, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"preAuthKey": map[string]string{"key": "hskey-auth-minted"}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/node":
		_, _ = w.Write([]byte(api.nodesJSON))
	default:
		http.NotFound(w, r)
	}
}

func apiKeySecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "headscale-api-key", Namespace: "headscale"},
		Data:       map[string][]byte{"HEADSCALE_API_KEY": []byte(value)},
	}
}

func newTestAPIClient(t *testing.T, server *httptest.Server, secret *corev1.Secret) *APIHeadscaleClient {
	t.Helper()
	client, err := NewAPIHeadscaleClient(fake.NewSimpleClientset(secret), Config{
		HeadscaleAPI: HeadscaleAPIConfig{URL: server.URL + "/"},
		Proxy:        ProxyConfig{HeadscaleUser: "k8s-apps", AuthKeyExpiration: "90d"},
	})
	if err != nil {
		t.Fatalf("NewAPIHeadscaleClient: %v", err)
	}
	return client
}

func TestAPIHeadscaleClientMintsTaggedKeyAndCreatesUser(t *testing.T) {
	api := &fakeHeadscaleAPI{key: "hskey-api-good", users: map[string]string{}}
	server := httptest.NewServer(api)
	defer server.Close()
	client := newTestAPIClient(t, server, apiKeySecret("hskey-api-good\n"))

	key, err := client.MintReusableAuthKey(context.Background(), []string{"tag:cluster", "tag:web"})
	if err != nil {
		t.Fatalf("MintReusableAuthKey: %v", err)
	}
	if key != "hskey-auth-minted" {
		t.Fatalf("key = %q, want hskey-auth-minted", key)
	}
	if api.users["k8s-apps"] != "7" {
		t.Fatalf("user k8s-apps was not created: %v", api.users)
	}
	if len(api.requests) != 1 {
		t.Fatalf("got %d preauthkey requests, want 1", len(api.requests))
	}
	request := api.requests[0]
	if request["user"] != "7" || request["reusable"] != true {
		t.Fatalf("unexpected request %v", request)
	}
	tags, _ := request["aclTags"].([]any)
	if len(tags) != 2 || tags[0] != "tag:cluster" || tags[1] != "tag:web" {
		t.Fatalf("aclTags = %v", request["aclTags"])
	}
	expiration, err := time.Parse(time.RFC3339, request["expiration"].(string))
	if err != nil {
		t.Fatalf("expiration %v: %v", request["expiration"], err)
	}
	if delta := time.Until(expiration) - 90*24*time.Hour; delta < -time.Minute || delta > time.Minute {
		t.Fatalf("expiration %s is not ~90 days away", expiration)
	}
}

func TestAPIHeadscaleClientReusesExistingUserAndOmitsEmptyTags(t *testing.T) {
	api := &fakeHeadscaleAPI{key: "k", users: map[string]string{"k8s-apps": "3"}}
	server := httptest.NewServer(api)
	defer server.Close()
	client := newTestAPIClient(t, server, apiKeySecret("k"))

	if _, err := client.MintReusableAuthKey(context.Background(), nil); err != nil {
		t.Fatalf("MintReusableAuthKey: %v", err)
	}
	if api.requests[0]["user"] != "3" {
		t.Fatalf("user = %v, want existing id 3", api.requests[0]["user"])
	}
	if _, ok := api.requests[0]["aclTags"]; ok {
		t.Fatalf("aclTags sent for an untagged key: %v", api.requests[0])
	}
}

func TestAPIHeadscaleClientNodeIPsMatchesNameOrGivenName(t *testing.T) {
	api := &fakeHeadscaleAPI{key: "k", users: map[string]string{}, nodesJSON: `{"nodes":[
		{"name":"proxy-abc","givenName":"web","ipAddresses":["100.64.0.5","fd7a:115c:a1e0::5"]},
		{"name":"other","givenName":"other","ipAddresses":["100.64.0.9"]}]}`}
	server := httptest.NewServer(api)
	defer server.Close()
	client := newTestAPIClient(t, server, apiKeySecret("k"))

	for _, name := range []string{"proxy-abc", "web"} {
		ips, err := client.NodeIPs(context.Background(), name)
		if err != nil {
			t.Fatalf("NodeIPs(%q): %v", name, err)
		}
		if strings.Join(ips, ",") != "100.64.0.5,fd7a:115c:a1e0::5" {
			t.Fatalf("NodeIPs(%q) = %v", name, ips)
		}
	}
	ips, err := client.NodeIPs(context.Background(), "missing")
	if err != nil || ips != nil {
		t.Fatalf("NodeIPs(missing) = %v, %v; want nil, nil", ips, err)
	}
}

func TestAPIHeadscaleClientReportsMissingAndRejectedKeys(t *testing.T) {
	api := &fakeHeadscaleAPI{key: "right", users: map[string]string{}}
	server := httptest.NewServer(api)
	defer server.Close()

	_, err := newTestAPIClient(t, server, apiKeySecret("")).NodeIPs(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "has no") {
		t.Fatalf("empty key error = %v", err)
	}
	_, err = newTestAPIClient(t, server, apiKeySecret("wrong")).NodeIPs(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("rejected key error = %v", err)
	}
}

func TestParseHeadscaleDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "12h": 12 * time.Hour, "1h30m": 90 * time.Minute} {
		got, err := parseHeadscaleDuration(input)
		if err != nil || got != want {
			t.Fatalf("parseHeadscaleDuration(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0d", "-1d", "xd", "soon", "0s"} {
		if _, err := parseHeadscaleDuration(input); err == nil {
			t.Fatalf("parseHeadscaleDuration(%q) succeeded", input)
		}
	}
}

func TestConfigValidatesHeadscaleAPI(t *testing.T) {
	valid := Config{HeadscaleAPI: HeadscaleAPIConfig{URL: "http://headscale.headscale.svc:8080"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if got := valid.withDefaults().HeadscaleAPI; got.KeySecretName != "headscale-api-key" || got.KeySecretKey != "HEADSCALE_API_KEY" {
		t.Fatalf("defaults = %+v", got)
	}
	for _, bad := range []Config{
		{HeadscaleAPI: HeadscaleAPIConfig{URL: "headscale:8080"}},
		{HeadscaleAPI: HeadscaleAPIConfig{URL: "ftp://headscale"}},
		{HeadscaleAPI: HeadscaleAPIConfig{URL: "http://headscale", KeySecretName: "Bad_Name"}},
		{HeadscaleAPI: HeadscaleAPIConfig{URL: "http://headscale"}, Proxy: ProxyConfig{AuthKeyExpiration: "soon"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("config %+v validated", bad.HeadscaleAPI)
		}
	}
	if (Config{}).HeadscaleAPI.Enabled() {
		t.Fatal("API client enabled without a URL")
	}
}
