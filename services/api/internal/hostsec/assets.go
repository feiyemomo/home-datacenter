package hostsec

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReadOnlyCommandRunner executes strictly allowlisted read-only host inspection commands.
type ReadOnlyCommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
}

// OSPackage models an installed OS package parsed from dpkg status or dpkg-query.
type OSPackage struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	SourceName    string `json:"source_name,omitempty"`
	SourceVersion string `json:"source_version,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
	Status        string `json:"status,omitempty"`
}

// ServiceVersionInfo captures version and package provenance for critical services (e.g. OpenSSH).
type ServiceVersionInfo struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	PackageName    string `json:"package_name,omitempty"`
	PackageVersion string `json:"package_version,omitempty"`
	Source         string `json:"source"`
}

// DockerEngineInfo captures Docker daemon version, runtime posture, and socket metadata.
type DockerEngineInfo struct {
	Available       bool     `json:"available"`
	Version         string   `json:"version,omitempty"`
	APIVersion      string   `json:"api_version,omitempty"`
	MinAPIVersion   string   `json:"min_api_version,omitempty"`
	KernelVersion   string   `json:"kernel_version,omitempty"`
	OS              string   `json:"os,omitempty"`
	Arch            string   `json:"arch,omitempty"`
	SecurityOptions []string `json:"security_options,omitempty"`
	Rootless        bool     `json:"rootless"`
	SocketPath      string   `json:"socket_path"`
	SocketMode      string   `json:"socket_mode,omitempty"`
	SocketUID       uint32   `json:"socket_uid,omitempty"`
	SocketGID       uint32   `json:"socket_gid,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// SystemdServiceStatus describes a systemd unit state.
type SystemdServiceStatus struct {
	Unit          string `json:"unit"`
	LoadState     string `json:"load_state"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state,omitempty"`
}

// FirewallStatus captures host firewall activation and default policy posture.
type FirewallStatus struct {
	Backend             string   `json:"backend"` // "ufw", "nftables", "iptables", "none", "unknown"
	Active              bool     `json:"active"`
	DefaultInputPolicy  string   `json:"default_input_policy,omitempty"`
	BlocklistSetPresent bool     `json:"blocklist_set_present"`
	RulesSummary        []string `json:"rules_summary,omitempty"`
	Error               string   `json:"error,omitempty"`
}

// ListeningPort describes an active listening socket on the host.
type ListeningPort struct {
	Protocol    string `json:"protocol"` // "tcp", "tcp6", "udp", "udp6"
	LocalAddr   string `json:"local_addr"`
	LocalPort   uint16 `json:"local_port"`
	IsWildcard  bool   `json:"is_wildcard"`
	State       string `json:"state"`
	PID         int    `json:"pid,omitempty"`
	ProcessName string `json:"process_name,omitempty"`
	Inode       string `json:"inode,omitempty"`
}

// HostUserInfo describes an OS account and its sudo/privileged group memberships.
type HostUserInfo struct {
	Username      string   `json:"username"`
	UID           int      `json:"uid"`
	GID           int      `json:"gid"`
	HomeDir       string   `json:"home_dir"`
	Shell         string   `json:"shell"`
	HasLoginShell bool     `json:"has_login_shell"`
	IsRootUID     bool     `json:"is_root_uid"`
	Groups        []string `json:"groups,omitempty"`
	HasSudo       bool     `json:"has_sudo"`
	SudoRules     []string `json:"sudo_rules,omitempty"`
}

// PendingPackageUpdate describes an available package upgrade reported by apt simulation.
type PendingPackageUpdate struct {
	Package        string `json:"package"`
	CurrentVersion string `json:"current_version,omitempty"`
	NewVersion     string `json:"new_version"`
	Archive        string `json:"archive,omitempty"`
	IsSecurity     bool   `json:"is_security"`
}

// SecurityUpdateStatus captures pending package and security patch status.
type SecurityUpdateStatus struct {
	Checked         bool                   `json:"checked"`
	PendingCount    int                    `json:"pending_count"`
	SecurityCount   int                    `json:"security_count"`
	PendingPackages []PendingPackageUpdate `json:"pending_packages,omitempty"`
	Source          string                 `json:"source,omitempty"`
	Error           string                 `json:"error,omitempty"`
}

