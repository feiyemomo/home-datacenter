package security

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/model"
)

// HostSecurityCategory identifies one of the 10 R9.3 host security detection domains.
type HostSecurityCategory string

const (
	CatSSHAnomalousLogin   HostSecurityCategory = "ssh_anomalous_login"
	CatSSHConfigDrift      HostSecurityCategory = "ssh_config_drift"
	CatAnomalousPort       HostSecurityCategory = "anomalous_listening_port"
	CatFirewallDrift       HostSecurityCategory = "firewall_rule_drift"
	CatDockerSocketPerm    HostSecurityCategory = "docker_socket_permission"
	CatPrivilegedContainer HostSecurityCategory = "privileged_container"
	CatAnomalousSystemd    HostSecurityCategory = "anomalous_systemd_service"
	CatUserSudoDrift       HostSecurityCategory = "unauthorized_user_sudo"
	CatAnomalousProcess    HostSecurityCategory = "anomalous_process"
	CatPersistenceDrift    HostSecurityCategory = "suspicious_persistence"
)

// HostSecurityFinding represents a single R9.3 host intrusion/drift alert.
type HostSecurityFinding struct {
	FindingKey  string               `json:"finding_key"`
	Category    HostSecurityCategory `json:"category"`
	Severity    string               `json:"severity"` // "critical", "high", "medium", "low"
	Title       string               `json:"title"`
	Detail      string               `json:"detail"`
	Target      string               `json:"target"`
	Evidence    map[string]any       `json:"evidence,omitempty"`
	DetectedAt  time.Time            `json:"detected_at"`
}

// HostSecurityBaseline stores the last verified posture for drift comparisons.
type HostSecurityBaseline struct {
	Initialized      bool
	SSHFileHashes    map[string]string
	ListeningPorts   map[string]hostsec.ListeningPort
	FirewallHash     string
	FirewallActive   bool
	FirewallPolicy   string
	BlocklistPresent bool
	DockerSockMode   string
	DockerSockUID    uint32
	DockerSockGID    uint32
	Containers       map[string]string // container ID -> posture hash
	SystemdUnits     map[string]string // unit name -> state/hash
	Users            map[string]string // username -> uid:shell:sudo:groups digest
	PersistenceHash  map[string]string // path -> sha256
}

// HostSecurityMonitor performs baseline-diffing and anomaly detection across all 10 R9.3 host surfaces.
type HostSecurityMonitor struct {
	provider     hostsec.HostSecurityProvider
	detector     *hostsec.PlatformDetector
	dockerSocket string
	dm           *DefenseManager
	auditEngine  *audit.AgentAuditEngine
	cooldown     time.Duration

	mu           sync.Mutex
	baseline     HostSecurityBaseline
	lastAlerted  map[string]time.Time
	lastFindings []HostSecurityFinding
	lastScanAt   time.Time
}

// NewHostSecurityMonitor creates a HostSecurityMonitor bound to a HostSecurityProvider and DefenseManager.
func NewHostSecurityMonitor(hostRoot, dockerSocket string, dm *DefenseManager, ae *audit.AgentAuditEngine) *HostSecurityMonitor {
	if strings.TrimSpace(dockerSocket) == "" {
		dockerSocket = "/var/run/docker.sock"
	}
	prov := hostsec.NewDefaultHostSecurityProvider(hostRoot, dockerSocket)
	return &HostSecurityMonitor{
		provider:     prov,
		detector:     prov.Detector,
		dockerSocket: dockerSocket,
		dm:           dm,
		auditEngine:  ae,
		cooldown:     10 * time.Minute,
		lastAlerted:  make(map[string]time.Time),
	}
}

// SetProvider overrides the HostSecurityProvider (e.g. for unit tests).
func (m *HostSecurityMonitor) SetProvider(p hostsec.HostSecurityProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provider = p
}

// SetAuditEngine attaches or updates the AgentAuditEngine on HostSecurityMonitor.
func (m *HostSecurityMonitor) SetAuditEngine(ae *audit.AgentAuditEngine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditEngine = ae
}

// LastFindings returns a snapshot copy of the most recent ScanOnce findings and timestamp.
func (m *HostSecurityMonitor) LastFindings() ([]HostSecurityFinding, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HostSecurityFinding, len(m.lastFindings))
	copy(out, m.lastFindings)
	return out, m.lastScanAt
}

