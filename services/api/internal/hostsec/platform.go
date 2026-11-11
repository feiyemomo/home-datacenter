package hostsec

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// OSFamily identifies normalized host operating system families.
type OSFamily string

const (
	OSFamilyFnOS    OSFamily = "fnos"
	OSFamilyDebian  OSFamily = "debian"
	OSFamilyUbuntu  OSFamily = "ubuntu"
	OSFamilyAlpine  OSFamily = "alpine"
	OSFamilyUnknown OSFamily = "unknown"
)

// PackageManagerKind identifies the host package management subsystem.
type PackageManagerKind string

const (
	PkgManagerAptDpkg     PackageManagerKind = "apt-dpkg"
	PkgManagerTrimManaged PackageManagerKind = "trim-managed"
	PkgManagerApk         PackageManagerKind = "apk"
	PkgManagerUnknown     PackageManagerKind = "unknown"
)

var (
	// ErrFnOSUpdateUnsupported is returned fail-closed when a mutating host package/OS
	// update is attempted on fnOS (trim), whose base OS packages are OTA-managed.
	ErrFnOSUpdateUnsupported = errors.New("host package mutation blocked on fnOS (trim): read-only security inspection only; apply OS patches via fnOS official OTA")

	// ErrUnsupportedHostOS is returned fail-closed when the host OS is not a verified
	// Debian/Ubuntu apt-dpkg environment (e.g., unknown or container Alpine rootfs).
	ErrUnsupportedHostOS = errors.New("host package mutation blocked: unsupported or unverified host OS platform")

	// ErrContainerHostExecutionBoundary is returned fail-closed when host package mutation
	// is attempted from inside a container or across a non-root HostRoot without an
	// authenticated RestrictedHostExecutionBridge (direct container apt-get is forbidden).
	ErrContainerHostExecutionBoundary = errors.New("host package mutation blocked across container namespace boundary: authenticated restricted host execution bridge required")
)

// HostCapabilityTier distinguishes strictly isolated privilege tiers so read-only
// asset inspection (A), correlation (B), proposal drafting (C), and approval (D)
// never grant host mutation capability (E).
type HostCapabilityTier string

const (
	CapabilityReadOnlyAssets     HostCapabilityTier = "A_READ_ONLY_ASSETS"
	CapabilityVulnCorrelation    HostCapabilityTier = "B_VULN_CORRELATION"
	CapabilityProposalDraft      HostCapabilityTier = "C_PROPOSAL_DRAFT"
	CapabilityHumanApproval      HostCapabilityTier = "D_HUMAN_APPROVAL"
	CapabilityHostUpdateMutation HostCapabilityTier = "E_HOST_UPDATE_MUTATION"
)

// PlatformInfo captures host OS identity, fnOS compatibility status, and update eligibility.
type PlatformInfo struct {
	OSFamily                OSFamily           `json:"os_family"`
	DistroID                string             `json:"distro_id"`
	DistroIDLike            string             `json:"distro_id_like,omitempty"`
	DistroName              string             `json:"distro_name"`
	PrettyName              string             `json:"pretty_name"`
	VersionID               string             `json:"version_id"`
	Codename                string             `json:"codename,omitempty"`
	KernelVersion           string             `json:"kernel_version"`
	KernelArch              string             `json:"kernel_arch"`
	IsFnOS                  bool               `json:"is_fnos"`
	FnOSVersion             string             `json:"fnos_version,omitempty"`
	PackageManager          PackageManagerKind `json:"package_manager"`
	HostUpdateSupported     bool               `json:"host_update_supported"`
	UnsupportedReason       string             `json:"unsupported_reason,omitempty"`
	Markers                 []string           `json:"markers,omitempty"`
	HostRoot                string             `json:"host_root"`
	InContainer             bool               `json:"in_container"`
	CrossNamespaceRoot      bool               `json:"cross_namespace_root"`
	RequiresExecutionBridge bool               `json:"requires_execution_bridge"`
}

// HostUpdatePolicyError returns a fail-closed error if mutating host package/system
// updates are not permitted on this platform. Read-only inspection remains allowed.
func (p PlatformInfo) HostUpdatePolicyError() error {
	if p.IsFnOS || p.OSFamily == OSFamilyFnOS {
		return fmt.Errorf("%w (distro=%q, markers=%v)", ErrFnOSUpdateUnsupported, p.PrettyName, p.Markers)
	}
	if !p.HostUpdateSupported || p.PackageManager != PkgManagerAptDpkg {
		reason := strings.TrimSpace(p.UnsupportedReason)
		if reason == "" {
			reason = fmt.Sprintf("os_family=%s pkg_manager=%s", p.OSFamily, p.PackageManager)
		}
		return fmt.Errorf("%w: %s", ErrUnsupportedHostOS, reason)
	}
	return nil
}

// RequiresHostExecutionBridge returns true when executing from a container namespace
// or against an external/non-root HostRoot (e.g., /host), where local apt-get execution
// is forbidden and an authenticated RestrictedHostExecutionBridge is mandatory.
func (p PlatformInfo) RequiresHostExecutionBridge() bool {
	return p.InContainer || p.CrossNamespaceRoot || p.RequiresExecutionBridge || isCrossNamespaceHostRoot(p.HostRoot)
}