// HostAssetSnapshot is the complete read-only R9.1 host inventory snapshot.
type HostAssetSnapshot struct {
	Platform        PlatformInfo           `json:"platform"`
	Hostname        string                 `json:"hostname"`
	KernelVersion   string                 `json:"kernel_version"`
	CPUArch         string                 `json:"cpu_arch"`
	OpenSSH         *ServiceVersionInfo    `json:"openssh,omitempty"`
	OpenSSL         *ServiceVersionInfo    `json:"openssl,omitempty"`
	DockerEngine    *DockerEngineInfo      `json:"docker_engine,omitempty"`
	Packages        []OSPackage            `json:"packages,omitempty"`
	PackageCount    int                    `json:"package_count"`
	SystemdServices []SystemdServiceStatus `json:"systemd_services,omitempty"`
	Firewall        *FirewallStatus        `json:"firewall,omitempty"`
	ListeningPorts  []ListeningPort        `json:"listening_ports,omitempty"`
	Users           []HostUserInfo         `json:"users,omitempty"`
	SecurityUpdates *SecurityUpdateStatus  `json:"security_updates,omitempty"`
	Warnings        []string               `json:"warnings,omitempty"`
	CollectedAt     time.Time              `json:"collected_at"`
}

// HostSecurityProvider defines the read-only host asset collection contract (R9.1).
type HostSecurityProvider interface {
	CollectAssets(ctx context.Context) (*HostAssetSnapshot, error)
}

type execReadOnlyRunner struct{}

// allowlistedReadOnlyCommands enforces that HostSecurityProvider can NEVER run mutating binaries.
var allowlistedReadOnlyCommands = map[string]bool{
	"systemctl": true,
	"ufw":       true,
	"nft":       true,
	"iptables":  true,
	"ipset":     true,
	"sshd":      true,
	"apt-get":   true,
	"uname":     true,
}

