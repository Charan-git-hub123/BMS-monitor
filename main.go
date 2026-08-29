package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type ChatState string

const (
	StateStart             ChatState = "START"
	StateAwaitingCity      ChatState = "AWAITING_CITY"
	StateAwaitingMovie     ChatState = "AWAITING_MOVIE"
	StateAwaitingLanguage  ChatState = "AWAITING_LANGUAGE"
	StateAwaitingDate      ChatState = "AWAITING_DATE"
	StateAwaitingShow      ChatState = "AWAITING_SHOW"
	StateAwaitingFrequency ChatState = "AWAITING_FREQUENCY"
	StateMonitoring        ChatState = "MONITORING"
)

// minFrequency throttles how often we hit BookMyShow. Polling harder than this
// is both rude and a good way to get the number or the IP blocked.
const minFrequency = 5 * time.Minute

// maxScrapeFailures stops a monitor that keeps failing, rather than messaging
// the same error every cycle forever.
const maxScrapeFailures = 5

type UserSession struct {
	CurrentState ChatState

	City     string
	Movie    string
	MovieURL string
	BookURL  string
	Date     string
	Venue    string
	ShowTime string
	SeatURL  string

	Frequency time.Duration

	Movies    []choice
	Languages []choice
	Shows     []choice

	CancelMonitor context.CancelFunc
	busy          bool
}

var (
	sessionStore = make(map[string]*UserSession)
	sessionMutex sync.Mutex
	waClient     *whatsmeow.Client

	connState   = "starting"
	connStateMu sync.RWMutex
)

func setConnState(s string) {
	connStateMu.Lock()
	connState = s
	connStateMu.Unlock()
}

func getConnState() string {
	connStateMu.RLock()
	defer connStateMu.RUnlock()
	return connState
}

func main() {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	phoneNum := os.Getenv("WHATSAPP_PHONE_NUMBER")
	cleanPhone := regexp.MustCompile("[^0-9]").ReplaceAllString(phoneNum, "")

	if dryRunMode {
		log.Println("[!] DRY_RUN=1 - outbound messages will be logged, not sent")
	}

	dbLog := waLog.Stdout("Database", envOr("DB_LOG_LEVEL", "WARN"), true)
	container, err := openStore(ctx,dbURL, dbLog)
	if err != nil {
		log.Fatalf("Failed to connect to Postgres: %v", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("Failed to fetch device store: %v", err)
	}

	clientLog := waLog.Stdout("WhatsApp", envOr("WA_LOG_LEVEL", "INFO"), true)
	waClient = whatsmeow.NewClient(deviceStore, clientLog)

	// Each event on its own goroutine: a scrape takes ~15s and must not block
	// whatsmeow's event loop.
	waClient.AddEventHandler(func(evt interface{}) {
		go handleWhatsAppEvent(evt)
	})

	if waClient.Store.ID == nil {
		if cleanPhone == "" {
			log.Fatal("WHATSAPP_PHONE_NUMBER is required for first-time pairing")
		}
		if err := waClient.Connect(); err != nil {
			log.Fatalf("Failed to connect to WhatsApp: %v", err)
		}
		time.Sleep(2 * time.Second)

		code, err := waClient.PairPhone(ctx, cleanPhone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			log.Fatalf("Failed to generate pairing code: %v", err)
		}
		setConnState("waiting pairing")
		// Logs only. This deliberately never reaches the HTTP status page:
		// that page is public on Render, and anyone holding this code during
		// the pairing window could link their own device to the account.
		log.Printf("[+] Pairing code (enter on your phone): %s", code)
	} else {
		if err := waClient.Connect(); err != nil {
			log.Fatalf("Failed to reconnect WhatsApp: %v", err)
		}
		setConnState("connected")
		log.Println("[+] WhatsApp client reconnected from stored session.")
	}
	refreshOwnerIdentities()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "BMS Monitor\nstatus: %s\n", getConnState())
	})
	go func() {
		log.Printf("Status server listening on :%s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("HTTP server terminated: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	log.Println("Shutting down gracefully...")
	waClient.Disconnect()
}

// openStore opens the Postgres store with explicit pool limits.
//
// Every Signal operation whatsmeow performs -- decrypting an incoming message,
// consuming a prekey, storing a session -- goes through this pool. A pooled
// connection that has silently died, which Supabase's pooler does to idle
// connections, therefore surfaces as "Node handling is taking long" repeating
// for minutes instead of as an error: the query simply never returns. Bounded
// lifetimes force a reconnect, and connect_timeout stops a dial-hanging.
func openStore(ctx context.Context, dsn string, dbLog waLog.Logger) (*sqlstore.Container, error) {
	if !strings.Contains(dsn, "connect_timeout=") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "connect_timeout=10"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	container := sqlstore.NewWithDB(db, "postgres", dbLog)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade database schema: %w", err)
	}
	return container, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
// refreshOwnerIdentities reads the linked device's own identity from the
// store. Called after pairing and on every reconnect.
// refreshOwnerIdentities reads the linked device's own identity from the
// store. Called after pairing and on every reconnect.
func refreshOwnerIdentities() {
	if waClient == nil || waClient.Store == nil {
		return
	}
	var ids []types.JID
	if waClient.Store.ID != nil {
		ids = append(ids, *waClient.Store.ID)
	}
	if waClient.Store.LID.User != "" {
		ids = append(ids, waClient.Store.LID)
	}
	setOwnerIdentities(ids...)
	log.Printf("[gate] owner identities: %v (bot will only talk to these)", ids)
}

func handleWhatsAppEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.PairSuccess:
		log.Printf("[+] Device linked: %s", v.ID.String())
		setConnState("connected")
		refreshOwnerIdentities()

	case *events.Connected:
		log.Println("[+] WhatsApp connection active.")
		setConnState("connected")
		refreshOwnerIdentities()

	case *events.Disconnected:
		setConnState("disconnected")

	case *events.LoggedOut:
		// Unlinked from the phone. Exit rather than idle in a half-alive state
		// that would spring back on the next pairing.
		log.Printf("[!] Device logged out (reason: %v). Exiting.", v.Reason)
		os.Exit(0)

	case *events.Message:
		// SAFETY GATE -- must stay the first thing in this case. Only the
		// owner's own self-chat may drive the bot; see guard.go.
		if ok, reason := allowInbound(v.Info, ownerIdentities(), startedAt); !ok {
			log.Printf("[gate] ignored message from %s in chat %s: %s",
				v.Info.Sender.ToNonAD(), v.Info.Chat.ToNonAD(), reason)
			return
		}

		text := v.Message.GetConversation()
		if text == "" && v.Message.GetExtendedTextMessage() != nil {
			text = v.Message.GetExtendedTextMessage().GetText()
		}
		msgText := strings.TrimSpace(text)
		if msgText == "" {
			return
		}
		log.Printf("[msg] %q from owner", msgText)

		target := v.Info.Sender.ToNonAD()
		key := target.String()
		lower := strings.ToLower(msgText)

		sessionMutex.Lock()
		session, exists := sessionStore[key]
		if !exists || lower == "hi" || lower == "restart" {
			if exists && session.CancelMonitor != nil {
				session.CancelMonitor()
			}
			session = &UserSession{CurrentState: StateStart}
			sessionStore[key] = session
		}
		if session.busy {
			sessionMutex.Unlock()
			sendWhatsAppMessage(target, "⏳ Still working on the previous step -- one moment.")
			return
		}
		session.busy = true
		sessionMutex.Unlock()

		defer func() {
			sessionMutex.Lock()
			session.busy = false
			sessionMutex.Unlock()
		}()

		processMessage(target, session, msgText)
	}
}

