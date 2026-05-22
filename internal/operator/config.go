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
}

func (config Config) Validate() error {
	_, err := config.validated()
	return err
}

func (config Config) withDefaults() Config {
	config.HeadscaleNamespace = strings.TrimSpace(config.HeadscaleNamespace)
	config.RecordsConfigMapName = strings.TrimSpace(config.RecordsConfigMapName)
	config.RecordsConfigMapKey = strings.TrimSpace(config.RecordsConfigMapKey)

	if config.HeadscaleNamespace == "" {
		config.HeadscaleNamespace = "headscale"
	}
	if config.RecordsConfigMapName == "" {
		config.RecordsConfigMapName = "headscale-extra-records"
	}
	if config.RecordsConfigMapKey == "" {
		config.RecordsConfigMapKey = "extra-records.json"
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
