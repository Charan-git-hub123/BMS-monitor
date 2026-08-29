package main

// Dynamic BookMyShow extraction layer.
//
// Design note: we never *construct* a BMS URL from user-typed internal IDs
// (that was the approach that failed -- BMS mints fresh ET/venue/session IDs
// per release and they expire). Instead each step navigates to a page we
// already hold a real link to, and harvests the next set of real links.
// Only the city explore URL is derived from user input; everything after it
// is discovered from the live DOM.
//
// Maps to the README checklist:
// // fetchCityMovies             -> fetchCityMovies
// // fetchMovieLanguagesAndTheaters -> fetchBookingOptions + fetchTheatersAndShowtimes
// // resolveSeatLayoutURL         -> resolveSeatLayoutURL

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// choice is one selectable option presented to the user in WhatsApp.
type choice struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Code  string `json:"code"`
	Venue string `json:"venue"`
	Time  string `json:"time"`
}

const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// withChrome runs fn in a fresh headless browser. Each step gets its own
// browser rather than holding one open across WhatsApp round-trips: a
// conversation can idle for minutes, and a leaked browser per abandoned
// session would exhaust the container.
func withChrome(timeout time.Duration, fn func(ctx context.Context) error) error {
	execPath := resolveChromePath()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.UserAgent(defaultUA),
		chromedp.WindowSize(1400, 1000),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("blink-settings", "imagesEnabled=false"),
		chromedp.Flag("disable-extensions", true),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	runCtx, cancelTimeout := context.WithTimeout(browserCtx, timeout)
	defer cancelTimeout()

	// Best effort: drop requests that cannot affect what we extract. Failure
	// here is not fatal, it just means a slower page.
	var blocked []string
	for _, pattern := range []string{
		"*://*/*.jpg", "*://*/*.jpeg", "*://*/*.png", "*://*/*.gif",
		"*://*/*.webp", "*://*/*.ico", "*://*/*.woff", "*://*/*.woff2",
		"*://*/*.ttf", "*://*/*.otf", "*://*/*.mp4", "*://*/*.webm",
		"*://www.google-analytics.com/*", "*://*.googletagmanager.com/*",
		"*://*.doubleclick.net/*", "*://*.facebook.net/*",
		"*://*.hotjar.com/*", "*://*.clarity.ms/*", "*://*.branch.io/*",
	} {
		blocked = append(blocked, pattern)
	}
	if err := chromedp.Run(runCtx,
		network.Enable(),
		network.SetBlockedURLs(blocked),
	); err != nil {
		log.Printf("[scrape] could not install request blocking: %v", err)
	}

	return fn(runCtx)
}

// blockCheckJS reports whether we landed on something other than BMS --
// a DNS filter page, a captcha wall, an error page. Without this a blocked
// fetch looks identical to "no movies playing", which is how the previous
// version silently returned empty menus.
const blockCheckJS = `(() => {
	let host = location.hostname;
	let t = (document.title || "").toLowerCase();
	let txt = (document.body ? document.body.innerText : "").slice(0, 400).toLowerCase();
	if (host.endsWith("bookmyshow.com")) return "REDIRECTED off BMS to " + host + " (title: " + t + ")";
	if (txt.indexOf("blocked") >= 0 || txt.indexOf("content filtering") >= 0) return "BLOCKED by a firewall";
	if (txt.indexOf("are you a robot") >= 0 || txt.indexOf("captcha") >= 0) return "BLOCKED by a CAPTCHA";
	if (txt.indexOf("access denied") >= 0 || txt.indexOf("403 forbidden") >= 0) return "ACCESS DENIED";
	if ((document.body ? document.body.innerHTML.length : 0) < 2000) return "PAGE NEARLY EMPTY (len " + (document.body ? document.body.innerHTML.length : 0) + ") - likely JS blocked or still loading";
	return "";
})()`

func checkNotBlocked(ctx context.Context, stage string) error {
	var reason string
	if err := chromedp.Run(ctx, chromedp.Evaluate(blockCheckJS, &reason)); err != nil {
		return nil
	}
	if reason != "" {
		return fmt.Errorf("%s: %s", stage, reason)
	}
	return nil
}

