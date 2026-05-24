package operator

import (
	"context"
	"encoding/json"
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
	managedByValue    = "headscale-ingress-operator"
	statusAnnotation  = "headscale-ingress-operator.lucasilverentand.dev/status"
	managedByLabel    = "app.kubernetes.io/managed-by"
	statusReady       = "Ready"
	statusPendingAuth = "PendingAuthKey"
	statusPendingNode = "PendingNodeIP"
	statusRejected    = "Rejected"
)

type Reconciler struct {
	Client    kubernetes.Interface
	Config    Config
	Headscale HeadscaleClient
}

type Summary struct {
	SourceIngresses int
	Records         int
	Skipped         []string
}

type sourceRef struct {
	apiVersion string
	namespace  string
	name       string
	uid        types.UID
}

type source struct {
	ref      sourceRef
	hosts    []string
	ingress  *networkingv1.Ingress
	routes   []proxyRoute
	routeErr error
}

func (source source) id() string {
	return source.ref.namespace + "/" + source.ref.name
}

func (source source) activeKey() string {
	return source.ref.namespace + "/" + source.ref.name
}

func (source source) ownerReference() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: source.ref.apiVersion,
		Kind:       "Ingress",
		Name:       source.ref.name,
		UID:        source.ref.uid,
	}
}

type sourceMark struct {
	source  source
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

	ingressList, err := reconciler.Client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Summary{}, fmt.Errorf("list ingresses: %w", err)
	}

	summary := Summary{}
	records := []DNSRecord{}
	readyMarks := []sourceMark{}
	hostOwners := map[string]map[string]struct{}{}

	activeProxies := map[string]struct{}{}
	sources := reconciler.sourcesFor(ctx, config, ingressList.Items)

	for _, source := range sources {
		sourceID := "Ingress/" + source.id()
		for _, host := range source.hosts {
			if hostOwners[host] == nil {
				hostOwners[host] = map[string]struct{}{}
			}
			hostOwners[host][sourceID] = struct{}{}
		}
	}

	for _, source := range sources {
		summary.SourceIngresses++
		sourceID := "Ingress/" + source.id()
		hosts := source.hosts
		if len(hosts) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no hostnames declared")
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
			summary.Skipped = append(summary.Skipped, sourceID+": host "+conflictingHost+" is claimed by another headscale source")
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}
		if source.routeErr != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+source.routeErr.Error())
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}
		if !config.Proxy.Enabled {
			summary.Skipped = append(summary.Skipped, sourceID+": proxy management is disabled")
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}

		var targets []string
		activeProxies[source.activeKey()] = struct{}{}
		spec, err := desiredProxySpec(config, source)
		if err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}
		if err := reconciler.ensureProxyAuthSecret(ctx, source, spec); err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markSource(ctx, source, statusPendingAuth, nil); err != nil {
				return summary, err
			}
			continue
		}
		if err := reconciler.reconcileProxy(ctx, config, source, spec); err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if markErr := reconciler.markSource(ctx, source, statusRejected, nil); markErr != nil {
				return summary, markErr
			}
			continue
		}

		targets, err = reconciler.proxyTargetIPs(ctx, spec)
		if err != nil {
			summary.Skipped = append(summary.Skipped, sourceID+": "+err.Error())
			if err := reconciler.markSource(ctx, source, statusRejected, nil); err != nil {
				return summary, err
			}
			continue
		}
		if len(targets) == 0 {
			summary.Skipped = append(summary.Skipped, sourceID+": no Headscale node IPs resolved for "+spec.tailnetName)
			if err := reconciler.markSource(ctx, source, statusPendingNode, nil); err != nil {
				return summary, err
			}
			continue
		}

		records = append(records, recordsFor(hosts, targets)...)
		readyMarks = append(readyMarks, sourceMark{source: source, status: statusReady, targets: targets})
	}

	records = dedupeRecords(records)
	if err := reconciler.publishRecords(ctx, config, records); err != nil {
		return summary, err
	}
	for _, mark := range readyMarks {
		if err := reconciler.markSource(ctx, mark.source, mark.status, mark.targets); err != nil {
			return summary, err
		}
	}
	if err := reconciler.cleanupInactiveProxies(ctx, activeProxies); err != nil {
		return summary, err
	}
	summary.Records = len(records)
	return summary, nil
}

func (reconciler Reconciler) sourcesFor(ctx context.Context, config Config, ingresses []networkingv1.Ingress) []source {
	sources := []source{}
	for i := range ingresses {
		ingress := ingresses[i]
		if !ingressOptedIn(ingress, config.IngressClassName) {
			continue
		}
		routes, err := reconciler.routesForIngress(ctx, ingress)
		sources = append(sources, source{
			ref: sourceRef{
				apiVersion: networkingv1.SchemeGroupVersion.String(),
				namespace:  ingress.Namespace,
				name:       ingress.Name,
				uid:        ingress.UID,
			},
			hosts:    hostsForIngress(ingress),
			ingress:  &ingress,
			routes:   routes,
			routeErr: err,
		})
	}
	return sources
}