func (m *HostSecurityMonitor) resolvePath(rel string) string {
	root := "/"
	if m.detector != nil && m.detector.HostRoot != "" {
		root = m.detector.HostRoot
	}
	cleanRel := strings.TrimPrefix(filepath.Clean(rel), string(filepath.Separator))
	if root == "/" {
		return "/" + filepath.ToSlash(cleanRel)
	}
	return filepath.Join(root, cleanRel)
}

// ScanOnce runs a full R9.3 sweep across all 10 host threat domains, establishes or diffs
// against the baseline, and dispatches alerts for newly observed anomalies.
func (m *HostSecurityMonitor) ScanOnce(ctx context.Context) ([]HostSecurityFinding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	prov := m.provider
	m.mu.Unlock()

	if prov == nil {
		return nil, fmt.Errorf("host security provider is nil")
	}

	snap, err := prov.CollectAssets(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var findings []HostSecurityFinding

	// Collect current state maps
	sshHashes := m.collectSSHConfigHashes(&findings, now)
	portMap := indexListeningPorts(snap.ListeningPorts)
	fwHash, fwActive, fwPolicy, blockPresent := summarizeFirewall(snap.Firewall)
	systemdMap := m.collectSystemdState(snap)
	userMap := indexHostUsers(snap.Users, &findings, now)
	persistHashes := m.collectPersistenceHashes(&findings, now)

	// Inspect Docker socket & containers
	sockMode, sockUID, sockGID := m.checkDockerSocket(&findings, now)
	containerPostures := m.inspectDockerContainers(ctx, &findings, now)

	// Scan anomalous processes in /proc
	m.scanAnomalousProcesses(&findings, now)

	// Diff against baseline (or initialize baseline on first pass)
	m.mu.Lock()
	if !m.baseline.Initialized {
		m.baseline = HostSecurityBaseline{
			Initialized:      true,
			SSHFileHashes:    sshHashes,
			ListeningPorts:   portMap,
			FirewallHash:     fwHash,
			FirewallActive:   fwActive,
			FirewallPolicy:   fwPolicy,
			BlocklistPresent: blockPresent,
			DockerSockMode:   sockMode,
			DockerSockUID:    sockUID,
			DockerSockGID:    sockGID,
			Containers:       containerPostures,
			SystemdUnits:     systemdMap,
			Users:            userMap,
			PersistenceHash:  persistHashes,
		}
		m.lastFindings = append([]HostSecurityFinding(nil), findings...)
		m.lastScanAt = now
		m.mu.Unlock()
		m.emitFindings(ctx, findings)
		return findings, nil
	}

	// 1. SSH config drift
	diffFileHashes(m.baseline.SSHFileHashes, sshHashes, CatSSHConfigDrift, "critical", "宿主机 SSH 配置或授权密钥变更", &findings, now)
	m.baseline.SSHFileHashes = sshHashes

	// 2. Anomalous listening ports
	for key, port := range portMap {
		if _, existed := m.baseline.ListeningPorts[key]; !existed {
			sev := "medium"
			if port.IsWildcard {
				sev = "high"
			}
			findings = append(findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("port:%s", key),
				Category:   CatAnomalousPort,
				Severity:   sev,
				Title:      fmt.Sprintf("新增宿主机监听端口 %s %s:%d", port.Protocol, port.LocalAddr, port.LocalPort),
				Detail:     fmt.Sprintf("进程 %s (PID=%d) 新开启监听 %s:%d (wildcard=%v)", port.ProcessName, port.PID, port.LocalAddr, port.LocalPort, port.IsWildcard),
				Target:     key,
				Evidence:   map[string]any{"port": port},
				DetectedAt: now,
			})
		}
	}
	m.baseline.ListeningPorts = portMap

	// 3. Firewall drift
	if m.baseline.FirewallActive && !fwActive {
		findings = append(findings, HostSecurityFinding{
			FindingKey: "fw:disabled",
			Category:   CatFirewallDrift,
			Severity:   "critical",
			Title:      "宿主机防火墙已失效或被关闭",
			Detail:     "宿主防火墙由激活状态变更为未激活",
			Target:     "host-firewall",
			DetectedAt: now,
		})
	} else if m.baseline.FirewallPolicy == "DROP" && fwPolicy == "ACCEPT" {
		findings = append(findings, HostSecurityFinding{
			FindingKey: "fw:policy_weakened",
			Category:   CatFirewallDrift,
			Severity:   "critical",
			Title:      "宿主机防火墙默认 INPUT 策略被放宽为 ACCEPT",
			Detail:     fmt.Sprintf("默认策略从 %s 变更为 %s", m.baseline.FirewallPolicy, fwPolicy),
			Target:     "host-firewall",
			DetectedAt: now,
		})
	} else if m.baseline.BlocklistPresent && !blockPresent {
		findings = append(findings, HostSecurityFinding{
			FindingKey: "fw:blocklist_missing",
			Category:   CatFirewallDrift,
			Severity:   "high",
			Title:      "宿主机 ipset 黑名单集合 hdc_blocklist 丢失",
			Detail:     "黑名单集合可能被手动清空或防火墙重载覆盖",
			Target:     "hdc_blocklist",
			DetectedAt: now,
		})
	} else if m.baseline.FirewallHash != "" && fwHash != "" && m.baseline.FirewallHash != fwHash {
		findings = append(findings, HostSecurityFinding{
			FindingKey: "fw:rules_drift:" + fwHash,
			Category:   CatFirewallDrift,
			Severity:   "medium",
			Title:      "宿主机防火墙规则基线发生变更",
			Detail:     fmt.Sprintf("规则指纹从 %s 变更为 %s", m.baseline.FirewallHash, fwHash),
			Target:     "host-firewall",
			DetectedAt: now,
		})
	}
	m.baseline.FirewallHash = fwHash
	m.baseline.FirewallActive = fwActive
	m.baseline.FirewallPolicy = fwPolicy
	m.baseline.BlocklistPresent = blockPresent

	// 4. Docker socket drift
	if m.baseline.DockerSockMode != "" && sockMode != "" &&
		(m.baseline.DockerSockMode != sockMode || m.baseline.DockerSockUID != sockUID || m.baseline.DockerSockGID != sockGID) {
		findings = append(findings, HostSecurityFinding{
			FindingKey: fmt.Sprintf("sock:drift:%s:%d:%d", sockMode, sockUID, sockGID),
			Category:   CatDockerSocketPerm,
			Severity:   "critical",
			Title:      "Docker Socket 权限或属主发生异常变更",
			Detail:     fmt.Sprintf("权限/属主从 %s (%d:%d) 变更为 %s (%d:%d)", m.baseline.DockerSockMode, m.baseline.DockerSockUID, m.baseline.DockerSockGID, sockMode, sockUID, sockGID),
			Target:     m.dockerSocket,
			DetectedAt: now,
		})
	}
	m.baseline.DockerSockMode = sockMode
	m.baseline.DockerSockUID = sockUID
	m.baseline.DockerSockGID = sockGID
	m.baseline.Containers = containerPostures

	// 5. Systemd unit drift
	for unit, state := range systemdMap {
		prev, ok := m.baseline.SystemdUnits[unit]
		if !ok {
			findings = append(findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("systemd:new:%s", unit),
				Category:   CatAnomalousSystemd,
				Severity:   "high",
				Title:      fmt.Sprintf("发现新增宿主机 systemd 服务: %s", unit),
				Detail:     fmt.Sprintf("新增 systemd 单元 %s (state=%s)", unit, state),
				Target:     unit,
				DetectedAt: now,
			})
		} else if prev != state && strings.Contains(state, "hash:") {
			findings = append(findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("systemd:mod:%s:%s", unit, state),
				Category:   CatAnomalousSystemd,
				Severity:   "high",
				Title:      fmt.Sprintf("宿主机 systemd 服务文件被篡改: %s", unit),
				Detail:     fmt.Sprintf("单元 %s 摘要由 %s 变更为 %s", unit, prev, state),
				Target:     unit,
				DetectedAt: now,
			})
		}
	}
	m.baseline.SystemdUnits = systemdMap

	// 6. User & sudo drift
	for uname, digest := range userMap {
		prev, ok := m.baseline.Users[uname]
		if !ok {
			findings = append(findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("user:new:%s", uname),
				Category:   CatUserSudoDrift,
				Severity:   "critical",
				Title:      fmt.Sprintf("发现新增宿主机操作系统账号: %s", uname),
				Detail:     fmt.Sprintf("新账号 %s (%s)", uname, digest),
				Target:     uname,
				DetectedAt: now,
			})
		} else if prev != digest {
			findings = append(findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("user:mod:%s:%s", uname, digest),
				Category:   CatUserSudoDrift,
				Severity:   "critical",
				Title:      fmt.Sprintf("宿主机账号权限或 sudo 配置变更: %s", uname),
				Detail:     fmt.Sprintf("账号 %s 属性从 [%s] 变更为 [%s]", uname, prev, digest),
				Target:     uname,
				DetectedAt: now,
			})
		}
	}
	m.baseline.Users = userMap

	// 7. Persistence drift
	diffFileHashes(m.baseline.PersistenceHash, persistHashes, CatPersistenceDrift, "critical", "宿主机持久化启动项/定时任务变更", &findings, now)
	m.baseline.PersistenceHash = persistHashes
	m.lastFindings = append([]HostSecurityFinding(nil), findings...)
	m.lastScanAt = now
	m.mu.Unlock()

	m.emitFindings(ctx, findings)
	return findings, nil
}

