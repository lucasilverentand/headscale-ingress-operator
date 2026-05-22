package operator

import (
	"encoding/json"
	"net/netip"
	"sort"
	"strings"
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
			recordType := "A"
			address, err := netip.ParseAddr(target)
			if err != nil {
				continue
			}
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
		record.Name = normalizeHost(record.Name)
		if record.Name == "" || record.Value == "" {
			continue
		}
		key := record.Name + "\x00" + record.Type + "\x00" + record.Value
		seen[key] = record
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
