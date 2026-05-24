package operator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	proxyTLSSecretAnnotation   = "headscale-ingress-operator.lucasilverentand.dev/proxy-tls-secret"
	proxyAuthSecretAnnotation  = "headscale-ingress-operator.lucasilverentand.dev/proxy-auth-secret"
	proxyStateSecretAnnotation = "headscale-ingress-operator.lucasilverentand.dev/proxy-state-secret"
	proxyTailnetNameAnnotation = "headscale-ingress-operator.lucasilverentand.dev/proxy-tailnet-name"
	proxyComponentLabel        = "headscale-ingress-operator.lucasilverentand.dev/component"
	proxySourceNamespaceLabel  = "headscale-ingress-operator.lucasilverentand.dev/source-namespace"
	proxySourceNameLabel       = "headscale-ingress-operator.lucasilverentand.dev/source-name"
	proxySourceKindLabel       = "headscale-ingress-operator.lucasilverentand.dev/source-kind"
	proxyComponentValue        = "tailnet-proxy"
)

type proxyRoute struct {
	Host             string
	Path             string
	PathType         networkingv1.PathType
	ServiceName      string
	ServiceNamespace string
	ServicePort      int32
}

type proxySpec struct {
	name            string
	tlsSecretName   string
	authSecretName  string
	stateSecret     string
	tailnetName     string
	servicePort     int32
	hosts           []string
	routes          []proxyRoute
	sourceName      string
	sourceNamespace string
}

func (reconciler Reconciler) reconcileProxy(ctx context.Context, config Config, source source, spec proxySpec) error {
	if !config.Proxy.Enabled {
		return nil
	}

	owner := source.ownerReference()
	if err := reconciler.upsertServiceAccount(ctx, source.ref.namespace, desiredProxyServiceAccount(spec, owner)); err != nil {
		return err
	}
	if err := reconciler.upsertRole(ctx, source.ref.namespace, desiredProxyRole(spec, owner)); err != nil {
		return err
	}
	if err := reconciler.upsertRoleBinding(ctx, source.ref.namespace, desiredProxyRoleBinding(spec, owner)); err != nil {
		return err
	}
	if err := reconciler.upsertConfigMap(ctx, source.ref.namespace, desiredProxyConfigMap(spec, owner)); err != nil {
		return err
	}
	if err := reconciler.upsertDeployment(ctx, source.ref.namespace, desiredProxyDeployment(config, spec, owner)); err != nil {
		return err
	}
	return nil
}

func (reconciler Reconciler) ensureProxyAuthSecret(ctx context.Context, source source, spec proxySpec) error {
	owner := source.ownerReference()
	client := reconciler.Client.CoreV1().Secrets(source.ref.namespace)
	existing, err := client.Get(ctx, spec.authSecretName, metav1.GetOptions{})
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return wrapProxyError("get proxy auth secret", source.ref.namespace, spec.authSecretName, err)
	}
	if err == nil && len(existing.Data["TS_AUTHKEY"]) > 0 {
		copy := existing.DeepCopy()
		copy.Labels = proxyLabelsForSpec(spec)
		copy.OwnerReferences = []metav1.OwnerReference{owner}
		if copy.Type == "" {
			copy.Type = corev1.SecretTypeOpaque
		}
		_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
		return wrapProxyError("adopt proxy auth secret", source.ref.namespace, spec.authSecretName, err)
	}
	if reconciler.Headscale == nil {
		return fmt.Errorf("managed proxy requires a Headscale client to mint %s", spec.authSecretName)
	}
	key, err := reconciler.Headscale.MintReusableAuthKey(ctx)
	if err != nil {
		return fmt.Errorf("mint proxy auth key: %w", err)
	}
	desired := &corev1.Secret{
		ObjectMeta: withOwner(metav1.ObjectMeta{
			Name:   spec.authSecretName,
			Labels: proxyLabelsForSpec(spec),
		}, owner),
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"TS_AUTHKEY": []byte(key)},
	}
	if missing {
		desired.Namespace = source.ref.namespace
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return wrapProxyError("create proxy auth secret", source.ref.namespace, spec.authSecretName, err)
	}
	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.OwnerReferences = desired.OwnerReferences
	copy.Type = desired.Type
	copy.Data = desired.Data
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return wrapProxyError("update proxy auth secret", source.ref.namespace, spec.authSecretName, err)
}