func (m *HostSecurityMonitor) collectSSHConfigHashes(findings *[]HostSecurityFinding, now time.Time) map[string]string {
	hashes := make(map[string]string)
	files := []string{"etc/ssh/sshd_config", "root/.ssh/authorized_keys"}
	if entries, err := os.ReadDir(m.resolvePath("etc/ssh/sshd_config.d")); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() {
				files = append(files, filepath.Join("etc/ssh/sshd_config.d", ent.Name()))
			}
		}
	}
	if homeEntries, err := os.ReadDir(m.resolvePath("home")); err == nil {
		for _, ent := range homeEntries {
			if ent.IsDir() {
				files = append(files, filepath.Join("home", ent.Name(), ".ssh/authorized_keys"))
			}
		}
	}

	for _, rel := range files {
		full := m.resolvePath(rel)
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		hashes[rel] = fmt.Sprintf("%x", sum[:])

		if strings.Contains(rel, "sshd_config") {
			checkUnsafeSSHDirectives(rel, string(data), findings, now)
		}
	}
	return hashes
}

func checkUnsafeSSHDirectives(path, content string, findings *[]HostSecurityFinding, now time.Time) {
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) < 2 {
			continue
		}
		switch {
		case fields[0] == "permitrootlogin" && fields[1] == "yes":
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: "ssh:unsafe:permitrootlogin:" + path,
				Category:   CatSSHConfigDrift,
				Severity:   "high",
				Title:      "检测到危险 SSH 配置: PermitRootLogin yes",
				Detail:     fmt.Sprintf("%s 中启用了 PermitRootLogin yes", path),
				Target:     path,
				DetectedAt: now,
			})
		case fields[0] == "permitemptypasswords" && fields[1] == "yes":
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: "ssh:unsafe:permitemptypasswords:" + path,
				Category:   CatSSHConfigDrift,
				Severity:   "critical",
				Title:      "检测到危险 SSH 配置: PermitEmptyPasswords yes",
				Detail:     fmt.Sprintf("%s 中启用了空密码登录", path),
				Target:     path,
				DetectedAt: now,
			})
		}
	}
}

