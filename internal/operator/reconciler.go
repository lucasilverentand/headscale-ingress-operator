package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	managedByValue       = "headscale-ingress-operator"
	sourceNamespaceLabel = "headscale.silverswarm.io/source-namespace"
	sourceNameLabel      = "headscale.silverswarm.io/source-name"
	statusAnnotation     = "headscale.silverswarm.io/status"
	publishAnnotation    = "headscale.silverswarm.io/publish"
	targetIPAnnotation   = "headscale.silverswarm.io/target-ip"
	externalDNSExclude   = "external-dns.alpha.kubernetes.io/exclude"
	managedByLabel       = "app.kubernetes.io/managed-by"
	statusReady          = "Ready"
	statusImplemented    = "Implemented"
	statusRejected       = "Rejected"
)

type Reconciler struct {
	Client kubernetes.Interface
	Config Config
}

type Summary struct {
	SourceIngresses         int
	ImplementationIngresses int
	DeletedImplementations  int
	Records                 int
	Skipped                 []string
}

func (reconciler Reconciler) Reconcile(ctx context.Context) (Summary, error) {
	config := reconciler.Config.withDefaults()
	if reconciler.Client == nil {
		return Summary{}, fmt.Errorf("kubernetes client is required")
	}
	if config.SourceClassName == config.ImplementationIngressClassName {
		return Summary{}, fmt.Errorf("source and implementation ingress classes must differ")
	}

	list, err := reconciler.Client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Summary{}, fmt.Errorf("list ingresses: %w", err)
	}

	summary := Summary{}
	records := append([]DNSRecord{}, config.StaticRecords...)
	activeImplementations := map[types.NamespacedName]struct{}{}
	hostOwners := map[string]map[string]struct{}{}

	for i := range list.Items {
		source := list.Items[i]
		if source.Spec.IngressClassName == nil || *source.Spec.IngressClassName != config.SourceClassName {
			continue
		}
		sourceID := source.Namespace + "/" + source.Name
		for _, host := range hostsForIngress(source) {
			if hostOwners[host] == nil {
				hostOwners[host] = map[string]struct{}{}
			}
			hostOwners[host][sourceID] = struct{}{}
		}
	}

	for i := range list.Items {
		source := list.Items[i]
		if source.Spec.IngressClassName == nil || *source.Spec.IngressClassName != config.SourceClassName {
			continue
		}

		summary.SourceIngresses++
		sourceID := source.Namespace + "/" + source.Name
		hosts := hostsForIngress(source)
		if len(hosts) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no hosts declared")
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}

		if invalidHost := firstDisallowedHost(hosts, config.AllowedZones); invalidHost != "" {
			summary.Skipped = append(summary.Skipped, sourceID+": host "+invalidHost+" is outside allowed zones")
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}

		if conflictingHost := firstConflictingHost(hosts, hostOwners, sourceID); conflictingHost != "" {
			summary.Skipped = append(summary.Skipped, sourceID+": host "+conflictingHost+" is claimed by another headscale ingress")
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}

		implementation := implementationIngressFor(source, config)
		activeImplementations[namespacedName(implementation.Namespace, implementation.Name)] = struct{}{}
		applied, err := reconciler.applyImplementationIngress(ctx, implementation)
		if err != nil {
			return summary, fmt.Errorf("apply implementation ingress %s/%s: %w", implementation.Namespace, implementation.Name, err)
		}
		summary.ImplementationIngresses++

		if source.Annotations[publishAnnotation] == "false" {
			if err := reconciler.markSource(ctx, source, statusImplemented, nil); err != nil {
				return summary, err
			}
			continue
		}

		targets := targetIPsFor(source, *applied, config.DefaultTargetIPs)
		if len(targets) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no A/AAAA target resolved")
			if err := reconciler.markSource(ctx, source, statusImplemented, nil); err != nil {
				return summary, err
			}
			continue
		}

		records = append(records, recordsFor(hosts, targets)...)
		if err := reconciler.markSource(ctx, source, statusReady, targets); err != nil {
			return summary, err
		}
	}

	deleted, err := reconciler.deleteStaleImplementations(ctx, list.Items, activeImplementations)
	if err != nil {
		return summary, err
	}
	summary.DeletedImplementations = deleted

	records = dedupeRecords(records)
	if err := reconciler.publishRecords(ctx, config, records); err != nil {
		return summary, err
	}
	summary.Records = len(records)
	return summary, nil
}

