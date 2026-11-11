package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/model"
)

const (
	// BaselineProtocolVersion identifies the cross-language Go/Python BaselineHash v1 contract.
	BaselineProtocolVersion = "v1"
)

var (
	ErrBaselineAmbiguousContainer = errors.New("ambiguous container match: multiple containers resolve to target component")
	ErrBaselineInvalidTimestamp   = errors.New("invalid container timestamp in baseline inspect payload")
	ErrBaselineIncompleteInspect  = errors.New("incomplete or unknown container inspect payload")

	v1BaselineHashRegex     = regexp.MustCompile(`^v1:[0-9a-f]{64}$`)
	hostV1BaselineHashRegex = regexp.MustCompile(`^host-v1:[0-9a-f]{64}$`)
	legacySHA256HashRegex   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	rfc3339StrictPattern    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$`)
)

// BaselineIdentity captures immutable/versioned container & image identity fields.
type BaselineIdentity struct {
	CanonicalComponent string `json:"canonical_component"`
	ContainerID        string `json:"container_id"`
	ContainerName      string `json:"container_name"`
	ImageID            string `json:"image_id"`
	ImageDigest        string `json:"image_digest"`
	ImageReference     string `json:"image_reference"`
}

// BaselineMount captures normalized container mount/bind properties.
type BaselineMount struct {
	Destination string `json:"destination"`
	Mode        string `json:"mode"`
	RW          bool   `json:"rw"`
	Source      string `json:"source"`
	Type        string `json:"type"`
}

// BaselineSecurity captures all 24 normalized container runtime security fields (R8).
type BaselineSecurity struct {
	Privileged             bool            `json:"privileged"`
	CapAdd                 []string        `json:"cap_add"`
	CapDrop                []string        `json:"cap_drop"`
	Mounts                 []BaselineMount `json:"mounts"`
	NetworkMode            string          `json:"network_mode"`
	PortBindings           []string        `json:"port_bindings"`
	ReadonlyRootfs         bool            `json:"readonly_rootfs"`
	Devices                []string        `json:"devices"`
	SecurityOpt            []string        `json:"security_opt"`
	EntrypointDigest       string          `json:"entrypoint_digest"`
	CommandDigest          string          `json:"command_digest"`
	User                   string          `json:"user"`
	RestartPolicy          string          `json:"restart_policy"`
	PidMode                string          `json:"pid_mode"`
	IpcMode                string          `json:"ipc_mode"`
	UTSMode                string          `json:"uts_mode"`
	CgroupMode             string          `json:"cgroup_mode"`
	CgroupParent           string          `json:"cgroup_parent"`
	MemoryLimit            int64           `json:"memory_limit"`
	NanoCPUs               int64           `json:"nano_cpus"`
	PidsLimit              int64           `json:"pids_limit"`
	Runtime                string          `json:"runtime"`
	RuntimeSecurityProfile string          `json:"runtime_security_profile"`
	EnvDigest              string          `json:"env_digest"`
}

// BaselineState captures mutable container lifecycle state fields.
type BaselineState struct {
	ContainerState string `json:"container_state"`
	CreatedAt      string `json:"created_at"`
	StartedAt      string `json:"started_at"`
	RestartCount   int64  `json:"restart_count"`
}

// BaselineDocument is the top-level canonical JSON payload hashed for Baseline Protocol v1.
type BaselineDocument struct {
	ProtocolVersion string           `json:"protocol_version"`
	Identity        BaselineIdentity `json:"identity"`
	Security        BaselineSecurity `json:"security"`
	State           BaselineState    `json:"state"`
}

// BaselineComputation holds the full Baseline Protocol v1 result and section sub-hashes.
type BaselineComputation struct {
	Hash            string           `json:"hash"`
	ProtocolVersion string           `json:"protocol_version"`
	IdentityHash    string           `json:"identity_hash"`
	SecurityHash    string           `json:"security_hash"`
	StateHash       string           `json:"state_hash"`
	Document        BaselineDocument `json:"document"`
}

// ResolveBaselineComponent resolves a component/container alias to the unified canonical
// infrastructure identifier shared between Go Core (lease/topology) and Python ops-agent.
// Notably, "1pwaf"/"waf"/"openresty-waf" map to "openresty" (container "1Panel-openresty-WuDU").
func ResolveBaselineComponent(name string) string {
	clean := strings.TrimSpace(name)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" {
		return ""
	}
	if canonical := ResolveTopologyComponent(clean); canonical != "" {
		return canonical
	}
	if canonical, err := ResolveComponent(clean); err == nil {
		if canonical == Component1PWAF {
			return ComponentOpenResty
		}
		return canonical
	}
	if canonical := model.CanonicalizeComponent(clean); canonical != "" {
		if canonical == Component1PWAF {
			return ComponentOpenResty
		}
		return canonical
	}
	return ""
}

// IsRealBaselineHash reports whether baselineHash is a valid non-placeholder BaselineHash.
// In production (`IsProductionRuntime()`), container components strictly require "v1:<64-hex>"
// (rejecting legacy "sha256:<64-hex>") and "host-os" strictly requires "host-v1:<64-hex>".
func IsRealBaselineHash(baselineHash string, component string) bool {
	trimmed := strings.TrimSpace(baselineHash)
	if trimmed == "" {
		return false
	}
	resolved := ResolveBaselineComponent(component)
	if resolved == "" {
		resolved = model.CanonicalizeComponent(component)
	}
	if component != "" && resolved == "" {
		return false
	}
	if IsOfflinePlaceholderBaseline(component, trimmed) {
		return false
	}
	if resolved == ComponentHostOS {
		return hostV1BaselineHashRegex.MatchString(trimmed)
	}
	if IsProductionRuntime() {
		return v1BaselineHashRegex.MatchString(trimmed)
	}
	return v1BaselineHashRegex.MatchString(trimmed) || legacySHA256HashRegex.MatchString(trimmed)
}

// IsRealBaselineHashV1 strictly requires Baseline Protocol v1 ("v1:<64-hex>" for containers,
// or "host-v1:<64-hex>" for "host-os") and non-placeholder status.
func IsRealBaselineHashV1(baselineHash string, component string) bool {
	trimmed := strings.TrimSpace(baselineHash)
	resolved := ResolveBaselineComponent(component)
	if resolved == "" {
		resolved = model.CanonicalizeComponent(component)
	}
	if resolved == ComponentHostOS {
		if !hostV1BaselineHashRegex.MatchString(trimmed) {
			return false
		}
		return IsRealBaselineHash(trimmed, component)
	}
	if !v1BaselineHashRegex.MatchString(trimmed) {
		return false
	}
	return IsRealBaselineHash(trimmed, component)
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func canonicalHashWithPrefix(prefix string, v interface{}) (string, error) {
	b, err := audit.CanonicalJSONBytes(v)
	if err != nil {
		return "", err
	}
	return prefix + sha256HexBytes(b), nil
}

func normalizeBaselineTimestamp(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !rfc3339StrictPattern.MatchString(trimmed) {
		return "", fmt.Errorf("%w: %q is not RFC3339", ErrBaselineInvalidTimestamp, raw)
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrBaselineInvalidTimestamp, raw, err)
	}
	utc := parsed.UTC()
	if utc.Year() <= 1 {
		return "", nil
	}
	return utc.Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z"), nil
}

func strFromMap(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}

func mapFromMap(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	sub, _ := v.(map[string]interface{})
	return sub
}

func boolFromMap(m map[string]interface{}, key string) (bool, error) {
	if m == nil {
		return false, nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("field %s must be boolean", key)
	}
	return b, nil
}

func int64FromMap(m map[string]interface{}, key string) (int64, error) {
	if m == nil {
		return 0, nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case float64:
		if float64(int64(t)) != t {
			return 0, fmt.Errorf("field %s must be an integer", key)
		}
		return int64(t), nil
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			return 0, fmt.Errorf("field %s must be an integer", key)
		}
		iv, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("field %s invalid integer: %w", key, err)
		}
		return iv, nil
	default:
		return 0, fmt.Errorf("field %s must be an integer", key)
	}
}

func stringSliceFromAny(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	switch t := raw.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if item == nil {
				continue
			}
			if s, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			} else {
				if trimmed := strings.TrimSpace(fmt.Sprintf("%v", item)); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	default:
		return nil
	}
}

func rawStringSliceFromAny(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	switch t := raw.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func sortedUniqueStrings(raw interface{}, upper bool) []string {
	items := stringSliceFromAny(raw)
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, s := range items {
		if upper {
			s = strings.ToUpper(s)
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func hasReadOnlyModeToken(mode string) bool {
	for _, tok := range strings.Split(strings.ToLower(mode), ",") {
		if strings.TrimSpace(tok) == "ro" {
			return true
		}
	}
	return false
}

func parseBindString(bind string) (BaselineMount, bool) {
	trimmed := strings.TrimSpace(bind)
	if trimmed == "" {
		return BaselineMount{}, false
	}
	parts := strings.Split(trimmed, ":")
	// Handle Windows drive letter prefix like C:\path:/dst[:ro]
	if len(parts) >= 3 && len(parts[0]) == 1 && (strings.HasPrefix(parts[1], `\`) || strings.HasPrefix(parts[1], "/")) {
		merged := parts[0] + ":" + parts[1]
		parts = append([]string{merged}, parts[2:]...)
	}
	if len(parts) < 2 {
		return BaselineMount{}, false
	}
	src := strings.TrimSpace(parts[0])
	dst := strings.TrimSpace(parts[1])
	mode := ""
	if len(parts) >= 3 {
		mode = strings.TrimSpace(strings.Join(parts[2:], ":"))
	}
	return BaselineMount{
		Destination: dst,
		Mode:        mode,
		RW:          !hasReadOnlyModeToken(mode),
		Source:      src,
		Type:        "bind",
	}, true
}

func normalizeBaselineMounts(inspect map[string]interface{}, hostCfg map[string]interface{}) []BaselineMount {
	seen := make(map[string]struct{})
	out := make([]BaselineMount, 0)

	addMount := func(m BaselineMount) {
		key := fmt.Sprintf("%s|%s|%s|%s|%t", m.Type, m.Source, m.Destination, m.Mode, m.RW)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}

	if rawMounts, ok := inspect["Mounts"].([]interface{}); ok && len(rawMounts) > 0 {
		for _, item := range rawMounts {
			mm, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			src := strFromMap(mm, "Source")
			if src == "" {
				src = strFromMap(mm, "Name")
			}
			dst := strFromMap(mm, "Destination")
			typ := strings.ToLower(strFromMap(mm, "Type"))
			mode := strFromMap(mm, "Mode")
			rw := !hasReadOnlyModeToken(mode)
			if rwRaw, hasRW := mm["RW"]; hasRW {
				if rwBool, ok := rwRaw.(bool); ok {
					rw = rwBool
				}
			}
			addMount(BaselineMount{
				Destination: dst,
				Mode:        mode,
				RW:          rw,
				Source:      src,
				Type:        typ,
			})
		}
	} else if hostCfg != nil {
		for _, b := range stringSliceFromAny(hostCfg["Binds"]) {
			if m, ok := parseBindString(b); ok {
				addMount(m)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Destination != out[j].Destination {
			return out[i].Destination < out[j].Destination
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].Mode != out[j].Mode {
			return out[i].Mode < out[j].Mode
		}
		return !out[i].RW && out[j].RW
	})
	return out
}

func normalizePortBindings(hostCfg map[string]interface{}) []string {
	pbMap := mapFromMap(hostCfg, "PortBindings")
	if len(pbMap) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	add := func(s string) {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	for portProto, rawList := range pbMap {
		cleanPort := strings.TrimSpace(portProto)
		bindings, ok := rawList.([]interface{})
		if !ok || len(bindings) == 0 {
			add(cleanPort + "->")
			continue
		}
		for _, b := range bindings {
			bm, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			hostIP := strFromMap(bm, "HostIp")
			hostPort := strFromMap(bm, "HostPort")
			add(fmt.Sprintf("%s->%s:%s", cleanPort, hostIP, hostPort))
		}
	}
	sort.Strings(out)
	return out
}

func normalizeDevices(hostCfg map[string]interface{}) []string {
	if hostCfg == nil {
		return []string{}
	}
	rawDevices, ok := hostCfg["Devices"].([]interface{})
	if !ok || len(rawDevices) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(rawDevices))
	for _, d := range rawDevices {
		dm, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		spec := fmt.Sprintf("%s:%s:%s",
			strFromMap(dm, "PathOnHost"),
			strFromMap(dm, "PathInContainer"),
			strFromMap(dm, "CgroupPermissions"),
		)
		if _, exists := seen[spec]; !exists {
			seen[spec] = struct{}{}
			out = append(out, spec)
		}
	}
	sort.Strings(out)
	return out
}

func normalizeEnvDigest(cfg map[string]interface{}) (string, error) {
	rawEnv := rawStringSliceFromAny(cfg["Env"])
	if len(rawEnv) == 0 {
		return "", nil
	}
	seen := make(map[string]struct{}, len(rawEnv))
	hashedEntries := make([]string, 0, len(rawEnv))
	for _, entry := range rawEnv {
		var k, v string
		if idx := strings.IndexByte(entry, '='); idx >= 0 {
			k = entry[:idx]
			v = entry[idx+1:]
		} else {
			k = entry
			v = ""
		}
		token := k + ":" + sha256HexBytes([]byte(v))
		if _, ok := seen[token]; !ok {
			seen[token] = struct{}{}
			hashedEntries = append(hashedEntries, token)
		}
	}
	sort.Strings(hashedEntries)
	return canonicalHashWithPrefix("sha256:", hashedEntries)
}

func normalizeImageDigest(inspect map[string]interface{}, imageRef string) string {
	seen := make(map[string]struct{})
	digests := make([]string, 0)
	for _, raw := range stringSliceFromAny(inspect["RepoDigests"]) {
		d := raw
		if idx := strings.LastIndex(raw, "@"); idx != -1 {
			d = strings.TrimSpace(raw[idx+1:])
		}
		if d == "" {
			continue
		}
		if _, ok := seen[d]; !ok {
			seen[d] = struct{}{}
			digests = append(digests, d)
		}
	}
	sort.Strings(digests)
	if len(digests) > 0 {
		return strings.Join(digests, ",")
	}
	if idx := strings.LastIndex(imageRef, "@sha256:"); idx != -1 {
		return strings.TrimSpace(imageRef[idx+1:])
	}
	return ""
}

// ComputeContainerBaselineFromInspect computes the Baseline Protocol v1 fingerprint and
// sub-hashes (identity_hash, security_hash, state_hash) from Docker inspect JSON.
// Strictly fails closed on unknown component, missing identity/state, or malformed timestamps.
func ComputeContainerBaselineFromInspect(component string, inspect map[string]interface{}) (*BaselineComputation, error) {
	canonical := ResolveBaselineComponent(component)
	if canonical == "" {
		return nil, fmt.Errorf("%w: unknown component %q", ErrComponentNotWhitelisted, component)
	}
	if len(inspect) == 0 {
		return nil, fmt.Errorf("%w: empty inspect payload for %s", ErrBaselineIncompleteInspect, canonical)
	}

	containerID := strFromMap(inspect, "Id")
	if containerID == "" {
		return nil, fmt.Errorf("%w: missing container Id for %s", ErrBaselineIncompleteInspect, canonical)
	}
	containerName := strings.TrimPrefix(strFromMap(inspect, "Name"), "/")

	cfg := mapFromMap(inspect, "Config")
	imageID := strFromMap(inspect, "Image")
	imageRef := strFromMap(cfg, "Image")
	if imageID == "" && imageRef == "" {
		return nil, fmt.Errorf("%w: missing both Image and Config.Image for %s", ErrBaselineIncompleteInspect, canonical)
	}
	imageDigest := normalizeImageDigest(inspect, imageRef)

	stateObj := mapFromMap(inspect, "State")
	containerState := strings.ToLower(strFromMap(stateObj, "Status"))
	if containerState == "" {
		if running, ok := stateObj["Running"].(bool); ok {
			if running {
				containerState = "running"
			} else {
				containerState = "exited"
			}
		}
	}
	if containerState == "" || containerState == "unknown" {
		return nil, fmt.Errorf("%w: invalid container state %q for %s", ErrBaselineIncompleteInspect, containerState, canonical)
	}

	createdAt, err := normalizeBaselineTimestamp(strFromMap(inspect, "Created"))
	if err != nil {
		return nil, err
	}
	startedAt, err := normalizeBaselineTimestamp(strFromMap(stateObj, "StartedAt"))
	if err != nil {
		return nil, err
	}
	if createdAt == "" && startedAt == "" && imageID == "" {
		return nil, fmt.Errorf("%w: missing timestamps and image_id for %s", ErrBaselineIncompleteInspect, canonical)
	}

	restartCount, err := int64FromMap(inspect, "RestartCount")
	if err != nil || restartCount < 0 {
		return nil, fmt.Errorf("%w: invalid RestartCount for %s", ErrBaselineIncompleteInspect, canonical)
	}

	hostCfg := mapFromMap(inspect, "HostConfig")
	privileged, err := boolFromMap(hostCfg, "Privileged")
	if err != nil {
		return nil, err
	}
	readonlyRootfs, err := boolFromMap(hostCfg, "ReadonlyRootfs")
	if err != nil {
		return nil, err
	}
	memLimit, err := int64FromMap(hostCfg, "Memory")
	if err != nil {
		return nil, err
	}
	nanoCPUs, err := int64FromMap(hostCfg, "NanoCpus")
	if err != nil {
		return nil, err
	}
	pidsLimit, err := int64FromMap(hostCfg, "PidsLimit")
	if err != nil {
		return nil, err
	}

	var entrypointDigest string
	if ep := rawStringSliceFromAny(cfg["Entrypoint"]); len(ep) > 0 {
		entrypointDigest, err = canonicalHashWithPrefix("sha256:", ep)
		if err != nil {
			return nil, err
		}
	}
	var cmdDigest string
	if cmd := rawStringSliceFromAny(cfg["Cmd"]); len(cmd) > 0 {
		cmdDigest, err = canonicalHashWithPrefix("sha256:", cmd)
		if err != nil {
			return nil, err
		}
	}
	envDigest, err := normalizeEnvDigest(cfg)
	if err != nil {
		return nil, err
	}

	restartPolicy := ""
	if rp := mapFromMap(hostCfg, "RestartPolicy"); len(rp) > 0 {
		rpName := strFromMap(rp, "Name")
		if rpName != "" {
			maxRetry, rErr := int64FromMap(rp, "MaximumRetryCount")
			if rErr != nil {
				return nil, rErr
			}
			if maxRetry > 0 {
				restartPolicy = fmt.Sprintf("%s:%d", rpName, maxRetry)
			} else {
				restartPolicy = rpName
			}
		}
	}

	cgroupMode := strFromMap(hostCfg, "CgroupnsMode")
	if cgroupMode == "" {
		cgroupMode = strFromMap(hostCfg, "CgroupMode")
	}

	appArmor := strFromMap(inspect, "AppArmorProfile")
	procLabel := strFromMap(inspect, "ProcessLabel")
	runtimeProfile := ""
	if appArmor != "" && procLabel != "" {
		runtimeProfile = appArmor + "|" + procLabel
	} else if appArmor != "" {
		runtimeProfile = appArmor
	} else if procLabel != "" {
		runtimeProfile = procLabel
	}

	doc := BaselineDocument{
		ProtocolVersion: BaselineProtocolVersion,
		Identity: BaselineIdentity{
			CanonicalComponent: canonical,
			ContainerID:        containerID,
			ContainerName:      containerName,
			ImageID:            imageID,
			ImageDigest:        imageDigest,
			ImageReference:     imageRef,
		},
		Security: BaselineSecurity{
			Privileged:             privileged,
			CapAdd:                 sortedUniqueStrings(hostCfg["CapAdd"], true),
			CapDrop:                sortedUniqueStrings(hostCfg["CapDrop"], true),
			Mounts:                 normalizeBaselineMounts(inspect, hostCfg),
			NetworkMode:            strFromMap(hostCfg, "NetworkMode"),
			PortBindings:           normalizePortBindings(hostCfg),
			ReadonlyRootfs:         readonlyRootfs,
			Devices:                normalizeDevices(hostCfg),
			SecurityOpt:            sortedUniqueStrings(hostCfg["SecurityOpt"], false),
			EntrypointDigest:       entrypointDigest,
			CommandDigest:          cmdDigest,
			User:                   strFromMap(cfg, "User"),
			RestartPolicy:          restartPolicy,
			PidMode:                strFromMap(hostCfg, "PidMode"),
			IpcMode:                strFromMap(hostCfg, "IpcMode"),
			UTSMode:                strFromMap(hostCfg, "UTSMode"),
			CgroupMode:             cgroupMode,
			CgroupParent:           strFromMap(hostCfg, "CgroupParent"),
			MemoryLimit:            memLimit,
			NanoCPUs:               nanoCPUs,
			PidsLimit:              pidsLimit,
			Runtime:                strFromMap(hostCfg, "Runtime"),
			RuntimeSecurityProfile: runtimeProfile,
			EnvDigest:              envDigest,
		},
		State: BaselineState{
			ContainerState: containerState,
			CreatedAt:      createdAt,
			StartedAt:      startedAt,
			RestartCount:   restartCount,
		},
	}

	idHash, err := canonicalHashWithPrefix("sha256:", doc.Identity)
	if err != nil {
		return nil, err
	}
	secHash, err := canonicalHashWithPrefix("sha256:", doc.Security)
	if err != nil {
		return nil, err
	}
	stHash, err := canonicalHashWithPrefix("sha256:", doc.State)
	if err != nil {
		return nil, err
	}
	fullHash, err := canonicalHashWithPrefix(BaselineProtocolVersion+":", doc)
	if err != nil {
		return nil, err
	}

	return &BaselineComputation{
		Hash:            fullHash,
		ProtocolVersion: BaselineProtocolVersion,
		IdentityHash:    idHash,
		SecurityHash:    secHash,
		StateHash:       stHash,
		Document:        doc,
	}, nil
}

// inspectContainerViaDockerSocket lists containers to detect ambiguous alias matches,
// then inspects /containers/{expectedDockerName}/json and verifies exact container Name match.
func inspectContainerViaDockerSocket(ctx context.Context, sockPath, canonical string) (map[string]interface{}, error) {
	if _, err := os.Stat(sockPath); err != nil {
		return nil, err
	}
	expectedDockerName := canonicalContainerNames[canonical]
	if expectedDockerName == "" {
		return nil, fmt.Errorf("%w: no container mapping for canonical %s", ErrComponentNotWhitelisted, canonical)
	}

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	// 1. Ambiguity detection across all containers (/containers/json?all=1)
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}
	listResp, err := client.Do(listReq)
	if err != nil {
		return nil, err
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker list HTTP %d", listResp.StatusCode)
	}
	listBody, err := io.ReadAll(listResp.Body)
	if err != nil {
		return nil, err
	}

	var rawList []struct {
		Id    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if err := json.Unmarshal(listBody, &rawList); err != nil {
		return nil, err
	}

	matchedContainerNames := make([]string, 0, 2)
	exactMatchName := ""
	for _, item := range rawList {
		itemMatched := false
		for _, rawName := range item.Names {
			cleanName := strings.TrimPrefix(strings.TrimSpace(rawName), "/")
			if cleanName == "" {
				continue
			}
			if strings.EqualFold(cleanName, expectedDockerName) || ResolveBaselineComponent(cleanName) == canonical {
				itemMatched = true
				if strings.EqualFold(cleanName, expectedDockerName) {
					exactMatchName = cleanName
				}
			}
		}
		if itemMatched {
			primary := ""
			if len(item.Names) > 0 {
				primary = strings.TrimPrefix(strings.TrimSpace(item.Names[0]), "/")
			}
			matchedContainerNames = append(matchedContainerNames, primary)
		}
	}

	if len(matchedContainerNames) > 1 {
		return nil, fmt.Errorf("%w: %s matched %v", ErrBaselineAmbiguousContainer, canonical, matchedContainerNames)
	}
	if len(matchedContainerNames) == 0 {
		return nil, fmt.Errorf("container %s (%s) not found in docker", canonical, expectedDockerName)
	}

	queryName := exactMatchName
	if queryName == "" {
		queryName = matchedContainerNames[0]
	}

	// 2. Full container inspect (/containers/{name}/json)
	inspectURL := fmt.Sprintf("http://localhost/containers/%s/json", url.PathEscape(queryName))
	insReq, err := http.NewRequestWithContext(ctx, http.MethodGet, inspectURL, nil)
	if err != nil {
		return nil, err
	}
	insResp, err := client.Do(insReq)
	if err != nil {
		return nil, err
	}
	defer insResp.Body.Close()
	if insResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(insResp.Body, 1024))
		return nil, fmt.Errorf("docker inspect %s HTTP %d: %s", queryName, insResp.StatusCode, string(b))
	}

	dec := json.NewDecoder(insResp.Body)
	dec.UseNumber()
	var inspectData map[string]interface{}
	if err := dec.Decode(&inspectData); err != nil {
		return nil, err
	}

	// Verify inspected Name matches expected Docker container name (prevents ID-prefix confusion)
	inspectedName := strings.TrimPrefix(strFromMap(inspectData, "Name"), "/")
	if !strings.EqualFold(inspectedName, expectedDockerName) {
		return nil, fmt.Errorf("container name mismatch on inspect: expected %q, got %q", expectedDockerName, inspectedName)
	}

	// 3. Best-effort image inspect for RepoDigests if absent on container inspect
	if len(stringSliceFromAny(inspectData["RepoDigests"])) == 0 {
		if imgID := strFromMap(inspectData, "Image"); imgID != "" {
			imgURL := fmt.Sprintf("http://localhost/images/%s/json", url.PathEscape(imgID))
			if imgReq, iErr := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil); iErr == nil {
				if imgResp, rErr := client.Do(imgReq); rErr == nil {
					if imgResp.StatusCode == http.StatusOK {
						var imgInfo struct {
							RepoDigests []string `json:"RepoDigests"`
						}
						if json.NewDecoder(imgResp.Body).Decode(&imgInfo) == nil && len(imgInfo.RepoDigests) > 0 {
							inspectData["RepoDigests"] = imgInfo.RepoDigests
						}
					}
					_ = imgResp.Body.Close()
				}
			}
		}
	}

	return inspectData, nil
}
