package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	managedByValue     = "headscale-ingress-operator"
	hostnameAnnotation = "headscale-ingress-operator.lucasilverentand.dev/hostname"
	statusAnnotation   = "headscale-ingress-operator.lucasilverentand.dev/status"
	targetIPAnnotation = "headscale-ingress-operator.lucasilverentand.dev/target-ip"
	managedByLabel     = "app.kubernetes.io/managed-by"
	statusReady        = "Ready"
	statusPending      = "PendingTarget"
	statusRejected     = "Rejected"
)

type Reconciler struct {
	Client kubernetes.Interface
	Config Config
}

type Summary struct {
	SourceServices int
	Records        int
	Skipped        []string
}

type serviceMark struct {
	service corev1.Service
	status  string
}

func (reconciler Reconciler) Reconcile(ctx context.Context) (Summary, error) {
	config, err := reconciler.Config.validated()
	if err != nil {
		return Summary{}, err
	}
	if reconciler.Client == nil {
		return Summary{}, fmt.Errorf("kubernetes client is required")
	}

	list, err := reconciler.Client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Summary{}, fmt.Errorf("list services: %w", err)
	}

	summary := Summary{}
	records := []DNSRecord{}
	readyMarks := []serviceMark{}
	hostOwners := map[string]map[string]struct{}{}

	for i := range list.Items {
		service := list.Items[i]
		if !serviceOptedIn(service) {
			continue
		}
		sourceID := service.Namespace + "/" + service.Name
		for _, host := range hostsForService(service) {
			if hostOwners[host] == nil {
				hostOwners[host] = map[string]struct{}{}
			}
			hostOwners[host][sourceID] = struct{}{}
		}
	}

	for i := range list.Items {
		service := list.Items[i]
		if !serviceOptedIn(service) {
			continue
		}

		summary.SourceServices++
		sourceID := service.Namespace + "/" + service.Name
		hosts := hostsForService(service)
		if len(hosts) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no hostnames declared")
			if err := reconciler.markService(ctx, service, statusRejected); err != nil {
				return summary, err
			}
			continue
		}

		if invalidHost, reason := firstRejectedHost(hosts, config.AllowedZones); invalidHost != "" {
			summary.Skipped = append(summary.Skipped, sourceID+": host "+invalidHost+" "+reason)
			if err := reconciler.markService(ctx, service, statusRejected); err != nil {
				return summary, err
			}
			continue
		}

		if conflictingHost := firstConflictingHost(hosts, hostOwners, sourceID); conflictingHost != "" {
			summary.Skipped = append(summary.Skipped, sourceID+": host "+conflictingHost+" is claimed by another headscale service")
			if err := reconciler.markService(ctx, service, statusRejected); err != nil {
				return summary, err
			}
			continue
		}

		targets, err := targetIPsForService(service)
		if err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markService(ctx, service, statusRejected); err != nil {
				return summary, err
			}
			continue
		}
		if len(targets) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no A/AAAA target resolved")
			if err := reconciler.markService(ctx, service, statusPending); err != nil {
				return summary, err
			}
			continue
		}

		records = append(records, recordsFor(hosts, targets)...)
		readyMarks = append(readyMarks, serviceMark{service: service, status: statusReady})
	}

	records = dedupeRecords(records)
	if err := reconciler.publishRecords(ctx, config, records); err != nil {
		return summary, err
	}
	for _, mark := range readyMarks {
		if err := reconciler.markService(ctx, mark.service, mark.status); err != nil {
			return summary, err
		}
	}
	summary.Records = len(records)
	return summary, nil
}

func (reconciler Reconciler) markService(ctx context.Context, service corev1.Service, status string) error {
	metadataPatch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				statusAnnotation: status,
			},
		},
	})
	if err != nil {
		return err
	}

	_, err = reconciler.Client.CoreV1().Services(service.Namespace).Patch(ctx, service.Name, types.MergePatchType, metadataPatch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("patch service annotation %s/%s: %w", service.Namespace, service.Name, err)
	}
	return nil
}

