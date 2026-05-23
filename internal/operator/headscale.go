package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type HeadscaleClient interface {
	MintReusableAuthKey(ctx context.Context) (string, error)
	NodeIPs(ctx context.Context, nodeName string) ([]string, error)
}

type KubernetesHeadscaleClient struct {
	client      kubernetes.Interface
	restConfig  *rest.Config
	namespace   string
	selector    string
	container   string
	user        string
	expiration  string
	authKeyTags []string
}

func NewKubernetesHeadscaleClient(client kubernetes.Interface, restConfig *rest.Config, config Config) *KubernetesHeadscaleClient {
	config = config.withDefaults()
	return &KubernetesHeadscaleClient{
		client:      client,
		restConfig:  restConfig,
		namespace:   config.HeadscaleNamespace,
		selector:    config.Proxy.HeadscalePodSelector,
		container:   config.Proxy.HeadscaleContainer,
		user:        config.Proxy.HeadscaleUser,
		expiration:  config.Proxy.AuthKeyExpiration,
		authKeyTags: append([]string(nil), config.Proxy.AuthKeyTags...),
	}
}

func (client *KubernetesHeadscaleClient) MintReusableAuthKey(ctx context.Context) (string, error) {
	if client == nil {
		return "", fmt.Errorf("headscale client is required")
	}
	_, _ = client.execHeadscale(ctx, "users", "create", client.user)
	usersJSON, err := client.execHeadscale(ctx, "users", "list", "--output", "json")
	if err != nil {
		return "", err
	}
	userID, err := headscaleUserID(usersJSON, client.user)
	if err != nil {
		return "", err
	}

	args := []string{
		"preauthkeys", "create",
		"--user", strconv.Itoa(userID),
		"--reusable",
		"--expiration", client.expiration,
	}
	if len(client.authKeyTags) > 0 {
		args = append(args, "--tags", strings.Join(client.authKeyTags, ","))
	}
	out, err := client.execHeadscale(ctx, args...)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("headscale preauthkeys create returned no key")
	}
	return fields[len(fields)-1], nil
}

func (client *KubernetesHeadscaleClient) NodeIPs(ctx context.Context, nodeName string) ([]string, error) {
	if client == nil {
		return nil, fmt.Errorf("headscale client is required")
	}
	out, err := client.execHeadscale(ctx, "nodes", "list", "--output", "json")
	if err != nil {
		return nil, err
	}
	return headscaleNodeIPs(out, nodeName)
}

func (client *KubernetesHeadscaleClient) execHeadscale(ctx context.Context, args ...string) (string, error) {
	pod, err := client.runningHeadscalePod(ctx)
	if err != nil {
		return "", err
	}
	command := append([]string{"headscale"}, args...)
	req := client.client.CoreV1().RESTClient().
		Post().
		Namespace(client.namespace).
		Resource("pods").
		Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: client.container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(client.restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("create headscale exec: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return "", fmt.Errorf("exec headscale %s: %w: %s", strings.Join(args, " "), err, message)
		}
		return "", fmt.Errorf("exec headscale %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

func (client *KubernetesHeadscaleClient) runningHeadscalePod(ctx context.Context) (string, error) {
	list, err := client.client.CoreV1().Pods(client.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: client.selector,
		FieldSelector: fields.OneTermEqualSelector("status.phase", string(corev1.PodRunning)).String(),
	})
	if err != nil {
		return "", fmt.Errorf("list headscale pods: %w", err)
	}
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no running Headscale pod found with selector %q", client.selector)
}

func headscaleUserID(payload string, name string) (int, error) {
	var users []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(payload), &users); err != nil {
		return 0, fmt.Errorf("parse headscale users: %w", err)
	}
	for _, user := range users {
		if user.Name == name {
			return user.ID, nil
		}
	}
	return 0, fmt.Errorf("headscale user %q not found", name)
}

func headscaleNodeIPs(payload string, name string) ([]string, error) {
	var nodes []struct {
		Name        string   `json:"name"`
		GivenName   string   `json:"given_name"`
		IPAddresses []string `json:"ip_addresses"`
	}
	if err := json.Unmarshal([]byte(payload), &nodes); err != nil {
		return nil, fmt.Errorf("parse headscale nodes: %w", err)
	}
	for _, node := range nodes {
		if node.Name == name || node.GivenName == name {
			return normalizeIPs(node.IPAddresses)
		}
	}
	return nil, nil
}
