// Package architecture enforces Jupiter's module boundaries mechanically.
//
// Jupiter is a modular monolith (ADR 0001). The Go compiler's internal/ rule stops other
// repositories from importing Jupiter's packages, but not one module from reaching into
// another inside this repository. These rules do, and a test in this package fails the
// build on any violation:
//
//   - module-boundary: a domain module is the package internal/<module>. Its
//     subpackages, internal/<module>/..., are private to it. Another module, or a
//     binary under cmd/, may import only the module's root package.
//   - shared-kernel: internal/money, internal/id and internal/platform/... are shared by
//     every module and must not import any domain module, the vault or a simulator.
//   - vault-isolation: the vault (internal/vault/..., cmd/vault) keeps card data in PCI
//     scope apart from everything else, so it imports only itself, the shared kernel
//     and pkg/.
//   - simulator-isolation: a simulator (internal/sim/<name>/..., cmd/sim-<name>) stands
//     for another company. It imports only itself, internal/platform/... and pkg/, and
//     nothing but tests under test/ may import it: Jupiter reaches simulators over the
//     wire only.
//   - pkg-independence: pkg/ is meant to be imported by other repositories, so it never
//     imports internal/.
//
// Packages under test/ compose the whole system for end-to-end tests and are exempt.
package architecture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Package is a package of the module under check with its intra-module imports, both
// given relative to the module path (such as "internal/ledger").
type Package struct {
	Path    string
	Imports []string
}

// Violation is one forbidden import.
type Violation struct {
	Rule string
	From string
	To   string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s imports %s", v.Rule, v.From, v.To)
}

// buildTags must name every build tag the repository uses, so that no file is hidden
// from the check. Keep it in step with the Makefile's vet target.
const buildTags = "integration,e2e"

// Load lists the packages of the Go module in dir, including test-only imports and files
// behind Jupiter's build tags.
func Load(ctx context.Context, dir string) ([]Package, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-tags="+buildTags,
		"-json=ImportPath,Module,Imports,TestImports,XTestImports,Error", "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %w: %s", dir, err, stderr.String())
	}
	return decodePackages(out)
}

type listedPackage struct {
	ImportPath   string
	Module       struct{ Path string }
	Imports      []string
	TestImports  []string
	XTestImports []string
	Error        *struct{ Err string }
}

func decodePackages(out []byte) ([]Package, error) {
	var pkgs []Package
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var lp listedPackage
		if err := dec.Decode(&lp); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		if lp.Error != nil {
			return nil, fmt.Errorf("package %s: %s", lp.ImportPath, lp.Error.Err)
		}
		module := lp.Module.Path
		pkg := Package{Path: relative(module, lp.ImportPath)}
		for _, imports := range [][]string{lp.Imports, lp.TestImports, lp.XTestImports} {
			for _, imp := range imports {
				if imp == module || strings.HasPrefix(imp, module+"/") {
					pkg.Imports = append(pkg.Imports, relative(module, imp))
				}
			}
		}
		pkgs = append(pkgs, pkg)
	}
}

func relative(module, path string) string {
	return strings.TrimPrefix(strings.TrimPrefix(path, module), "/")
}

// Check returns every import in pkgs that breaks a rule.
func Check(pkgs []Package) []Violation {
	var violations []Violation
	for _, pkg := range pkgs {
		from := classify(pkg.Path)
		for _, imp := range pkg.Imports {
			if rule := broken(from, classify(imp)); rule != "" {
				violations = append(violations, Violation{Rule: rule, From: pkg.Path, To: imp})
			}
		}
	}
	return violations
}