func (reconciler Reconciler) proxyTargetIPs(ctx context.Context, spec proxySpec) ([]string, error) {
	if reconciler.Headscale == nil {
		return nil, fmt.Errorf("managed proxy requires a Headscale client")
	}
	return reconciler.Headscale.NodeIPs(ctx, spec.tailnetName)
}

func (reconciler Reconciler) markSource(ctx context.Context, source source, status string, targets []string) error {
	if err := reconciler.markIngress(ctx, *source.ingress, status, targets); err != nil {
		return err
	}
	return nil
}

func (reconciler Reconciler) markIngress(ctx context.Context, ingress networkingv1.Ingress, status string, targets []string) error {
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
	client := reconciler.Client.NetworkingV1().Ingresses(ingress.Namespace)
	updated, err := client.Patch(ctx, ingress.Name, types.MergePatchType, metadataPatch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("patch ingress annotation %s/%s: %w", ingress.Namespace, ingress.Name, err)
	}
	if status != statusReady || len(targets) == 0 {
		return nil
	}
	statusIngress := ingressLoadBalancerStatus(targets)
	if loadBalancerStatusEqual(updated.Status.LoadBalancer.Ingress, statusIngress) {
		return nil
	}
	statusCopy := updated.DeepCopy()
	statusCopy.Status.LoadBalancer.Ingress = statusIngress
	if _, err := client.UpdateStatus(ctx, statusCopy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ingress status %s/%s: %w", ingress.Namespace, ingress.Name, err)
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

func ingressOptedIn(ingress networkingv1.Ingress, ingressClassName string) bool {
	if ingress.Spec.IngressClassName != nil && strings.TrimSpace(*ingress.Spec.IngressClassName) == ingressClassName {
		return true
	}
	return strings.TrimSpace(ingress.Annotations["kubernetes.io/ingress.class"]) == ingressClassName
}

func hostsForIngress(ingress networkingv1.Ingress) []string {
	hosts := map[string]struct{}{}
	for _, rule := range ingress.Spec.Rules {
		host := normalizeHost(rule.Host)
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

func (reconciler Reconciler) routesForIngress(ctx context.Context, ingress networkingv1.Ingress) ([]proxyRoute, error) {
	var routes []proxyRoute
	for _, rule := range ingress.Spec.Rules {
		host := normalizeHost(rule.Host)
		if host == "" || rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil {
				return nil, fmt.Errorf("Ingress path for host %s does not use a Service backend", host)
			}
			serviceName := strings.TrimSpace(path.Backend.Service.Name)
			if serviceName == "" {
				return nil, fmt.Errorf("Ingress path for host %s has an empty Service backend name", host)
			}
			servicePort, err := reconciler.ingressBackendServicePort(ctx, ingress.Namespace, serviceName, path.Backend.Service.Port)
			if err != nil {
				return nil, err
			}
			pathValue := strings.TrimSpace(path.Path)
			if pathValue == "" {
				pathValue = "/"
			}
			if !strings.HasPrefix(pathValue, "/") {
				return nil, fmt.Errorf("Ingress path for host %s must start with /", host)
			}
			pathType := networkingv1.PathTypePrefix
			if path.PathType != nil {
				pathType = *path.PathType
			}
			routes = append(routes, proxyRoute{
				Host:             host,
				Path:             pathValue,
				PathType:         pathType,
				ServiceName:      serviceName,
				ServiceNamespace: ingress.Namespace,
				ServicePort:      servicePort,
			})
		}
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("Ingress has no routable HTTP Service paths")
	}
	return routes, nil
}

func (reconciler Reconciler) ingressBackendServicePort(ctx context.Context, namespace string, serviceName string, servicePort networkingv1.ServiceBackendPort) (int32, error) {
	if servicePort.Number > 0 {
		return servicePort.Number, nil
	}
	portName := strings.TrimSpace(servicePort.Name)
	if portName == "" {
		return 0, fmt.Errorf("Ingress backend %s/%s must name a Service port or port number", namespace, serviceName)
	}
	service, err := reconciler.Client.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get Ingress backend Service %s/%s: %w", namespace, serviceName, err)
	}
	for _, port := range service.Spec.Ports {
		if port.Name == portName {
			return port.Port, nil
		}
	}
	return 0, fmt.Errorf("Ingress backend Service %s/%s has no port named %s", namespace, serviceName, portName)
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

func ingressLoadBalancerStatus(targets []string) []networkingv1.IngressLoadBalancerIngress {
	status := make([]networkingv1.IngressLoadBalancerIngress, 0, len(targets))
	for _, target := range targets {
		status = append(status, networkingv1.IngressLoadBalancerIngress{IP: target})
	}
	return status
}

func loadBalancerStatusEqual(left, right []networkingv1.IngressLoadBalancerIngress) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].IP != right[i].IP || left[i].Hostname != right[i].Hostname {
			return false
		}
	}
	return true
}