func (r execReadOnlyRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	base := filepath.Base(strings.TrimSpace(name))
	if !allowlistedReadOnlyCommands[base] {
		return "", "", fmt.Errorf("read-only runner refused non-allowlisted command: %s", base)
	}
	if base == "apt-get" {
		// Strictly require dry-run simulation for read-only asset inspection and forbid mutating verbs/flags.
		hasSim := false
		hasUpgrade := false
		for _, a := range args {
			lowerArg := strings.ToLower(strings.TrimSpace(a))
			switch lowerArg {
			case "-s", "--simulate", "--dry-run":
				hasSim = true
			case "upgrade":
				hasUpgrade = true
			case "install", "remove", "purge", "autoremove", "dist-upgrade", "full-upgrade", "dselect-upgrade", "--only-upgrade", "--allow-downgrades":
				return "", "", fmt.Errorf("read-only runner refused mutating apt-get argument %q (Tier A is strictly read-only)", a)
			}
		}
		if !hasSim || !hasUpgrade {
			return "", "", fmt.Errorf("read-only runner refused non-simulated apt-get command")
		}
	}
	cmd := exec.CommandContext(ctx, base, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// DefaultHostSecurityProvider collects R9.1 host assets strictly read-only.
type DefaultHostSecurityProvider struct {
	Detector     *PlatformDetector
	DockerSocket string
	Runner       ReadOnlyCommandRunner
	MonitoredSys []string
}

// NewDefaultHostSecurityProvider creates a read-only host asset collector.
func NewDefaultHostSecurityProvider(hostRoot, dockerSocket string) *DefaultHostSecurityProvider {
	if strings.TrimSpace(dockerSocket) == "" {
		dockerSocket = "/var/run/docker.sock"
	}
	return &DefaultHostSecurityProvider{
		Detector:     NewPlatformDetector(hostRoot),
		DockerSocket: dockerSocket,
		Runner:       execReadOnlyRunner{},
		MonitoredSys: []string{"ssh", "sshd", "docker", "containerd", "ufw", "nftables", "fail2ban", "tailscaled"},
	}
}

func (p *DefaultHostSecurityProvider) resolvePath(rel string) string {
	if p.Detector == nil {
		return filepath.Join("/", rel)
	}
	return p.Detector.resolvePath(rel)
}

// CollectAssets gathers R9.1 host OS, package, OpenSSH, Docker Engine, systemd,
// firewall, listening ports, user/sudo, and security update status read-only.
func (p *DefaultHostSecurityProvider) CollectAssets(ctx context.Context) (*HostAssetSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.Detector == nil {
		p.Detector = NewPlatformDetector("")
	}
	if p.Runner == nil {
		p.Runner = execReadOnlyRunner{}
	}

	platform := p.Detector.Detect(ctx)
	hostname := ""
	if hb, err := os.ReadFile(p.resolvePath("etc/hostname")); err == nil {
		hostname = strings.TrimSpace(string(hb))
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	snap := &HostAssetSnapshot{
		Platform:      platform,
		Hostname:      hostname,
		KernelVersion: platform.KernelVersion,
		CPUArch:       platform.KernelArch,
		CollectedAt:   time.Now().UTC(),
	}

	// 1. Installed OS packages from /var/lib/dpkg/status
	dpkgPath := p.resolvePath("var/lib/dpkg/status")
	if rawDpkg, err := os.ReadFile(dpkgPath); err == nil {
		pkgs := ParseDpkgStatus(string(rawDpkg))
		snap.Packages = pkgs
		snap.PackageCount = len(pkgs)
		for _, pkg := range pkgs {
			switch pkg.Name {
			case "openssh-server", "openssh-client":
				if snap.OpenSSH == nil || pkg.Name == "openssh-server" {
					snap.OpenSSH = &ServiceVersionInfo{
						Name:           "openssh",
						Version:        pkg.Version,
						PackageName:    pkg.Name,
						PackageVersion: pkg.Version,
						Source:         "dpkg",
					}
				}
			case "openssl", "libssl3", "libssl1.1":
				if snap.OpenSSL == nil || pkg.Name == "openssl" {
					snap.OpenSSL = &ServiceVersionInfo{
						Name:           "openssl",
						Version:        pkg.Version,
						PackageName:    pkg.Name,
						PackageVersion: pkg.Version,
						Source:         "dpkg",
					}
				}
			}
		}
	} else {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("dpkg status unavailable at %s: %v", dpkgPath, err))
	}

	// Fallback OpenSSH banner check if dpkg status didn't contain openssh
	if snap.OpenSSH == nil {
		if _, errOut, _ := p.Runner.Run(ctx, "sshd", "-V"); strings.TrimSpace(errOut) != "" {
			if ver := parseOpenSSHVersionBanner(errOut); ver != "" {
				snap.OpenSSH = &ServiceVersionInfo{
					Name:    "openssh",
					Version: ver,
					Source:  "sshd -V",
				}
			}
		}
	}

	// 2. Docker Engine version & socket metadata
	snap.DockerEngine = p.collectDockerEngine(ctx)

	// 3. Systemd unit status
	snap.SystemdServices = p.collectSystemdServices(ctx)

	// 4. Firewall status
	snap.Firewall = p.collectFirewallStatus(ctx)

	// 5. Listening ports from /proc/net/{tcp,tcp6,udp,udp6}
	ports, portWarn := p.collectListeningPorts()
	snap.ListeningPorts = ports
	if portWarn != "" {
		snap.Warnings = append(snap.Warnings, portWarn)
	}

	// 6. Users & sudo privileges
	users, userWarn := p.collectUsersAndSudo()
	snap.Users = users
	if userWarn != "" {
		snap.Warnings = append(snap.Warnings, userWarn)
	}

	// 7. Security update status (read-only apt simulation)
	snap.SecurityUpdates = p.collectSecurityUpdates(ctx, platform)

	return snap, nil
}

// ParseDpkgStatus parses Debian/Ubuntu/fnOS /var/lib/dpkg/status into installed packages.
func ParseDpkgStatus(content string) []OSPackage {
	var pkgs []OSPackage
	stanzas := strings.Split(content, "\n\n")
	for _, stanza := range stanzas {
		stanza = strings.TrimSpace(stanza)
		if stanza == "" {
			continue
		}
		var pkg OSPackage
		for _, rawLine := range strings.Split(stanza, "\n") {
			if strings.HasPrefix(rawLine, " ") || strings.HasPrefix(rawLine, "\t") {
				continue
			}
			idx := strings.IndexByte(rawLine, ':')
			if idx <= 0 {
				continue
			}
			key := strings.TrimSpace(rawLine[:idx])
			val := strings.TrimSpace(rawLine[idx+1:])
			switch key {
			case "Package":
				pkg.Name = val
			case "Version":
				pkg.Version = val
			case "Architecture":
				pkg.Architecture = val
			case "Status":
				pkg.Status = val
			case "Source":
				// Source can be "openssh (1:9.2p1-2+deb12u3)" or just "openssh"
				if spIdx := strings.IndexByte(val, '('); spIdx > 0 {
					pkg.SourceName = strings.TrimSpace(val[:spIdx])
					endIdx := strings.IndexByte(val[spIdx+1:], ')')
					if endIdx > 0 {
						pkg.SourceVersion = strings.TrimSpace(val[spIdx+1 : spIdx+1+endIdx])
					}
				} else {
					pkg.SourceName = val
				}
			}
		}
		if pkg.Name == "" || pkg.Version == "" {
			continue
		}
		if pkg.Status != "" && !strings.Contains(pkg.Status, "installed") {
			continue
		}
		if pkg.SourceName == "" {
			pkg.SourceName = pkg.Name
		}
		if pkg.SourceVersion == "" {
			pkg.SourceVersion = pkg.Version
		}
		pkgs = append(pkgs, pkg)
	}
	sort.Slice(pkgs, func(i, j int) bool {
		return pkgs[i].Name < pkgs[j].Name
	})
	return pkgs
}

func parseOpenSSHVersionBanner(output string) string {
	for _, tok := range strings.Fields(output) {
		if strings.HasPrefix(tok, "OpenSSH_") {
			return strings.TrimSuffix(strings.TrimPrefix(tok, "OpenSSH_"), ",")
		}
	}
	return ""
}

func (p *DefaultHostSecurityProvider) collectDockerEngine(ctx context.Context) *DockerEngineInfo {
	info := &DockerEngineInfo{SocketPath: p.DockerSocket}
	fi, err := os.Stat(p.DockerSocket)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.SocketMode = fmt.Sprintf("%04o", fi.Mode().Perm())
	if uid, gid, ok := ExtractFileOwner(fi); ok {
		info.SocketUID = uid
		info.SocketGID = gid
	}

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", p.DockerSocket)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	verReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/version", nil)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	verResp, err := client.Do(verReq)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	defer verResp.Body.Close()
	if verResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(verResp.Body, 1024))
		info.Error = fmt.Sprintf("docker /version HTTP %d: %s", verResp.StatusCode, strings.TrimSpace(string(b)))
		return info
	}

	var verPayload struct {
		Version       string `json:"Version"`
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
		KernelVersion string `json:"KernelVersion"`
		Os            string `json:"Os"`
		Arch          string `json:"Arch"`
	}
	if err := json.NewDecoder(verResp.Body).Decode(&verPayload); err != nil {
		info.Error = err.Error()
		return info
	}
	info.Available = true
	info.Version = verPayload.Version
	info.APIVersion = verPayload.APIVersion
	info.MinAPIVersion = verPayload.MinAPIVersion
	info.KernelVersion = verPayload.KernelVersion
	info.OS = verPayload.Os
	info.Arch = verPayload.Arch

	infoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/info", nil)
	if err == nil {
		if infoResp, err2 := client.Do(infoReq); err2 == nil {
			defer infoResp.Body.Close()
			if infoResp.StatusCode == http.StatusOK {
				var daemonInfo struct {
					SecurityOptions []string `json:"SecurityOptions"`
				}
				if json.NewDecoder(infoResp.Body).Decode(&daemonInfo) == nil {
					info.SecurityOptions = daemonInfo.SecurityOptions
					for _, opt := range daemonInfo.SecurityOptions {
						if strings.Contains(strings.ToLower(opt), "rootless") {
							info.Rootless = true
						}
					}
				}
			}
		}
	}
	return info
}

