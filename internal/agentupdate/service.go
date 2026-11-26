package agentupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArcheMind/agentx/internal/clibin"
	"github.com/ArcheMind/agentx/internal/runtime"
)

type Report struct {
	Agent          string `json:"agent" yaml:"agent"`
	Installed      bool   `json:"installed" yaml:"installed"`
	Path           string `json:"path,omitempty" yaml:"path,omitempty"`
	CurrentVersion string `json:"current_version,omitempty" yaml:"current_version,omitempty"`
	LatestVersion  string `json:"latest_version,omitempty" yaml:"latest_version,omitempty"`
	Status         string `json:"status" yaml:"status"`
	Error          string `json:"error,omitempty" yaml:"error,omitempty"`
}

type Installed struct{ Path, Version string }
type Release struct{ Version, URL, SHA512 string }
type Service struct {
	InstallRoot string
	Detect      func(context.Context, string, string) (Installed, error)
	Latest      func(context.Context, string) (Release, error)
	Stage       func(context.Context, string, Release, string) (string, error)
	Probe       func(context.Context, string) (string, error)
	HTTPClient  *http.Client
	Runner      runtime.Runner
	Debug       bool
	Log         io.Writer
}

func supported(agent string) bool {
	return agent == "claude" || agent == "codex" || agent == "pi" || agent == "agy"
}

func (s Service) defaults() Service {
	s.InstallRoot = clibin.ManagedRoot(s.InstallRoot)
	if s.Log == nil {
		s.Log = io.Discard
	}
	if s.HTTPClient == nil {
		s.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if s.Runner == nil {
		s.Runner = runtime.ExecRunner{Debug: s.Debug, Log: s.Log}
	}
	if s.Probe == nil {
		s.Probe = func(ctx context.Context, path string) (string, error) {
			return clibin.Probe(ctx, path, 30*time.Second, s.probeLog)
		}
	}
	if s.Detect == nil {
		s.Detect = func(ctx context.Context, agent, root string) (Installed, error) {
			ctx, cancel := context.WithTimeout(ctx, clibin.DiscoveryTimeout)
			defer cancel()
			candidate, err := clibin.Resolve(ctx, agent, root, s.probeLog)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
				return Installed{}, nil
			}
			if err != nil {
				return Installed{}, err
			}
			if candidate.Version == "" {
				candidate.Version, err = s.Probe(ctx, candidate.Path)
			}
			return Installed{candidate.Path, candidate.Version}, err
		}
	}
	if s.Latest == nil {
		s.Latest = s.latest
	}
	if s.Stage == nil {
		s.Stage = s.stage
	}
	return s
}

func (s Service) Check(ctx context.Context, agent string) Report {
	s = s.defaults()
	report, _ := s.check(ctx, agent)
	return report
}

func (s Service) check(ctx context.Context, agent string) (Report, Release) {
	r := Report{Agent: agent, Status: "unknown"}
	if !updatesSupported || !supported(agent) {
		r.Status = "unsupported"
		return r, Release{}
	}
	current, err := s.Detect(ctx, agent, s.InstallRoot)
	r.Path, r.CurrentVersion, r.Installed = current.Path, current.Version, current.Path != ""
	if err != nil {
		r.Error = runtime.RedactSecrets(err.Error())
		return r, Release{}
	}
	if !r.Installed {
		r.Status = "not_installed"
		return r, Release{}
	}
	currentVersion, err := clibin.StableVersion(current.Version)
	if err != nil {
		r.Error = runtime.RedactSecrets(err.Error())
		return r, Release{}
	}
	r.CurrentVersion = currentVersion
	release, err := s.Latest(ctx, agent)
	if err != nil {
		r.Error = runtime.RedactSecrets(err.Error())
		return r, Release{}
	}
	latest, err := clibin.StableVersion(release.Version)
	if err != nil {
		r.Error = runtime.RedactSecrets(err.Error())
		return r, Release{}
	}
	release.Version = latest
	r.LatestVersion = latest
	cmp, _ := clibin.CompareVersions(currentVersion, latest)
	r.Status = "up_to_date"
	if cmp < 0 {
		r.Status = "available"
	}
	return r, release
}

func (s Service) Update(ctx context.Context, agent string) (Report, error) {
	s = s.defaults()
	report := Report{Agent: agent, Status: "unknown"}
	fail := func(err error) (Report, error) {
		report.Status = "unknown"
		report.Error = runtime.RedactSecrets(err.Error())
		return report, errors.New(report.Error)
	}
	if !updatesSupported || !supported(agent) {
		report.Status = "unsupported"
		report.Error = "agent updates are not supported"
		return report, errors.New(report.Error)
	}
	if s.InstallRoot == "" {
		return fail(errors.New("--install-root or SM_CLI_INSTALL_ROOT is required"))
	}
	if err := os.MkdirAll(s.InstallRoot, 0700); err != nil {
		return fail(err)
	}
	err := withLock(filepath.Join(s.InstallRoot, "."+agent+".lock"), func() error {
		var release Release
		report, release = s.check(ctx, agent)
		if report.Status == "up_to_date" {
			return nil
		}
		if report.Status != "available" {
			if report.Error != "" {
				return errors.New(report.Error)
			}
			return fmt.Errorf("cannot update %s: %s", agent, report.Status)
		}
		parent := filepath.Join(s.InstallRoot, "versions", agent)
		if err := os.MkdirAll(parent, 0700); err != nil {
			return err
		}
		stage, err := os.MkdirTemp(parent, ".stage-")
		if err != nil {
			return err
		}
		defer func() {
			if stage != "" {
				_ = os.RemoveAll(stage)
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		executable, err := s.Stage(ctx, agent, release, stage)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, executable)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
			return errors.New("installer returned an executable outside staging directory")
		}
		actual, err := s.Probe(ctx, executable)
		if err != nil {
			return err
		}
		cmp, err := clibin.CompareVersions(actual, release.Version)
		if err != nil {
			return err
		}
		if cmp != 0 {
			return fmt.Errorf("staged CLI version %q does not match requested %s", actual, release.Version)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := s.Detect(ctx, agent, s.InstallRoot)
		if err != nil {
			return err
		}
		cmp, err = clibin.CompareVersions(current.Version, release.Version)
		if err != nil {
			return err
		}
		if cmp >= 0 {
			report.Path = current.Path
			report.CurrentVersion = current.Version
			report.Status = "up_to_date"
			return nil
		}
		committed := filepath.Join(parent, release.Version+"-"+strings.TrimPrefix(filepath.Base(stage), ".stage-"))
		if err := os.Rename(stage, committed); err != nil {
			return err
		}
		stage = ""
		newPath := filepath.Join(committed, rel)
		rollback, err := activate(s.InstallRoot, agent, newPath)
		if err != nil {
			_ = os.RemoveAll(committed)
			return err
		}
		selected, err := s.Detect(ctx, agent, s.InstallRoot)
		if err == nil {
			cmp, err = clibin.CompareVersions(selected.Version, release.Version)
			if err == nil && cmp < 0 {
				err = errors.New("updated CLI is not selected for execution")
			}
			if err == nil {
				report.Path = selected.Path
				report.CurrentVersion = selected.Version
				report.Status = "up_to_date"
				report.Error = ""
				return nil
			}
		}
		if restoreErr := rollback(); restoreErr != nil {
			return fmt.Errorf("verify update: %w; restore prior selection: %v", err, restoreErr)
		}
		_ = os.RemoveAll(committed)
		return err
	})
	if err != nil {
		return fail(err)
	}
	return report, nil
}