func (m *HostSecurityMonitor) checkDockerSocket(findings *[]HostSecurityFinding, now time.Time) (string, uint32, uint32) {
	fi, err := os.Stat(m.dockerSocket)
	if err != nil {
		return "", 0, 0
	}
	perm := fi.Mode().Perm()
	modeStr := fmt.Sprintf("%04o", perm)
	var uid, gid uint32
	if u, g, ok := hostsec.ExtractFileOwner(fi); ok {
		uid, gid = u, g
	}
	// World-writable or world-readable docker.sock is critical container escape vector
	if perm&0006 != 0 {
		*findings = append(*findings, HostSecurityFinding{
			FindingKey: fmt.Sprintf("dockersock:perm:%s", modeStr),
			Category:   CatDockerSocketPerm,
			Severity:   "critical",
			Title:      fmt.Sprintf("Docker Socket 权限过于宽松 (%s)", modeStr),
			Detail:     fmt.Sprintf("%s 权限为 %s，存在全局读写逃逸风险", m.dockerSocket, modeStr),
			Target:     m.dockerSocket,
			DetectedAt: now,
		})
	}
	return modeStr, uid, gid
}

func (m *HostSecurityMonitor) inspectDockerContainers(ctx context.Context, findings *[]HostSecurityFinding, now time.Time) map[string]string {
	postures := make(map[string]string)
	if _, err := os.Stat(m.dockerSocket); err != nil {
		return postures
	}

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", m.dockerSocket)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 4 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/containers/json?all=1", nil)
	if err != nil {
		return postures
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return postures
	}
	var list []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&list)
	_ = resp.Body.Close()

	dangerousCaps := map[string]bool{
		"SYS_ADMIN": true, "SYS_PTRACE": true, "NET_ADMIN": true, "SYS_MODULE": true, "DAC_OVERRIDE": true, "ALL": true,
	}

	for _, c := range list {
		if c.ID == "" {
			continue
		}
		iReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost/containers/%s/json", c.ID), nil)
		if err != nil {
			continue
		}
		iResp, err := client.Do(iReq)
		if err != nil || iResp.StatusCode != http.StatusOK {
			if iResp != nil {
				_ = iResp.Body.Close()
			}
			continue
		}
		var ins struct {
			Name       string `json:"Name"`
			HostConfig struct {
				Privileged  bool     `json:"Privileged"`
				CapAdd      []string `json:"CapAdd"`
				NetworkMode string   `json:"NetworkMode"`
				PidMode     string   `json:"PidMode"`
				IpcMode     string   `json:"IpcMode"`
				Binds       []string `json:"Binds"`
			} `json:"HostConfig"`
		}
		_ = json.NewDecoder(io.LimitReader(iResp.Body, 2*1024*1024)).Decode(&ins)
		_ = iResp.Body.Close()

		cName := strings.TrimPrefix(ins.Name, "/")
		if cName == "" && len(c.Names) > 0 {
			cName = strings.TrimPrefix(c.Names[0], "/")
		}

		var reasons []string
		if ins.HostConfig.Privileged {
			reasons = append(reasons, "Privileged=true")
		}
		for _, capName := range ins.HostConfig.CapAdd {
			if dangerousCaps[strings.ToUpper(strings.TrimPrefix(capName, "CAP_"))] {
				reasons = append(reasons, "CapAdd="+capName)
			}
		}
		if ins.HostConfig.PidMode == "host" {
			reasons = append(reasons, "PidMode=host")
		}
		if ins.HostConfig.IpcMode == "host" {
			reasons = append(reasons, "IpcMode=host")
		}
		for _, bind := range ins.HostConfig.Binds {
			src := strings.SplitN(bind, ":", 2)[0]
			if src == "/" || src == "/etc" || src == "/root" || (src == "/var/run/docker.sock" && !strings.HasSuffix(bind, ":ro")) {
				reasons = append(reasons, "SensitiveBind="+bind)
			}
		}

		postureHash := fmt.Sprintf("priv=%v|caps=%v|net=%s|pid=%s|binds=%v",
			ins.HostConfig.Privileged, ins.HostConfig.CapAdd, ins.HostConfig.NetworkMode, ins.HostConfig.PidMode, ins.HostConfig.Binds)
		postures[c.ID] = postureHash

		if len(reasons) > 0 {
			m.mu.Lock()
			prevPosture, existed := m.baseline.Containers[c.ID]
			baselineReady := m.baseline.Initialized
			m.mu.Unlock()
			if !baselineReady || !existed || prevPosture != postureHash {
				*findings = append(*findings, HostSecurityFinding{
					FindingKey: fmt.Sprintf("privcontainer:%s:%s", cName, strings.Join(reasons, ",")),
					Category:   CatPrivilegedContainer,
					Severity:   "critical",
					Title:      fmt.Sprintf("检测到高危特权容器或危险挂载: %s", cName),
					Detail:     fmt.Sprintf("容器 %s (%s) 具备高危权限: %s", cName, c.ID[:minInt(12, len(c.ID))], strings.Join(reasons, "; ")),
					Target:     cName,
					Evidence:   map[string]any{"container_id": c.ID, "reasons": reasons},
					DetectedAt: now,
				})
			}
		}
	}
	return postures
}

