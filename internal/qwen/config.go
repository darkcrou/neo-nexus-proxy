// SPDX-License-Identifier: Apache-2.0

package qwen

import (
	"fmt"
	"strings"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
)

// EnableProvider adds the subscription entry after a successful sign-in,
// preserving an existing valid entry and its operator-configured settings.
func EnableProvider(configPath string) error {
	return config.Update(configPath, func(cfg *config.Config) (bool, error) {
		nameInUse := false
		for _, p := range cfg.Providers {
			if strings.EqualFold(p.Type, ProviderType) {
				if p.APIKey != "" || len(p.APIKeys) > 0 || p.BaseURL != "" {
					return false, fmt.Errorf("remove api_key, api_keys and base_url from the %s provider entry", ProviderType)
				}
				return false, nil
			}
			nameInUse = nameInUse || strings.EqualFold(p.Name, ProviderType)
		}
		if nameInUse {
			return false, fmt.Errorf("provider name %q is in use; rename that provider or set its type to %q", ProviderType, ProviderType)
		}
		cfg.Providers = append(cfg.Providers, config.Provider{Name: ProviderType, Type: ProviderType})
		return true, nil
	})
}