// processMessage advances the conversation. Unlike the earlier design it sends
// its own messages rather than returning one string, because the scraping steps
// take ten to twenty seconds and the user needs an acknowledgement first.
func processMessage(target types.JID, session *UserSession, input string) {
	switch session.CurrentState {
	case StateStart:
		session.CurrentState = StateAwaitingCity
		sendWhatsAppMessage(target, "🍿 *BMS Seat Monitor*\n\nWhich city? (e.g. Hyderabad, Mumbai, Bengaluru)")

	case StateAwaitingCity:
		session.City = input
		sendWhatsAppMessage(target, fmt.Sprintf("🔍 %s -- fetching what's playing, this takes a few seconds...", input))

		movies, err := fetchCityMovies(input)
		if err != nil {
			sendWhatsAppMessage(target, fmt.Sprintf("❌ Couldn't list movies for %s.\n\n%v\n\nSend a different city, or *restart*.", input, err))
			return
		}
		session.Movies = movies
		session.CurrentState = StateAwaitingMovie
		sendWhatsAppMessage(target, renderChoices("🎬 *Now showing -- reply with a number:*", movies))

	case StateAwaitingMovie:
		pickedMovie, ok := pickChoice(session.Movies, input)
		if !ok {
			sendWhatsAppMessage(target, "Please reply with one of the numbers above, or *restart*.")
			return
		}
		session.Movie = pickedMovie.Label
		session.MovieURL = pickedMovie.URL
		sendWhatsAppMessage(target, fmt.Sprintf("🎟️ %s -- checking languages and formats...", pickedMovie.Label))

		opts, err := fetchBookingOptions(pickedMovie.URL)
		if err != nil {
			sendWhatsAppMessage(target, fmt.Sprintf("❌ Couldn't open booking for %s.\n\n%v\n\nSend *restart* to try again.", pickedMovie.Label, err))
			return
		}
		if len(opts) == 1 {
			session.BookURL = opts[0].URL
			session.CurrentState = StateAwaitingDate
			sendWhatsAppMessage(target, "Which date? Reply *today*, *tomorrow*, or *YYYYMMDD* (e.g. 20260901):")
			return
		}
		session.Languages = opts
		session.CurrentState = StateAwaitingLanguage
		sendWhatsAppMessage(target, renderChoices("🗣️ *Language / format -- reply with a number:*", opts))

	case StateAwaitingLanguage:
		pickedLang, ok := pickChoice(session.Languages, input)
		if !ok {
			sendWhatsAppMessage(target, "Please reply with one of the numbers above, or *restart*.")
			return
		}
		session.BookURL = pickedLang.URL
		session.CurrentState = StateAwaitingDate
		sendWhatsAppMessage(target, "Which date? Reply *today*, *tomorrow*, or *YYYYMMDD* (e.g. 20260901):")

	case StateAwaitingDate:
		date, ok := parseDate(input)
		if !ok {
			sendWhatsAppMessage(target, "Date must be *today*, *tomorrow*, or *YYYYMMDD* (e.g. 20260901).")
			return
		}
		session.Date = date
		sendWhatsAppMessage(target, fmt.Sprintf("📅 %s -- fetching theatres and showtimes...", date))

		shows, err := fetchTheatersAndShowtimes(session.BookURL, date)
		if err != nil {
			sendWhatsAppMessage(target, fmt.Sprintf("❌ Couldn't list showtimes.\n\n%v\n\nTry another date, or *restart*.", err))
			return
		}
		session.Shows = shows
		session.CurrentState = StateAwaitingShow
		sendWhatsAppMessage(target, renderChoices("🎭 *Theatre & showtime -- reply with a number:*", shows))

	case StateAwaitingShow:
		pickedShow, ok := pickChoice(session.Shows, input)
		if !ok {
			sendWhatsAppMessage(target, "Please reply with one of the numbers above, or *restart*.")
			return
		}
		session.Venue = pickedShow.Venue
		session.ShowTime = pickedShow.Time
		sendWhatsAppMessage(target, fmt.Sprintf("🎟️ %s -- opening the seat layout...", pickedShow.Label))

		seatURL := pickedShow.URL
		if seatURL == "" || !strings.Contains(seatURL, "seat-layout") {
			resolved, err := resolveSeatLayoutURL(session.BookURL, session.Date, pickedShow.Venue, pickedShow.Time)
			if err != nil {
				sendWhatsAppMessage(target, fmt.Sprintf("❌ Couldn't reach the seat layout.\n\n%v\n\nPick another show, or *restart*.", err))
				return
			}
			seatURL = resolved
		}
		session.SeatURL = seatURL
		session.CurrentState = StateAwaitingFrequency
		sendWhatsAppMessage(target, fmt.Sprintf("🔒 Locked on.\n\nHow often should I check? Reply with minutes (minimum %d):", int(minFrequency.Minutes())))

	case StateAwaitingFrequency:
		mins, err := strconv.Atoi(strings.TrimSpace(input))
		if err != nil || mins <= 0 {
			sendWhatsAppMessage(target, fmt.Sprintf("Reply with a positive number of minutes (minimum %d).", int(minFrequency.Minutes())))
			return
		}
		freq := time.Duration(mins) * time.Minute
		if freq < minFrequency {
			freq = minFrequency
			sendWhatsAppMessage(target, fmt.Sprintf("ℹ️ Raised to the %d minute minimum to avoid being blocked by BookMyShow.", int(minFrequency.Minutes())))
		}
		session.Frequency = freq
		session.CurrentState = StateMonitoring

		monCtx, cancel := context.WithCancel(context.Background())
		session.CancelMonitor = cancel
		go startBackgroundMonitor(monCtx, target, session)

		sendWhatsAppMessage(target, fmt.Sprintf(
			"🚀 *Monitoring started*\n\n%s\n%s | %s\n📅 %s\n⏱️ every %v\n\nSend *stop* to end, *restart* to reconfigure.",
			session.Movie, session.Venue, session.ShowTime, session.Date, freq))

	case StateMonitoring:
		if strings.ToLower(input) == "stop" {
			if session.CancelMonitor != nil {
				session.CancelMonitor()
			}
			session.CurrentState = StateStart
			sendWhatsAppMessage(target, "⏹️ Stopped. Send *hi* to set up a new tracker.")
			return
		}
		sendWhatsAppMessage(target, fmt.Sprintf("Currently watching %s at %s (%s). Send *stop* or *restart*.",
			session.Movie, session.Venue, session.ShowTime))
	}
}

