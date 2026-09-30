// Command vault tokenizes card data in its own process, with its own database, so that
// the rest of Jupiter stays outside PCI DSS scope (ADR 0001).
//
// Until phase 4 adds tokenization, it serves only health checks.
package main

import "github.com/iricardofernandes/jupiter/internal/platform/service"

func main() {
	service.Main("vault", ":8082")
}
