package agentupdate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ArcheMind/sessionman/internal/agentupdate"
)

func TestSpecEveryInstalledAgentCanCheckWithoutInstalling(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi", "agy"} {
		t.Run(agent, func(t *testing.T) {
			staged := false
			service := agentupdate.Service{
				Detect: func(_ context.Context, got, _ string) (agentupdate.Installed, error) {
					if got != agent {
						t.Fatalf("checked %q instead of selected %q", got, agent)
					}
					return agentupdate.Installed{Path: "/existing/" + agent, Version: "0.9.0"}, nil
				},
				Latest: func(_ context.Context, got string) (agentupdate.Release, error) {
					if got != agent {
						t.Fatalf("looked up %q instead of selected %q", got, agent)
					}
					return agentupdate.Release{Version: "0.10.0"}, nil
				},
				Stage: func(context.Context, string, agentupdate.Release, string) (string, error) {
					staged = true
					return "", errors.New("checking must not install")
				},
			}
			report := service.Check(context.Background(), agent)
			if report.Agent != agent || !report.Installed || report.Status != "available" || report.CurrentVersion != "0.9.0" || report.LatestVersion != "0.10.0" {
				t.Fatalf("release check did not expose the installed version and newer stable release: %+v", report)
			}
			if staged {
				t.Fatal("a release check installed an agent")
			}
		})
	}
}

func TestSpecReleaseChecksNeverMislabelUnknownOrPrereleaseVersionsAsCurrent(t *testing.T) {
	for _, tc := range []struct {
		current, latest, status string
		fail                    bool
	}{
		{"1.2.3", "1.2.3", "up_to_date", false},
		{"1.3.0", "1.2.3", "up_to_date", false},
		{"development", "1.3.0", "unknown", false},
		{"1.2.3", "development", "unknown", false},
		{"1.2.3", "1.3.0-beta.1", "unknown", false},
		{"1.2.3", "1.3.0", "unknown", true},
	} {
		t.Run(tc.current+"/"+tc.latest, func(t *testing.T) {
			service := agentupdate.Service{
				Detect: func(context.Context, string, string) (agentupdate.Installed, error) {
					return agentupdate.Installed{Path: "/existing/codex", Version: tc.current}, nil
				},
				Latest: func(context.Context, string) (agentupdate.Release, error) {
					if tc.fail {
						return agentupdate.Release{}, errors.New("release check failed")
					}
					return agentupdate.Release{Version: tc.latest}, nil
				},
			}
			report := service.Check(context.Background(), "codex")
			if report.Status != tc.status {
				t.Fatalf("got %+v; want status %q", report, tc.status)
			}
			if tc.status == "unknown" && report.Error == "" {
				t.Fatal("unresolved release comparison needs an explanation")
			}
		})
	}
}

func TestSpecMissingUnknownAndNonNewerAgentsNeverInstall(t *testing.T) {
	for _, tc := range []struct{ agent, path, current, latest string }{
		{"codex", "", "", "1.3.0"},
		{"unrecognized", "/existing/other", "1.2.0", "1.3.0"},
		{"codex", "/existing/codex", "1.3.0", "1.3.0"},
		{"codex", "/existing/codex", "1.4.0", "1.3.0"},
		{"codex", "/existing/codex", "development", "1.3.0"},
		{"codex", "/existing/codex", "1.2.0", "1.3.0-rc.1"},
	} {
		t.Run(tc.agent+"/"+tc.current+"/"+tc.latest, func(t *testing.T) {
			service := agentupdate.Service{
				InstallRoot: t.TempDir(),
				Detect: func(context.Context, string, string) (agentupdate.Installed, error) {
					return agentupdate.Installed{Path: tc.path, Version: tc.current}, nil
				},
				Latest: func(context.Context, string) (agentupdate.Release, error) {
					return agentupdate.Release{Version: tc.latest}, nil
				},
				Stage: func(context.Context, string, agentupdate.Release, string) (string, error) {
					t.Fatal("an absent, unsupported, unresolved or non-newer agent reached installation")
					return "", nil
				},
			}
			_, _ = service.Update(context.Background(), tc.agent)
		})
	}
}