func (p *DefaultHostSecurityProvider) collectSystemdServices(ctx context.Context) []SystemdServiceStatus {
	out, _, err := p.Runner.Run(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-pager", "--no-legend")
	if err == nil && strings.TrimSpace(out) != "" {
		return ParseSystemctlListUnits(out)
	}

	// Fallback: inspect unit files under host /etc/systemd/system and /lib/systemd/system
	var services []SystemdServiceStatus
	seen := make(map[string]bool)
	for _, svc := range p.MonitoredSys {
		unit := svc
		if !strings.HasSuffix(unit, ".service") {
			unit += ".service"
		}
		for _, dir := range []string{"etc/systemd/system", "lib/systemd/system", "usr/lib/systemd/system"} {
			full := p.resolvePath(filepath.Join(dir, unit))
			if _, statErr := os.Stat(full); statErr == nil && !seen[unit] {
				seen[unit] = true
				services = append(services, SystemdServiceStatus{
					Unit:          unit,
					LoadState:     "loaded",
					ActiveState:   "unknown",
					SubState:      "unit-file-present",
					UnitFileState: "present",
				})
			}
		}
	}
	return services
}

// ParseSystemctlListUnits parses `systemctl list-units --type=service --all --no-pager --no-legend`.
func ParseSystemctlListUnits(output string) []SystemdServiceStatus {
	var out []SystemdServiceStatus
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "●")
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 {
			continue
		}
		out = append(out, SystemdServiceStatus{
			Unit:        fields[0],
			LoadState:   fields[1],
			ActiveState: fields[2],
			SubState:    fields[3],
		})
	}
	return out
}

