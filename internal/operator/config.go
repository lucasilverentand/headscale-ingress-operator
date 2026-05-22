package operator

import (
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

type Config struct {
	SourceClassName                string
	ImplementationIngressClassName string
	ImplementationNameSuffix       string
	HeadscaleNamespace             string
	RecordsConfigMapName           string
	RecordsConfigMapKey            string
	AllowedZones                   []string
	DefaultTargetIPs               []string
	StaticRecords                  []DNSRecord
}

func (config Config) withDefaults() Config {
	config.SourceClassName = strings.TrimSpace(config.SourceClassName)
	config.ImplementationIngressClassName = strings.TrimSpace(config.ImplementationIngressClassName)
	config.ImplementationNameSuffix = strings.TrimSpace(config.ImplementationNameSuffix)
	config.HeadscaleNamespace = strings.TrimSpace(config.HeadscaleNamespace)
	config.RecordsConfigMapName = strings.TrimSpace(config.RecordsConfigMapName)
	config.RecordsConfigMapKey = strings.TrimSpace(config.RecordsConfigMapKey)

	if config.SourceClassName == "" {
		config.SourceClassName = "headscale"
	}
	if config.ImplementationIngressClassName == "" {
		config.ImplementationIngressClassName = "traefik-internal"
	}
	if config.ImplementationNameSuffix == "" {
		config.ImplementationNameSuffix = "-headscale"
	}
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
	if config.SourceClassName == config.ImplementationIngressClassName {
		return Config{}, fmt.Errorf("source and implementation ingress classes must differ")
	}

	zones, err := normalizeZones(config.AllowedZones)
	if err != nil {
		return Config{}, fmt.Errorf("allowed zones: %w", err)
	}
	config.AllowedZones = zones
	targets, err := normalizeIPs(config.DefaultTargetIPs)
	if err != nil {
		return Config{}, fmt.Errorf("default target IPs: %w", err)
	}
	config.DefaultTargetIPs = targets

	staticRecords := make([]DNSRecord, 0, len(config.StaticRecords))
	for _, record := range config.StaticRecords {
		normalized, ok := normalizeRecord(record)
		if !ok {
			return Config{}, fmt.Errorf("invalid static record %q %q %q", record.Name, record.Type, record.Value)
		}
		if !hostAllowed(normalized.Name, config.AllowedZones) {
			return Config{}, fmt.Errorf("static record %q is outside allowed zones", normalized.Name)
		}
		staticRecords = append(staticRecords, normalized)
	}
	config.StaticRecords = staticRecords
	return config, nil
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
		value = address.String()
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}
