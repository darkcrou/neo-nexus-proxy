// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"testing"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
)

// scrubProviderEnv blanks every API-key environment variable that NewHandler
// consults through config.DiscoverFromEnv. Ambient keys would silently add
// providers the test never configured; the catalog then seeds them with
// providers.DefaultModels and the router's model-id partition heads the chain
// with them, so the test's requests — and the warm loop's ListModels calls —
// would reach live provider APIs carrying the developer's real credentials.
// Every fixture that goes through NewHandler or New must call this first: those
// constructors are the only path that consults env discovery. The list is derived
// from discovery itself, so a provider family added there is neutralized here too.
func scrubProviderEnv(t *testing.T) {
	t.Helper()
	for _, v := range config.EnvKeyVars() {
		t.Setenv(v, "")
	}
}