func (p *DefaultHostSecurityProvider) collectFirewallStatus(ctx context.Context) *FirewallStatus {
	fw := &FirewallStatus{Backend: "none"}

	// Check ipset hdc_blocklist presence
	if out, _, err := p.Runner.Run(ctx, "ipset", "list", "-n"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == "hdc_blocklist" {
				fw.BlocklistSetPresent = true
				break
			}
		}
	}

	// 1. Check UFW
	if out, _, err := p.Runner.Run(ctx, "ufw", "status", "verbose"); err == nil && strings.TrimSpace(out) != "" {
		parsed := ParseUFWStatus(out)
		parsed.BlocklistSetPresent = fw.BlocklistSetPresent
		return &parsed
	}

	// 2. Check nftables
	if out, _, err := p.Runner.Run(ctx, "nft", "list", "ruleset"); err == nil && strings.TrimSpace(out) != "" {
		fw.Backend = "nftables"
		fw.Active = true
		for _, line := range strings.Split(out, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "chain ") || strings.Contains(t, "policy ") {
				fw.RulesSummary = append(fw.RulesSummary, t)
			}
			if strings.Contains(t, "policy drop") {
				fw.DefaultInputPolicy = "DROP"
			} else if strings.Contains(t, "policy accept") && fw.DefaultInputPolicy == "" {
				fw.DefaultInputPolicy = "ACCEPT"
			}
		}
		return fw
	}

	// 3. Check iptables
	if out, _, err := p.Runner.Run(ctx, "iptables", "-S"); err == nil && strings.TrimSpace(out) != "" {
		parsed := ParseIptablesSave(out)
		parsed.BlocklistSetPresent = fw.BlocklistSetPresent
		return &parsed
	}

	fw.Error = "no host firewall tool accessible (ufw/nft/iptables)"
	return fw
}

// ParseUFWStatus parses `ufw status verbose`.
func ParseUFWStatus(output string) FirewallStatus {
	fw := FirewallStatus{Backend: "ufw"}
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "status:") {
			if strings.Contains(lower, "active") && !strings.Contains(lower, "inactive") {
				fw.Active = true
			}
		} else if strings.HasPrefix(lower, "default:") {
			if strings.Contains(lower, "deny (incoming)") || strings.Contains(lower, "drop (incoming)") {
				fw.DefaultInputPolicy = "DROP"
			} else if strings.Contains(lower, "allow (incoming)") {
				fw.DefaultInputPolicy = "ACCEPT"
			}
		} else if line != "" && !strings.HasPrefix(line, "--") && !strings.HasPrefix(lower, "to ") {
			fw.RulesSummary = append(fw.RulesSummary, line)
		}
	}
	return fw
}