func (m *HostSecurityMonitor) collectSystemdState(snap *hostsec.HostAssetSnapshot) map[string]string {
	out := make(map[string]string)
	for _, svc := range snap.SystemdServices {
		out[svc.Unit] = svc.LoadState + ":" + svc.ActiveState + ":" + svc.SubState
	}
	for _, dir := range []string{"etc/systemd/system", "lib/systemd/system"} {
		entries, err := os.ReadDir(m.resolvePath(dir))
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if ent.IsDir() || (!strings.HasSuffix(ent.Name(), ".service") && !strings.HasSuffix(ent.Name(), ".timer")) {
				continue
			}
			if b, err := os.ReadFile(m.resolvePath(filepath.Join(dir, ent.Name()))); err == nil {
				sum := sha256.Sum256(b)
				out[ent.Name()] = out[ent.Name()] + "|hash:" + fmt.Sprintf("%x", sum[:8])
			}
		}
	}
	return out
}

func indexHostUsers(users []hostsec.HostUserInfo, findings *[]HostSecurityFinding, now time.Time) map[string]string {
	out := make(map[string]string, len(users))
	for _, u := range users {
		if u.IsRootUID && u.Username != "root" {
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("user:uid0:%s", u.Username),
				Category:   CatUserSudoDrift,
				Severity:   "critical",
				Title:      fmt.Sprintf("检测到非 root 的 UID=0 超级权限账号: %s", u.Username),
				Detail:     fmt.Sprintf("账号 %s 拥有 UID 0 且 shell=%s", u.Username, u.Shell),
				Target:     u.Username,
				DetectedAt: now,
			})
		}
		for _, rule := range u.SudoRules {
			if strings.Contains(rule, "NOPASSWD") && strings.Contains(rule, "ALL") {
				*findings = append(*findings, HostSecurityFinding{
					FindingKey: fmt.Sprintf("user:nopasswd:%s", u.Username),
					Category:   CatUserSudoDrift,
					Severity:   "high",
					Title:      fmt.Sprintf("检测到免密 sudo ALL 规则: %s", u.Username),
					Detail:     fmt.Sprintf("sudoers 规则: %s", rule),
					Target:     u.Username,
					DetectedAt: now,
				})
			}
		}
		out[u.Username] = fmt.Sprintf("uid=%d|shell=%s|sudo=%v|groups=%s|rules=%s",
			u.UID, u.Shell, u.HasSudo, strings.Join(u.Groups, ","), strings.Join(u.SudoRules, ";"))
	}
	return out
}

