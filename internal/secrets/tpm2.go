//go:build linux

package secrets

import (
	"encoding/binary"
	"fmt"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
)

// TPMSealer implements Sealer by sealing data to the TPM2 SRK.
// Data is sealed under a fixed SRK policy: a primary RSA SRK at TPM_RH_OWNER
// using the TCG reference RSASRKTemplate (restricted decrypt key, FixedTPM+FixedParent).
// Without the TPM's seed, the blob cannot be unsealed. No password or PCR policy
// is used; binding is to the TPM hardware itself.
type TPMSealer struct {
	devicePath string
	// opener overrides TPM opening for tests; nil uses defaultOpen.
	opener func() (transport.TPMCloser, error)
}

// NewTPMSealer creates a Sealer that uses the default TPM device
// (/dev/tpmrm0, falling back to /dev/tpm0).
func NewTPMSealer() *TPMSealer {
	return &TPMSealer{devicePath: ""}
}

// NewTPMSealerWithDevice creates a Sealer that uses the given device path.
// An empty path is equivalent to NewTPMSealer.
func NewTPMSealerWithDevice(path string) *TPMSealer {
	return &TPMSealer{devicePath: path}
}

// NewTPMSealerWithOpener is a test helper that creates a Sealer with a custom
// TPM opener (e.g., a simulator). The devicePath is ignored when opener is set.
func NewTPMSealerWithOpener(opener func() (transport.TPMCloser, error)) *TPMSealer {
	return &TPMSealer{opener: opener}
}

func (s *TPMSealer) openTPM() (transport.TPMCloser, error) {
	if s.opener != nil {
		return s.opener()
	}
	if s.devicePath != "" {
		tpm, err := linuxtpm.Open(s.devicePath)
		if err != nil {
			return nil, fmt.Errorf("tpm2: open %s: %w", s.devicePath, err)
		}
		return tpm, nil
	}
	for _, p := range []string{"/dev/tpmrm0", "/dev/tpm0"} {
		tpm, err := linuxtpm.Open(p)
		if err == nil {
			return tpm, nil
		}
	}
	return nil, fmt.Errorf("%w: no TPM device found at /dev/tpmrm0 or /dev/tpm0", ErrTPMNotAvailable)
}

func createSRK(tpm transport.TPM) (*tpm2.CreatePrimaryResponse, error) {
	rsp, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHOwner,
		InPublic:      tpm2.New2B(tpm2.RSASRKTemplate),
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm2 create primary: %w", err)
	}
	return rsp, nil
}

func flushHandle(tpm transport.TPM, h tpm2.TPMHandle) {
	_, _ = tpm2.FlushContext{FlushHandle: h}.Execute(tpm)
}

// Seal seals plaintext to the TPM SRK and returns an opaque blob that can be
// persisted to disk. The blob contains the TPM2BPrivate and TPM2BPublic of the
// sealed object, length-prefixed for Unseal.
func (s *TPMSealer) Seal(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("tpm2 seal: plaintext is required")
	}
	tpm, err := s.openTPM()
	if err != nil {
		return nil, err
	}
	defer tpm.Close()

	srk, err := createSRK(tpm)
	if err != nil {
		return nil, err
	}
	defer flushHandle(tpm, srk.ObjectHandle)

	createRsp, err := tpm2.Create{
		ParentHandle: tpm2.AuthHandle{
			Handle: srk.ObjectHandle,
			Name:   srk.Name,
			Auth:   tpm2.PasswordAuth(nil),
		},
		InSensitive: tpm2.TPM2BSensitiveCreate{
			Sensitive: &tpm2.TPMSSensitiveCreate{
				Data: tpm2.NewTPMUSensitiveCreate(&tpm2.TPM2BSensitiveData{
					Buffer: plaintext,
				}),
			},
		},
		InPublic: tpm2.New2B(tpm2.TPMTPublic{
			Type:    tpm2.TPMAlgKeyedHash,
			NameAlg: tpm2.TPMAlgSHA256,
			ObjectAttributes: tpm2.TPMAObject{
				FixedTPM:     true,
				FixedParent:  true,
				UserWithAuth: true,
				NoDA:         true,
			},
		}),
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm2 create sealed object: %w", err)
	}

	privM := tpm2.Marshal(createRsp.OutPrivate)
	pubM := tpm2.Marshal(createRsp.OutPublic)

	buf := make([]byte, 4+len(privM)+4+len(pubM))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(privM)))
	copy(buf[4:], privM)
	binary.BigEndian.PutUint32(buf[4+len(privM):], uint32(len(pubM)))
	copy(buf[4+len(privM)+4:], pubM)
	return buf, nil
}

// Unseal reverses Seal, loading the sealed blob under the same fixed SRK and
// returning the original plaintext.
func (s *TPMSealer) Unseal(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 8 {
		return nil, fmt.Errorf("tpm2 unseal: invalid sealed blob")
	}
	privLen := binary.BigEndian.Uint32(ciphertext[0:4])
	if uint32(len(ciphertext)) < 4+privLen+4 {
		return nil, fmt.Errorf("tpm2 unseal: truncated sealed blob")
	}
	privM := ciphertext[4 : 4+privLen]
	pubLenOffset := 4 + privLen
	pubLen := binary.BigEndian.Uint32(ciphertext[pubLenOffset : pubLenOffset+4])
	pubM := ciphertext[pubLenOffset+4:]
	if uint32(len(pubM)) != pubLen {
		return nil, fmt.Errorf("tpm2 unseal: mismatched public length")
	}

	priv, err := tpm2.Unmarshal[tpm2.TPM2BPrivate](privM)
	if err != nil {
		return nil, fmt.Errorf("tpm2 unseal: unmarshal private: %w", err)
	}
	pub, err := tpm2.Unmarshal[tpm2.TPM2BPublic](pubM)
	if err != nil {
		return nil, fmt.Errorf("tpm2 unseal: unmarshal public: %w", err)
	}

	tpm, err := s.openTPM()
	if err != nil {
		return nil, err
	}
	defer tpm.Close()

	srk, err := createSRK(tpm)
	if err != nil {
		return nil, err
	}
	defer flushHandle(tpm, srk.ObjectHandle)

	loadRsp, err := tpm2.Load{
		ParentHandle: tpm2.AuthHandle{
			Handle: srk.ObjectHandle,
			Name:   srk.Name,
			Auth:   tpm2.PasswordAuth(nil),
		},
		InPrivate: *priv,
		InPublic:  *pub,
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm2 load sealed object: %w", err)
	}
	defer flushHandle(tpm, loadRsp.ObjectHandle)

	unsealRsp, err := tpm2.Unseal{
		ItemHandle: tpm2.AuthHandle{
			Handle: loadRsp.ObjectHandle,
			Name:   loadRsp.Name,
			Auth:   tpm2.PasswordAuth(nil),
		},
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("tpm2 unseal: %w", err)
	}
	// Copy out before returning; TPM buffer is owned by response.
	out := make([]byte, len(unsealRsp.OutData.Buffer))
	copy(out, unsealRsp.OutData.Buffer)
	return out, nil
}

// Ensure TPMSealer implements Sealer.
var _ Sealer = (*TPMSealer)(nil)
