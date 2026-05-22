package operator

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