func startBackgroundMonitor(ctx context.Context, target types.JID, session *UserSession) {
	log.Printf("[monitor] started: %s | %s | %s | every %v", session.Movie, session.Venue, session.ShowTime, session.Frequency)
	failures := 0
	for {
		matrix, err := fetchCanvasMatrix(session.SeatURL)
		if err != nil {
			failures++
			log.Printf("[monitor] scrape failed (%d/%d): %v", failures, maxScrapeFailures, err)
			sendWhatsAppMessage(target, fmt.Sprintf("⚠️ Check failed (%d/%d): %v", failures, maxScrapeFailures, err))
			if failures >= maxScrapeFailures {
				sendWhatsAppMessage(target, "🛑 Too many consecutive failures -- monitoring stopped. Send *restart* to set it up again.")
				sessionMutex.Lock()
				session.CurrentState = StateStart
				sessionMutex.Unlock()
				return
			}
		} else {
			failures = 0
			body := matrix
			if runes := []rune(body); len(runes) > 1200 {
				body = string(runes[:1200]) + "\n...[truncated]"
			}
			sendWhatsAppMessage(target, fmt.Sprintf("🎟️ *%s*\n📍 %s | ⏰ %s\n\n```\n%s\n```",
				session.Movie, session.Venue, session.ShowTime, body))
		}

		// Jitter so the polling pattern is not perfectly periodic.
		sleep := session.Frequency + time.Duration(rand.Intn(61)+60)*time.Second
		log.Printf("[monitor] next check in %v", sleep)
		select {
		case <-ctx.Done():
			log.Printf("[monitor] cancelled: %s", session.Movie)
			return
		case <-time.After(sleep):
		}
	}
}

