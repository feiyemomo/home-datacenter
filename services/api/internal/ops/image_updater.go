package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"home-datacenter-api/internal/model"
)

type imageTarget struct {
	Component string
	Container string
	ImageRef  string
	ImageID   string
	Risk      string
}

// imageUpdateCheckContainers lists fallback managed containers when /containers/json is unavailable.
var imageUpdateCheckContainers = []struct {
	Component string
	Container string
	Risk      string
}{
	{ComponentFrigate, "home-frigate", model.RiskLevelMedium},
	{ComponentMosquitto, "home-mosquitto", model.RiskLevelMedium},
	{ComponentMihomo, "mihomo", model.RiskLevelMedium},
	{ComponentTailscale, "tailscale", model.RiskLevelMedium},
	{ComponentCloudflared, "home-cloudflared", model.RiskLevelLow},
	{ComponentNewAPI, "home-new-api", model.RiskLevelLow},
	{ComponentHomeBackup, "home-backup", model.RiskLevelLow},
	{ComponentOpenResty, "1Panel-openresty-WuDU", model.RiskLevelHigh},
}

// CheckDockerImageUpdates dynamically discovers running containers via Docker socket,
// inspects local RepoDigests, queries remote registry manifest digests (via direct Registry v2
// with proxy auto-detection, falling back to Docker /distribution/{image}/json), and creates
// idempotent ActionDockerUpdateImage proposals when a newer digest is available.
func (s *VulnScanner) CheckDockerImageUpdates(ctx context.Context, dockerSocket string) int {
	if s == nil || s.db == nil || dockerSocket == "" {
		return 0
	}
	if _, err := os.Stat(dockerSocket); err != nil {
		return 0
	}

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(dCtx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dCtx, "unix", dockerSocket)
		},
	}
	defer tr.CloseIdleConnections()
	dockerClient := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	targets := discoverRunningImageTargets(ctx, dockerClient)
	if len(targets) == 0 {
		for _, fallback := range imageUpdateCheckContainers {
			targets = append(targets, imageTarget{
				Component: fallback.Component,
				Container: fallback.Container,
				Risk:      fallback.Risk,
			})
		}
	}

	regClient := buildRegistryHTTPClient()
	defer regClient.CloseIdleConnections()

	log.Printf("[image-updater] checking %d container(s) for remote image updates", len(targets))
	created := 0
	for _, item := range targets {
		if ctx.Err() != nil {
			break
		}
		imageRef, localImageID := item.ImageRef, item.ImageID
		if imageRef == "" || localImageID == "" {
			var err error
			imageRef, localImageID, err = inspectContainerImage(ctx, dockerClient, item.Container)
			if err != nil || imageRef == "" {
				continue
			}
		}
		// Skip digest-pinned or local-only images
		if strings.Contains(imageRef, "@sha256:") || strings.HasPrefix(imageRef, "home-datacenter") {
			continue
		}
		localDigests, err := inspectLocalImageDigests(ctx, dockerClient, localImageID)
		if err != nil || len(localDigests) == 0 {
			continue
		}

		reqCtx, cancel := context.WithTimeout(ctx, 14*time.Second)
		remoteDigest, err := fetchRemoteManifestDigestWithRegistry(reqCtx, regClient, dockerClient, imageRef)
		cancel()
		if err != nil || remoteDigest == "" {
			log.Printf("[image-updater] skip %s (%s): remote digest check failed: %v", item.Container, imageRef, err)
			continue
		}

		upToDate := false
		for _, ld := range localDigests {
			if strings.HasSuffix(ld, "@"+remoteDigest) || ld == remoteDigest {
				upToDate = true
				break
			}
		}
		if upToDate {
			continue
		}

		idemKey := fmt.Sprintf("imgupdate:%s:%s", item.Component, remoteDigest)
		baselineHash := ""
		if s.baselineFn != nil {
			baselineHash = s.baselineFn(item.Component)
		}
		if baselineHash == "" && !IsProductionRuntime() {
			baselineHash = model.ComputeDefaultBaseline(item.Component)
		}

		var existing model.OpsProposal
		if err := s.db.Where("idempotency_key = ? AND status NOT IN ?",
			idemKey,
			[]string{model.ProposalStatusRejected, model.ProposalStatusCancelled, model.ProposalStatusExpired},
		).First(&existing).Error; err == nil {
			if existing.Status == model.ProposalStatusAwaitingApproval && baselineHash != "" && existing.BaselineHash != baselineHash {
				_ = s.db.Model(&model.OpsProposal{}).
					Where("id = ?", existing.ID).
					Update("baseline_hash", baselineHash).Error
			}
			continue
		}

		specBytes, _ := json.Marshal(OperationSpec{
			Action: ActionDockerUpdateImage,
			Target: item.Component,
			Params: map[string]interface{}{
				"image":         imageRef,
				"remote_digest": remoteDigest,
			},
		})
		prop := model.OpsProposal{
			HouseholdID:      1,
			ProposalID:       fmt.Sprintf("prop-img-%d", time.Now().UnixNano()),
			IdempotencyKey:   idemKey,
			Title:            fmt.Sprintf("更新 %s 镜像至最新版本 (%s)", item.Component, imageRef),
			Description:      fmt.Sprintf("检测到容器 %s 镜像 %s 远端存在新版本 Digest (%s)，建议拉取新镜像并安全重建容器。", item.Container, imageRef, remoteDigest),
			TargetComponent:  item.Component,
			ActionSpec:       string(specBytes),
			BaselineHash:     baselineHash,
			RiskLevel:        item.Risk,
			EstimatedSeconds: 45,
			DowntimeSeconds:  5,
			ExecutionPlan:    fmt.Sprintf("1. 通过 Docker Socket 拉取最新镜像 %s\n2. 备份旧容器并按原配置重建启动\n3. 执行健康探针校验并在异常时自动回滚", imageRef),
			RollbackPlan:     fmt.Sprintf("docker rename %s_bak -> %s", item.Container, item.Container),
			Status:           model.ProposalStatusAwaitingApproval,
			Version:          1,
			Revision:         1,
			CreatedAt:        time.Now().UTC(),
		}
		if err := s.db.Create(&prop).Error; err != nil {
			log.Printf("[image-updater] failed to create update proposal for %s: %v", item.Component, err)
			continue
		}
		created++
	}
	log.Printf("[image-updater] scan completed: checked=%d created=%d", len(targets), created)
	return created
}