func desiredProxySpec(config Config, source source) (proxySpec, error) {
	spec := proxySpec{
		name:            proxyNameFor(source.ref.name),
		tlsSecretName:   sourceAnnotationOrDefault(source, proxyTLSSecretAnnotation, config.Proxy.DefaultTLSSecretName),
		authSecretName:  sourceAnnotationOrDefault(source, proxyAuthSecretAnnotation, source.ref.name+"-tailnet-authkey"),
		stateSecret:     sourceAnnotationOrDefault(source, proxyStateSecretAnnotation, "tailscale-"+source.ref.name),
		tailnetName:     sourceAnnotationOrDefault(source, proxyTailnetNameAnnotation, source.ref.name),
		hosts:           append([]string(nil), source.hosts...),
		routes:          append([]proxyRoute(nil), source.routes...),
		sourceName:      source.ref.name,
		sourceNamespace: source.ref.namespace,
	}
	if spec.tlsSecretName == config.Proxy.DefaultTLSSecretName && source.ingress != nil {
		if tlsSecret := ingressTLSSecretName(*source.ingress); tlsSecret != "" {
			spec.tlsSecretName = tlsSecret
		}
	}
	if spec.tlsSecretName == "" {
		return proxySpec{}, fmt.Errorf("managed proxy requires %s, Ingress TLS, or a chart default TLS Secret", proxyTLSSecretAnnotation)
	}
	if len(spec.routes) == 0 {
		return proxySpec{}, fmt.Errorf("managed proxy requires at least one backend route")
	}
	spec.servicePort = spec.routes[0].ServicePort
	return spec, nil
}

func sourceAnnotationOrDefault(source source, annotation string, fallback string) string {
	value := strings.TrimSpace(sourceAnnotations(source)[annotation])
	if value != "" {
		return value
	}
	return fallback
}

func sourceAnnotations(source source) map[string]string {
	if source.ingress != nil {
		return source.ingress.Annotations
	}
	return nil
}

func ingressTLSSecretName(ingress networkingv1.Ingress) string {
	for _, tls := range ingress.Spec.TLS {
		if strings.TrimSpace(tls.SecretName) != "" {
			return strings.TrimSpace(tls.SecretName)
		}
	}
	return ""
}

func proxyNameFor(serviceName string) string {
	name := serviceName + "-tailnet-sidecar"
	if len(name) <= 63 {
		return name
	}
	sum := sha1.Sum([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return strings.TrimSuffix(name[:63-len(suffix)], "-") + suffix
}

func proxyLabels(serviceName string) map[string]string {
	return map[string]string{
		managedByLabel:           managedByValue,
		proxyComponentLabel:      proxyComponentValue,
		proxySourceNameLabel:     serviceName,
		"app.kubernetes.io/name": proxyNameFor(serviceName),
	}
}

func proxyLabelsForSpec(spec proxySpec) map[string]string {
	labels := proxyLabels(spec.sourceName)
	labels[proxySourceNamespaceLabel] = spec.sourceNamespace
	labels[proxySourceKindLabel] = "Ingress"
	return labels
}

func withOwner(meta metav1.ObjectMeta, owner metav1.OwnerReference) metav1.ObjectMeta {
	meta.OwnerReferences = []metav1.OwnerReference{owner}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[managedByLabel] = managedByValue
	meta.Labels[proxyComponentLabel] = proxyComponentValue
	return meta
}

func desiredProxyServiceAccount(spec proxySpec, owner metav1.OwnerReference) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: withOwner(metav1.ObjectMeta{
			Name:   spec.name,
			Labels: proxyLabelsForSpec(spec),
		}, owner),
	}
}

