package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// buildGenericAdapter returns a build func for the adapter directory
// <adaptersDir>/<name>: it expects the documented two-line shell shim at
// <adaptersDir>/<name>/run (docs/CONFORMANCE.md section 6, step 1 --
// "adapter.py (executable) + run (a 2-line shell shim so the runner has one
// uniform way to start any adapter)"). This is the only way the runner
// starts any adapter -- it has no per-language special cases, since each
// SDK's toolchain lives with that SDK's own repo, not with the spec repo.
func buildGenericAdapter(adaptersDir, name string) func(repoRoot, workDir string) (string, error) {
	return func(repoRoot, workDir string) (string, error) {
		run := filepath.Join(adaptersDir, name, "run")
		info, err := os.Stat(run)
		if err != nil {
			return "", fmt.Errorf("no %q shim (conformance/README.md's adapter checklist): %w", run, err)
		}
		if info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%q is not executable", run)
		}
		return run, nil
	}
}