func discoverRunningImageTargets(ctx context.Context, client *http.Client) []imageTarget {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/containers/json", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()

	var containers []struct {
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		ImageID string   `json:"ImageID"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&containers); err != nil {
		return nil
	}

	var out []imageTarget
	seenComp := make(map[string]bool)
	for _, ctr := range containers {
		if len(ctr.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(ctr.Names[0]), "/")
		if name == "" {
			continue
		}
		comp, err := ResolveComponent(name)
		if err != nil || seenComp[comp] {
			continue
		}
		seenComp[comp] = true

		risk := model.RiskLevelMedium
		switch comp {
		case ComponentHomeAPI, ComponentOpenResty, Component1PWAF:
			risk = model.RiskLevelHigh
		case ComponentCloudflared, ComponentNewAPI, ComponentHomeBackup:
			risk = model.RiskLevelLow
		}
		out = append(out, imageTarget{
			Component: comp,
			Container: name,
			ImageRef:  strings.TrimSpace(ctr.Image),
			ImageID:   strings.TrimSpace(ctr.ImageID),
			Risk:      risk,
		})
	}
	return out
}

func buildRegistryHTTPClient() *http.Client {
	proxyFunc := http.ProxyFromEnvironment
	for _, envKey := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return &http.Client{
				Transport: &http.Transport{Proxy: proxyFunc, DisableKeepAlives: true},
				Timeout:   10 * time.Second,
			}
		}
	}
	// Auto-detect local mihomo HTTP proxy when running inside Docker or on NAS host
	for _, candidate := range []string{"mihomo:7890", "172.18.0.1:7890", "127.0.0.1:7890"} {
		conn, err := net.DialTimeout("tcp", candidate, 350*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			if u, parseErr := url.Parse("http://" + candidate); parseErr == nil {
				return &http.Client{
					Transport: &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true},
					Timeout:   10 * time.Second,
				}
			}
		}
	}
	return &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   10 * time.Second,
	}
}

func fetchRemoteManifestDigestWithRegistry(ctx context.Context, regClient, dockerClient *http.Client, imageRef string) (string, error) {
	if regClient != nil {
		regCtx, regCancel := context.WithTimeout(ctx, 8*time.Second)
		digest, err := fetchRegistryV2Digest(regCtx, regClient, imageRef)
		regCancel()
		if err == nil && digest != "" {
			return digest, nil
		}
	}
	return fetchRemoteManifestDigest(ctx, dockerClient, imageRef)
}

func splitRegistryRepoTag(imageRef string) (registryHost, repo, tag string) {
	repoRef, tag := parseDockerImageRef(imageRef)
	if tag == "" {
		tag = "latest"
	}
	parts := strings.SplitN(repoRef, "/", 2)
	if len(parts) == 1 {
		return "registry-1.docker.io", "library/" + parts[0], tag
	}
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		if first == "docker.io" || first == "index.docker.io" {
			first = "registry-1.docker.io"
		}
		return first, parts[1], tag
	}
	return "registry-1.docker.io", repoRef, tag
}

func fetchRegistryV2Digest(ctx context.Context, client *http.Client, imageRef string) (string, error) {
	registryHost, repo, tag := splitRegistryRepoTag(imageRef)
	hosts := []string{registryHost}
	if registryHost == "registry-1.docker.io" {
		// Direct registry-1.docker.io may be unreachable without a container-accessible proxy;
		// fall back to fast domestic Registry v2 mirrors automatically.
		hosts = []string{"docker.1ms.run", "docker.xuanyuan.me", "registry-1.docker.io"}
	}
	var lastErr error
	for _, host := range hosts {
		hCtx, hCancel := context.WithTimeout(ctx, 3*time.Second)
		digest, err := fetchRegistryV2DigestFromHost(hCtx, client, host, repo, tag)
		hCancel()
		if err == nil && digest != "" {
			return digest, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func fetchRegistryV2DigestFromHost(ctx context.Context, client *http.Client, registryHost, repo, tag string) (string, error) {
	scheme := "https"
	if strings.HasPrefix(registryHost, "127.0.0.1") || strings.HasPrefix(registryHost, "localhost") {
		scheme = "http"
	}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, registryHost, repo, url.PathEscape(tag))
	const acceptHeader = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", acceptHeader)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		token, tokErr := fetchBearerToken(ctx, client, resp.Header.Get("WWW-Authenticate"), repo)
		if tokErr != nil {
			return "", tokErr
		}
		req2, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
		if err != nil {
			return "", err
		}
		req2.Header.Set("Accept", acceptHeader)
		req2.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req2)
		if err != nil {
			return "", err
		}
		_ = resp.Body.Close()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("registry %s returned status %d", registryHost, resp.StatusCode)
	}
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return "", fmt.Errorf("registry %s omitted Docker-Content-Digest", registryHost)
	}
	return digest, nil
}

func fetchBearerToken(ctx context.Context, client *http.Client, wwwAuth, repo string) (string, error) {
	lower := strings.ToLower(wwwAuth)
	idx := strings.Index(lower, "bearer ")
	if idx < 0 {
		return "", fmt.Errorf("unsupported WWW-Authenticate header: %s", wwwAuth)
	}
	params := make(map[string]string)
	for _, part := range strings.Split(wwwAuth[idx+len("bearer "):], ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			params[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
		}
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("missing realm in WWW-Authenticate")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	} else {
		q.Set("scope", fmt.Sprintf("repository:%s:pull", repo))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&tok); err != nil {
		return "", err
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", fmt.Errorf("empty bearer token in response")
}

func inspectContainerImage(ctx context.Context, client *http.Client, containerName string) (imageRef string, imageID string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost/containers/%s/json", url.PathEscape(containerName)), nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("inspect container status %d", resp.StatusCode)
	}
	var payload struct {
		Image  string `json:"Image"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(payload.Config.Image), strings.TrimSpace(payload.Image), nil
}

func inspectLocalImageDigests(ctx context.Context, client *http.Client, imageID string) ([]string, error) {
	if imageID == "" {
		return nil, fmt.Errorf("empty image id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost/images/%s/json", url.PathEscape(imageID)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inspect image status %d", resp.StatusCode)
	}
	var payload struct {
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.RepoDigests, nil
}

func fetchRemoteManifestDigest(ctx context.Context, client *http.Client, imageRef string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost/distribution/%s/json", url.PathEscape(imageRef)), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("distribution inspect status %d", resp.StatusCode)
	}
	var payload struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(payload.Descriptor.Digest), nil
}
