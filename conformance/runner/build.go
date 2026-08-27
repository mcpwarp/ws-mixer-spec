package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func goBin() string {
	g := os.Getenv("GO")
	if g == "" {
		return "go"
	}
	return expandHome(g)
}

// expandHome expands a leading "~" or "~/" and any "$HOME"/"${HOME}"
// reference in p into the current user's home directory. exec.Command never
// invokes a shell -- fork/exec goes straight to the named file -- so it never
// does the tilde/env expansion a shell would: GO=~/.goenv/versions/1.24.4/bin/go
// would otherwise silently try to exec a literal "~" path that doesn't exist,
// which os/exec reports as a plain "file not found" that's easy to misread as
// "go isn't installed" rather than "GO wasn't expanded".
func expandHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	if home != "" {
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
	}
	return os.Expand(p, func(key string) string {
		if key == "HOME" && home != "" {
			return home
		}
		return os.Getenv(key)
	})
}

// buildGoAdapter builds conformance/adapters/go into workDir/go-adapter.
func buildGoAdapter(repoRoot, workDir string) (string, error) {
	src := filepath.Join(repoRoot, "conformance", "adapters", "go")
	out := filepath.Join(workDir, "go-adapter")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	// -tags conformance: pulls in go/wsmixer/conformance_hooks.go's
	// AllowSubfloorTiming, the adapter's only escape hatch into
	// wsmixer-internal state (see that file's comment) -- without this tag
	// the adapter fails to compile at all (undefined: wsmixer.AllowSubfloorTiming).
	cmd := exec.Command(goBin(), "build", "-tags", "conformance", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s: %v\n%s", src, err, b)
	}
	return out, nil
}

// buildGenericAdapter returns a build func for any adapter directory under
// conformance/adapters/ that isn't go or js: it expects the documented
// two-line shell shim at conformance/adapters/<name>/run (docs/CONFORMANCE.md
// section 6, step 1 -- "adapter.py (executable) + run (a 2-line shell shim
// so the runner has one uniform way to start any adapter)"). Nothing else is
// generic across languages, so this is deliberately the entire fallback.
func buildGenericAdapter(name string) func(repoRoot, workDir string) (string, error) {
	return func(repoRoot, workDir string) (string, error) {
		run := filepath.Join(repoRoot, "conformance", "adapters", name, "run")
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

// buildJSAdapter locates a Node.js runtime and returns a launcher.
//
// docs/CONFORMANCE.md section 5 says the JS adapter needs "no build step;
// imports ../../../js/src" -- but a plain `node` cannot resolve a `.ts`
// import with no loader, so this adapter imports the package's built
// `dist/index.js` instead (see conformance/README.md's documented deviation
// on this point). To make sure that dist is never stale relative to src
// (especially after this task's own client.ts change), this runs `npm run
// build` in js/ once per invocation before handing back the launcher.
func buildJSAdapter(repoRoot, workDir string) (string, error) {
	node := os.Getenv("NODE")
	if node == "" {
		node = "node"
	}
	if _, err := exec.LookPath(node); err != nil {
		if _, err2 := os.Stat(node); err2 != nil {
			return "", fmt.Errorf("node runtime %q not found: %w", node, err)
		}
	}

	jsDir := filepath.Join(repoRoot, "js")
	npm := os.Getenv("NPM")
	if npm == "" {
		npm = "npm"
	}
	if _, err := exec.LookPath(npm); err == nil {
		build := exec.Command(npm, "run", "build")
		build.Dir = jsDir
		if b, err := build.CombinedOutput(); err != nil {
			return "", fmt.Errorf("npm run build (js/): %v\n%s", err, b)
		}
	}

	adapterEntry := filepath.Join(repoRoot, "conformance", "adapters", "js", "adapter.mjs")
	if _, err := os.Stat(adapterEntry); err != nil {
		return "", fmt.Errorf("js adapter entrypoint missing: %w", err)
	}

	shimPath := filepath.Join(workDir, "js-adapter"+shellExt())
	shim := shellShim(node, adapterEntry)
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		return "", err
	}
	return shimPath, nil
}

func shellExt() string {
	if runtime.GOOS == "windows" {
		return ".cmd"
	}
	return ".sh"
}

func shellShim(node, entry string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\n\"" + node + "\" \"" + entry + "\" %*\r\n"
	}
	return "#!/bin/sh\nexec \"" + node + "\" \"" + entry + "\" \"$@\"\n"
}
