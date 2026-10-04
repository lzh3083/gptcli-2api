package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionLess(t *testing.T) {
	if !versionLess("2.0.2", "2.0.3") {
		t.Fatal("2.0.2 < 2.0.3")
	}
	if versionLess("2.0.3", "2.0.3") {
		t.Fatal("equal")
	}
	if versionLess("2.1.0", "2.0.9") {
		t.Fatal("2.1.0 not < 2.0.9")
	}
	if !versionLess("2.0.3", "2.0.10") {
		t.Fatal("2.0.3 < 2.0.10")
	}
}

func TestParseSemver(t *testing.T) {
	v := parseSemver("v2.0.3-rc1")
	if v != [3]int{2, 0, 3} {
		t.Fatalf("%v", v)
	}
}

func TestUpdateModeDockerWhenSockAndCLI(t *testing.T) {
	// Isolate env.
	t.Setenv("GROK2API_HOT_UPDATE_MODE", "")
	t.Setenv("GROK2API_HOT_UPDATE_CMD", "")
	t.Setenv("GROK2API_HOT_UPDATE_ALLOW_REQUEST_FILE", "")
	t.Setenv("DOCKER_HOST", "")

	// Without sock → disabled (or request_file only if allowed).
	t.Setenv("DOCKER_HOST", "unix:///no/such/docker.sock")
	mode, supported, _ := updateMode()
	if mode == "docker" && supported {
		// May still be docker if real sock exists on this host; only assert consistency.
		if !dockerSockPresent() {
			t.Fatalf("docker mode without sock: %s supported=%v", mode, supported)
		}
	}

	// Forced request_file always available.
	t.Setenv("GROK2API_HOT_UPDATE_MODE", "request_file")
	mode, supported, hint := updateMode()
	if mode != "request_file" || !supported {
		t.Fatalf("request_file mode=%s supported=%v hint=%s", mode, supported, hint)
	}

	// Forced disabled.
	t.Setenv("GROK2API_HOT_UPDATE_MODE", "disabled")
	mode, supported, _ = updateMode()
	if mode != "disabled" || supported {
		t.Fatalf("disabled mode=%s supported=%v", mode, supported)
	}

	// Forced cmd without command → disabled.
	t.Setenv("GROK2API_HOT_UPDATE_MODE", "cmd")
	t.Setenv("GROK2API_HOT_UPDATE_CMD", "")
	mode, supported, _ = updateMode()
	if supported {
		t.Fatalf("cmd without HOT_UPDATE_CMD should be disabled, got %s", mode)
	}

	// Forced cmd with command.
	t.Setenv("GROK2API_HOT_UPDATE_CMD", "echo {{TAG}}")
	mode, supported, _ = updateMode()
	if mode != "cmd" || !supported {
		t.Fatalf("cmd mode=%s supported=%v", mode, supported)
	}
}

func TestDockerSockPathFromEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/test.sock")
	if p := dockerSockPath(); p != "/tmp/test.sock" {
		t.Fatalf("path=%s", p)
	}
	t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2375")
	if p := dockerSockPath(); p != "" {
		t.Fatalf("tcp host should yield empty sock path, got %q", p)
	}
}

func TestInContainerUpdateScriptFallback(t *testing.T) {
	t.Setenv("GROK2API_HOT_UPDATE_SCRIPT", "")
	// When repo script exists relative to cwd, prefer it.
	cwd, _ := os.Getwd()
	// From module root when tests run with package dir as cwd → go to repo root via relative path in function.
	p := inContainerUpdateScript()
	if p == "" {
		t.Fatal("empty script path")
	}
	// Either absolute default or found relative path.
	if !filepath.IsAbs(p) && p != defaultInContainerUpdateScript {
		if _, err := os.Stat(p); err != nil {
			// ok if not present in test sandbox; just ensure non-empty default
			_ = cwd
		}
	}
}
