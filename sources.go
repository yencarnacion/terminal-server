package main

// Attach the same direct-first fallback policy to every external-data page.
// The transport keeps private LAN sources direct and preserves each service's
// own timeout, redirect policy, parsing, and freshness validation.
func (a *app) useScrapfly(endpoint string) {
	if endpoint == "" {
		return
	}
	if a.news != nil {
		a.news.client = withScrapflyFallback(a.news.client, endpoint)
	}
	if a.frontpages != nil {
		a.frontpages.client = withScrapflyFallback(a.frontpages.client, endpoint)
	}
	if a.weather != nil {
		a.weather.client = withScrapflyFallback(a.weather.client, endpoint)
	}
	if a.fearGreed != nil {
		a.fearGreed.client = withScrapflyFallback(a.fearGreed.client, endpoint)
	}
	if a.kalshi != nil {
		a.kalshi.client = withScrapflyFallback(a.kalshi.client, endpoint)
	}
	if a.polymarket != nil {
		a.polymarket.client = withScrapflyFallback(a.polymarket.client, endpoint)
	}
}
