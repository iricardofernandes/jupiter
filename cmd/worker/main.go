// Command worker runs Jupiter's background jobs: webhook delivery, unknown-outcome
// resolution, settlement and reconciliation.
//
// Until the first jobs arrive, it serves only health checks.
package main

import "github.com/iricardofernandes/jupiter/internal/platform/service"

func main() {
	service.Main("worker", ":8081")
}
