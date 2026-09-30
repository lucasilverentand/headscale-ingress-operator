package operator

import (
	"bytes"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartHeadscaleAPIMode(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is not installed")
	}
	for _, tc := range []struct {
		name string
		url  string
		null bool
	}{
		{name: "default"},
		{name: "null", null: true},
		{name: "whitespace", url: " \t "},
		{name: "API", url: "http://headscale.headscale.svc:8080"},
		{name: "padded API", url: " http://headscale.headscale.svc:8080 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := []string{"template", "test", "../../charts/headscale-ingress-operator", "--set-string", "headscale.api.url=" + tc.url}
			if tc.null {
				flags = []string{"template", "test", "../../charts/headscale-ingress-operator", "--set-json", "headscale.api.url=null"}
			}
			output, err := exec.Command(helm, flags...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			var args []string
			var rules []rbacv1.PolicyRule
			for {
				var resource struct {
					Kind  string                `json:"kind"`
					Rules []rbacv1.PolicyRule   `json:"rules"`
					Spec  appsv1.DeploymentSpec `json:"spec"`
				}
				if err := decoder.Decode(&resource); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if resource.Kind == "Deployment" {
					args = resource.Spec.Template.Spec.Containers[0].Args
				}
				if resource.Kind == "Role" {
					rules = resource.Rules
				}
			}
			if len(args) == 0 || len(rules) == 0 {
				t.Fatal("deployment or Headscale namespace Role missing")
			}
			apiURL := strings.TrimSpace(tc.url)
			wantAPI := apiURL != ""
			var gotAPI bool
			for _, arg := range args {
				if strings.HasPrefix(arg, "--headscale-api-url=") {
					gotAPI = true
					if arg != "--headscale-api-url="+apiURL {
						t.Fatalf("API URL argument = %q", arg)
					}
				}
			}
			if gotAPI != wantAPI {
				t.Fatalf("API selected = %v, want %v", gotAPI, wantAPI)
			}
			var podLookup, podExec, keySecret bool
			for _, rule := range rules {
				podLookup = podLookup || slices.Contains(rule.Resources, "pods")
				podExec = podExec || slices.Contains(rule.Resources, "pods/exec")
				if slices.Contains(rule.Resources, "secrets") {
					keySecret = slices.Equal(rule.ResourceNames, []string{"headscale-api-key"}) && slices.Equal(rule.Verbs, []string{"get"})
				}
			}
			if podLookup != !wantAPI || podExec != !wantAPI || keySecret != wantAPI {
				t.Fatalf("namespace Role: pod lookup=%v, exec=%v, key Secret=%v; API=%v", podLookup, podExec, keySecret, wantAPI)
			}
			if wantAPI && (!slices.Contains(args, "--headscale-api-key-secret=headscale-api-key") || !slices.Contains(args, "--headscale-api-key-secret-key=HEADSCALE_API_KEY")) {
				t.Fatal("API Secret flags missing")
			}
		})
	}
}