func desiredProxyRole(spec proxySpec, owner metav1.OwnerReference) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: withOwner(metav1.ObjectMeta{
			Name:   spec.name,
			Labels: proxyLabelsForSpec(spec),
		}, owner),
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{spec.stateSecret},
				Verbs:         []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"get", "create", "patch"},
			},
		},
	}
}

func desiredProxyRoleBinding(spec proxySpec, owner metav1.OwnerReference) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: withOwner(metav1.ObjectMeta{
			Name:   spec.name,
			Labels: proxyLabelsForSpec(spec),
		}, owner),
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     spec.name,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount",
			Name: spec.name,
		}},
	}
}

func desiredProxyConfigMap(spec proxySpec, owner metav1.OwnerReference) *corev1.ConfigMap {
	hosts := append([]string(nil), spec.hosts...)
	sort.Strings(hosts)
	return &corev1.ConfigMap{
		ObjectMeta: withOwner(metav1.ObjectMeta{
			Name:   spec.name + "-nginx",
			Labels: proxyLabelsForSpec(spec),
		}, owner),
		Data: map[string]string{
			"nginx.conf": fmt.Sprintf(`worker_processes 1;
error_log /dev/stderr info;
events { worker_connections 1024; }

http {
  access_log /dev/stdout;
  sendfile on;

  proxy_read_timeout 1h;
  proxy_send_timeout 1h;
  proxy_http_version 1.1;
  proxy_set_header Upgrade $http_upgrade;
  proxy_set_header Connection "upgrade";

  proxy_set_header Host $host;
  proxy_set_header X-Real-IP $remote_addr;
  proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
  proxy_set_header X-Forwarded-Proto https;

%s
}
`, nginxServerBlocks(hosts, spec.routes)),
		},
	}
}

func nginxServerBlocks(hosts []string, routes []proxyRoute) string {
	var builder strings.Builder
	for _, host := range hosts {
		builder.WriteString("  server {\n")
		builder.WriteString("    listen 127.0.0.1:443 ssl;\n")
		builder.WriteString("    http2 on;\n")
		builder.WriteString("    server_name " + host + ";\n\n")
		builder.WriteString("    ssl_certificate     /etc/tls/tls.crt;\n")
		builder.WriteString("    ssl_certificate_key /etc/tls/tls.key;\n")
		builder.WriteString("    ssl_protocols       TLSv1.2 TLSv1.3;\n\n")
		for _, route := range routesForHost(host, routes) {
			builder.WriteString("    ")
			builder.WriteString(nginxLocation(route))
			builder.WriteString(" {\n")
			builder.WriteString(fmt.Sprintf("      proxy_pass http://%s.%s.svc.cluster.local:%d;\n", route.ServiceName, route.ServiceNamespace, route.ServicePort))
			builder.WriteString("    }\n")
		}
		builder.WriteString("  }\n")
	}
	return builder.String()
}

func routesForHost(host string, routes []proxyRoute) []proxyRoute {
	var out []proxyRoute
	for _, route := range routes {
		if route.Host == "" || route.Host == host {
			out = append(out, route)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PathType != out[j].PathType {
			return out[i].PathType == networkingv1.PathTypeExact
		}
		return len(out[i].Path) > len(out[j].Path)
	})
	return out
}

func nginxLocation(route proxyRoute) string {
	path := route.Path
	if path == "" {
		path = "/"
	}
	if route.PathType == networkingv1.PathTypeExact {
		return "location = " + strconv.Quote(path)
	}
	return "location " + strconv.Quote(path)
}

