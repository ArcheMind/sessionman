package subagents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/presets"
)

func installPi(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidModesRejectedBeforeAnyWork(t *testing.T) {
	record := installFakeNative(t, "codex", nil)
	s := testServer(t, nil)
	installPi(t)
	savePreset(t, s.Home, presets.Preset{Name: "pi-review", Agent: "pi", Model: "provider/model", Description: "d"})
	for input, want := range map[string]string{
		`{"description":"x","prompt":"x","model":"review","mode":"dontAsk"}`:        `mode "dontAsk" unsupported`,
		`{"description":"x","prompt":"x","model":"review","mode":""}`:               "must not be empty",
		`{"description":"x","prompt":"x","model":"review","write":true}`:            "unknown field",
		`{"description":"x","prompt":"x","model":"review","writable_roots":["/x"]}`: "unknown field",
	} {
		_, err := s.call("ExternalSubagent", []byte(input))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", input, err, want)
		}
	}
	if _, err := os.Stat(record); err == nil || len(s.agents) != 0 {
		t.Fatal("a rejected request started work")
	}
}

func TestNormalModePassesNoPermissionFlags(t *testing.T) {
	record := installFakeNative(t, "codex", nil)
	s := testServer(t, nil)
	text := callTest(t, s, "ExternalSubagent", map[string]any{"description": "read", "prompt": "review", "model": "review", "mode": "normal", "run_in_background": false})
	records := readFakeRecords(t, record)
	thread := fakeRequest(t, records, "thread/start")
	if thread["approvalPolicy"] != nil || thread["sandbox"] != nil || !strings.Contains(text, "Mode: normal\n") || !strings.Contains(text, `Auth source for this turn: CODEX_HOME="`+os.Getenv("CODEX_HOME")+`"`+"\n") {
		t.Fatal(thread, text)
	}
	if records[0].Env["CODEX_HOME"] != os.Getenv("CODEX_HOME") {
		t.Fatalf("auth environment not explicit: %#v", records[0].Env)
	}
}

func TestYoloModeBypassesNativePermissions(t *testing.T) {
	record := installFakeNative(t, "codex", nil)
	s := testServer(t, nil)
	text := callTest(t, s, "ExternalSubagent", map[string]any{"description": "x", "prompt": "x", "model": "review", "mode": "yolo", "run_in_background": false})
	thread := fakeRequest(t, readFakeRecords(t, record), "thread/start")
	if thread["approvalPolicy"] != "never" || thread["sandbox"] != "danger-full-access" || !strings.Contains(text, "Mode: yolo\n") {
		t.Fatal(thread, text)
	}
}

func TestModeIsFixedForContinuation(t *testing.T) {
	record := installFakeNative(t, "codex", nil)
	s := testServer(t, nil)
	callTest(t, s, "ExternalSubagent", map[string]any{"description": "x", "prompt": "x", "model": "review", "name": "y", "mode": "yolo", "run_in_background": false})
	callTest(t, s, "ExternalSubagent", map[string]any{"description": "x", "prompt": "x", "model": "review", "name": "n", "mode": "normal", "run_in_background": false})
	for _, name := range []string{"y", "n"} {
		callTest(t, s, "SendExternalSubagentMessage", map[string]any{"to": name, "message": "again"})
		waitReports(t, s, name, 2)
	}
	if _, err := s.call("SendExternalSubagentMessage", []byte(`{"to":"n","message":"x","mode":"yolo"}`)); err == nil {
		t.Fatal("continuation accepted a mode change")
	}
	records := readFakeRecords(t, record)
	if countFakeRequests(t, records, "thread/start") != 2 || countFakeRequests(t, records, "turn/start") != 4 {
		t.Fatal("continuation restarted a session or lost a turn", records)
	}
	var policies []string
	for _, entry := range records {
		if entry.Kind == "request" && strings.Contains(string(entry.Line), `"method":"thread/start"`) {
			var req struct {
				Params struct {
					ApprovalPolicy string `json:"approvalPolicy"`
				} `json:"params"`
			}
			_ = json.Unmarshal(entry.Line, &req)
			policies = append(policies, req.Params.ApprovalPolicy)
		}
	}
	if len(policies) != 2 || policies[0] != "never" || policies[1] != "" {
		t.Fatal("continuation changed mode", policies)
	}
}

func TestAuthFromEnvReadsProcessEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", "/resolved/codex")
	t.Setenv("PI_CODING_AGENT_DIR", "/resolved/pi")
	auth := AuthFromEnv("codex")
	if auth.Key != "CODEX_HOME" || auth.Dir != "/resolved/codex" {
		t.Fatal(auth)
	}
	auth = AuthFromEnv("pi")
	if auth.Key != "PI_CODING_AGENT_DIR" || auth.Dir != "/resolved/pi" {
		t.Fatal(auth)
	}
	auth = AuthFromEnv("claude")
	if auth.Key != "" {
		t.Fatal(auth)
	}
}

func TestAuthSourcePinnedWithoutPreflight(t *testing.T) {
	record := installFakeNative(t, "pi", nil)
	s := testServer(t, nil)
	savePreset(t, s.Home, presets.Preset{Name: "review", Agent: "pi", Model: "provider/model", Description: "d"})
	text := callTest(t, s, "ExternalSubagent", map[string]any{"description": "x", "prompt": "x", "model": "review", "run_in_background": false})
	if !strings.Contains(text, `PI_CODING_AGENT_DIR="`+os.Getenv("PI_CODING_AGENT_DIR")+`"`+"\n") {
		t.Fatal(text)
	}
	records := readFakeRecords(t, record)
	if records[0].Env["PI_CODING_AGENT_DIR"] != os.Getenv("PI_CODING_AGENT_DIR") || countFakeRequests(t, records, "prompt") != 1 {
		t.Fatalf("native execution not pinned to the auth source: %#v", records)
	}
}

func must(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeFailureReturnedToClaudeIsRedacted(t *testing.T) {
	installFakeNative(t, "codex", map[string]string{"SM_FAKE_FAILMSG": "auth header Bearer abcdefghijklmnopqrstuvwxyz0123 key sk-proj-abcdefghijklmnopqrstuvwxyz"})
	s := testServer(t, nil)
	_, err := s.call("ExternalSubagent", []byte(`{"description":"x","prompt":"FAIL","model":"review","run_in_background":false}`))
	if err == nil || strings.Contains(err.Error(), "abcdefghijklmnop") || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatal(err)
	}
}

func TestUserConfigFalseRejectsBeforeStarting(t *testing.T) {
	record := installFakeNative(t, "codex", nil)
	s := testServer(t, nil)
	f := false
	p := reviewPreset
	p.UserConfig = &f
	savePreset(t, s.Home, p)
	_, err := s.call("ExternalSubagent", []byte(`{"description":"x","prompt":"x","model":"review","run_in_background":false}`))
	if err == nil || !strings.Contains(err.Error(), "user_config=false") || !strings.Contains(err.Error(), "no task started") {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) || len(s.agents) != 0 {
		t.Fatal("rejected preset started work")
	}
}

func TestPresetFlagsAppendedToNativeArgs(t *testing.T) {
	record := installFakeNative(t, "codex", nil)
	s := testServer(t, nil)
	p := reviewPreset
	p.Flags = []string{"-c", "custom_setting=true"}
	savePreset(t, s.Home, p)
	callTest(t, s, "ExternalSubagent", map[string]any{"description": "x", "prompt": "x", "model": "review", "run_in_background": false})
	args := strings.Join(readFakeRecords(t, record)[0].Argv, " ")
	if !strings.Contains(args, "-c custom_setting=true") {
		t.Fatal("preset flags not forwarded", args)
	}
}

func TestPiUserConfigAndFlagsReachRPC(t *testing.T) {
	record := installFakeNative(t, "pi", nil)
	s := testServer(t, nil)
	f := false
	savePreset(t, s.Home, presets.Preset{Name: "pi-config", Agent: "pi", Model: "provider/model", Description: "test", UserConfig: &f, Flags: []string{"--trace"}})
	callTest(t, s, "ExternalSubagent", map[string]any{"description": "x", "prompt": "x", "model": "pi-config", "run_in_background": false})
	args := strings.Join(readFakeRecords(t, record)[0].Argv, " ")
	for _, want := range []string{"--mode rpc", "--no-skills", "--no-prompt-templates", "--trace"} {
		if !strings.Contains(args, want) {
			t.Fatal(want, args)
		}
	}
}
