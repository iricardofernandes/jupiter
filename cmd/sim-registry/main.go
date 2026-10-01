package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/sim/registry"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

const minToken = 16

// The receivables registry simulator, for tests and local development only.
//
//   - JUPITER_HTTP_ADDR (default 127.0.0.1:8588): the registry's API.
//   - JUPITER_SIM_REGISTRY_PARTICIPANTS: who may use it, as token:cnpj:role, comma
//     separated, role accreditor or financier.
func main() {
	service.Main("sim-registry", "127.0.0.1:8588", func(_ context.Context, _ service.Config, logger *slog.Logger) (service.App, error) {
		participants, err := parseParticipants(os.Getenv("JUPITER_SIM_REGISTRY_PARTICIPANTS"))
		if err != nil {
			return service.App{}, err
		}
		sim := registry.New(registry.Config{Logger: logger, Participants: participants})
		return service.App{Handler: sim.Handler()}, nil
	})
}

func parseParticipants(s string) ([]registry.Participant, error) {
	var out []registry.Participant
	tokens := map[string]bool{}
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		if len(parts) != 3 || (parts[2] != string(registry.Accreditor) && parts[2] != string(registry.Financier)) {
			return nil, errors.New("JUPITER_SIM_REGISTRY_PARTICIPANTS: an entry is not token:cnpj:accreditor or token:cnpj:financier")
		}
		if len(parts[0]) < minToken || tokens[parts[0]] || len(parts[1]) != 14 || !taxid.Valid(parts[1]) {
			return nil, fmt.Errorf("JUPITER_SIM_REGISTRY_PARTICIPANTS: each needs a token of %d characters or more of its own and a valid CNPJ", minToken)
		}
		tokens[parts[0]] = true
		out = append(out, registry.Participant{Token: parts[0], TaxID: parts[1], Role: registry.Role(parts[2])})
	}
	if len(out) == 0 {
		return nil, errors.New("JUPITER_SIM_REGISTRY_PARTICIPANTS names no participant")
	}
	return out, nil
}