func sendWhatsAppMessage(targetJID types.JID, messageText string) {
	// SAFETY GATE -- last check before the network. See guard.go.
	if ok, reason := allowOutbound(targetJID, ownerIdentities()); !ok {
		log.Printf("[gate] BLOCKED outbound message to %s: %s", targetJID.ToNonAD(), reason)
		return
	}
	if dryRunMode {
		log.Printf("[dry-run] would send to %s:\n%s", targetJID.ToNonAD(), messageText)
		return
	}
	msg := &waE2E.Message{Conversation: proto.String(messageText)}
	if _, err := waClient.SendMessage(context.Background(), targetJID, msg); err != nil {
		log.Printf("[-] Failed to deliver message to %s: %v", targetJID.ToNonAD(), err)
		return
	}
	log.Printf("[+] Delivered message to %s", targetJID.ToNonAD())
}

// renderChoices formats a numbered menu for WhatsApp.
func renderChoices(header string, list []choice) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n\n")
	for i, c := range list {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c.Label)
	}
	b.WriteString("\nReply with the number, or *restart* to start over.")
	return b.String()
}

// pickChoice resolves a reply to a menu entry: a number, or enough of the
// label to identify it unambiguously.
func pickChoice(list []choice, input string) (choice, bool) {
	if len(list) == 0 {
		return choice{}, false
	}
	trimmed := strings.TrimSpace(input)
	if n, err := strconv.Atoi(trimmed); err == nil {
		if n >= 1 && n <= len(list) {
			return list[n-1], true
		}
		return choice{}, false
	}
	needle := strings.ToLower(trimmed)
	if needle == "" {
		return choice{}, false
	}
	var hit choice
	found := 0
	for _, c := range list {
		if strings.Contains(strings.ToLower(c.Label), needle) {
			hit = c
			found++
		}
	}
	if found == 1 {
		return hit, true
	}
	return choice{}, false
}

// parseDate accepts today, tomorrow, or YYYYMMDD, and returns YYYYMMDD.
func parseDate(input string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(input))
	switch s {
	case "today":
		return time.Now().Format("20060102"), true
	case "tomorrow":
		return time.Now().AddDate(0, 0, 1).Format("20060102"), true
	}
	if _, err := time.Parse("20060102", s); err == nil {
		return s, true
	}
	return "", false
}

// citySlug maps a typed city name to the slug BookMyShow uses in its URLs.
func citySlug(city string) string {
	s := strings.ToLower(strings.TrimSpace(city))
	s = strings.Join(strings.Fields(s), "-")

	aliases := map[string]string{
		"bangalore":  "bengaluru",
		"bombay":     "mumbai",
		"madras":     "chennai",
		"calcutta":   "kolkata",
		"vizag":      "visakhapatnam",
		"trivandrum": "thiruvananthapuram",
		"delhi":      "national-capital-region-ncr",
		"new-delhi":  "national-capital-region-ncr",
		"ncr":        "national-capital-region-ncr",
		"gurgaon":    "national-capital-region-ncr",
		"noida":      "national-capital-region-ncr",
	}
	if alias, ok := aliases[s]; ok {
		return alias
	}
	return s
}