func desiredProxyDeployment(config Config, spec proxySpec, owner metav1.OwnerReference) *appsv1.Deployment {
	replicas := int32(1)
	falseValue := false
	labels := proxyLabelsForSpec(spec)
	return &appsv1.Deployment{
		ObjectMeta: withOwner(metav1.ObjectMeta{
			Name:   spec.name,
			Labels: labels,
		}, owner),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name": spec.name,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: spec.name,
					Containers: []corev1.Container{
						{
							Name:    "tailscale",
							Image:   config.Proxy.TailscaleImage,
							Command: []string{"/bin/sh", "-c"},
							Args: []string{`set -e

/usr/local/bin/containerboot &
BOOT_PID=$!

until /usr/local/bin/tailscale status > /dev/null 2>&1; do
  sleep 1
done

/usr/local/bin/tailscale serve \
  --bg --tcp 443 \
  "tcp://127.0.0.1:443"

wait $BOOT_PID
`},
							Env: []corev1.EnvVar{
								{Name: "TS_USERSPACE", Value: "true"},
								{Name: "TS_HOSTNAME", Value: spec.tailnetName},
								{Name: "TS_ACCEPT_DNS", Value: "false"},
								{Name: "TS_AUTH_ONCE", Value: "true"},
								{Name: "TS_KUBE_SECRET", Value: spec.stateSecret},
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
									},
								},
								{
									Name: "POD_UID",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
									},
								},
								{Name: "TS_EXTRA_ARGS", Value: "--login-server=" + config.Proxy.HeadscaleServerURL},
								{
									Name: "TS_AUTHKEY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: spec.authSecretName},
											Key:                  "TS_AUTHKEY",
											Optional:             &falseValue,
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "state", MountPath: "/var/lib/tailscale"},
								{Name: "tmp", MountPath: "/tmp"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"/usr/local/bin/tailscale", "status"}},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       15,
							},
						},
						{
							Name:  "nginx",
							Image: config.Proxy.NginxImage,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "nginx-config", MountPath: "/etc/nginx/nginx.conf", SubPath: "nginx.conf", ReadOnly: true},
								{Name: "tls", MountPath: "/etc/tls", ReadOnly: true},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "nc -z 127.0.0.1 443"}},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "nginx-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: spec.name + "-nginx"},
						}}},
						{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: spec.tlsSecretName,
							Optional:   &falseValue,
						}}},
					},
				},
			},
		},
	}
}

func (reconciler Reconciler) upsertServiceAccount(ctx context.Context, namespace string, desired *corev1.ServiceAccount) error {
	client := reconciler.Client.CoreV1().ServiceAccounts(namespace)
	existing, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		desired.Namespace = namespace
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return wrapProxyError("create service account", namespace, desired.Name, err)
	}
	if err != nil {
		return wrapProxyError("get service account", namespace, desired.Name, err)
	}
	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.OwnerReferences = desired.OwnerReferences
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return wrapProxyError("update service account", namespace, desired.Name, err)
}

func (reconciler Reconciler) upsertRole(ctx context.Context, namespace string, desired *rbacv1.Role) error {
	client := reconciler.Client.RbacV1().Roles(namespace)
	existing, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		desired.Namespace = namespace
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return wrapProxyError("create role", namespace, desired.Name, err)
	}
	if err != nil {
		return wrapProxyError("get role", namespace, desired.Name, err)
	}
	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.OwnerReferences = desired.OwnerReferences
	copy.Rules = desired.Rules
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return wrapProxyError("update role", namespace, desired.Name, err)
}

func (reconciler Reconciler) upsertRoleBinding(ctx context.Context, namespace string, desired *rbacv1.RoleBinding) error {
	client := reconciler.Client.RbacV1().RoleBindings(namespace)
	existing, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		desired.Namespace = namespace
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return wrapProxyError("create role binding", namespace, desired.Name, err)
	}
	if err != nil {
		return wrapProxyError("get role binding", namespace, desired.Name, err)
	}
	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.OwnerReferences = desired.OwnerReferences
	copy.RoleRef = desired.RoleRef
	copy.Subjects = desired.Subjects
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return wrapProxyError("update role binding", namespace, desired.Name, err)
}

