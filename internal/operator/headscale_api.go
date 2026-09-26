package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// APIHeadscaleClient talks to the Headscale REST API with an API key read from
// a Secret. Unlike KubernetesHeadscaleClient it needs no pods/exec permission:
// only get on one named Secret. The key is read on every call, so a rotated key
// is picked up without restarting the operator.
type APIHeadscaleClient struct {
	client     kubernetes.Interface
	http       *http.Client
	apiURL     string
	secretNS   string
	secretName string
	secretKey  string
	user       string
	expiration time.Duration
}

func NewAPIHeadscaleClient(client kubernetes.Interface, config Config) (*APIHeadscaleClient, error) {
	config = config.withDefaults()
	expiration, err := parseHeadscaleDuration(config.Proxy.AuthKeyExpiration)
	if err != nil {
		return nil, fmt.Errorf("proxy auth key expiration: %w", err)
	}
	return &APIHeadscaleClient{
		client:     client,
		http:       &http.Client{Timeout: 30 * time.Second},
		apiURL:     strings.TrimRight(config.HeadscaleAPI.URL, "/"),
		secretNS:   config.HeadscaleNamespace,
		secretName: config.HeadscaleAPI.KeySecretName,
		secretKey:  config.HeadscaleAPI.KeySecretKey,
		user:       config.Proxy.HeadscaleUser,
		expiration: expiration,
	}, nil
}

func (client *APIHeadscaleClient) MintReusableAuthKey(ctx context.Context, tags []string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("headscale client is required")
	}
	key, err := client.apiKey(ctx)
	if err != nil {
		return "", err
	}
	userID, err := client.ensureUser(ctx, key)
	if err != nil {
		return "", err
	}

	request := map[string]any{
		"user":       userID,
		"reusable":   true,
		"expiration": time.Now().Add(client.expiration).UTC().Format(time.RFC3339),
	}
	if len(tags) > 0 {
		request["aclTags"] = tags
	}
	var response struct {
		PreAuthKey struct {
			Key string `json:"key"`
		} `json:"preAuthKey"`
	}
	if err := client.do(ctx, key, http.MethodPost, "/preauthkey", request, &response); err != nil {
		return "", fmt.Errorf("create headscale preauth key: %w", err)
	}
	if response.PreAuthKey.Key == "" {
		return "", fmt.Errorf("headscale preauth key create returned no key")
	}
	return response.PreAuthKey.Key, nil
}

func (client *APIHeadscaleClient) NodeIPs(ctx context.Context, nodeName string) ([]string, error) {
	if client == nil {
		return nil, fmt.Errorf("headscale client is required")
	}
	key, err := client.apiKey(ctx)
	if err != nil {
		return nil, err
	}
	var response struct {
		Nodes []struct {
			Name        string   `json:"name"`
			GivenName   string   `json:"givenName"`
			IPAddresses []string `json:"ipAddresses"`
		} `json:"nodes"`
	}
	if err := client.do(ctx, key, http.MethodGet, "/node", nil, &response); err != nil {
		return nil, fmt.Errorf("list headscale nodes: %w", err)
	}
	for _, node := range response.Nodes {
		if node.Name == nodeName || node.GivenName == nodeName {
			return normalizeIPs(node.IPAddresses)
		}
	}
	return nil, nil
}

func (client *APIHeadscaleClient) ensureUser(ctx context.Context, key string) (string, error) {
	var users struct {
		Users []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"users"`
	}
	path := "/user?name=" + url.QueryEscape(client.user)
	if err := client.do(ctx, key, http.MethodGet, path, nil, &users); err != nil {
		return "", fmt.Errorf("list headscale users: %w", err)
	}
	for _, user := range users.Users {
		if user.Name == client.user {
			return user.ID, nil
		}
	}

	var created struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := client.do(ctx, key, http.MethodPost, "/user", map[string]string{"name": client.user}, &created); err != nil {
		return "", fmt.Errorf("create headscale user %q: %w", client.user, err)
	}
	if created.User.ID == "" {
		return "", fmt.Errorf("headscale user %q create returned no id", client.user)
	}
	return created.User.ID, nil
}

func (client *APIHeadscaleClient) apiKey(ctx context.Context) (string, error) {
	secret, err := client.client.CoreV1().Secrets(client.secretNS).Get(ctx, client.secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read headscale API key Secret %s/%s: %w", client.secretNS, client.secretName, err)
	}
	key := strings.TrimSpace(string(secret.Data[client.secretKey]))
	if key == "" {
		return "", fmt.Errorf("headscale API key Secret %s/%s has no %q yet", client.secretNS, client.secretName, client.secretKey)
	}
	return key, nil
}

func (client *APIHeadscaleClient) do(ctx context.Context, key string, method string, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, client.apiURL+"/api/v1"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("parse %s %s response: %w", method, path, err)
	}
	return nil
}

// parseHeadscaleDuration accepts the forms the headscale CLI does for
// --expiration: Go durations plus a day suffix, e.g. "90d" or "12h".
func parseHeadscaleDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil || count <= 0 {
			return 0, fmt.Errorf("%q is not a positive number of days", value)
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration: %w", value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration", value)
	}
	return duration, nil
}
