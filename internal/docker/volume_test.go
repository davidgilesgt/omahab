package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeSettingsMarshalRoundTrip(t *testing.T) {
	original := ApplicationLegacyVolumeSettings{
		Keys: Keys{
			SecretKeyBase:   "secret",
			VAPIDPublicKey:  "pub123",
			VAPIDPrivateKey: "priv456",
		},
	}

	restored, err := UnmarshalApplicationLegacyVolumeSettings(original.Marshal())
	require.NoError(t, err)
	assert.Equal(t, original, restored)
}

func TestVolumeSettingsUnmarshalLegacyLabel(t *testing.T) {
	restored, err := UnmarshalApplicationLegacyVolumeSettings(`{"secretKeyBase":"secret","vapidPublicKey":"pub123","vapidPrivateKey":"priv456"}`)
	require.NoError(t, err)
	assert.Equal(t, "secret", restored.SecretKeyBase)
	assert.Equal(t, "pub123", restored.VAPIDPublicKey)
	assert.Equal(t, "priv456", restored.VAPIDPrivateKey)
}
