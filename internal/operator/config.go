package operator

import (
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

type Config struct {
	HeadscaleNamespace   string
	RecordsConfigMapName string
	RecordsConfigMapKey  string
	AllowedZones         []string
	IngressClassName     string
	Proxy                ProxyConfig
}

type ProxyConfig struct {
	Enabled              bool
	HeadscaleServerURL   string
	TailscaleImage       string
	NginxImage           string
	DefaultTLSSecretName string
	HeadscalePodSelector string
	HeadscaleContainer   string
	HeadscaleUser        string
	AuthKeyExpiration    string
	AuthKeyTags          []string
}

func (config Config) Validate() error {
	_, err := config.validated()
	return err
}

func (config Config) withDefaults() Config {
	config.HeadscaleNamespace = strings.TrimSpace(config.HeadscaleNamespace)
	config.RecordsConfigMapName = strings.TrimSpace(config.RecordsConfigMapName)
	config.RecordsConfigMapKey = strings.TrimSpace(config.RecordsConfigMapKey)
	config.IngressClassName = strings.TrimSpace(config.IngressClassName)
	config.Proxy.HeadscaleServerURL = strings.TrimSpace(config.Proxy.HeadscaleServerURL)
	config.Proxy.TailscaleImage = strings.TrimSpace(config.Proxy.TailscaleImage)
	config.Proxy.NginxImage = strings.TrimSpace(config.Proxy.NginxImage)
	config.Proxy.DefaultTLSSecretName = strings.TrimSpace(config.Proxy.DefaultTLSSecretName)
	config.Proxy.HeadscalePodSelector = strings.TrimSpace(config.Proxy.HeadscalePodSelector)
	config.Proxy.HeadscaleContainer = strings.TrimSpace(config.Proxy.HeadscaleContainer)
	config.Proxy.HeadscaleUser = strings.TrimSpace(config.Proxy.HeadscaleUser)
	config.Proxy.AuthKeyExpiration = strings.TrimSpace(config.Proxy.AuthKeyExpiration)

	if config.HeadscaleNamespace == "" {
		config.HeadscaleNamespace = "headscale"
	}
	if config.RecordsConfigMapName == "" {
		config.RecordsConfigMapName = "headscale-extra-records"
	}
	if config.RecordsConfigMapKey == "" {
		config.RecordsConfigMapKey = "extra-records.json"
	}
	if config.IngressClassName == "" {
		config.IngressClassName = "headscale"
	}
	if config.Proxy.TailscaleImage == "" {
		config.Proxy.TailscaleImage = "tailscale/tailscale:stable"
	}
	if config.Proxy.NginxImage == "" {
		config.Proxy.NginxImage = "nginx:1.27-alpine"
	}
	if config.Proxy.HeadscalePodSelector == "" {
		config.Proxy.HeadscalePodSelector = "app.kubernetes.io/name=headscale"
	}
	if config.Proxy.HeadscaleContainer == "" {
		config.Proxy.HeadscaleContainer = "headscale"
	}
	if config.Proxy.HeadscaleUser == "" {
		config.Proxy.HeadscaleUser = "k8s-apps"
	}
	if config.Proxy.AuthKeyExpiration == "" {
		config.Proxy.AuthKeyExpiration = "90d"
	}
	if len(config.Proxy.AuthKeyTags) == 0 {
		config.Proxy.AuthKeyTags = []string{"tag:cluster"}
	}
	return config
}

func (config Config) validated() (Config, error) {
	config = config.withDefaults()
	if err := validateDNS1123Label("headscale namespace", config.HeadscaleNamespace); err != nil {
		return Config{}, err
	}
	if err := validateDNS1123Subdomain("records ConfigMap name", config.RecordsConfigMapName); err != nil {
		return Config{}, err
	}
	if errs := validation.IsConfigMapKey(config.RecordsConfigMapKey); len(errs) > 0 {
		return Config{}, fmt.Errorf("records ConfigMap key %q is invalid: %s", config.RecordsConfigMapKey, strings.Join(errs, "; "))
	}
	if err := validateDNS1123Subdomain("ingress class name", config.IngressClassName); err != nil {
		return Config{}, err
	}
	if config.Proxy.Enabled {
		if config.Proxy.HeadscaleServerURL == "" {
			return Config{}, fmt.Errorf("proxy Headscale server URL is required when proxy management is enabled")
		}
		authKeyTags, err := normalizeAuthKeyTags(config.Proxy.AuthKeyTags)
		if err != nil {
			return Config{}, fmt.Errorf("proxy auth key tags: %w", err)
		}
		config.Proxy.AuthKeyTags = authKeyTags
		if config.Proxy.DefaultTLSSecretName != "" {
			if err := validateDNS1123Subdomain("proxy default TLS Secret name", config.Proxy.DefaultTLSSecretName); err != nil {
				return Config{}, err
			}
		}
	}

	zones, err := normalizeZones(config.AllowedZones)
	if err != nil {
		return Config{}, fmt.Errorf("allowed zones: %w", err)
	}
	config.AllowedZones = zones
	return config, nil
}

func validateDNS1123Subdomain(field string, value string) error {
	if errs := validation.IsDNS1123Subdomain(value); len(errs) > 0 {
		return fmt.Errorf("%s %q is invalid: %s", field, value, strings.Join(errs, "; "))
	}
	return nil
}

func validateDNS1123Label(field string, value string) error {
	if errs := validation.IsDNS1123Label(value); len(errs) > 0 {
		return fmt.Errorf("%s %q is invalid: %s", field, value, strings.Join(errs, "; "))
	}
	return nil
}

func normalizeZones(zones []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(zones))
	for _, zone := range zones {
		raw := zone
		zone = normalizeHost(zone)
		if zone == "" {
			continue
		}
		if errs := validation.IsDNS1123Subdomain(zone); len(errs) > 0 {
			return nil, fmt.Errorf("%q is not a valid DNS zone: %s", raw, strings.Join(errs, "; "))
		}
		if _, ok := seen[zone]; ok {
			continue
		}
		seen[zone] = struct{}{}
		out = append(out, zone)
	}
	return out, nil
}

func normalizeIPs(values []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		address, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address", value)
		}
		if !usableAddress(address) {
			return nil, fmt.Errorf("%q is not a usable DNS target", value)
		}
		value = address.String()
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func usableAddress(address netip.Addr) bool {
	return !address.IsUnspecified() && !address.IsMulticast()
}
