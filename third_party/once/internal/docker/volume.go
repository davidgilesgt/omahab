package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

var ErrVolumeNotFound = errors.New("volume not found")

type ApplicationLegacyVolumeSettings struct {
	Keys
}

func UnmarshalApplicationLegacyVolumeSettings(s string) (ApplicationLegacyVolumeSettings, error) {
	var settings ApplicationLegacyVolumeSettings
	err := json.Unmarshal([]byte(s), &settings)
	return settings, err
}

func (s ApplicationLegacyVolumeSettings) Marshal() string {
	b, _ := json.Marshal(s)
	return string(b)
}

type ApplicationVolume struct {
	namespace *Namespace
	name      string
	Settings  ApplicationLegacyVolumeSettings
}

func (v *ApplicationVolume) Name() string {
	return v.name
}

func (v *ApplicationVolume) Destroy(ctx context.Context) error {
	if _, err := v.namespace.client.VolumeRemove(ctx, v.name, client.VolumeRemoveOptions{Force: true}); err != nil {
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("removing volume: %w", err)
		}
	}
	return nil
}

func FindVolume(ctx context.Context, ns *Namespace, name string) (*ApplicationVolume, error) {
	volumeName := fmt.Sprintf("%s-app-%s", ns.name, name)

	vol, err := ns.client.VolumeInspect(ctx, volumeName, client.VolumeInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrVolumeNotFound
		}
		return nil, fmt.Errorf("inspecting volume: %w", err)
	}

	label := vol.Volume.Labels[labelKey]
	if label == "" {
		return nil, fmt.Errorf("volume %s exists but has no once label", volumeName)
	}

	settings, err := UnmarshalApplicationLegacyVolumeSettings(label)
	if err != nil {
		return nil, fmt.Errorf("parsing volume settings: %w", err)
	}

	return &ApplicationVolume{
		namespace: ns,
		name:      volumeName,
		Settings:  settings,
	}, nil
}

func CreateVolume(ctx context.Context, ns *Namespace, name string, settings ApplicationLegacyVolumeSettings) (*ApplicationVolume, error) {
	volumeName := fmt.Sprintf("%s-app-%s", ns.name, name)

	_, err := ns.client.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name: volumeName,
		Labels: map[string]string{
			labelKey: settings.Marshal(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating volume: %w", err)
	}

	return &ApplicationVolume{
		namespace: ns,
		name:      volumeName,
		Settings:  settings,
	}, nil
}
