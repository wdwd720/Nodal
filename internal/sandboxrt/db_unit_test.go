//go:build !integration

package sandboxrt

// setupDatabase is a no-op without the integration build tag.
func setupDatabase() (func(), error) { return func() {}, nil }
