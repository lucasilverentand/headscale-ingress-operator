package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	managedByValue            = "headscale-ingress-operator"
	sourceNamespaceAnnotation = "headscale-ingress-operator.lucasilverentand.dev/source-namespace"
	sourceNameAnnotation      = "headscale-ingress-operator.lucasilverentand.dev/source-name"
	sourceUIDAnnotation       = "headscale-ingress-operator.lucasilverentand.dev/source-uid"
	statusAnnotation          = "headscale-ingress-operator.lucasilverentand.dev/status"
	publishAnnotation         = "headscale-ingress-operator.lucasilverentand.dev/publish"
	targetIPAnnotation        = "headscale-ingress-operator.lucasilverentand.dev/target-ip"
	externalDNSExclude        = "external-dns.alpha.kubernetes.io/exclude"
	managedByLabel            = "app.kubernetes.io/managed-by"
	statusReady               = "Ready"
	statusImplemented         = "Implemented"
	statusRejected            = "Rejected"
)

var errImplementationConflict = errors.New("implementation ingress already exists but is not managed by this source")

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

type sourceMark struct {
	source  networkingv1.Ingress
	status  string
	targets []string
}

func (reconciler Reconciler) Reconcile(ctx context.Context) (Summary, error) {
	config, err := reconciler.Config.validated()
	if err != nil {
		return Summary{}, err
	}
	if reconciler.Client == nil {
		return Summary{}, fmt.Errorf("kubernetes client is required")
	}

	list, err := reconciler.Client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Summary{}, fmt.Errorf("list ingresses: %w", err)
	}

	summary := Summary{}
	records := append([]DNSRecord{}, config.StaticRecords...)
	readyMarks := []sourceMark{}
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

		if invalidHost, reason := firstRejectedHost(hosts, config.AllowedZones); invalidHost != "" {
			summary.Skipped = append(summary.Skipped, sourceID+": host "+invalidHost+" "+reason)
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

		if _, _, err := explicitTargetIPsFor(source); err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}

		publish, err := publishEnabled(source)
		if err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}

		implementation := implementationIngressFor(source, config)
		activeImplementations[namespacedName(implementation.Namespace, implementation.Name)] = struct{}{}
		applied, err := reconciler.applyImplementationIngress(ctx, implementation)
		if err != nil {
			if errors.Is(err, errImplementationConflict) {
				summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
				if markErr := reconciler.markSource(ctx, source, statusRejected, nil); markErr != nil {
					return summary, markErr
				}
				continue
			}
			return summary, fmt.Errorf("apply implementation ingress %s/%s: %w", implementation.Namespace, implementation.Name, err)
		}
		summary.ImplementationIngresses++

		if !publish {
			if err := reconciler.markSource(ctx, source, statusImplemented, nil); err != nil {
				return summary, err
			}
			continue
		}

		targets, err := targetIPsFor(source, *applied, config.DefaultTargetIPs)
		if err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}
		if len(targets) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no A/AAAA target resolved")
			if err := reconciler.markSource(ctx, source, statusImplemented, nil); err != nil {
				return summary, err
			}
			continue
		}

		records = append(records, recordsFor(hosts, targets)...)
		readyMarks = append(readyMarks, sourceMark{source: source, status: statusReady, targets: targets})
	}

	deleted, err := reconciler.deleteStaleImplementations(ctx, config, list.Items, activeImplementations)
	if err != nil {
		return summary, err
	}
	summary.DeletedImplementations = deleted

	records = dedupeRecords(records)
	if err := reconciler.publishRecords(ctx, config, records); err != nil {
		return summary, err
	}
	for _, mark := range readyMarks {
		if err := reconciler.markSource(ctx, mark.source, mark.status, mark.targets); err != nil {
			return summary, err
		}
	}
	summary.Records = len(records)
	return summary, nil
}