// ParseIptablesSave parses `iptables -S` output.
func ParseIptablesSave(output string) FirewallStatus {
	fw := FirewallStatus{Backend: "iptables"}
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-P INPUT ") {
			fw.DefaultInputPolicy = strings.TrimSpace(strings.TrimPrefix(line, "-P INPUT "))
			if fw.DefaultInputPolicy == "DROP" || fw.DefaultInputPolicy == "REJECT" {
				fw.Active = true
			}
		}
		if strings.HasPrefix(line, "-A ") || strings.HasPrefix(line, "-P ") {
			fw.RulesSummary = append(fw.RulesSummary, line)
			if strings.HasPrefix(line, "-A ") {
				fw.Active = true
			}
		}
	}
	return fw
}

func (p *DefaultHostSecurityProvider) collectListeningPorts() ([]ListeningPort, string) {
	procBase := p.resolvePath("proc")
	if _, err := os.Stat(filepath.Join(procBase, "net", "tcp")); err != nil {
		procBase = "/proc"
	}

	inodeProcs := scanProcInodes(procBase)
	var ports []ListeningPort
	var missing []string

	specs := []struct {
		file  string
		proto string
		isV6  bool
	}{
		{"net/tcp", "tcp", false},
		{"net/tcp6", "tcp6", true},
		{"net/udp", "udp", false},
		{"net/udp6", "udp6", true},
	}

	for _, sp := range specs {
		content, err := os.ReadFile(filepath.Join(procBase, sp.file))
		if err != nil {
			missing = append(missing, sp.file)
			continue
		}
		parsed := ParseProcNetSockets(string(content), sp.proto, sp.isV6)
		for i := range parsed {
			if procInfo, ok := inodeProcs[parsed[i].Inode]; ok {
				parsed[i].PID = procInfo.pid
				parsed[i].ProcessName = procInfo.comm
			}
		}
		ports = append(ports, parsed...)
	}

	sort.Slice(ports, func(i, j int) bool {
		if ports[i].LocalPort != ports[j].LocalPort {
			return ports[i].LocalPort < ports[j].LocalPort
		}
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		return ports[i].LocalAddr < ports[j].LocalAddr
	})

	warn := ""
	if len(missing) == len(specs) {
		warn = "procfs network socket tables inaccessible"
	}
	return ports, warn
}

type procInodeOwner struct {
	pid  int
	comm string
}

func scanProcInodes(procBase string) map[string]procInodeOwner {
	owners := make(map[string]procInodeOwner)
	entries, err := os.ReadDir(procBase)
	if err != nil {
		return owners
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		commBytes, _ := os.ReadFile(filepath.Join(procBase, ent.Name(), "comm"))
		comm := strings.TrimSpace(string(commBytes))
		fds, err := os.ReadDir(filepath.Join(procBase, ent.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(procBase, ent.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
				inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
				owners[inode] = procInodeOwner{pid: pid, comm: comm}
			}
		}
	}
	return owners
}

// ParseProcNetSockets parses Linux /proc/net/{tcp,tcp6,udp,udp6} for listening/bound sockets.
func ParseProcNetSockets(content, proto string, isV6 bool) []ListeningPort {
	var out []ListeningPort
	lines := strings.Split(content, "\n")
	for i, rawLine := range lines {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(strings.TrimSpace(rawLine))
		if len(fields) < 10 {
			continue
		}
		stateHex := strings.ToUpper(fields[3])
		// TCP LISTEN = 0A; UDP unbound/listening = 07
		isListen := false
		stateName := ""
		if strings.HasPrefix(proto, "tcp") && stateHex == "0A" {
			isListen = true
			stateName = "LISTEN"
		} else if strings.HasPrefix(proto, "udp") && stateHex == "07" {
			isListen = true
			stateName = "UNCONN"
		}
		if !isListen {
			continue
		}

		ipStr, port, err := parseProcHexAddrPort(fields[1], isV6)
		if err != nil || port == 0 {
			continue
		}
		isWildcard := ipStr == "0.0.0.0" || ipStr == "::"
		out = append(out, ListeningPort{
			Protocol:   proto,
			LocalAddr:  ipStr,
			LocalPort:  port,
			IsWildcard: isWildcard,
			State:      stateName,
			Inode:      fields[9],
		})
	}
	return out
}

func parseProcHexAddrPort(addrPort string, isV6 bool) (string, uint16, error) {
	parts := strings.SplitN(addrPort, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid proc addr: %s", addrPort)
	}
	portVal, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return "", 0, err
	}
	rawHex, err := hex.DecodeString(parts[0])
	if err != nil {
		return "", 0, err
	}
	if !isV6 && len(rawHex) == 4 {
		// Linux /proc/net/tcp stores IPv4 in little-endian 32-bit hex on x86/arm64 LE
		ip := net.IPv4(rawHex[3], rawHex[2], rawHex[1], rawHex[0])
		return ip.String(), uint16(portVal), nil
	}
	if isV6 && len(rawHex) == 16 {
		// Each 32-bit word is little-endian in /proc/net/tcp6
		ipBytes := make([]byte, 16)
		for w := 0; w < 4; w++ {
			off := w * 4
			ipBytes[off+0] = rawHex[off+3]
			ipBytes[off+1] = rawHex[off+2]
			ipBytes[off+2] = rawHex[off+1]
			ipBytes[off+3] = rawHex[off+0]
		}
		ip := net.IP(ipBytes)
		return ip.String(), uint16(portVal), nil
	}
	return "", 0, fmt.Errorf("unexpected hex addr length %d", len(rawHex))
}