// Digest returns a deterministic SHA-256 baseline hash of the platform identity.
func (p PlatformInfo) Digest() string {
	b, _ := json.Marshal(struct {
		OSFamily       OSFamily           `json:"os_family"`
		DistroID       string             `json:"distro_id"`
		VersionID      string             `json:"version_id"`
		KernelVersion  string             `json:"kernel_version"`
		KernelArch     string             `json:"kernel_arch"`
		IsFnOS         bool               `json:"is_fnos"`
		PackageManager PackageManagerKind `json:"package_manager"`
	}{
		OSFamily:       p.OSFamily,
		DistroID:       p.DistroID,
		VersionID:      p.VersionID,
		KernelVersion:  p.KernelVersion,
		KernelArch:     p.KernelArch,
		IsFnOS:         p.IsFnOS,
		PackageManager: p.PackageManager,
	})
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum[:])
}

// PlatformDetector inspects the host filesystem to identify Linux distro vs fnOS.
type PlatformDetector struct {
	HostRoot string
	InDocker bool
}

// detectInContainer returns true if the current process appears to be running
// inside a Linux container namespace (Docker, containerd, Podman, Kubernetes, LXC).
func detectInContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if strings.TrimSpace(os.Getenv("container")) != "" {
		return true
	}
	if cgroup, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		lower := strings.ToLower(string(cgroup))
		for _, tok := range []string{"docker", "containerd", "kubepods", "lxc", "podman"} {
			if strings.Contains(lower, tok) {
				return true
			}
		}
	}
	return false
}

func isCrossNamespaceHostRoot(hostRoot string) bool {
	trimmed := strings.TrimSpace(hostRoot)
	if trimmed == "" || trimmed == "/" {
		return false
	}
	return filepath.ToSlash(filepath.Clean(trimmed)) != "/"
}

