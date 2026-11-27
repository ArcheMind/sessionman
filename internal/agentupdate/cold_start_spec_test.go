package agentupdate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ArcheMind/sessionman/internal/agentupdate"
)

const coldStartFixtureEnv = "SM_AGENTUPDATE_COLD_START_FIXTURE"

type coldStartFixture struct {
	Version   string
	Delay     time.Duration
	Started   string
	Completed string
}

func TestMain(m *testing.M) {
	if fixture := os.Getenv(coldStartFixtureEnv); fixture != "" && len(os.Args) == 2 && os.Args[1] == "--version" {
		if err := runColdStartCLI(fixture); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runColdStartCLI(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fixture coldStartFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err
	}
	started, err := os.OpenFile(fixture.Started, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if err := started.Close(); err != nil {
			return err
		}
		time.Sleep(fixture.Delay)
		if err := os.WriteFile(fixture.Completed, []byte("completed"), 0600); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%s (Claude Code)\n", fixture.Version)
	return err
}

// Spec obligation: ColdStartUpdateVerification.
func TestSpecColdStartUpdateVerification(t *testing.T) {
	service, active, previous, fixture := coldStartInstalledService(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	results := runColdStartUpdate(ctx, service)
	waitForColdStartProbe(t, ctx, fixture.Started, results)
	assertColdStartActiveTarget(t, active, previous)

	result := awaitColdStartUpdate(t, results, 10*time.Second)
	if result.err != nil {
		t.Fatalf("a valid CLI whose first version query takes longer than 3s must activate: %v; report=%+v", result.err, result.report)
	}
	if _, err := os.Stat(fixture.Completed); err != nil {
		t.Fatalf("the first version query must finish successfully before activation: %v", err)
	}
	if !result.report.Installed || result.report.Status != "up_to_date" || result.report.Path != active || coldStartVersionIdentity(result.report.CurrentVersion) != fixture.Version || result.report.LatestVersion != fixture.Version {
		t.Fatalf("update must report the exact new version at the active installation: %+v", result.report)
	}
	target, err := filepath.EvalSymlinks(active)
	if err != nil || target == previous {
		t.Fatalf("successful update did not activate the verified replacement: target=%q error=%v", target, err)
	}
	report := service.Check(context.Background(), "claude")
	if report.Path != active || coldStartVersionIdentity(report.CurrentVersion) != fixture.Version || report.Status != "up_to_date" {
		t.Fatalf("ordinary discovery must see the newly activated CLI: %+v", report)
	}
}

// Spec obligation: CancelledUpdateKeepsWorkingInstallation.
func TestSpecCancelledColdStartUpdateKeepsWorkingInstallation(t *testing.T) {
	for _, cause := range []string{"cancel", "deadline"} {
		t.Run(cause, func(t *testing.T) {
			service, active, previous, fixture := coldStartInstalledService(t, 8*time.Second)
			var ctx context.Context
			var cancel context.CancelFunc
			if cause == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			} else {
				ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
			}
			defer cancel()
			results := runColdStartUpdate(ctx, service)
			waitForColdStartProbe(t, ctx, fixture.Started, results)
			assertColdStartActiveTarget(t, active, previous)
			if cause == "cancel" {
				cancel()
			}
			<-ctx.Done()
			result := awaitColdStartUpdate(t, results, 1500*time.Millisecond)
			if result.err == nil {
				t.Fatalf("a cancelled version query must not report update success: %+v", result.report)
			}
			if result.report.Path != active || coldStartVersionIdentity(result.report.CurrentVersion) != "2.1.331" {
				t.Fatalf("cancellation lost the previous working selection: %+v", result.report)
			}
			assertColdStartActiveTarget(t, active, previous)
			if _, err := os.Stat(fixture.Completed); !os.IsNotExist(err) {
				t.Fatalf("the cancelled first version query must not run to completion: %v", err)
			}
			report := service.Check(context.Background(), "claude")
			if report.Path != active || coldStartVersionIdentity(report.CurrentVersion) != "2.1.331" || report.Status != "available" {
				t.Fatalf("the original CLI must remain discoverable after cancellation: %+v", report)
			}
		})
	}
}

// Spec obligation: InstalledDiscoveryRemainsBounded.
func TestSpecInstalledDiscoveryRemainsBoundedDuringSlowVersionQuery(t *testing.T) {
	service, active, _, fixture := coldStartInstalledService(t, 8*time.Second)
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	writeColdStartExecutable(t, active)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	start := time.Now()
	report := service.Check(ctx, "claude")
	elapsed := time.Since(start)
	if _, err := os.Stat(fixture.Started); err != nil {
		t.Fatalf("the installed executable was not actually queried: %v", err)
	}
	if elapsed >= 5*time.Second || ctx.Err() != nil {
		t.Fatalf("ordinary installed discovery must retain its 3s budget, independent of the longer installation budget: elapsed=%v context=%v report=%+v", elapsed, ctx.Err(), report)
	}
	if report.Status == "up_to_date" || coldStartVersionIdentity(report.CurrentVersion) == fixture.Version {
		t.Fatalf("a timed-out version query must not be accepted as a confirmed installed version: %+v", report)
	}
	if _, err := os.Stat(fixture.Completed); !os.IsNotExist(err) {
		t.Fatalf("ordinary discovery waited for the slow first version response: %v", err)
	}
}

type coldStartUpdateResult struct {
	report agentupdate.Report
	err    error
}

func runColdStartUpdate(ctx context.Context, service agentupdate.Service) <-chan coldStartUpdateResult {
	results := make(chan coldStartUpdateResult, 1)
	go func() {
		report, err := service.Update(ctx, "claude")
		results <- coldStartUpdateResult{report: report, err: err}
	}()
	return results
}

func coldStartVersionIdentity(version string) string {
	return strings.TrimSuffix(version, " (Claude Code)")
}

func waitForColdStartProbe(t *testing.T, ctx context.Context, started string, results <-chan coldStartUpdateResult) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(started); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case result := <-results:
			t.Fatalf("update returned before querying the staged executable: %v; report=%+v", result.err, result.report)
		case <-ctx.Done():
			t.Fatalf("staged executable did not begin its version query within the caller budget: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func awaitColdStartUpdate(t *testing.T, results <-chan coldStartUpdateResult, timeout time.Duration) coldStartUpdateResult {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result
	case <-timer.C:
		t.Fatalf("update did not return within %v", timeout)
		return coldStartUpdateResult{}
	}
}

func assertColdStartActiveTarget(t *testing.T, active, previous string) {
	t.Helper()
	target, err := filepath.EvalSymlinks(active)
	if err != nil || target != previous {
		t.Fatalf("the working CLI must remain active until verification succeeds: target=%q want=%q error=%v", target, previous, err)
	}
}

func coldStartInstalledService(t *testing.T, delay time.Duration) (agentupdate.Service, string, string, coldStartFixture) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed CLI activation is not supported on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SM_CLI_INSTALL_ROOT", "")
	fixture := coldStartFixture{
		Version:   "2.1.332",
		Delay:     delay,
		Started:   filepath.Join(root, "first-query-started"),
		Completed: filepath.Join(root, "first-query-completed"),
	}
	config := filepath.Join(root, "fixture.json")
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(coldStartFixtureEnv, config)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(root, "previous-claude")
	if err := os.WriteFile(previous, []byte("#!/bin/sh\nprintf '2.1.331 (Claude Code)\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(bin, "claude")
	if err := os.Symlink(previous, active); err != nil {
		t.Fatal(err)
	}
	service := agentupdate.Service{
		InstallRoot: root,
		Latest: func(context.Context, string) (agentupdate.Release, error) {
			return agentupdate.Release{Version: fixture.Version}, nil
		},
		Stage: func(_ context.Context, agent string, release agentupdate.Release, destination string) (string, error) {
			if agent != "claude" || release.Version != fixture.Version {
				return "", fmt.Errorf("unexpected staged agent %q release %q", agent, release.Version)
			}
			if err := os.MkdirAll(destination, 0755); err != nil {
				return "", err
			}
			path := filepath.Join(destination, agent)
			writeColdStartExecutable(t, path)
			return path, nil
		},
	}
	return service, active, previous, fixture
}

func writeColdStartExecutable(t *testing.T, destination string) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(source, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\nexec " + quoted + " \"$@\"\n"
	if err := os.WriteFile(destination, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}
