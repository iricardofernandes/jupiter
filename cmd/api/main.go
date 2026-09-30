// Command api serves Jupiter's public REST API to merchants.
//
// Until phase 2 adds the API itself, it serves only health checks.
package main

import "github.com/iricardofernandes/jupiter/internal/platform/service"

func main() {
	service.Main("api", ":8080")
}
