package main

import (
	"github.com/nodal/controlplane/internal/capresolver"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
)

// gateCapabilityResolver is internal/capresolver's Resolver under the name
// this composition root has always used for it. The type moved out of cmd/api
// so that scripts/demodata, which builds the same domain services, reads the
// same source of "which capabilities are ACTIVE" rather than none (F-223).
type gateCapabilityResolver = capresolver.Resolver

// conversionCapabilities is the list the resolver answers about; the tests in
// this package hold it against settlement.AllRequiredCapabilities().
var conversionCapabilities = capresolver.ConversionCapabilities

func newGateCapabilityResolver(checker *gates.Checker, q db.Querier) gateCapabilityResolver {
	return capresolver.New(checker, q)
}