func (p *DefaultHostSecurityProvider) collectUsersAndSudo() ([]HostUserInfo, string) {
	passwdPath := p.resolvePath("etc/passwd")
	passwdBytes, err := os.ReadFile(passwdPath)
	if err != nil {
		return nil, fmt.Sprintf("host passwd unavailable at %s: %v", passwdPath, err)
	}

	var groupBytes []byte
	if gb, gErr := os.ReadFile(p.resolvePath("etc/group")); gErr == nil {
		groupBytes = gb
	}

	var sudoersSnippets []string
	if sb, sErr := os.ReadFile(p.resolvePath("etc/sudoers")); sErr == nil {
		sudoersSnippets = append(sudoersSnippets, string(sb))
	}
	sudoDir := p.resolvePath("etc/sudoers.d")
	if entries, dErr := os.ReadDir(sudoDir); dErr == nil {
		for _, ent := range entries {
			if ent.IsDir() || strings.HasPrefix(ent.Name(), ".") || strings.HasSuffix(ent.Name(), "~") {
				continue
			}
			if b, rErr := os.ReadFile(filepath.Join(sudoDir, ent.Name())); rErr == nil {
				sudoersSnippets = append(sudoersSnippets, string(b))
			}
		}
	}

	return ParseUsersAndSudo(string(passwdBytes), string(groupBytes), strings.Join(sudoersSnippets, "\n")), ""
}

