package architecture

import "strings"

type kind int

const (
	kindOther kind = iota
	kindDomain
	kindShared
	kindVault
	kindSim
	kindCmd
	kindPkg
	kindTest
)

var sharedKernel = map[string]bool{"money": true, "id": true, "platform": true}

type unit struct {
	kind   kind
	name   string
	isRoot bool
}

func classify(path string) unit {
	segments := strings.Split(path, "/")
	switch segments[0] {
	case "cmd":
		return classifyCmd(segments)
	case "internal":
		return classifyInternal(segments)
	case "pkg":
		return unit{kind: kindPkg}
	case "test":
		return unit{kind: kindTest}
	default:
		return unit{kind: kindOther}
	}
}

func classifyCmd(segments []string) unit {
	if len(segments) < 2 {
		return unit{kind: kindOther}
	}
	binary := segments[1]
	switch {
	case binary == "vault":
		return unit{kind: kindVault, name: "vault"}
	case strings.HasPrefix(binary, "sim-"):
		return unit{kind: kindSim, name: strings.TrimPrefix(binary, "sim-")}
	default:
		return unit{kind: kindCmd, name: binary}
	}
}

func classifyInternal(segments []string) unit {
	if len(segments) < 2 {
		return unit{kind: kindOther}
	}
	module := segments[1]
	switch {
	case sharedKernel[module]:
		return unit{kind: kindShared, name: module}
	case module == "sim":
		if len(segments) < 3 {
			return unit{kind: kindSim}
		}
		return unit{kind: kindSim, name: segments[2]}
	case module == "vault":
		return unit{kind: kindVault, name: "vault", isRoot: len(segments) == 2}
	default:
		return unit{kind: kindDomain, name: module, isRoot: len(segments) == 2}
	}
}

func broken(from, to unit) string {
	switch {
	case from.kind == kindTest:
		return ""
	case from.kind == kindPkg && to.kind != kindPkg:
		return "pkg-independence"
	case from.kind == kindSim && !simMayImport(from, to):
		return "simulator-isolation"
	case to.kind == kindSim && from.kind != kindSim:
		return "simulator-isolation"
	case from.kind == kindShared && (to.kind == kindDomain || to.kind == kindVault):
		return "shared-kernel"
	case from.kind == kindVault && !vaultMayImport(to):
		return "vault-isolation"
	case crossesModuleBoundary(from, to):
		return "module-boundary"
	default:
		return ""
	}
}

func vaultMayImport(to unit) bool {
	return to.kind == kindVault || to.kind == kindShared || to.kind == kindPkg
}

func simMayImport(from, to unit) bool {
	switch to.kind {
	case kindSim:
		return to.name == from.name
	case kindShared:
		return to.name == "platform"
	case kindPkg:
		return true
	default:
		return false
	}
}

func crossesModuleBoundary(from, to unit) bool {
	if to.kind != kindDomain && to.kind != kindVault {
		return false
	}
	sameModule := from.kind == to.kind && from.name == to.name
	return !sameModule && !to.isRoot
}