func (reconciler Reconciler) deleteStaleImplementations(ctx context.Context, config Config, ingresses []networkingv1.Ingress, active map[types.NamespacedName]struct{}) (int, error) {
	deleted := 0
	for _, ingress := range ingresses {
		if !isManagedImplementation(ingress, config) {
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

func isManagedImplementation(ingress networkingv1.Ingress, config Config) bool {
	if ingress.Labels[managedByLabel] != managedByValue {
		return false
	}
	sourceNamespace := ingress.Annotations[sourceNamespaceAnnotation]
	sourceName := ingress.Annotations[sourceNameAnnotation]
	if sourceNamespace == "" || sourceName == "" {
		return false
	}
	if sourceNamespace != ingress.Namespace {
		return false
	}
	if ingress.Name != generatedIngressName(sourceName, config.ImplementationNameSuffix) {
		return false
	}
	return hasMatchingControllerOwnerReference(ingress, sourceName)
}

func implementationIngressFor(source networkingv1.Ingress, config Config) networkingv1.Ingress {
	labels := map[string]string{
		managedByLabel: managedByValue,
	}
	annotations := map[string]string{
		sourceNamespaceAnnotation: source.Namespace,
		sourceNameAnnotation:      source.Name,
		externalDNSExclude:        "true",
	}
	for key, value := range source.Annotations {
		if skipSourceAnnotation(key) {
			continue
		}
		annotations[key] = value
	}
	annotations[externalDNSExclude] = "true"
	if source.UID != "" {
		annotations[sourceUIDAnnotation] = string(source.UID)
	}

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
			APIVersion: "networking.k8s.io/v1",
			Kind:       "Ingress",
			Name:       source.Name,
			UID:        source.UID,
			Controller: ptr(true),
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
	if !canUpdateImplementation(*existing, desired) {
		return nil, fmt.Errorf("%w: %s/%s", errImplementationConflict, existing.Namespace, existing.Name)
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
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("patch source ingress annotation %s/%s: %w", source.Namespace, source.Name, err)
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
	if _, err := client.Patch(ctx, source.Name, types.MergePatchType, statusPatch, metav1.PatchOptions{}, "status"); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("patch source ingress status %s/%s: %w", source.Namespace, source.Name, err)
	}
	return nil
}

func canUpdateImplementation(existing networkingv1.Ingress, desired networkingv1.Ingress) bool {
	if existing.Labels[managedByLabel] != managedByValue {
		return false
	}
	if existing.Annotations[sourceNamespaceAnnotation] != desired.Annotations[sourceNamespaceAnnotation] {
		return false
	}
	if existing.Annotations[sourceNameAnnotation] != desired.Annotations[sourceNameAnnotation] {
		return false
	}
	if len(desired.OwnerReferences) == 0 {
		return len(existing.OwnerReferences) == 0
	}
	return hasMatchingControllerOwnerReference(existing, desired.Annotations[sourceNameAnnotation])
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

func targetIPsFor(source networkingv1.Ingress, implementation networkingv1.Ingress, defaults []string) ([]string, error) {
	var candidates []string
	if targets, ok, err := explicitTargetIPsFor(source); ok || err != nil {
		return targets, err
	} else {
		for _, item := range implementation.Status.LoadBalancer.Ingress {
			if item.IP != "" {
				targets, err := normalizeIPs([]string{item.IP})
				if err == nil {
					candidates = append(candidates, targets...)
				}
			}
		}
		if len(candidates) == 0 {
			candidates = append(candidates, defaults...)
		}
	}

	targets, _ := normalizeIPs(candidates)
	return targets, nil
}

func explicitTargetIPsFor(source networkingv1.Ingress) ([]string, bool, error) {
	raw := strings.TrimSpace(source.Annotations[targetIPAnnotation])
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

func publishEnabled(source networkingv1.Ingress) (bool, error) {
	raw := strings.TrimSpace(source.Annotations[publishAnnotation])
	if raw == "" {
		return true, nil
	}
	switch strings.ToLower(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid publish annotation: must be true or false")
	}
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

	prefix := strings.TrimRight(sourceName[:min(len(sourceName), maxPrefix)], "-.")
	if prefix == "" {
		return hash
	}
	return prefix + "-" + hash
}

func hasMatchingControllerOwnerReference(ingress networkingv1.Ingress, sourceName string) bool {
	sourceUID := ingress.Annotations[sourceUIDAnnotation]
	if sourceUID == "" {
		return false
	}
	for _, owner := range ingress.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			return owner.APIVersion == "networking.k8s.io/v1" &&
				owner.Kind == "Ingress" &&
				owner.Name == sourceName &&
				string(owner.UID) == sourceUID
		}
	}
	return false
}

func skipSourceAnnotation(key string) bool {
	switch key {
	case sourceNamespaceAnnotation, sourceNameAnnotation, sourceUIDAnnotation, statusAnnotation, publishAnnotation, targetIPAnnotation,
		externalDNSExclude, "kubectl.kubernetes.io/last-applied-configuration":
		return true
	default:
		return strings.HasPrefix(key, "argocd.argoproj.io/") ||
			strings.HasPrefix(key, "external-dns.alpha.kubernetes.io/") ||
			strings.HasPrefix(key, "helm.sh/") ||
			strings.HasPrefix(key, "kustomize.toolkit.fluxcd.io/") ||
			strings.HasPrefix(key, "meta.helm.sh/")
	}
}

func ptr[T any](value T) *T {
	return &value
}

func namespacedName(namespace string, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}
