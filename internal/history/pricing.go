package history

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// skuRates are GitHub-hosted runner list prices in USD per minute, copied
// from https://docs.github.com/en/billing/reference/actions-runner-pricing
// (checked 2026-11-12). Update them when that page changes.
var skuRates = map[string]float64{
	// Standard runners.
	"actions_linux_slim":  0.002,
	"actions_linux":       0.006,
	"actions_linux_arm":   0.005,
	"actions_windows":     0.010,
	"actions_windows_arm": 0.010,
	"actions_macos":       0.062,
	// Larger runners, x64.
	"linux_2_core_advanced": 0.006,
	"linux_4_core":          0.012,
	"linux_8_core":          0.022,
	"linux_16_core":         0.042,
	"linux_32_core":         0.082,
	"linux_64_core":         0.162,
	"linux_96_core":         0.252,
	"windows_4_core":        0.022,
	"windows_8_core":        0.042,
	"windows_16_core":       0.082,
	"windows_32_core":       0.162,
	"windows_64_core":       0.322,
	"windows_96_core":       0.552,
	"macos_l":               0.077,
	// Larger runners, arm64.
	"linux_2_core_arm":    0.005,
	"linux_4_core_arm":    0.008,
	"linux_8_core_arm":    0.014,
	"linux_16_core_arm":   0.026,
	"linux_32_core_arm":   0.050,
	"linux_64_core_arm":   0.098,
	"windows_2_core_arm":  0.008,
	"windows_4_core_arm":  0.014,
	"windows_8_core_arm":  0.026,
	"windows_16_core_arm": 0.050,
	"windows_32_core_arm": 0.098,
	"windows_64_core_arm": 0.194,
	"macos_xl":            0.102,
	// GPU runners.
	"linux_4_core_gpu":   0.052,
	"windows_4_core_gpu": 0.102,
}

// standardLabels maps GitHub-hosted runs-on labels to their SKU. Order
// matters: the more specific patterns come first.
var standardLabels = []struct {
	re  *regexp.Regexp
	sku string
}{
	{regexp.MustCompile(`^ubuntu-slim$`), "actions_linux_slim"},
	{regexp.MustCompile(`^ubuntu-.*-arm$`), "actions_linux_arm"},
	{regexp.MustCompile(`^ubuntu-`), "actions_linux"},
	{regexp.MustCompile(`^windows-.*-arm$`), "actions_windows_arm"},
	{regexp.MustCompile(`^windows-`), "actions_windows"},
	{regexp.MustCompile(`^macos-.*-xlarge$`), "macos_xl"},
	{regexp.MustCompile(`^macos-.*-large$`), "macos_l"},
	{regexp.MustCompile(`^macos-`), "actions_macos"},
}

// Runner is how one job is priced.
type Runner struct {
	// Class is the SKU name, "self-hosted", a --rate label, or
	// "unpriced:<label>" when nothing matched.
	Class string
	// Rate is USD per billed minute.
	Rate float64
	// Billed is false for self-hosted and unpriced runners.
	Billed bool
}

// Pricing resolves job labels to rates, with user overrides on top of the
// built-in table.
type Pricing struct {
	overrides map[string]float64 // runs-on label -> USD/min
}

// NewPricing parses --rate values of the form LABEL=USD or LABEL=SKU, for
// larger runners whose labels are chosen per organization.
func NewPricing(rates []string) (*Pricing, error) {
	p := &Pricing{overrides: map[string]float64{}}
	for _, r := range rates {
		label, val, ok := strings.Cut(r, "=")
		label, val = strings.TrimSpace(label), strings.TrimSpace(val)
		if !ok || label == "" || val == "" {
			return nil, fmt.Errorf("--rate wants LABEL=USD_PER_MIN or LABEL=SKU, got %q", r)
		}
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			if f < 0 {
				return nil, fmt.Errorf("--rate %q: negative price", r)
			}
			p.overrides[strings.ToLower(label)] = f
			continue
		}
		f, ok := skuRates[strings.ToLower(val)]
		if !ok {
			return nil, fmt.Errorf("--rate %q: %q is neither a number nor a known SKU (%s)", r, val, strings.Join(SKUs(), ", "))
		}
		p.overrides[strings.ToLower(label)] = f
	}
	return p, nil
}

// SKUs lists the built-in SKU names, sorted.
func SKUs() []string {
	out := make([]string, 0, len(skuRates))
	for k := range skuRates {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resolve prices a job from its runs-on labels. A --rate override wins, then
// self-hosted (not billed by GitHub per minute), then the standard labels.
func (p *Pricing) Resolve(labels []string) Runner {
	for _, l := range labels {
		if f, ok := p.overrides[strings.ToLower(l)]; ok {
			return Runner{Class: l, Rate: f, Billed: true}
		}
	}
	for _, l := range labels {
		if strings.EqualFold(l, "self-hosted") {
			return Runner{Class: "self-hosted"}
		}
	}
	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, s := range standardLabels {
			if s.re.MatchString(ll) {
				return Runner{Class: s.sku, Rate: skuRates[s.sku], Billed: true}
			}
		}
	}
	label := "none"
	if len(labels) > 0 {
		label = strings.Join(labels, ",")
	}
	return Runner{Class: "unpriced:" + label}
}
