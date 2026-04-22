package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"polysnipe/internal/gamma"
)

// etZone is US Eastern Time, used for 1h slug generation.
var etZone = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// assetNameMap maps the short 15m slug prefix to the full 1h name prefix.
var assetNameMap = map[string]string{
	"btc":  "bitcoin",
	"eth":  "ethereum",
	"sol":  "solana",
	"xrp":  "xrp",
	"bnb":  "bnb",
	"matic": "polygon",
}

// upDownDiscovery fetches the recurring crypto Up/Down markets by generating
// deterministic slugs from the current time, then looking each up via the Gamma API.
// This targets only known-liquid markets (BTC/ETH/SOL 15m and 1h series).
type upDownDiscovery struct {
	gamma  *gamma.Client
	assets []string // e.g. ["btc", "eth", "sol"]
	log    zerolog.Logger
}

func newUpDownDiscovery(gc *gamma.Client, assets []string, log zerolog.Logger) *upDownDiscovery {
	return &upDownDiscovery{
		gamma:  gc,
		assets: assets,
		log:    log.With().Str("component", "updown_discovery").Logger(),
	}
}

// poll generates slug candidates for the current time window and fetches any that exist.
// Returns the markets as GammaMarkets ready to be merged into the engine's active set.
func (d *upDownDiscovery) poll(ctx context.Context) []gamma.GammaMarket {
	now := time.Now()
	slugs := d.candidateSlugs(now)

	seen := make(map[string]bool)
	var markets []gamma.GammaMarket

	for _, slug := range slugs {
		if seen[slug] {
			continue
		}
		seen[slug] = true

		if ctx.Err() != nil {
			break
		}

		ev, err := d.gamma.GetEventBySlug(ctx, slug)
		if err != nil {
			d.log.Debug().Err(err).Str("slug", slug).Msg("slug lookup failed")
			continue
		}
		if ev == nil || ev.Closed || len(ev.Markets) == 0 {
			continue
		}

		mkt := ev.Markets[0]
		if mkt.Closed || !mkt.Active {
			continue
		}
		if time.Until(mkt.EndDate) < 2*time.Minute {
			continue
		}

		d.log.Debug().Str("slug", slug).Str("question", mkt.Question).Msg("updown market found")
		markets = append(markets, mkt)
	}

	return markets
}

// candidateSlugs generates all slug candidates for all configured assets.
func (d *upDownDiscovery) candidateSlugs(now time.Time) []string {
	var slugs []string
	for _, asset := range d.assets {
		slugs = append(slugs, candidate15mSlugs(asset, now)...)
		if namePrefix, ok := assetNameMap[asset]; ok {
			slugs = append(slugs, candidate1hSlugs(namePrefix, now)...)
		}
	}
	return slugs
}

// candidate15mSlugs generates slugs for the 15-minute Up/Down series.
// Format: {asset}-updown-15m-{start_epoch}
// Covers now-30min to now+15min so we always catch the active window.
func candidate15mSlugs(assetPrefix string, now time.Time) []string {
	nowSec := now.Unix()
	from := nowSec - 1800 // 30 min back
	to := nowSec + 900    // 15 min ahead

	startFrom := (from / 900) * 900
	startTo := (to / 900) * 900

	var out []string
	for start := startFrom; start <= startTo; start += 900 {
		out = append(out, fmt.Sprintf("%s-updown-15m-%d", assetPrefix, start))
	}
	return out
}

// candidate1hSlugs generates slugs for the 1-hour Up/Down series.
// Format: {name}-up-or-down-{month}-{day}-{hour}{ampm}-et
// Covers now±2 hours so we don't miss a market around the hour boundary.
func candidate1hSlugs(namePrefix string, now time.Time) []string {
	et := now.In(etZone)
	hourStart := et.Truncate(time.Hour)

	times := []time.Time{
		hourStart.Add(-2 * time.Hour),
		hourStart.Add(-1 * time.Hour),
		hourStart,
		hourStart.Add(time.Hour),
	}

	var out []string
	for _, t := range times {
		out = append(out, build1hSlug(namePrefix, t))
	}
	return out
}

func build1hSlug(namePrefix string, t time.Time) string {
	month := strings.ToLower(t.Month().String())
	day := t.Day()
	hour24 := t.Hour()
	hour12 := hour24 % 12
	if hour12 == 0 {
		hour12 = 12
	}
	ampm := "am"
	if hour24 >= 12 {
		ampm = "pm"
	}
	return fmt.Sprintf("%s-up-or-down-%s-%d-%d%s-et", namePrefix, month, day, hour12, ampm)
}
