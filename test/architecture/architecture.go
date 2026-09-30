// Package architecture enforces the module boundaries Go's internal/ rule cannot:
//   - module-boundary: from outside a module, only its root package internal/<module>
//     may be imported.
//   - shared-kernel: money, id and platform import no domain module, vault or simulator.
//   - vault-isolation: the vault imports only itself, the shared kernel and pkg/, which
//     keeps PCI scope small.
//   - simulator-isolation: a simulator imports only itself, platform and pkg/, and only
//     test/ imports it; Jupiter reaches simulators over the wire.
//   - pkg-independence: pkg/ never imports internal/.
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

type Package struct {
	Path    string
	Imports []string
}

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