// NewPlatformDetector constructs a detector, defaulting HostRoot to "/host" when mounted,
// otherwise "/".
func NewPlatformDetector(hostRoot string) *PlatformDetector {
	root := strings.TrimSpace(hostRoot)
	if root == "" {
		if envRoot := strings.TrimSpace(os.Getenv("HOST_ROOT_PATH")); envRoot != "" {
			root = envRoot
		} else if envRoot := strings.TrimSpace(os.Getenv("HOST_ROOT")); envRoot != "" {
			root = envRoot
		} else if _, err := os.Stat("/host/etc/os-release"); err == nil {
			root = "/host"
		} else {
			root = "/"
		}
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == "" {
		cleanRoot = "/"
	}
	return &PlatformDetector{
		HostRoot: cleanRoot,
		InDocker: detectInContainer(),
	}
}

func (d *PlatformDetector) resolvePath(rel string) string {
	cleanRel := strings.TrimPrefix(filepath.Clean(rel), string(filepath.Separator))
	if d.HostRoot == "/" || d.HostRoot == "" {
		return "/" + filepath.ToSlash(cleanRel)
	}
	return filepath.Join(d.HostRoot, cleanRel)
}

// Detect inspects os-release, procfs, and fnOS trim markers to classify the host OS.
func (d *PlatformDetector) Detect(ctx context.Context) PlatformInfo {
	inContainer := d.InDocker || detectInContainer()
	crossNS := isCrossNamespaceHostRoot(d.HostRoot)
	if ctx != nil && ctx.Err() != nil {
		return PlatformInfo{
			OSFamily:                OSFamilyUnknown,
			PackageManager:          PkgManagerUnknown,
			UnsupportedReason:       ctx.Err().Error(),
			HostRoot:                d.HostRoot,
			InContainer:             inContainer,
			CrossNamespaceRoot:      crossNS,
			RequiresExecutionBridge: inContainer || crossNS,
		}
	}

	info := PlatformInfo{
		OSFamily:                OSFamilyUnknown,
		KernelArch:              runtime.GOARCH,
		PackageManager:          PkgManagerUnknown,
		HostRoot:                d.HostRoot,
		InContainer:             inContainer,
		CrossNamespaceRoot:      crossNS,
		RequiresExecutionBridge: inContainer || crossNS,
	}

	if k, err := os.ReadFile(d.resolvePath("proc/sys/kernel/osrelease")); err == nil {
		info.KernelVersion = strings.TrimSpace(string(k))
	} else if k, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		info.KernelVersion = strings.TrimSpace(string(k))
	}

	if arch, err := os.ReadFile(d.resolvePath("proc/sys/kernel/arch")); err == nil {
		if trimmed := strings.TrimSpace(string(arch)); trimmed != "" {
			info.KernelArch = trimmed
		}
	}

	osRelMap := map[string]string{}
	if osRelBytes, err := os.ReadFile(d.resolvePath("etc/os-release")); err == nil {
		osRelMap = parseOSRelease(string(osRelBytes))
	} else if osRelBytes, err := os.ReadFile(d.resolvePath("usr/lib/os-release")); err == nil {
		osRelMap = parseOSRelease(string(osRelBytes))
	}

	info.DistroID = strings.ToLower(strings.TrimSpace(osRelMap["ID"]))
	info.DistroIDLike = strings.ToLower(strings.TrimSpace(osRelMap["ID_LIKE"]))
	info.DistroName = strings.TrimSpace(osRelMap["NAME"])
	info.PrettyName = strings.TrimSpace(osRelMap["PRETTY_NAME"])
	info.VersionID = strings.TrimSpace(osRelMap["VERSION_ID"])
	info.Codename = strings.ToLower(strings.TrimSpace(osRelMap["VERSION_CODENAME"]))

	// Detect fnOS ("trim" / "fnOS") markers on host root
	fnosMarkers := []struct {
		rel  string
		note string
	}{
		{"etc/trim-release", "file:/etc/trim-release"},
		{"etc/fnos-release", "file:/etc/fnos-release"},
		{"usr/trim", "dir:/usr/trim"},
		{"usr/trim/bin", "dir:/usr/trim/bin"},
		{"var/lib/trim", "dir:/var/lib/trim"},
		{"etc/systemd/system/trim_srv.service", "unit:trim_srv.service"},
		{"lib/systemd/system/trim_srv.service", "unit:trim_srv.service"},
	}

	for _, m := range fnosMarkers {
		if _, err := os.Stat(d.resolvePath(m.rel)); err == nil {
			info.Markers = append(info.Markers, m.note)
			info.IsFnOS = true
			if strings.HasSuffix(m.rel, "-release") && info.FnOSVersion == "" {
				if raw, rErr := os.ReadFile(d.resolvePath(m.rel)); rErr == nil {
					info.FnOSVersion = strings.TrimSpace(string(raw))
				}
			}
		}
	}

	lowerPretty := strings.ToLower(info.PrettyName + " " + info.DistroName + " " + info.KernelVersion)
	if info.DistroID == "trim" || info.DistroID == "fnos" ||
		strings.Contains(lowerPretty, "fnos") ||
		strings.Contains(lowerPretty, "fnnas") ||
		strings.Contains(lowerPretty, "-trim") {
		info.IsFnOS = true
		info.Markers = append(info.Markers, "os-release:fnos")
	}

	if info.IsFnOS {
		info.OSFamily = OSFamilyFnOS
		info.PackageManager = PkgManagerTrimManaged
		info.HostUpdateSupported = false
		info.UnsupportedReason = "fnOS (trim) detected: host OS updates must be performed via fnOS official OTA; automated apt/dpkg mutation is fail-closed"
		return info
	}

	// Containerized API without host /host mount reading its own Alpine rootfs must fail-closed
	if d.InDocker && d.HostRoot == "/" && info.DistroID == "alpine" {
		info.OSFamily = OSFamilyAlpine
		info.PackageManager = PkgManagerApk
		info.HostUpdateSupported = false
		info.UnsupportedReason = "running inside Alpine container without host rootfs mount (/host); refusing container-local OS update"
		return info
	}

	switch {
	case info.DistroID == "debian" || strings.Contains(info.DistroIDLike, "debian"):
		if info.DistroID == "ubuntu" {
			info.OSFamily = OSFamilyUbuntu
		} else {
			info.OSFamily = OSFamilyDebian
		}
		if _, err := os.Stat(d.resolvePath("var/lib/dpkg/status")); err == nil {
			info.PackageManager = PkgManagerAptDpkg
			info.HostUpdateSupported = true
		} else {
			info.PackageManager = PkgManagerUnknown
			info.HostUpdateSupported = false
			info.UnsupportedReason = "Debian-like OS detected but /var/lib/dpkg/status is absent or unmounted"
		}
	case info.DistroID == "ubuntu":
		info.OSFamily = OSFamilyUbuntu
		if _, err := os.Stat(d.resolvePath("var/lib/dpkg/status")); err == nil {
			info.PackageManager = PkgManagerAptDpkg
			info.HostUpdateSupported = true
		} else {
			info.PackageManager = PkgManagerUnknown
			info.HostUpdateSupported = false
			info.UnsupportedReason = "Ubuntu detected but /var/lib/dpkg/status is absent or unmounted"
		}
	case info.DistroID == "alpine":
		info.OSFamily = OSFamilyAlpine
		info.PackageManager = PkgManagerApk
		info.HostUpdateSupported = false
		info.UnsupportedReason = "Alpine Linux is not supported for host apt/dpkg updates"
	default:
		info.OSFamily = OSFamilyUnknown
		info.PackageManager = PkgManagerUnknown
		info.HostUpdateSupported = false
		info.UnsupportedReason = fmt.Sprintf("unrecognized host distribution (id=%q, pretty=%q)", info.DistroID, info.PrettyName)
	}

	return info
}

func parseOSRelease(content string) map[string]string {
	out := make(map[string]string)
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		v = strings.Trim(v, `"'`)
		out[k] = v
	}
	return out
}
