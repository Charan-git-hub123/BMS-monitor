package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// UserState tracking categories
type ChatState string

const (
	StateStart             ChatState = "START"
	StateAwaitingCity      ChatState = "AWAITING_CITY"
	StateAwaitingMovie     ChatState = "AWAITING_MOVIE"
	StateAwaitingDate      ChatState = "AWAITING_DATE"
	StateAwaitingTheater   ChatState = "AWAITING_THEATER"
	StateAwaitingTime      ChatState = "AWAITING_TIME"
	StateAwaitingFrequency ChatState = "AWAITING_FREQUENCY"
	StateMonitoring        ChatState = "MONITORING"
)

// Session holds the collection criteria for the active scraper task
type UserSession struct {
	CurrentState ChatState
	City         string
	Movie        string
	Date         string
	Theater      string
	Time         string
	Frequency    time.Duration
}

var (
	// Thread-safe map to store conversation histories
	sessionStore = make(map[string]*UserSession)
	sessionMutex sync.Mutex
)

// Incoming Twilio/Meta standard webhook format wrapper
type WebhookPayload struct {
	From string `json:"From"`
	Body string `json:"Body"`
}

func main() {
	http.HandleFunc("/whatsapp", handleWhatsAppIncoming)
	
	log.Println("BMS Chatbot Server running on port 8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func handleWhatsAppIncoming(w http.ResponseWriter, r *http.Request) {
	// Parse input variables (handles form parameters or JSON payloads depending on your sandbox provider)
	r.ParseForm()
	fromUser := r.FormValue("From")
	msgBody := strings.TrimSpace(r.FormValue("Body"))

	if fromUser == "" {
		// Fallback for raw JSON direct testing
		var p WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
			fromUser = p.From
			msgBody = strings.TrimSpace(p.Body)
		}
	}

	sessionMutex.Lock()
	session, exists := sessionStore[fromUser]
	if !exists || strings.ToLower(msgBody) == "hi" || strings.ToLower(msgBody) == "restart" {
		session = &UserSession{CurrentState: StateStart}
		sessionStore[fromUser] = session
	}
	sessionMutex.Unlock()

	responseMessage := processState(fromUser, session, msgBody)

	// Reply back through the HTTP protocol format
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Message>%s</Message></Response>`, responseMessage)
}

func processState(user string, session *UserSession, input string) string {
	switch session.CurrentState {
	
	case StateStart:
		session.CurrentState = StateAwaitingCity
		return "Welcome to BMS Monitor! 🎬\n\nPlease enter your city name (e.g., Hyderabad, Mumbai) to find what's playing nearby:"

	case StateAwaitingCity:
		session.City = input
		session.CurrentState = StateAwaitingMovie
		// TODO: Hook up a quick server-side request to check what movies are available in this city
		return fmt.Sprintf("📍 Location set to: %s.\n\nHere are the top trending movies. Please reply with the exact name:\n1. Kalki 2898 AD\n2. Indian 2\n3. Devara", session.City)

	case StateAwaitingMovie:
		session.Movie = input
		session.CurrentState = StateAwaitingDate
		// TODO: Extract the active release dates from the target show catalog
		return fmt.Sprintf("Selected Movie: %s 🎥\n\nWhen would you like to watch? Reply with one of these dates:\n- 2026-07-19 (Sunday)\n- 2026-07-20 (Monday)", session.Movie)

	case StateAwaitingDate:
		session.Date = input
		session.CurrentState = StateAwaitingTheater
		return "Got it. Select your preferred cinema by replying with the choice name:\n1. AMB Cinemas: Gachibowli\n2. INOX: Sattva Necklace Mall\n3. Prasads Multiplex"

	case StateAwaitingTheater:
		session.Theater = input
		session.CurrentState = StateAwaitingTime
		return "Available Showtimes:\n- 11:30 AM\n- 02:45 PM\n- 07:15 PM\n- 10:30 PM\n\nPlease type your target time choice exactly:"

	case StateAwaitingTime:
		session.Time = input
		session.CurrentState = StateAwaitingFrequency
		return "Perfect. Final step: how often do you want updates sent to your phone?\n\nReply with a number in minutes (e.g., type 15 or 30):"

	case StateAwaitingFrequency:
		var mins int
		_, err := fmt.Sscanf(input, "%d", &mins)
		if err != nil || mins <= 0 {
			return "Please enter a valid positive number for the tracking frequency (e.g., 30):"
		}
		
		session.Frequency = time.Duration(mins) * time.Minute
		session.CurrentState = StateMonitoring
		
		// Start background task loop
		go startBackgroundMonitor(user, session)
		
		return fmt.Sprintf("✅ Configuration Complete!\n\nBMS Monitor has successfully locked onto your screening selection. I will scrape the live theater seating map and send visual updates straight here every %d minutes.", mins)

	case StateMonitoring:
		return "I am currently monitoring your target showtime! If you want to reset parameters or build a new session, simply type 'restart'."
	}
	
	return "System Error. Type 'restart' to return to step one."
}

func startBackgroundMonitor(user string, session *UserSession) {
	ticker := time.NewTicker(session.Frequency)
	defer ticker.Stop()

	log.Printf("[+] Starting background tracker loop for user %s every %v", user, session.Frequency)
	
	// Fire immediately on initialization
	triggerScrapeAndSend(user, session)

	for range ticker.C {
		// Check if user wiped configuration state out-of-band
		sessionMutex.Lock()
		currentSession, exists := sessionStore[user]
		if !exists || currentSession.CurrentState != StateMonitoring {
			sessionMutex.Unlock()
			log.Printf("[-] Stopping monitor routine loop for user %s.", user)
			return
		}
		sessionMutex.Unlock()

		triggerScrapeAndSend(user, session)
	}
}

func triggerScrapeAndSend(user string, session *UserSession) {
	log.Printf("[Scraper] Running Chromedp engine cycle for showtime setup: %s | %s", session.Theater, session.Time)
	
	// TODO: Wire our working main.go canvas layout coordinator logic right here to fetch the emoji grid string
	dummyMap := "```\nA: 🟥 🟥 ⬜ 🟩 🟩 🟩 🟩 ⬜ 🟥 🟥\nB: 🟥 🟥 ⬜ 🟩 🟩 🟥 🟩 ⬜ 🟥 🟥\n```"
	
	// Send message block layout out via Twilio/Meta API call here
	fmt.Printf("[NOTIFICATION TO USER %s]:\n🚨 Live Seating update for %s:\n%s\n", user, session.Movie, dummyMap)
}