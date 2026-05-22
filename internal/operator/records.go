package operator

import (
	"encoding/json"
	"net/netip"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

type DNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

func recordsFor(hosts []string, targets []string) []DNSRecord {
	records := make([]DNSRecord, 0, len(hosts)*len(targets))
	for _, host := range hosts {
		for _, target := range targets {
			address, err := netip.ParseAddr(target)
			if err != nil {
				continue
			}
			recordType := "A"
			if address.Is6() {
				recordType = "AAAA"
			}
			records = append(records, DNSRecord{
				Name:  normalizeHost(host),
				Type:  recordType,
				Value: target,
			})
		}
	}
	return records
}

func stableRecordsJSON(records []DNSRecord) (string, error) {
	records = dedupeRecords(records)
	payload, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return "", err
	}
	return string(payload) + "\n", nil
}

func dedupeRecords(records []DNSRecord) []DNSRecord {
	seen := map[string]DNSRecord{}
	for _, record := range records {
		normalized, ok := normalizeRecord(record)
		if !ok {
			continue
		}
		key := normalized.Name + "\x00" + normalized.Type + "\x00" + normalized.Value
		seen[key] = normalized
	}

	deduped := make([]DNSRecord, 0, len(seen))
	for _, record := range seen {
		deduped = append(deduped, record)
	}
	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].Name != deduped[j].Name {
			return deduped[i].Name < deduped[j].Name
		}
		if deduped[i].Type != deduped[j].Type {
			return deduped[i].Type < deduped[j].Type
		}
		return deduped[i].Value < deduped[j].Value
	})
	return deduped
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func normalizeRecord(record DNSRecord) (DNSRecord, bool) {
	name := normalizeHost(record.Name)
	if name == "" || strings.Contains(name, "*") {
		return DNSRecord{}, false
	}
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		return DNSRecord{}, false
	}

	address, err := netip.ParseAddr(strings.TrimSpace(record.Value))
	if err != nil {
		return DNSRecord{}, false
	}

	recordType := strings.ToUpper(strings.TrimSpace(record.Type))
	switch recordType {
	case "A":
		if !address.Is4() {
			return DNSRecord{}, false
		}
	case "AAAA":
		if !address.Is6() {
			return DNSRecord{}, false
		}
	default:
		return DNSRecord{}, false
	}

	return DNSRecord{
		Name:  name,
		Type:  recordType,
		Value: address.String(),
	}, true
}