func (reconciler Reconciler) publishRecords(ctx context.Context, config Config, records []DNSRecord) error {
	payload, err := stableRecordsJSON(records)
	if err != nil {
		return err
	}

	client := reconciler.Client.CoreV1().ConfigMaps(config.HeadscaleNamespace)
	desiredData := map[string]string{config.RecordsConfigMapKey: payload}
	existing, err := client.Get(ctx, config.RecordsConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.RecordsConfigMapName,
				Namespace: config.HeadscaleNamespace,
				Labels: map[string]string{
					managedByLabel: managedByValue,
				},
			},
			Data: desiredData,
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create records ConfigMap %s/%s: %w", config.HeadscaleNamespace, config.RecordsConfigMapName, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get records ConfigMap %s/%s: %w", config.HeadscaleNamespace, config.RecordsConfigMapName, err)
	}
	if existing.Labels[managedByLabel] != managedByValue {
		return fmt.Errorf("refusing to update ConfigMap %s/%s without %s=%s label", config.HeadscaleNamespace, config.RecordsConfigMapName, managedByLabel, managedByValue)
	}

	copy := existing.DeepCopy()
	if copy.Labels == nil {
		copy.Labels = map[string]string{}
	}
	copy.Labels[managedByLabel] = managedByValue
	if copy.Data == nil {
		copy.Data = map[string]string{}
	}
	copy.Data[config.RecordsConfigMapKey] = payload
	if _, err = client.Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update records ConfigMap %s/%s: %w", config.HeadscaleNamespace, config.RecordsConfigMapName, err)
	}
	return nil
}

func serviceOptedIn(service corev1.Service) bool {
	return strings.TrimSpace(service.Annotations[hostnameAnnotation]) != ""
}

func hostsForService(service corev1.Service) []string {
	hosts := map[string]struct{}{}
	for _, host := range strings.Split(service.Annotations[hostnameAnnotation], ",") {
		host = normalizeHost(host)
		if host != "" {
			hosts[host] = struct{}{}
		}
	}

	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

func firstRejectedHost(hosts []string, zones []string) (string, string) {
	for _, host := range hosts {
		if strings.Contains(host, "*") {
			return host, "uses unsupported wildcard DNS"
		}
		if len(validation.IsDNS1123Subdomain(host)) > 0 {
			return host, "is not a valid DNS host"
		}
		if !hostAllowed(host, zones) {
			return host, "is outside allowed zones"
		}
	}
	return "", ""
}

func firstConflictingHost(hosts []string, hostOwners map[string]map[string]struct{}, sourceID string) string {
	for _, host := range hosts {
		owners := hostOwners[host]
		if len(owners) <= 1 {
			continue
		}
		if _, ok := owners[sourceID]; ok {
			return host
		}
	}
	return ""
}

func hostAllowed(host string, zones []string) bool {
	if len(zones) == 0 {
		return true
	}
	for _, zone := range zones {
		zone = normalizeHost(zone)
		if host == zone || strings.HasSuffix(host, "."+zone) {
			return true
		}
	}
	return false
}

func targetIPsForService(service corev1.Service) ([]string, error) {
	if targets, ok, err := explicitTargetIPsFor(service); ok || err != nil {
		return targets, err
	}

	var candidates []string
	for _, item := range service.Status.LoadBalancer.Ingress {
		if item.IP != "" {
			candidates = append(candidates, item.IP)
		}
	}
	candidates = append(candidates, service.Spec.ExternalIPs...)
	for _, clusterIP := range service.Spec.ClusterIPs {
		if serviceIPUsable(clusterIP) {
			candidates = append(candidates, clusterIP)
		}
	}
	if len(service.Spec.ClusterIPs) == 0 && serviceIPUsable(service.Spec.ClusterIP) {
		candidates = append(candidates, service.Spec.ClusterIP)
	}

	targets, err := normalizeIPs(candidates)
	if err != nil {
		return nil, fmt.Errorf("invalid service target IP: %w", err)
	}
	return targets, nil
}

func serviceIPUsable(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != corev1.ClusterIPNone
}

func explicitTargetIPsFor(service corev1.Service) ([]string, bool, error) {
	raw := strings.TrimSpace(service.Annotations[targetIPAnnotation])
	if raw == "" {
		return nil, false, nil
	}
	targets, err := normalizeIPs(strings.Split(raw, ","))
	if err != nil {
		return nil, true, fmt.Errorf("invalid target IP annotation: %w", err)
	}
	if len(targets) == 0 {
		return nil, true, fmt.Errorf("invalid target IP annotation: must contain at least one IP")
	}
	return targets, true, nil
}
