package knowledge

import (
	"encoding/json"
	"sync"

	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

type seedCatalog struct {
	Domains      []StandardDomain
	Wikipedia    map[string][]string
	Tiers        map[string]string
	Roles        map[string][]string
	ComputerUse  []SeedCorpusEntry
	Workbench    []SeedCorpusEntry
	RecentChat   []SeedCorpusEntry
	WorkGuidance []SeedCorpusEntry
	Retired      []struct {
		DomainID  string
		SourceRef string
	}
}

// Read only the core catalog here: pack init registers additional domains, so
// loading/caching the whole automation tree during package initialization would
// freeze it before those packs mount. The catalog runs on the same interpreter
// with no native operations and cannot perform effects.
var shippedSeedCatalog = sync.OnceValue(func() seedCatalog {
	data := seeddata.CatalogJSON()
	var encoded struct {
		Domains   []StandardDomain
		Wikipedia []struct {
			Key   string
			Value []string
		}
		Tiers []struct {
			Key   string
			Value string
		}
		Roles []struct {
			Key   string
			Value []string
		}
		ComputerUse, Workbench, RecentChat, WorkGuidance []SeedCorpusEntry
		Retired                                          []struct {
			DomainID  string
			SourceRef string
		}
	}
	if err := json.Unmarshal(data, &encoded); err != nil {
		panic(err)
	}
	catalog := seedCatalog{Domains: encoded.Domains, Wikipedia: map[string][]string{}, Tiers: map[string]string{}, Roles: map[string][]string{}, ComputerUse: encoded.ComputerUse, Workbench: encoded.Workbench, RecentChat: encoded.RecentChat, WorkGuidance: encoded.WorkGuidance, Retired: encoded.Retired}
	for _, entry := range encoded.Wikipedia {
		catalog.Wikipedia[entry.Key] = entry.Value
	}
	for _, entry := range encoded.Tiers {
		catalog.Tiers[entry.Key] = entry.Value
	}
	for _, entry := range encoded.Roles {
		catalog.Roles[entry.Key] = entry.Value
	}
	return catalog
})