func TestSpecSuccessfulUpdateActivatesOnlyTheSelectedAgentAndRedetects(t *testing.T) {
	service, active := specInstalledService(t)
	other := filepath.Join(service.InstallRoot, "bin", "claude")
	if err := os.WriteFile(other, []byte("unchanged Claude"), 0755); err != nil {
		t.Fatal(err)
	}
	var detected, staged int
	detect := service.Detect
	service.Detect = func(ctx context.Context, agent, root string) (agentupdate.Installed, error) {
		detected++
		if agent != "codex" {
			t.Fatalf("updated another agent: %s", agent)
		}
		return detect(ctx, agent, root)
	}
	stage := service.Stage
	service.Stage = func(ctx context.Context, agent string, release agentupdate.Release, destination string) (string, error) {
		staged++
		if agent != "codex" {
			t.Fatalf("staged another agent: %s", agent)
		}
		return stage(ctx, agent, release, destination)
	}
	report, err := service.Update(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "up_to_date" || !report.Installed || report.CurrentVersion != "1.3.0" || report.Path != active {
		t.Fatalf("success must report the redetected active installation: %+v", report)
	}
	if detected < 2 || staged != 1 {
		t.Fatalf("detection calls=%d, installations=%d; need before/after detection and one update", detected, staged)
	}
	data, err := os.ReadFile(other)
	if err != nil || string(data) != "unchanged Claude" {
		t.Fatalf("another agent changed: %q %v", data, err)
	}
}

func TestSpecDownloadAndVerificationFailuresKeepTheWorkingInstallation(t *testing.T) {
	for _, failure := range []string{"download", "probe", "wrong-version"} {
		t.Run(failure, func(t *testing.T) {
			service, active := specInstalledService(t)
			switch failure {
			case "download":
				service.Stage = func(context.Context, string, agentupdate.Release, string) (string, error) {
					return "", errors.New("download failed")
				}
			case "probe":
				service.Probe = func(context.Context, string) (string, error) { return "", errors.New("new CLI cannot run") }
			case "wrong-version":
				service.Probe = func(context.Context, string) (string, error) { return "1.1.0", nil }
			}
			report, err := service.Update(context.Background(), "codex")
			if err == nil {
				t.Fatal("failed installation was reported as successful")
			}
			if report.CurrentVersion != "1.2.0" || report.Path != active {
				t.Fatalf("failure forgot the usable old selection: %+v", report)
			}
			data, readErr := os.ReadFile(active)
			if readErr != nil || string(data) != "1.2.0" {
				t.Fatalf("old installation was replaced after failure: %q %v", data, readErr)
			}
		})
	}
}

func TestSpecUnverifiedRedetectionCannotDeclareUpdateSuccess(t *testing.T) {
	service, _ := specInstalledService(t)
	detect := service.Detect
	calls := 0
	service.Detect = func(ctx context.Context, agent, root string) (agentupdate.Installed, error) {
		calls++
		if calls > 1 {
			return agentupdate.Installed{}, errors.New("active executable cannot be redetected")
		}
		return detect(ctx, agent, root)
	}
	if report, err := service.Update(context.Background(), "codex"); err == nil {
		t.Fatalf("unverified actual selection was reported as successful: %+v", report)
	}
}

func specInstalledService(t *testing.T) (agentupdate.Service, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(bin, "codex")
	previous := filepath.Join(root, "previous-codex")
	if err := os.WriteFile(previous, []byte("1.2.0"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previous, active); err != nil {
		t.Fatal(err)
	}
	return agentupdate.Service{
		InstallRoot: root,
		Detect: func(_ context.Context, agent, installRoot string) (agentupdate.Installed, error) {
			path := filepath.Join(installRoot, "bin", agent)
			data, err := os.ReadFile(path)
			return agentupdate.Installed{Path: path, Version: string(data)}, err
		},
		Latest: func(context.Context, string) (agentupdate.Release, error) {
			return agentupdate.Release{Version: "1.3.0"}, nil
		},
		Stage: func(_ context.Context, agent string, release agentupdate.Release, destination string) (string, error) {
			if err := os.MkdirAll(destination, 0755); err != nil {
				return "", err
			}
			path := filepath.Join(destination, agent)
			return path, os.WriteFile(path, []byte(release.Version), 0755)
		},
		Probe: func(_ context.Context, path string) (string, error) {
			data, err := os.ReadFile(path)
			return string(data), err
		},
	}, active
}
