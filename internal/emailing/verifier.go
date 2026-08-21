package emailing

import (
	"context"
	"strings"
)

// StaticVerifier is a test helper implementing DKIMVerifier with fixed result.
type StaticVerifier struct {
	Result DKIMResult
	Err    error
}

func (v StaticVerifier) Verify(_ context.Context, _ []byte) (DKIMResult, error) {
	if v.Err != nil {
		return DKIMResult{}, v.Err
	}
	return v.Result, nil
}

// FuncVerifier allows a function to implement DKIMVerifier.
type FuncVerifier func(ctx context.Context, raw []byte) (DKIMResult, error)

func (f FuncVerifier) Verify(ctx context.Context, raw []byte) (DKIMResult, error) {
	return f(ctx, raw)
}

// ValidVerifier returns a verifier that always reports Valid=true, From signed, aligned.
func ValidVerifier(domain string) DKIMVerifier {
	domain = strings.ToLower(domain)
	return StaticVerifier{Result: DKIMResult{
		Valid:         true,
		Domain:        domain,
		SignedHeaders: []string{"from", "subject", "to", "date"},
		Aligned:       true,
	}}
}

// InvalidVerifier returns a verifier that reports Valid=false.
func InvalidVerifier() DKIMVerifier {
	return StaticVerifier{Result: DKIMResult{Valid: false}}
}

// MissingFromVerifier reports Valid but From not in SignedHeaders.
func MissingFromVerifier(domain string) DKIMVerifier {
	return StaticVerifier{Result: DKIMResult{
		Valid:         true,
		Domain:        strings.ToLower(domain),
		SignedHeaders: []string{"subject", "to"},
		Aligned:       true,
	}}
}

// MisalignedVerifier reports Valid and From signed but not aligned.
func MisalignedVerifier(domain string) DKIMVerifier {
	return StaticVerifier{Result: DKIMResult{
		Valid:         true,
		Domain:        strings.ToLower(domain),
		SignedHeaders: []string{"from"},
		Aligned:       false,
	}}
}
