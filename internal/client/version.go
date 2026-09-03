package client

// Version is the compiled-in client daemon version, set via ldflags
// -X github.com/omahab/omahab/internal/client.Version=${version} in flake.nix.
// The daemon compares this to the server's Status.Version and self-updates on skew.
var Version = "dev"