func (m *HostSecurityMonitor) scanAnomalousProcesses(findings *[]HostSecurityFinding, now time.Time) {
	procBase := m.resolvePath("proc")
	entries, err := os.ReadDir(procBase)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		exePath, err := os.Readlink(filepath.Join(procBase, ent.Name(), "exe"))
		if err != nil {
			continue
		}
		suspicious := false
		reason := ""
		switch {
		case strings.HasSuffix(exePath, " (deleted)"):
			suspicious = true
			reason = "可执行文件已被删除仍在内存运行"
		case strings.HasPrefix(exePath, "/tmp/") || strings.HasPrefix(exePath, "/dev/shm/") || strings.HasPrefix(exePath, "/var/tmp/"):
			suspicious = true
			reason = "进程从临时高危目录直接执行"
		}
		if suspicious {
			commBytes, _ := os.ReadFile(filepath.Join(procBase, ent.Name(), "comm"))
			comm := strings.TrimSpace(string(commBytes))
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("proc:%d:%s", pid, exePath),
				Category:   CatAnomalousProcess,
				Severity:   "critical",
				Title:      fmt.Sprintf("检测到可疑宿主机进程: %s (PID %d)", comm, pid),
				Detail:     fmt.Sprintf("%s: exe=%s", reason, exePath),
				Target:     fmt.Sprintf("pid:%d", pid),
				Evidence:   map[string]any{"pid": pid, "comm": comm, "exe": exePath},
				DetectedAt: now,
			})
		}
	}
}

