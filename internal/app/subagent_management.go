package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/hookmount"
	"github.com/ArcheMind/sessionman/internal/nativedirs"
	"github.com/ArcheMind/sessionman/internal/presets"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/subagents"
)

func (a App) preset(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return presetUsageError()
	}
	home := a.presetHome()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: sm [--json|--yaml] preset list")
		}
		return a.presetList(home)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: sm [--json|--yaml] preset show <name>")
		}
		return a.presetShow(home, args[1])
	default:
		return presetUsageError()
	}
}

func (a App) mcpServe(ctx context.Context) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return a.serveMCPAt(ctx, cwd)
}

func (a App) agyMCPServe(ctx context.Context) error {
	cwd := os.Getenv("SM_MCP_CWD")
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("SessionMan's native agy plugin requires a workspace from sm agy; start agy through SessionMan")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("SessionMan agy workspace %q is not accessible", cwd)
	}
	return a.serveMCPAt(ctx, cwd)
}

func (a App) serveMCPAt(ctx context.Context, cwd string) error {
	server := &subagents.Server{Home: a.presetHome(), Cwd: cwd, Runner: a.Runner}
	if r, ok := a.Runner.(runtime.ExecRunner); ok && r.Debug {
		server.Log = a.Stderr
	}
	return server.Serve(ctx, a.Stdin, a.Stdout)
}

// mcpDaemon is the hidden per-agent daemon entry spawned by the MCP thin shell.
func (a App) mcpDaemon(ctx context.Context, args []string) error {
	var home, agent string
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--home":
			home = args[i+1]
		case "--agent":
			agent = args[i+1]
		default:
			return fmt.Errorf("usage: sm mcp __daemon --home <home> --agent <id>")
		}
	}
	if home == "" || agent == "" {
		return fmt.Errorf("usage: sm mcp __daemon --home <home> --agent <id>")
	}
	daemon := &subagents.Daemon{Home: home, Dir: subagents.AgentDir(home, agent), Runner: a.Runner}
	if r, ok := a.Runner.(runtime.ExecRunner); ok && r.Debug {
		daemon.Log = a.Stderr
	}
	return subagents.RunDaemon(daemon)
}

func presetUsageError() error {
	return fmt.Errorf("usage: sm [--json|--yaml] preset <list|show> [args...]")
}

func presetEntry(o presets.Preset) map[string]any {
	reason := "static requirements satisfied; login and model access verified per execution"
	if err := subagents.Ready(o); err != nil {
		reason = err.Error()
	}
	entry := map[string]any{"name": o.Name, "agent": o.Agent, "model": o.Model, "effort": presets.EffortLabel(o.Effort), "description": o.Description, "status": reason, "limits": subagents.Limits(o)}
	return entry
}

func (a App) presetList(home string) error {
	list, problems, err := presets.Load(home)
	if err != nil {
		return err
	}
	options := []map[string]any{}
	for _, o := range list {
		options = append(options, presetEntry(o))
	}
	invalid := append([]presets.Problem{}, problems...)
	if a.Output != OutputText {
		return writeStructured(a.Stdout, map[string]any{
			"presets_directory": presets.Dir(home),
			"presets":           options,
			"invalid_presets":   invalid,
		}, a.Output)
	}
	for _, o := range options {
		fmt.Fprintf(a.Stdout, "%-20s %-10s %s\n", o["name"], o["agent"], o["status"])
	}
	for _, p := range invalid {
		fmt.Fprintf(a.Stdout, "%-20s %-10s invalid: %s\n", p.Name, "-", p.Err)
	}
	return nil
}

func (a App) presetShow(home string, name string) error {
	list, problems, err := presets.Load(home)
	if err != nil {
		return err
	}
	var found *presets.Preset
	for i := range list {
		if list[i].Name == name {
			found = &list[i]
			break
		}
	}
	if found == nil {
		for _, p := range problems {
			if p.Name == name {
				return fmt.Errorf("preset %s is invalid: %s", name, p.Err)
			}
		}
		return fmt.Errorf("preset %q not found in %s", name, presets.Dir(home))
	}
	result := presetEntry(*found)
	if a.Output != OutputText {
		return writeStructured(a.Stdout, result, a.Output)
	}
	for _, key := range []string{"name", "agent", "model", "effort", "description", "status", "limits"} {
		if value, ok := result[key]; ok {
			fmt.Fprintf(a.Stdout, "%s: %v\n", key, value)
		}
	}
	return nil
}

func (a App) presetHome() string {
	if override := os.Getenv("SM_SUBAGENT_HOME"); override != "" {
		return override
	}
	return a.Hooks.Home
}

func (a App) injectClaudeSessionMan(request *runtime.RunRequest) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	mountSessionMan(request, agentparams.MCPServer{Command: executable, Args: []string{"mcp", "serve"}})
	request.Flags = append([]string{"--allowedTools", "mcp__sessionman__StartExternalSubagent", "mcp__sessionman__SendExternalSubagentMessage", "mcp__sessionman__StopExternalSubagent", "mcp__sessionman__ListExternalSubagentSessions"}, request.Flags...)

	return nil
}

