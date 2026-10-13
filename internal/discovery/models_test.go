package discovery_test

import (
	"testing"

	"cnm/internal/config"
	"cnm/internal/discovery"
	"cnm/internal/provider"

	_ "cnm/internal/provider/axis"
	_ "cnm/internal/provider/dahua"
	_ "cnm/internal/provider/generic"
	_ "cnm/internal/provider/hikvision"
	_ "cnm/internal/provider/sofia"
	_ "cnm/internal/provider/uniview"
)

// Every suggestion the scanner can produce must name an existing protocol
// and only ports/options that protocol knows.
func TestSuggestionsResolve(t *testing.T) {
	for _, s := range discovery.AllSuggestions() {
		_, m, err := provider.Resolve(s.Protocol)
		if err != nil {
			t.Fatalf("%q: %v", s.Protocol, err)
		}
		known := map[string]bool{}
		for _, ps := range m.Ports {
			known[ps.Key] = true
		}
		for k := range s.Ports {
			if !known[k] {
				t.Fatalf("%s: unknown port key %q", m.ID, k)
			}
		}
		for k := range s.Options {
			found := false
			for _, o := range m.Options {
				found = found || o.Key == k
			}
			if !found {
				t.Fatalf("%s: unknown option %q", m.ID, k)
			}
		}
	}
}

// The protocol ids the configuration (and its migration) uses exist.
func TestConfigProtocolsRegistered(t *testing.T) {
	for _, id := range []string{config.ProtoDahua, config.ProtoSofia, config.ProtoHikvision, config.ProtoAxis, config.ProtoUniview,
		config.ProtoONVIF, config.ProtoRTSP, config.ProtoRTMP, config.ProtoHTTP, config.ProtoTCP} {
		if _, _, err := provider.Resolve(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if n := len(provider.Protocols()); n != 10 {
		t.Fatalf("%d protocols in the device dialog", n)
	}
}