// ParseUsersAndSudo parses /etc/passwd, /etc/group, and /etc/sudoers* contents.
func ParseUsersAndSudo(passwdContent, groupContent, sudoersContent string) []HostUserInfo {
	userGroups := make(map[string][]string)
	groupByGID := make(map[int]string)

	for _, rawLine := range strings.Split(groupContent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue
		}
		gName := parts[0]
		if gid, err := strconv.Atoi(parts[2]); err == nil {
			groupByGID[gid] = gName
		}
		for _, member := range strings.Split(parts[3], ",") {
			m := strings.TrimSpace(member)
			if m != "" {
				userGroups[m] = append(userGroups[m], gName)
			}
		}
	}

	sudoUserRules := make(map[string][]string)
	sudoGroupRules := make(map[string][]string)
	for _, rawLine := range strings.Split(sudoersContent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "Defaults") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		subject := fields[0]
		if strings.HasPrefix(subject, "%") {
			g := strings.TrimPrefix(subject, "%")
			sudoGroupRules[g] = append(sudoGroupRules[g], line)
		} else {
			sudoUserRules[subject] = append(sudoUserRules[subject], line)
		}
	}

	var users []HostUserInfo
	for _, rawLine := range strings.Split(passwdContent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 {
			continue
		}
		uid, err1 := strconv.Atoi(parts[2])
		gid, err2 := strconv.Atoi(parts[3])
		if err1 != nil || err2 != nil {
			continue
		}
		uname := parts[0]
		shell := parts[6]
		hasLogin := shell != "" &&
			!strings.HasSuffix(shell, "/nologin") &&
			!strings.HasSuffix(shell, "/false") &&
			!strings.HasSuffix(shell, "/sync")

		groups := append([]string{}, userGroups[uname]...)
		if primaryG, ok := groupByGID[gid]; ok {
			found := false
			for _, g := range groups {
				if g == primaryG {
					found = true
					break
				}
			}
			if !found {
				groups = append(groups, primaryG)
			}
		}
		sort.Strings(groups)

		hasSudo := uid == 0
		var rules []string
		if r, ok := sudoUserRules[uname]; ok {
			hasSudo = true
			rules = append(rules, r...)
		}
		for _, g := range groups {
			if g == "sudo" || g == "wheel" || g == "admin" {
				hasSudo = true
			}
			if r, ok := sudoGroupRules[g]; ok {
				hasSudo = true
				rules = append(rules, r...)
			}
		}

		users = append(users, HostUserInfo{
			Username:      uname,
			UID:           uid,
			GID:           gid,
			HomeDir:       parts[5],
			Shell:         shell,
			HasLoginShell: hasLogin,
			IsRootUID:     uid == 0,
			Groups:        groups,
			HasSudo:       hasSudo,
			SudoRules:     rules,
		})
	}

	sort.Slice(users, func(i, j int) bool {
		if users[i].UID != users[j].UID {
			return users[i].UID < users[j].UID
		}
		return users[i].Username < users[j].Username
	})
	return users
}

func (p *DefaultHostSecurityProvider) collectSecurityUpdates(ctx context.Context, platform PlatformInfo) *SecurityUpdateStatus {
	if platform.PackageManager != PkgManagerAptDpkg {
		return &SecurityUpdateStatus{
			Checked: false,
			Source:  string(platform.PackageManager),
			Error:   fmt.Sprintf("apt security update simulation skipped (package_manager=%s)", platform.PackageManager),
		}
	}
	out, errOut, err := p.Runner.Run(ctx, "apt-get", "-s", "upgrade")
	if err != nil {
		return &SecurityUpdateStatus{
			Checked: false,
			Source:  "apt-get -s upgrade",
			Error:   fmt.Sprintf("%v: %s", err, strings.TrimSpace(errOut)),
		}
	}
	updates := ParseAptSimulateUpgrade(out)
	secCount := 0
	for _, u := range updates {
		if u.IsSecurity {
			secCount++
		}
	}
	return &SecurityUpdateStatus{
		Checked:         true,
		PendingCount:    len(updates),
		SecurityCount:   secCount,
		PendingPackages: updates,
		Source:          "apt-get -s upgrade",
	}
}

// ParseAptSimulateUpgrade parses `apt-get -s upgrade` lines of the form:
// `Inst openssh-server [1:9.2p1-2+deb12u2] (1:9.2p1-2+deb12u3 Debian-Security:12/stable-security [amd64])`
func ParseAptSimulateUpgrade(output string) []PendingPackageUpdate {
	var updates []PendingPackageUpdate
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Inst "))
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		pkg := fields[0]
		curVer := ""
		newVer := ""
		archive := ""

		idx := 1
		if strings.HasPrefix(fields[idx], "[") && strings.HasSuffix(fields[idx], "]") {
			curVer = strings.TrimSuffix(strings.TrimPrefix(fields[idx], "["), "]")
			idx++
		}
		if idx < len(fields) && strings.HasPrefix(fields[idx], "(") {
			newVer = strings.TrimPrefix(fields[idx], "(")
			newVer = strings.TrimSuffix(newVer, ")")
			if idx+1 < len(fields) {
				archive = strings.TrimSuffix(strings.Join(fields[idx+1:], " "), ")")
			}
		}
		isSec := strings.Contains(strings.ToLower(archive), "security")
		if newVer != "" {
			updates = append(updates, PendingPackageUpdate{
				Package:        pkg,
				CurrentVersion: curVer,
				NewVersion:     newVer,
				Archive:        archive,
				IsSecurity:     isSec,
			})
		}
	}
	return updates
}
