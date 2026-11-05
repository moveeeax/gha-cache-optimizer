package analyzer

import (
	"regexp"
	"strings"
)

// ecosystem describes one dependency toolchain: how to recognize a step that
// installs its dependencies, and the actions/cache block that would fix a miss.
type ecosystem struct {
	name string
	// lockfile is the file whose hash keys the cache.
	lockfile string
	// cachePaths are the directories actions/cache should store.
	cachePaths []string
	// setupAction is the actions/setup-* that offers a built-in `cache:` input.
	setupAction string
	// setupMajor is the current major version tag of setupAction used in
	// suggested fixes (e.g. "v7").
	setupMajor string
	// setupCacheValue is the value for that action's `cache:` input, if any.
	setupCacheValue string
	// installRe matches a `run:` command that installs dependencies.
	installRe *regexp.Regexp
}

// cacheActionMajor is the actions/cache major version tag used in suggested
// fixes. Keep it (and each ecosystem's setupMajor) on the latest major.
const cacheActionMajor = "v6"

// ecosystems is the ordered detection table. Order matters only for stable
// output; each step matches at most one ecosystem (first hit wins).
var ecosystems = []ecosystem{
	{
		name:            "node",
		lockfile:        "package-lock.json",
		cachePaths:      []string{"~/.npm"},
		setupAction:     "actions/setup-node",
		setupMajor:      "v7",
		setupCacheValue: "npm",
		installRe:       regexp.MustCompile(`\bnpm (ci|install|i)\b|\byarn(\s+install)?\b|\bpnpm (install|i)\b`),
	},
	{
		name:            "go",
		lockfile:        "go.sum",
		cachePaths:      []string{"~/.cache/go-build", "~/go/pkg/mod"},
		setupAction:     "actions/setup-go",
		setupMajor:      "v7",
		setupCacheValue: "", // setup-go caches by default (cache: true); flag only run-based installs
		installRe:       regexp.MustCompile(`\bgo (build|test|mod download|install)\b`),
	},
	{
		name:            "python",
		lockfile:        "requirements.txt",
		cachePaths:      []string{"~/.cache/pip"},
		setupAction:     "actions/setup-python",
		setupMajor:      "v7",
		setupCacheValue: "pip",
		installRe:       regexp.MustCompile(`\bpip3? install\b|\bpoetry install\b`),
	},
	{
		name:            "java-maven",
		lockfile:        "pom.xml",
		cachePaths:      []string{"~/.m2/repository"},
		setupAction:     "actions/setup-java",
		setupMajor:      "v6",
		setupCacheValue: "maven",
		installRe:       regexp.MustCompile(`\bmvn\b`),
	},
	{
		name:            "ruby",
		lockfile:        "Gemfile.lock",
		cachePaths:      []string{"vendor/bundle"},
		setupAction:     "ruby/setup-ruby",
		setupMajor:      "v1",
		setupCacheValue: "", // ruby/setup-ruby uses bundler-cache: true
		installRe:       regexp.MustCompile(`\bbundle install\b`),
	},
}

// detectEcosystem returns the ecosystem an install command belongs to, if any.
func detectEcosystem(runCmd string) (ecosystem, bool) {
	for _, e := range ecosystems {
		if e.installRe.MatchString(runCmd) {
			return e, true
		}
	}
	return ecosystem{}, false
}

// usesAction reports whether an "uses:" value references the given action,
// ignoring the @version suffix.
func usesAction(uses, action string) bool {
	uses = strings.TrimSpace(uses)
	if i := strings.IndexByte(uses, '@'); i >= 0 {
		uses = uses[:i]
	}
	return strings.EqualFold(uses, action)
}
