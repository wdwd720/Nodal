// Package deployed runs against a deployment that is actually running,
// reached over the public internet the way a provider reaches it.
//
// It is not a substitute for the integration tests. Those prove the code is
// right against a database in a container; these prove that THIS deployment --
// its configuration, its TLS, its routing, its provider credentials, its
// database -- behaves the way the code says it should. Every defect these
// found was invisible to every other test in the repository, because each one
// lived in the gap between a correct binary and the environment handed to it:
// a provider name two characters off, an environment label that disagreed with
// the provider mode, an account the credentials were never checked against.
//
// Nothing here is run by `go test ./...`. It needs a live deployment, real
// provider credentials and a real database, so it is behind the `deployed`
// build tag and skips unless told where to point:
//
//	NODAL_DEPLOYED_URL     base URL, e.g. https://api-nodal.actorvia.xyz
//	NODAL_WEBHOOK_SECRET   the Stripe signing secret for that endpoint
//	NODAL_DEPLOYED_DB_URL  a connection string for the database it uses
//
// Tests that would write to the deployment are explicit about it and clean up
// after themselves. A test that cannot clean up does not run.
package deployed
