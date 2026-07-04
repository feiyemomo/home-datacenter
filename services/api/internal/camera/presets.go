package camera

import (
	"context"
	"fmt"

	"home-datacenter-api/internal/model"
)

// SetPreset stores (or updates) a single friendly-name → ONVIF
// preset-token mapping. Existing aliases are preserved.
func (r *Registry) SetPreset(camID uint, alias, token string) (*model.Camera, error) {
	cam, err := r.Get(camID)
	if err != nil {
		return nil, err
	}
	if cam.Presets == nil {
		cam.Presets = model.JSON{}
	}
	cam.Presets[alias] = token
	if err := r.DB.Model(&model.Camera{}).Where("id = ?", camID).Updates(map[string]any{
		"presets":    model.JSON(cam.Presets),
		"updated_at": nowFn(),
	}).Error; err != nil {
		return nil, err
	}
	return r.Get(camID)
}

// DeletePreset removes an alias. Deleting a non-existent alias is
// a no-op (returns the current camera, no error).
func (r *Registry) DeletePreset(camID uint, alias string) (*model.Camera, error) {
	cam, err := r.Get(camID)
	if err != nil {
		return nil, err
	}
	if cam.Presets == nil {
		return cam, nil
	}
	delete(cam.Presets, alias)
	if err := r.DB.Model(&model.Camera{}).Where("id = ?", camID).Updates(map[string]any{
		"presets":    model.JSON(cam.Presets),
		"updated_at": nowFn(),
	}).Error; err != nil {
		return nil, err
	}
	return r.Get(camID)
}

// GotoPreset looks up the preset token by alias and dispatches an
// ONVIF GotoPreset call.
func (r *Registry) GotoPreset(
	ctx context.Context,
	camID uint, alias string, speed float64,
) error {
	cam, err := r.Get(camID)
	if err != nil {
		return err
	}
	if cam.Credentials == nil {
		return fmt.Errorf("camera %d: no credentials", camID)
	}
	if cam.Presets == nil {
		return fmt.Errorf("camera %d: no presets defined", camID)
	}
	raw, ok := cam.Presets[alias]
	if !ok {
		return fmt.Errorf("camera %d: preset %q not defined", camID, alias)
	}
	token, _ := raw.(string)
	if token == "" {
		return fmt.Errorf("camera %d: preset %q has empty token", camID, alias)
	}
	profile := cam.OnvifProfileToken
	if profile == "" {
		return fmt.Errorf("camera %d: no onvif profile token", camID)
	}
	user, pass, err := r.DecryptCredentials(cam)
	if err != nil {
		return err
	}
	return r.ONVIF.GotoPreset(ctx, camID, cam.Host, cam.ONVIFPort, user, pass, profile, token, speed)
}

// ListPresets hits ONVIF GetPresets on the camera and returns the
// canonical list. We do NOT persist the result — ONVIF is the
// source of truth. The alias layer (model.Presets) is on top.
func (r *Registry) ListPresets(
	ctx context.Context, camID uint,
) ([]Preset, error) {
	cam, err := r.Get(camID)
	if err != nil {
		return nil, err
	}
	profile := cam.OnvifProfileToken
	if profile == "" {
		return nil, fmt.Errorf("camera %d: no onvif profile token", camID)
	}
	user, pass, err := r.DecryptCredentials(cam)
	if err != nil {
		return nil, err
	}
	return r.ONVIF.DiscoverPresets(ctx, cam.Host, cam.ONVIFPort, user, pass, profile)
}
