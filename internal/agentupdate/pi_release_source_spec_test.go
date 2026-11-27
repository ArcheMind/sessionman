package agentupdate_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/agentupdate"
	"github.com/ArcheMind/sessionman/internal/drivers"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

func TestSpecPiReleaseCheckUsesTheCurrentPackageSource(t *testing.T) {
	for _, tc := range []struct {
		current, status string
	}{
		{"0.84.3", "available"},
		{"1.6.1", "up_to_date"},
	} {
		t.Run(tc.current, func(t *testing.T) {
			var requests []string
			service := agentupdate.Service{
				HTTPClient: specPiReleaseClient(&requests),
				Detect: func(context.Context, string, string) (agentupdate.Installed, error) {
					return agentupdate.Installed{Path: "/existing/pi", Version: tc.current}, nil
				},
			}
			report := service.Check(context.Background(), "pi")
			if report.Status != tc.status || report.CurrentVersion != tc.current || report.LatestVersion != "1.6.1" {
				t.Fatalf("Pi must compare the installed version with its current package's stable release: %+v; requests=%v", report, requests)
			}
			if len(requests) != 1 || requests[0] != "GET https://registry.npmjs.org/@earendil-works/pi-coding-agent/latest" {
				t.Fatalf("Pi release source diverged from the installed product: %v", requests)
			}
		})
	}
}

func TestSpecPiManagedAndDirectInstallationUseTheSameReleaseSource(t *testing.T) {
	const packageVersion = "@earendil-works/pi-coding-agent@1.6.1"
	agent, err := drivers.NewRegistry().Get("pi")
	if err != nil {
		t.Fatal(err)
	}
	directPlan, err := agent.Install.PlanInstall("1.6.1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(directPlan.Args, packageVersion) {
		t.Fatalf("direct Pi installation does not use the checked release source: %v", directPlan.Args)
	}
	var requests []string
	runner := &piReleaseRecordingRunner{}
	service := agentupdate.Service{
		InstallRoot: t.TempDir(),
		HTTPClient:  specPiReleaseClient(&requests),
		Runner:      runner,
		Detect: func(context.Context, string, string) (agentupdate.Installed, error) {
			return agentupdate.Installed{Path: "/existing/pi", Version: "0.84.3"}, nil
		},
		Probe: func(context.Context, string) (string, error) { return "1.6.1", nil },
	}
	if _, err := service.Update(context.Background(), "pi"); err == nil {
		t.Fatal("the recording runner stops before executing an installer")
	}
	if len(runner.plans) != 1 || !slices.Contains(runner.plans[0].Args, packageVersion) {
		t.Fatalf("managed Pi installation does not use the checked exact release: %+v; requests=%v", runner.plans, requests)
	}
	for _, plan := range []runtime.CommandPlan{directPlan, runner.plans[0]} {
		for _, arg := range plan.Args {
			if strings.Contains(arg, "@mariozechner/pi-coding-agent") {
				t.Errorf("Pi installation still references the retired package source: %v", plan.Args)
			}
		}
	}
}

func specPiReleaseClient(requests *[]string) *http.Client {
	return &http.Client{Transport: piReleaseRoundTripper(func(request *http.Request) (*http.Response, error) {
		*requests = append(*requests, request.Method+" "+request.URL.String())
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "registry.npmjs.org" || request.URL.Path != "/@earendil-works/pi-coding-agent/latest" {
			return nil, fmt.Errorf("unexpected Pi release request: %s %s", request.Method, request.URL)
		}
		const fixture = `{"name":"@earendil-works/pi-coding-agent","version":"1.6.1","dist":{"tarball":"https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/pi-coding-agent-1.6.1.tgz","integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(fixture)),
			Request:    request,
		}, nil
	})}
}

type piReleaseRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip piReleaseRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type piReleaseRecordingRunner struct {
	plans []runtime.CommandPlan
}

func (runner *piReleaseRecordingRunner) Execute(_ context.Context, plan runtime.CommandPlan, _ runtime.ExecuteOptions) (runtime.CommandResult, error) {
	runner.plans = append(runner.plans, plan)
	return runtime.CommandResult{ExitCode: 1}, errors.New("fixture stops before installation")
}