func (reconciler Reconciler) upsertConfigMap(ctx context.Context, namespace string, desired *corev1.ConfigMap) error {
	client := reconciler.Client.CoreV1().ConfigMaps(namespace)
	existing, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		desired.Namespace = namespace
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return wrapProxyError("create proxy configmap", namespace, desired.Name, err)
	}
	if err != nil {
		return wrapProxyError("get proxy configmap", namespace, desired.Name, err)
	}
	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.OwnerReferences = desired.OwnerReferences
	copy.Data = desired.Data
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return wrapProxyError("update proxy configmap", namespace, desired.Name, err)
}

func (reconciler Reconciler) upsertDeployment(ctx context.Context, namespace string, desired *appsv1.Deployment) error {
	client := reconciler.Client.AppsV1().Deployments(namespace)
	existing, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		desired.Namespace = namespace
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return wrapProxyError("create proxy deployment", namespace, desired.Name, err)
	}
	if err != nil {
		return wrapProxyError("get proxy deployment", namespace, desired.Name, err)
	}
	copy := existing.DeepCopy()
	copy.Labels = desired.Labels
	copy.OwnerReferences = desired.OwnerReferences
	copy.Spec = desired.Spec
	_, err = client.Update(ctx, copy, metav1.UpdateOptions{})
	return wrapProxyError("update proxy deployment", namespace, desired.Name, err)
}

func (reconciler Reconciler) cleanupInactiveProxies(ctx context.Context, active map[string]struct{}) error {
	selector := proxyComponentLabel + "=" + proxyComponentValue
	deployments, err := reconciler.Client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list managed proxy deployments: %w", err)
	}
	for _, deployment := range deployments.Items {
		sourceName := deployment.Labels[proxySourceNameLabel]
		if sourceName == "" {
			continue
		}
		if _, ok := active[deployment.Namespace+"/"+sourceName]; ok {
			continue
		}
		if err := reconciler.deleteProxyResourceSet(ctx, deployment.Namespace, deployment.Name); err != nil {
			return err
		}
	}

	secrets, err := reconciler.Client.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list managed proxy auth secrets: %w", err)
	}
	for _, secret := range secrets.Items {
		sourceName := secret.Labels[proxySourceNameLabel]
		if sourceName == "" {
			continue
		}
		if _, ok := active[secret.Namespace+"/"+sourceName]; ok {
			continue
		}
		if err := reconciler.deleteIfManagedSecret(ctx, secret.Namespace, secret.Name); err != nil {
			return err
		}
	}
	return nil
}

func (reconciler Reconciler) deleteProxyResourceSet(ctx context.Context, namespace string, name string) error {
	if err := ignoreNotFound(reconciler.Client.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return wrapProxyError("delete proxy deployment", namespace, name, err)
	}
	if err := ignoreNotFound(reconciler.Client.CoreV1().ConfigMaps(namespace).Delete(ctx, name+"-nginx", metav1.DeleteOptions{})); err != nil {
		return wrapProxyError("delete proxy configmap", namespace, name+"-nginx", err)
	}
	if err := ignoreNotFound(reconciler.Client.RbacV1().RoleBindings(namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return wrapProxyError("delete proxy role binding", namespace, name, err)
	}
	if err := ignoreNotFound(reconciler.Client.RbacV1().Roles(namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return wrapProxyError("delete proxy role", namespace, name, err)
	}
	if err := ignoreNotFound(reconciler.Client.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return wrapProxyError("delete proxy service account", namespace, name, err)
	}
	return nil
}

func (reconciler Reconciler) deleteIfManagedSecret(ctx context.Context, namespace string, name string) error {
	secret, err := reconciler.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return wrapProxyError("get proxy auth secret", namespace, name, err)
	}
	if secret.Labels[managedByLabel] != managedByValue || secret.Labels[proxyComponentLabel] != proxyComponentValue {
		return nil
	}
	return wrapProxyError("delete proxy auth secret", namespace, name, ignoreNotFound(reconciler.Client.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})))
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func wrapProxyError(action string, namespace string, name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s/%s: %w", action, namespace, name, err)
}
