package scm

import (
	"fmt"
	"strings"
)

// PipelineTemplateInput holds the values substituted into the Woodpecker
// pipeline template. No secret values are interpolated; secrets are referenced
// by name and resolved inside Woodpecker from repository secrets.
type PipelineTemplateInput struct {
	Owner              string
	Name               string
	DefaultBranch      string
	RegistryHost       string
	ReleaseCallbackURL string
	BuilderImage       string
	ProjectID          string
}

// PipelineTemplate returns a Woodpecker pipeline YAML for the repository.
//
// The pipeline:
//   - runs only on Woodpecker (DESIGN §11.2); Forgejo Actions remains disabled
//   - builds the project OCI image
//   - pushes it to the Forgejo registry by digest
//   - calls the narrow omahabd release callback with commit and digest
//
// The callback URL and registry host are templated from inputs; if empty,
// sensible defaults are used so tests need not wire real hostnames.
// Secrets are referenced by name (OMAHAB_REGISTRY_USER, etc.) and must be
// provisioned as Woodpecker repository secrets via the project's secret
// namespace — they are never written into the YAML.
func PipelineTemplate(in PipelineTemplateInput) string {
	owner := in.Owner
	if owner == "" {
		owner = "omahab"
	}
	name := in.Name
	if name == "" {
		name = "project"
	}
	branch := in.DefaultBranch
	if branch == "" {
		branch = "master"
	}
	registry := in.RegistryHost
	if registry == "" {
		registry = "git.example.com"
	}
	callback := strings.TrimSpace(in.ReleaseCallbackURL)
	if callback == "" {
		callback = fmt.Sprintf("https://omahabd.example.com/v1/projects/%s/releases", name)
		if strings.TrimSpace(in.ProjectID) != "" {
			callback = fmt.Sprintf("https://omahab.example.com/api/v1/projects/%s/releases/with-token", strings.TrimSpace(in.ProjectID))
		}
	}
	builderImage := strings.TrimSpace(in.BuilderImage)
	if builderImage == "" {
		builderImage = "quay.io/podman/stable"
	}
	image := fmt.Sprintf("%s/%s/%s", registry, owner, name)

	return fmt.Sprintf(`# Managed by Omahab — do not edit the header.
# Woodpecker is the only CI system for this project (DESIGN §11.2).
# Forgejo Actions is disabled. Commit this file as .woodpecker.yaml
# in the repository root.
#
# Required Woodpecker repository secrets (project secret namespace):
#   OMAHAB_REGISTRY_USER / OMAHAB_REGISTRY_PASSWORD — Forgejo registry credentials
#   OMAHAB_RELEASE_TOKEN                              — narrow release-callback token
# Secrets are references, never committed in this file.

when:
  - event: push
    branch: %s

steps:
  build-and-push:
    image: %s
    environment:
      IMAGE: %s
    secrets: [omahab_registry_user, omahab_registry_password, omahab_release_token]
    commands:
      - echo "$OMAHAB_REGISTRY_PASSWORD" | podman --remote --url unix:///run/omahab-builder/podman.sock login %s -u "$OMAHAB_REGISTRY_USER" --password-stdin
      - podman --remote --url unix:///run/omahab-builder/podman.sock build -t "$IMAGE:sha-$CI_COMMIT_SHA" .
      - podman --remote --url unix:///run/omahab-builder/podman.sock push --digestfile /tmp/digestfile "$IMAGE:sha-$CI_COMMIT_SHA"
      - DIGEST="$(cat /tmp/digestfile)"
      - echo "$DIGEST" | grep -Eq "^sha256:[0-9a-f]{64}$" || (echo "invalid digest $DIGEST" >&2; exit 1)
      - echo "image digest: $DIGEST"
      - >
        curl -fsS -X POST %s
        -H "Authorization: Bearer $OMAHAB_RELEASE_TOKEN"
        -H "Content-Type: application/json"
        -d "{\"commit\":\"$CI_COMMIT_SHA\",\"digest\":\"$DIGEST\"}"
`, branch, builderImage, image, registry, callback)
}

