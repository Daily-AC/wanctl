package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Keep these checks about trust boundaries and ordering, not Docker's progress
// output or YAML formatting. actionlint remains the full workflow parser.
func TestReleaseWorkflowSupplyChain(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	uses := regexp.MustCompile(`(?m)^\s*uses:\s*(\S+)`).FindAllStringSubmatch(workflow, -1)
	if len(uses) != 2 {
		t.Fatalf("release job must only use checkout and setup-go; got %d actions", len(uses))
	}
	seen := make(map[string]bool)
	for _, use := range uses {
		parts := strings.Split(use[1], "@")
		if len(parts) != 2 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(parts[1]) {
			t.Fatalf("action is not SHA-pinned: %s", use[1])
		}
		if parts[0] != "actions/checkout" && parts[0] != "actions/setup-go" {
			t.Fatalf("unexpected action: %s", use[1])
		}
		seen[parts[0]] = true
	}
	if len(seen) != 2 {
		t.Fatal("missing checkout or setup-go")
	}

	// Each block starts at a named step; secrets in the job/workflow preamble
	// would otherwise be inherited by the image and tool-installation steps.
	starts := regexp.MustCompile(`(?m)^      - name: ([^\n]+)`).FindAllStringSubmatchIndex(workflow, -1)
	if len(starts) == 0 {
		t.Fatal("release steps missing")
	}
	preamble := workflow[:starts[0][0]]
	if strings.Contains(preamble, "secrets.") {
		t.Fatal("signing secrets must not be job-scoped")
	}
	for _, permission := range []string{"contents", "packages", "id-token"} {
		if !strings.Contains(preamble, "      "+permission+": write") {
			t.Errorf("missing job permission %s: write", permission)
		}
	}
	type step struct{ name, body string }
	var steps []step
	for i, start := range starts {
		end := len(workflow)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		steps = append(steps, step{workflow[start[2]:start[3]], workflow[start[0]:end]})
	}
	allowedSecrets := map[string]bool{
		"Validate release secrets": true,
		"Build signed release":     true,
	}
	secretRefs := regexp.MustCompile(`secrets\.([A-Z][A-Z0-9_]+)`)
	counts := make(map[string]int)
	for _, s := range steps {
		for _, secret := range secretRefs.FindAllStringSubmatch(s.body, -1) {
			if !allowedSecrets[s.name] {
				t.Errorf("secret %s exposed to %s", secret[1], s.name)
			}
			counts[secret[1]]++
		}
		if strings.Contains(s.body, "continue-on-error:") || regexp.MustCompile(`(?m)^        if:`).MatchString(s.body) {
			t.Errorf("%s must fail closed before publishing", s.name)
		}
	}
	for _, name := range []string{"WANCTL_RELEASE_SIGNING_KEY", "WANCTL_RELEASE_RSA_KEY", "WANCTL_ANDROID_KEYSTORE_B64", "WANCTL_ANDROID_KEYSTORE_PASS"} {
		if counts[name] != 2 {
			t.Errorf("%s must be scoped to validation and release building", name)
		}
	}
	if len(counts) != 4 {
		t.Fatalf("unexpected signing secret set: %v", counts)
	}

	find := func(command string) (int, string) {
		t.Helper()
		for i, s := range steps {
			if strings.Contains(s.body, command) {
				return i, s.body
			}
		}
		t.Fatalf("missing workflow command %q", command)
		return -1, ""
	}
	build, release := find(". ./scripts/build-release.sh")
	validate, _ := find("./scripts/validate-release.sh")
	install, cosign := find("https://github.com/sigstore/cosign/releases/download/")
	image, container := find("docker build --platform linux/amd64")
	publish, _ := find("gh release create")
	if !(build < validate && validate < install && install < image && image < publish) {
		t.Fatal("image installation/build/signing must succeed before publication")
	}
	if !strings.Contains(release, `printf "keys=%s\n" "$TRUSTED_KEYS" >> "$GITHUB_OUTPUT"`) || !strings.Contains(container, "${{ steps.release.outputs.keys }}") {
		t.Fatal("image must consume the trusted keys computed by the unchanged release builder")
	}
	if !regexp.MustCompile(`https://github\.com/sigstore/cosign/releases/download/v2\.[0-9]+\.[0-9]+/cosign-linux-amd64`).MatchString(cosign) {
		t.Fatal("cosign must be a pinned official v2 linux/amd64 binary")
	}
	if !regexp.MustCompile(`(?m)^\s*'[0-9a-f]{64}'\s*\\$`).MatchString(cosign) || !strings.Contains(cosign, "sha256sum --check --strict") {
		t.Fatal("cosign binary must be checked against a hard-coded SHA-256")
	}
	if strings.Index(cosign, "sha256sum --check --strict") > strings.Index(cosign, "chmod 0755") {
		t.Fatal("verify cosign before making it executable")
	}
	for _, invariant := range []string{
		`set -euo pipefail`,
		`image="ghcr.io/daily-ac/wanctl:$tag"`,
		`printf '%s' "$GHCR_TOKEN" | docker login ghcr.io --username "$GITHUB_ACTOR" --password-stdin`,
		`--build-arg "WANCTL_VERSION=$tag"`,
		`--build-arg "WANCTL_RELEASE_PUBLIC_KEYS=$WANCTL_RELEASE_PUBLIC_KEYS"`,
		`org.opencontainers.image.source=https://github.com/Daily-AC/wanctl`,
		`org.opencontainers.image.revision=$GITHUB_SHA`,
		`org.opencontainers.image.version=$tag`,
		`--tag "$image"`,
		`docker push "$image"`,
		`^sha256:[0-9a-f]{64}$`,
		`"$RUNNER_TEMP/cosign" sign --yes "ghcr.io/daily-ac/wanctl@$digest"`,
		`"$GITHUB_STEP_SUMMARY"`,
	} {
		if !strings.Contains(container, invariant) {
			t.Errorf("missing container invariant %q", invariant)
		}
	}
	if strings.Index(container, "docker push") > strings.Index(container, `sign --yes`) {
		t.Fatal("must push before signing")
	}
	if len(regexp.MustCompile(`(?m)^\s*docker push\b`).FindAllString(container, -1)) != 1 {
		t.Fatal("only the version tag may be pushed")
	}
	if strings.Contains(container, ":latest") || strings.Contains(workflow, "container-image.txt") {
		t.Fatal("moving tags and device-manifest image metadata are forbidden")
	}
}