// citySlug normalizes city names to BookMyShow's path format.
func citySlug(city string) string {
	s := strings.ToLower(strings.TrimSpace(city))
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// movieListJS harvests every anchor carrying a BMS event code (ET######).
// Keyed on the URL shape rather than CSS classes, as a BMS restyle does not
// break it.
const movieListJS = `(() => {
	let out = [];
	let seen = {};
	document.querySelectorAll('a[href]').forEach(a => {
		let href = a.href || "";
		let m = href.match(/\/ET(\d{6})/);
		if (!m) return;
		let code = m[1];
		if (seen[code]) return;
		let label = (a.getAttribute('aria-label') || "").trim();
		if (!label) {
			let img = a.querySelector('img[alt]');
			if (img) label = img.alt || "";
			img = document.querySelector('img');
			if (img && img.alt) label = img.alt || "";
		}
		if (!label) label = (a.innerText || "").trim().split("\n")[0].trim();
		if (!label) {
			let slug = href.split('?')[0].split('/').filter(s => s && s !== code);
			label = slug.length ? slug[slug.length - 1].replace(/-/g, ' ') : code;
		}
		if (!label) return;
		seen[code] = 1;
		out.push({label: label, url: href, code: code});
	});
	return out;
})()`

func fetchCityMovies(city string) ([]choice, error) {
	slug := citySlug(city)
	var found []choice
	var lastErr error

	// Two known shapes for the city listing page; try each until one yields.
	for _, u := range []string{
		"https://in.bookmyshow.com/explore/movies-" + slug,
		"https://in.bookmyshow.com/" + slug + "/movies",
	} {
		var got []choice
		err := withChrome(75*time.Second, func(ctx context.Context) error {
			if err := navigateAndSettle(ctx, u, 20*time.Second); err != nil {
				return err
			}
			if err := checkNotBlocked(ctx, "city listing"); err != nil {
				return err
			}
			return extractWithRetry(ctx, "movie list", movieListJS, &got,
				func() int { return len(got) }, 25*time.Second)
		})
		log.Printf("[scrape] city listing %s -> %d movies, err=%v", u, len(got), err)
		if err != nil {
			lastErr = err
			continue
		}
		if len(got) > 0 {
			found = got
			break
		}
		lastErr = fmt.Errorf("no movie links found on %s", u)
	}
	if len(found) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no movies found for city %q", city)
		}
		return nil, lastErr
	}
	if len(found) > 25 {
		found = found[:25]
	}
	return found, nil
}

// bookingOptionsJS collects the real /buytickets/ links from a movie page.
// On multi-language releases BMS exposes one link per language+format, which
// is exactly the "language popup" the README describes -- reading the links
// avoids having to drive a modal.
const bookingOptionsJS = `(() => {
	let seen = {}, out = [];
	document.querySelectorAll('a[href*="/buytickets/"]').forEach(a => {
		let href = a.href || "";
		if (seen[href]) return;
		seen[href] = 1;
		let label = (a.getAttribute('aria-label') || a.innerText || "").trim().split("\n")[0].trim();
		if (!label) {
			let parts = href.split('/').filter(Boolean);
			label = parts.length ? parts[parts.length - 1].replace(/-/g, ' ') : href;
		}
		out.push({label: label, url: href});
	});
	return out;
})()`

// bookButtonJS clicks a book-tickets call to action when the movie page does
// not expose /buytickets/ links directly.
const bookButtonJS = `(() => {
	let els = Array.from(document.querySelectorAll('button, a, div[role="button"]'));
	let hit = els.find(e => /book\s*tickets?/i.test(e.innerText || "") || /book\s*now/i.test(e.innerText || ""));
	if (!hit) return false;
	hit.click();
	return true;
})()`