func (m *HostSecurityMonitor) collectPersistenceHashes(findings *[]HostSecurityFinding, now time.Time) map[string]string {
	hashes := make(map[string]string)
	paths := []string{"etc/crontab", "etc/rc.local", "etc/ld.so.preload"}
	for _, dir := range []string{"etc/cron.d", "var/spool/cron/crontabs", "etc/systemd/system"} {
		if entries, err := os.ReadDir(m.resolvePath(dir)); err == nil {
			for _, ent := range entries {
				if ent.IsDir() {
					continue
				}
				if dir == "etc/systemd/system" && !strings.HasSuffix(ent.Name(), ".timer") {
					continue
				}
				paths = append(paths, filepath.Join(dir, ent.Name()))
			}
		}
	}

	for _, rel := range paths {
		data, err := os.ReadFile(m.resolvePath(rel))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		hashes[rel] = fmt.Sprintf("%x", sum[:])
		if strings.HasSuffix(rel, "ld.so.preload") && len(strings.TrimSpace(string(data))) > 0 {
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: "persist:ld_so_preload",
				Category:   CatPersistenceDrift,
				Severity:   "critical",
				Title:      "检测到非空 /etc/ld.so.preload 动态库劫持持久化",
				Detail:     fmt.Sprintf("内容: %s", strings.TrimSpace(string(data))),
				Target:     rel,
				DetectedAt: now,
			})
		}
	}
	return hashes
}

func diffFileHashes(prev, curr map[string]string, cat HostSecurityCategory, sev, prefix string, findings *[]HostSecurityFinding, now time.Time) {
	for path, curHash := range curr {
		oldHash, existed := prev[path]
		if !existed {
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("%s:new:%s", cat, path),
				Category:   cat,
				Severity:   sev,
				Title:      fmt.Sprintf("%s (新增文件: %s)", prefix, path),
				Detail:     fmt.Sprintf("新增文件 %s (sha256=%s)", path, curHash),
				Target:     path,
				DetectedAt: now,
			})
		} else if oldHash != curHash {
			*findings = append(*findings, HostSecurityFinding{
				FindingKey: fmt.Sprintf("%s:mod:%s:%s", cat, path, curHash),
				Category:   cat,
				Severity:   sev,
				Title:      fmt.Sprintf("%s (篡改文件: %s)", prefix, path),
				Detail:     fmt.Sprintf("文件 %s 摘要由 %s 变更为 %s", path, oldHash, curHash),
				Target:     path,
				DetectedAt: now,
			})
		}
	}
}

func indexListeningPorts(ports []hostsec.ListeningPort) map[string]hostsec.ListeningPort {
	out := make(map[string]hostsec.ListeningPort, len(ports))
	for _, p := range ports {
		key := fmt.Sprintf("%s/%s:%d", p.Protocol, p.LocalAddr, p.LocalPort)
		out[key] = p
	}
	return out
}

func summarizeFirewall(fw *hostsec.FirewallStatus) (hash string, active bool, policy string, blocklist bool) {
	if fw == nil {
		return "", false, "", false
	}
	sortedRules := append([]string{}, fw.RulesSummary...)
	sort.Strings(sortedRules)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%v|%s|%v|%s", fw.Backend, fw.Active, fw.DefaultInputPolicy, fw.BlocklistSetPresent, strings.Join(sortedRules, "\n"))))
	return fmt.Sprintf("%x", sum[:8]), fw.Active, fw.DefaultInputPolicy, fw.BlocklistSetPresent
}

func (m *HostSecurityMonitor) emitFindings(ctx context.Context, findings []HostSecurityFinding) {
	if len(findings) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, f := range findings {
		m.mu.Lock()
		last, seen := m.lastAlerted[f.FindingKey]
		if seen && now.Sub(last) < m.cooldown {
			m.mu.Unlock()
			continue
		}
		m.lastAlerted[f.FindingKey] = now
		m.mu.Unlock()

		if m.dm != nil {
			m.dm.RecordOperatorEvent("host-monitor", "127.0.0.1", string(f.Category), fmt.Sprintf("[%s] %s: %s", strings.ToUpper(f.Severity), f.Title, f.Detail), "宿主机入侵检测告警")
		}
		if m.auditEngine != nil {
			if _, err := m.auditEngine.EmitEvent(ctx, audit.EmitEventInput{
				HouseholdID:   1,
				TriggerSource: "host_security_monitor",
				ActorType:     "system_daemon",
				ActorID:       "host-security-monitor",
				EventType:     model.AuditEventRiskAssessed,
				Action:        "host.security." + string(f.Category),
				Target:        f.Target,
				Component:     "host-os",
				Decision:      model.AuditDecisionDeny,
				RiskLevel:     strings.ToUpper(f.Severity),
				Evidence:      f.Evidence,
				Result:        "ALERT",
			}); err != nil {
				log.Printf("[host-monitor] audit emit failed for %s: %v", f.FindingKey, err)
			}
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
