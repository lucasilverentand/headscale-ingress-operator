package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lucasilverentand/headscale-ingress-operator/internal/operator"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var kubeconfig string
	var interval time.Duration
	var sourceClass string
	var implementationClass string
	var implementationSuffix string
	var headscaleNamespace string
	var recordsConfigMap string
	var recordsKey string
	var allowedZones string
	var defaultTargets string

	flag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig. Defaults to in-cluster config when empty.")
	flag.DurationVar(&interval, "interval", 30*time.Second, "Reconcile interval.")
	flag.StringVar(&sourceClass, "source-class", "headscale", "IngressClass handled by this operator.")
	flag.StringVar(&implementationClass, "implementation-class", "traefik-internal", "IngressClass used for generated implementation Ingresses.")
	flag.StringVar(&implementationSuffix, "implementation-suffix", "-headscale", "Suffix for generated implementation Ingress names.")
	flag.StringVar(&headscaleNamespace, "headscale-namespace", "headscale", "Namespace containing the Headscale records ConfigMap.")
	flag.StringVar(&recordsConfigMap, "records-configmap", "headscale-extra-records", "Name of the Headscale records ConfigMap.")
	flag.StringVar(&recordsKey, "records-key", "extra-records.json", "ConfigMap key containing Headscale extra_records_path JSON.")
	flag.StringVar(&allowedZones, "allowed-zones", "", "Comma-separated DNS zones this operator may publish. Empty allows all hosts.")
	flag.StringVar(&defaultTargets, "default-targets", "", "Comma-separated fallback A/AAAA targets when generated Ingress status is empty.")
	flag.Parse()

	if interval <= 0 {
		slog.Error("reconcile interval must be positive", "interval", interval)
		os.Exit(1)
	}

	operatorConfig := operator.Config{
		SourceClassName:                sourceClass,
		ImplementationIngressClassName: implementationClass,
		ImplementationNameSuffix:       implementationSuffix,
		HeadscaleNamespace:             headscaleNamespace,
		RecordsConfigMapName:           recordsConfigMap,
		RecordsConfigMapKey:            recordsKey,
		AllowedZones:                   splitCSV(allowedZones),
		DefaultTargetIPs:               splitCSV(defaultTargets),
	}
	if err := operatorConfig.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(kubernetesConfig(kubeconfig))
	if err != nil {
		slog.Error("create Kubernetes client", "error", err)
		os.Exit(1)
	}

	reconciler := operator.Reconciler{
		Client: client,
		Config: operatorConfig,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		summary, err := reconciler.Reconcile(ctx)
		if err != nil {
			slog.Error("reconcile failed", "error", err)
		} else {
			slog.Info("reconciled",
				"sources", summary.SourceIngresses,
				"implementations", summary.ImplementationIngresses,
				"deletedImplementations", summary.DeletedImplementations,
				"records", summary.Records,
				"skipped", summary.Skipped,
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func kubernetesConfig(kubeconfig string) *rest.Config {
	if kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			slog.Error("load kubeconfig", "path", kubeconfig, "error", err)
			os.Exit(1)
		}
		configureJSON(config)
		return config
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			slog.Error("not running in cluster and no kubeconfig was provided")
		} else {
			slog.Error("load in-cluster config", "error", err)
		}
		os.Exit(1)
	}
	configureJSON(config)
	return config
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}

	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func configureJSON(config *rest.Config) {
	config.ContentConfig.ContentType = "application/json"
	config.ContentConfig.AcceptContentTypes = "application/json"
}