func fetchBookingOptions(movieURL string) ([]choice, error) {
	var opts []choice
	err := withChrome(75*time.Second, func(ctx context.Context) error {
		if err := navigateAndSettle(ctx, movieURL, 20*time.Second); err != nil {
			return err
		}
		if err := checkNotBlocked(ctx, "movie page"); err != nil {
			return err
		}
		if err := extractWithRetry(ctx, "booking options", bookingOptionsJS, &opts,
			func() int { return len(opts) }, 20*time.Second); err != nil {
			return err
		}
		if len(opts) > 0 {
			return nil
		}
		// No direct links: press the CTA and look again.
		var clicked bool
		if err := chromedp.Run(ctx,
			chromedp.Evaluate(bookButtonJS, &clicked),
			chromedp.Sleep(6*time.Second),
			chromedp.Evaluate(bookingOptionsJS, &opts),
		); err != nil {
			return err
		}
		log.Printf("[scrape] movie page CTA clicked=%v -> %d booking links", clicked, len(opts))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(opts) == 0 {
		return nil, fmt.Errorf("no booking links on movie page (BMS may not have opened bookings yet)")
	}
	return opts, nil
}

// showtimesJS pairs each showtime with its venue. Times are located by text
// pattern (a time is a time regardless of markup) and the venue is the
// nearest ancestor that also holds a cinema link or venue-ish heading.
const showtimesJS = `(() => {
	let out = [], seen = {};
	let timeRe = /^\d{1,2}[:]\d{2}\s*(AM|PM|am|pm)?$/;
	let nodes = Array.from(document.querySelectorAll('a, div, span, li, button'));
	function venueFor(el) {
		let cur = el;
		for (let i = 0; i < 10 && cur; i++) {
			cur = cur.parentElement;
			if (!cur) break;
			let vlink = cur.querySelector('a[href*="/cinemas/"], a[href*="/venue"], [class*="venue"], [class*="Venue"]');
			if (vlink) {
				let t = (vlink.innerText || vlink.getAttribute('aria-label') || "").trim().split("\n")[0].trim();
				if (t && t.length > 2 && !timeRe.test(t)) return t;
			}
			let h = cur.querySelector('h1, h2, h3, h4, h5, h6, strong');
			if (h) {
				let t = (h.innerText || "").trim();
				if (t && t.length > 2 && !timeRe.test(t)) return t;
			}
		}
		return "";
	}
	nodes.forEach(el => {
		if (el.children.length > 0) return; // leaf nodes only
		let txt = (el.innerText || "").trim();
		if (!timeRe.test(txt)) return;
		let href = "";
		let anchor = el.closest('a[href]');
		if (anchor) href = anchor.href || "";
		let venue = venueFor(el);
		let key = venue + "|" + txt;
		if (seen[key]) return;
		seen[key] = 1;
		out.push({label: venue ? (venue + " - " + txt) : txt, venue: venue, time: txt, url: href});
	});
	return out;
})()`

// fetchTheatersAndShowtimes lists every venue/showtime pair for a date.
// date is YYYYMMDD; BMS accepts it as a path suffix on buytickets URLs.
func fetchTheatersAndShowtimes(bookURL, date string) ([]choice, error) {
	target := strings.TrimRight(bookURL, "/")
	if date != "" {
		target += "/" + date
	}

	var shows []choice
	err := withChrome(85*time.Second, func(ctx context.Context) error {
		if err := navigateAndSettle(ctx, target, 20*time.Second); err != nil {
			return err
		}
		if err := checkNotBlocked(ctx, "showtimes page"); err != nil {
			return err
		}
		return extractWithRetry(ctx, "showtimes", showtimesJS, &shows,
			func() int { return len(shows) }, 25*time.Second)
	})
	log.Printf("[scrape] showtimes %s -> %d shows, err=%v", target, len(shows), err)
	if err != nil {
		return nil, err
	}
	if len(shows) == 0 {
		return nil, fmt.Errorf("no showtimes found for %s (wrong date, or none on sale)", date)
	}
	if len(shows) > 40 {
		shows = shows[:40]
	}
	return shows, nil
}

func resolveSeatLayoutURL(bookURL, date, venue, showTime string) (string, error) {
	target := strings.TrimRight(bookURL, "/")
	if date != "" {
		target += "/" + date
	}

	clickJS := fmt.Sprintf(`(() => {
		let want = %q;
		let wantVenue = %q;
		let timeRe = /^\s*\d{1,2}[:]\d{2}\s*(AM|PM|am|pm)?\s*$/;
		let nodes = Array.from(document.querySelectorAll('a, div, span, li, button'));
		let cands = nodes.filter(el => el.children.length === 0 && timeRe.test(el.innerText || ""));
		if (!cands.length) return "NO_MATCHING_TIME";
		let pick = cands[0];
		if (wantVenue) {
			let better = cands.find(el => {
				let cur = el;
				for (let i = 0; i < 10 && cur; i++) {
					cur = cur.parentElement;
					if (cur && (cur.innerText || "").indexOf(wantVenue) >= 0) return true;
				}
				return false;
			});
			if (better) pick = better;
		}
		let target = pick.closest('a, button, [role="button"]') || pick;
		target.click();
		return "CLICKED";
	})()`, showTime, venue)

	var seatURL string
	err := withChrome(95*time.Second, func(ctx context.Context) error {
		if err := navigateAndSettle(ctx, target, 20*time.Second); err != nil {
			return err
		}
		if err := checkNotBlocked(ctx, "showtimes page"); err != nil {
			return err
		}

		var status string
		if err := chromedp.Run(ctx, chromedp.Evaluate(clickJS, &status)); err != nil {
			return err
		}
		if status != "CLICKED" {
			return fmt.Errorf("could not find showtime %q on the page (%s)", showTime, status)
		}

		// Booking flow may interpose a terms/accept modal before the layout.
		var landed string
		if err := chromedp.Run(ctx,
			chromedp.Sleep(9*time.Second),
			chromedp.Location(&landed),
		); err != nil {
			return err
		}
		if strings.Contains(landed, "seat-layout") {
			seatURL = landed
			return nil
		}
		var accepted bool
		if err := chromedp.Run(ctx,
			chromedp.Evaluate(`(() => {
				let els = Array.from(document.querySelectorAll('button, a, div[role="button"]'));
				let hit = els.find(e => /accept|continue|proceed|i agree|ok/i.test(e.innerText || ""));
				if (!hit) return false;
				hit.click();
				return true;
			})()`, &accepted),
			chromedp.Sleep(8*time.Second),
			chromedp.Location(&landed),
		); err != nil {
			log.Printf("[scrape] interstitial accepted=%v, now at %s", accepted, landed)
		}
		seatURL = landed
		return nil
	})
	if err != nil {
		return "", err
	}
	if !strings.Contains(seatURL, "seat-layout") {
		return "", fmt.Errorf("click did not reach a seat layout; landed on %s", seatURL)
	}
	log.Printf("[scrape] resolved seat layout URL: %s", seatURL)
	return seatURL, nil
}

// seatMatrixJS reads the Konva canvas and renders the seat grid as emoji.
// Unchanged in behaviour from the original inline script, lifted to a const
// so it can be exercised against a synthetic stage in scrape_test.go.
const seatMatrixJS = `(() => {
	if (!window.Konva || !window.Konva.stages || window.Konva.stages.length === 0) {
		return "ERROR: Konva stage object not found on page (structure changed, or blocked).";
	}
	let stage = window.Konva.stages[0];
	let rows = {};
	let tiers = [];
	let textNodes = stage.find('Text');
	textNodes.forEach(t => {
		let textStr = (t.attrs.text || "").trim();
		if (textStr && (textStr.indexOf('Rs.') >= 0 || textStr.length > 8)) {
			if (textStr.indexOf(':') < 0 && textStr.match(/^\d+$/) && textStr.length < 9) {
				tiers.push({ y: t.attrs.y, name: textStr.toUpperCase() });
			}
		}
	});
	tiers.sort((a, b) => a.y - b.y);
	let rectShapes = stage.find('Rect');
	rectShapes.forEach(shape => {
		let attrs = shape.attrs || {};
		if (attrs.width === 22 && attrs.height === 22) {
			let y = attrs.y;
			let fill = String(attrs.fill || "").toUpperCase();
			if (!rows[y]) rows[y] = [];
			if (fill === 'ESESES' || fill === 'ESESES') {
				rows[y].push({ x: attrs.x, emoji: "🟥" });
			} else {
				rows[y].push({ x: attrs.x, emoji: "🟩" });
			}
		}
	});
	let sortedY = Object.keys(rows).sort((a, b) => Number(a) - Number(b));
	let output = "";
	let currentTierIndex = 0;
	let prevX = null;
	let rowLabelChar = 65;
	let free = 0, taken = 0;
	sortedY.forEach(y => {
		let rowNum = Number(y);
		while (currentTierIndex < tiers.length && rowNum > tiers[currentTierIndex].y) {
			output += "\n-- " + tiers[currentTierIndex].name + " --\n";
			currentTierIndex++;
		}
		let seatRow = rows[y];
		seatRow.sort((a, b) => a.x - b.x);
		let rowString = String.fromCharCode(rowLabelChar) + ": ";
		prevX = null;
		seatRow.forEach(seat => {
			if (prevX !== null) {
				let diff = seat.x - prevX;
				if (diff > 35) {
					let emptySlots = Math.max(1, Math.round(diff / 26) - 1);
					for (let i = 0; i < emptySlots; i++) rowString += "⬜ ";
				}
			}
			if (seat.emoji === "🟥") {
				taken++;
				rowString += "🟥 ";
			} else {
				free++;
				rowString += "🟩 ";
			}
			prevX = seat.x;
		});
		output += rowString + "\n";
		rowLabelChar++;
	});
	if (!output) return "ERROR: Canvas rect elements matched zero seats.";
	return "AVAILABLE: " + free + " / " + (free + taken) + "\n" + output;
})()`

func fetchCanvasMatrix(seatURL string) (string, error) {
	var matrix string
	err := withChrome(90*time.Second, func(ctx context.Context) error {
		if err := navigateAndSettle(ctx, seatURL, 25*time.Second); err != nil {
			return err
		}
		if err := checkNotBlocked(ctx, "seat layout"); err != nil {
			return err
		}
		return extractWithRetry(ctx, "seat matrix", seatMatrixJS, &matrix,
			func() int {
				if matrix == "" || strings.HasPrefix(matrix, "ERROR:") {
					return 0
				}
				return 1
			}, 30*time.Second)
	})
	if err != nil {
		return "", err
	}
	return matrix, nil
}

func resolveChromePath() string {
	candidates := []string{
		os.Getenv("CHROME_PATH"),
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if v := os.Getenv("CHROME_PATH"); v != "" {
		return v
	}
	return "/usr/bin/chromium-browser"
}

// navigateAndSettle loads a URL without insisting on the browser's load event.
func navigateAndSettle(ctx context.Context, url string, settle time.Duration) error {
	start := time.Now()
	navCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := chromedp.Run(navCtx, chromedp.Navigate(url))
	switch {
	case err == nil:
		log.Printf("[scrape] navigated %s in %v (load fired)", url, time.Since(start).Round(time.Millisecond))
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("[scrape] navigated %s in %v (load never fired, using partial page)", url, time.Since(start).Round(time.Millisecond))
	default:
		return fmt.Errorf("navigate %s: %w", url, err)
	}

	deadline := time.Now().Add(settle)
	for {
		var state string
		if err := chromedp.Run(ctx, chromedp.Evaluate("document.readyState", &state)); err != nil {
			return fmt.Errorf("read document state for %s: %w", url, err)
		}
		if state == "interactive" || state == "complete" {
			log.Printf("[scrape] document %s after %v, continuing anyway", state, time.Since(start).Round(time.Millisecond))
			return nil
		}
		if time.Now().After(deadline) {
			log.Printf("[scrape] document still %q after %v, continuing anyway", state, time.Since(start).Round(time.Millisecond))
			return nil
		}
		if !sleepCtx(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

func extractWithRetry(ctx context.Context, stage, js string, out any, count func() int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, out)); err != nil {
			return fmt.Errorf("%s: evaluate: %w", stage, err)
		}
		n := count()
		log.Printf("[scrape] %s attempt %d -> %d", stage, attempt, n)
		if n > 0 || time.Now().After(deadline) {
			return nil
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return ctx.Err()
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}