func injectPiSessionMan(home string, request *runtime.RunRequest, write bool) error {
	path := subagents.PiBridgePath(home)
	if write {
		if err := subagents.WritePiBridge(path); err != nil {
			return err
		}
	}
	request.Flags = append(request.Flags, "--extension", path)
	return nil
}

func injectCodexSessionMan(request *runtime.RunRequest) {
	bin, err := os.Executable()
	if err != nil {
		return
	}
	server := agentparams.MCPServer{Command: bin, Args: []string{"mcp", "serve"}}
	if subHome := os.Getenv("SM_SUBAGENT_HOME"); subHome != "" {
		server.Env = map[string]string{"SM_SUBAGENT_HOME": subHome}
	}
	mountSessionMan(request, server)
}

func mountSessionMan(request *runtime.RunRequest, server agentparams.MCPServer) {
	servers := make(map[string]agentparams.MCPServer, len(request.MCP.Servers)+1)
	for name, existing := range request.MCP.Servers {
		servers[name] = existing
	}
	servers["sessionman"] = server
	request.MCP.Servers = servers
}

const agyPluginOwner = "sessionman native agy plugin\n"

// injectAgySessionMan mounts SessionMan's MCP-only agy plugin via a symlink at
// ~/.gemini/config/plugins/sessionman when SM_CONFIG_DIR is unset, and returns
// a release function that unlinks it once the last sm agy session exits.
func injectAgySessionMan(home string, write bool, roleHook ...bool) (func(), error) {
	noop := func() {}
	if !write || nativedirs.FromEnv().ConfigDir != "" {
		return noop, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return noop, err
	}
	includeRole := len(roleHook) > 0 && roleHook[0]
	files, err := hookmount.AgyPluginWithRole(executable, nil, includeRole)
	if err != nil {
		return noop, err
	}
	stateDir := filepath.Join(home, ".local", "state", "sessionman", "agy-plugin")
	pluginDir := filepath.Join(stateDir, "plugin")
	leasesDir := filepath.Join(stateDir, "leases")
	lockPath := filepath.Join(stateDir, "lock")
	linkPath := filepath.Join(home, ".gemini", "config", "plugins", "sessionman")

	var leasePath string
	err = withAgyPluginLock(lockPath, func() error {
		if err := writeAgyPluginFiles(pluginDir, files); err != nil {
			return err
		}
		if err := ensureAgyPluginLink(linkPath, pluginDir); err != nil {
			return err
		}
		if err := os.MkdirAll(leasesDir, 0o700); err != nil {
			return err
		}
		f, err := os.CreateTemp(leasesDir, fmt.Sprintf("%d-*.lease", os.Getpid()))
		if err != nil {
			return err
		}
		leasePath = f.Name()
		return f.Close()
	})
	if err != nil {
		return noop, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = withAgyPluginLock(lockPath, func() error {
				_ = os.Remove(leasePath)
				if countLiveAgyLeases(leasesDir) == 0 {
					if target, err := os.Readlink(linkPath); err == nil && target == pluginDir {
						_ = os.Remove(linkPath)
					}
				}
				return nil
			})
		})
	}, nil
}

func withAgyPluginLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockAgyPluginFile(f); err != nil {
		return err
	}
	defer unlockAgyPluginFile(f)
	return fn()
}

func writeAgyPluginFiles(pluginDir string, files map[string][]byte) error {
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		return err
	}
	for name, data := range files {
		f, err := os.CreateTemp(pluginDir, ".sessionman-")
		if err != nil {
			return err
		}
		tmp := f.Name()
		if _, err = f.Write(data); err == nil {
			err = f.Close()
		} else {
			_ = f.Close()
		}
		if err == nil {
			err = os.Rename(tmp, filepath.Join(pluginDir, name))
		}
		_ = os.Remove(tmp)
		if err != nil {
			return err
		}
	}
	return nil
}

func ensureAgyPluginLink(linkPath, pluginDir string) error {
	info, err := os.Lstat(linkPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(linkPath)
			if err == nil && target == pluginDir {
				return nil
			}
			return fmt.Errorf("refusing to overwrite non-SessionMan plugin %s", linkPath)
		}
		if info.IsDir() {
			data, err := os.ReadFile(filepath.Join(linkPath, ".sessionman-owned"))
			if err == nil && string(data) == agyPluginOwner {
				if err := os.RemoveAll(linkPath); err != nil {
					return err
				}
				return os.Symlink(pluginDir, linkPath)
			}
		}
		return fmt.Errorf("refusing to overwrite non-SessionMan plugin %s", linkPath)
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
		return err
	}
	return os.Symlink(pluginDir, linkPath)
}

func countLiveAgyLeases(leasesDir string) int {
	entries, err := os.ReadDir(leasesDir)
	if err != nil {
		return 0
	}
	live := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lease") {
			continue
		}
		pidStr, _, ok := strings.Cut(entry.Name(), "-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 || !agyLeaseProcessAlive(pid) {
			_ = os.Remove(filepath.Join(leasesDir, entry.Name()))
			continue
		}
		live++
	}
	return live
}

func agyLeaseProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