func (reconciler Reconciler) deleteStaleImplementations(ctx context.Context, ingresses []networkingv1.Ingress, active map[types.NamespacedName]struct{}) (int, error) {
	deleted := 0
	for _, ingress := range ingresses {
		if !isManagedImplementation(ingress) {
			continue
		}
		key := namespacedName(ingress.Namespace, ingress.Name)
		if _, ok := active[key]; ok {
			continue
		}
		if err := reconciler.Client.NetworkingV1().Ingresses(ingress.Namespace).Delete(ctx, ingress.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return deleted, fmt.Errorf("delete stale implementation ingress %s/%s: %w", ingress.Namespace, ingress.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

func isManagedImplementation(ingress networkingv1.Ingress) bool {
	if ingress.Labels[managedByLabel] != managedByValue {
		return false
	}
	if ingress.Labels[sourceNamespaceLabel] == "" || ingress.Labels[sourceNameLabel] == "" {
		return false
	}
	return ingress.Labels[sourceNamespaceLabel] == ingress.Namespace
}

func implementationIngressFor(source networkingv1.Ingress, config Config) networkingv1.Ingress {
	labels := map[string]string{
		managedByLabel:       managedByValue,
		sourceNamespaceLabel: source.Namespace,
		sourceNameLabel:      source.Name,
	}
	annotations := map[string]string{
		externalDNSExclude: "true",
	}
	for key, value := range source.Annotations {
		annotations[key] = value
	}
	annotations[externalDNSExclude] = "true"

	spec := *source.Spec.DeepCopy()
	spec.IngressClassName = &config.ImplementationIngressClassName

	implementation := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        generatedIngressName(source.Name, config.ImplementationNameSuffix),
			Namespace:   source.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: spec,
	}

	if source.UID != "" {
		implementation.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         "networking.k8s.io/v1",
			Kind:               "Ingress",
			Name:               source.Name,
			UID:                source.UID,
			Controller:         ptr(true),
			BlockOwnerDeletion: ptr(true),
		}}
	}

	return implementation
}

func (reconciler Reconciler) applyImplementationIngress(ctx context.Context, desired networkingv1.Ingress) (*networkingv1.Ingress, error) {
	client := reconciler.Client.NetworkingV1().Ingresses(desired.Namespace)
	existing, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return client.Create(ctx, &desired, metav1.CreateOptions{})
	}
	if err != nil {
		return nil, err
	}

	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.Annotations = desired.Annotations
	copy.OwnerReferences = desired.OwnerReferences
	copy.Spec = desired.Spec
	return client.Update(ctx, copy, metav1.UpdateOptions{})
}

func (reconciler Reconciler) markSource(ctx context.Context, source networkingv1.Ingress, status string, targets []string) error {
	client := reconciler.Client.NetworkingV1().Ingresses(source.Namespace)

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
	_, err = client.Patch(ctx, source.Name, types.MergePatchType, metadataPatch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch source ingress annotation %s/%s: %w", source.Namespace, source.Name, err)
	}

	if len(targets) == 0 {
		return nil
	}

	statusIngresses := make([]map[string]string, 0, len(targets))
	for _, target := range targets {
		statusIngresses = append(statusIngresses, map[string]string{"ip": target})
	}
	statusPatch, err := json.Marshal(map[string]any{
		"status": map[string]any{
			"loadBalancer": map[string]any{
				"ingress": statusIngresses,
			},
		},
	})
	if err != nil {
		return err
	}
	if _, err := client.Patch(ctx, source.Name, types.MergePatchType, statusPatch, metav1.PatchOptions{}, "status"); err != nil {
		return fmt.Errorf("patch source ingress status %s/%s: %w", source.Namespace, source.Name, err)
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
		return err
	}
	if err != nil {
		return err
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
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return err
}

func hostsForIngress(ingress networkingv1.Ingress) []string {
	hosts := map[string]struct{}{}
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			hosts[normalizeHost(rule.Host)] = struct{}{}
		}
	}
	for _, tls := range ingress.Spec.TLS {
		for _, host := range tls.Hosts {
			if host != "" {
				hosts[normalizeHost(host)] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

func firstDisallowedHost(hosts []string, zones []string) string {
	for _, host := range hosts {
		if strings.Contains(host, "*") || !hostAllowed(host, zones) {
			return host
		}
	}
	return ""
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

func targetIPsFor(source networkingv1.Ingress, implementation networkingv1.Ingress, defaults []string) []string {
	var candidates []string
	if source.Annotations[targetIPAnnotation] != "" {
		candidates = strings.Split(source.Annotations[targetIPAnnotation], ",")
	} else {
		for _, item := range implementation.Status.LoadBalancer.Ingress {
			if item.IP != "" {
				candidates = append(candidates, item.IP)
			}
		}
		candidates = append(candidates, defaults...)
	}

	seen := map[string]struct{}{}
	var targets []string
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, err := netip.ParseAddr(candidate); err != nil {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		targets = append(targets, candidate)
	}
	sort.Strings(targets)
	return targets
}

func generatedIngressName(sourceName string, suffix string) string {
	name := sourceName + suffix
	if len(name) <= 63 {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:])[:12]
	maxPrefix := 63 - len(hash) - 1
	if maxPrefix < 1 {
		return hash
	}

	prefix := strings.TrimRight(sourceName[:min(len(sourceName), maxPrefix)], "-")
	if prefix == "" {
		return hash
	}
	return prefix + "-" + hash
}

func ptr[T any](value T) *T {
	return &value
}

func namespacedName(namespace string, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}
