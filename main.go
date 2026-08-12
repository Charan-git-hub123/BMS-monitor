package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io" // Added io import for reading response body
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// ================= CONFIGURATION =================
var (
	TwilioAccountSID = getEnv("TWILIO_ACCOUNT_SID", "YOUR_TWILIO_ACCOUNT_SID")
	TwilioAuthToken  = getEnv("TWILIO_AUTH_TOKEN", "YOUR_TWILIO_AUTH_TOKEN")
	TwilioFromNumber = getEnv("TWILIO_FROM_NUMBER", "whatsapp:+14155238886")
)

type ChatState string

const (
	StateStart            ChatState = "START"
	StateAwaitingCity     ChatState = "AWAITING_CITY"
	StateAwaitingMovie    ChatState = "AWAITING_MOVIE"
	StateAwaitingDate     ChatState = "AWAITING_DATE"
	StateAwaitingTheater  ChatState = "AWAITING_THEATER"
	StateAwaitingTime     ChatState = "AWAITING_TIME"
	StateAwaitingFrequency ChatState = "AWAITING_FREQUENCY"
	StateMonitoring       ChatState = "MONITORING"
)

type UserSession struct {
	CurrentState ChatState
	City         string
	Movie        string
	Date         string
	Theater      string
	Time         string
	Frequency    time.Duration
	TargetURL    string
}

var (
	sessionStore = make(map[string]*UserSession)
	sessionMutex sync.Mutex
)

type WebhookPayload struct {
	From string `json:"From"`
	Body string `json:"Body"`
}

func main() {
	http.HandleFunc("/whatsapp", handleWhatsAppIncoming)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("BMS Monitor Engine listening on :%s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func handleWhatsAppIncoming(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	fromUser := r.FormValue("From")
	msgBody := strings.TrimSpace(r.FormValue("Body"))

	if fromUser == "" {
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
		return fmt.Sprintf("📍 Location set to: %s.\n\nType the exact movie name you want to watch:", session.City)

	case StateAwaitingMovie:
		session.Movie = input
		session.CurrentState = StateAwaitingDate
		return fmt.Sprintf("Selected Movie: %s 🎥\n\nWhen would you like to watch? Reply with date (YYYYMMDD, e.g. 20260718):", session.Movie)

	case StateAwaitingDate:
		session.Date = input
		session.CurrentState = StateAwaitingTheater
		return "Select your preferred cinema / theater name or venue code:"

	case StateAwaitingTheater:
		session.Theater = input
		session.CurrentState = StateAwaitingTime
		return "Available Showtimes (e.g., 07:15 PM):\n\nPlease type your target time choice:"

	case StateAwaitingTime:
		session.Time = input
		session.CurrentState = StateAwaitingFrequency
		session.TargetURL = "https://in.bookmyshow.com/movies/hyd/seat-layout/ET00441159/AMBH/115212/" + session.Date
		return "Perfect. Final step: how often do you want updates sent to your phone?\n\nReply with a number in minutes (e.g., type 15 or 30):"

	case StateAwaitingFrequency:
		var mins int
		_, err := fmt.Sscanf(input, "%d", &mins)
		if err != nil || mins <= 0 {
			return "Please enter a valid positive number for the tracking frequency (e.g., 30):"
		}

		session.Frequency = time.Duration(mins) * time.Minute
		session.CurrentState = StateMonitoring

		go startBackgroundMonitor(user, session)

		return fmt.Sprintf("✅ Configuration Complete!\n\nBMS Monitor is actively scraping seat availability for %s every %d minutes.", session.Movie, mins)

	case StateMonitoring:
		return "I am currently monitoring your target showtime! Type 'restart' anytime to build a new session."
	}

	return "System Error. Type 'restart' to return to step one."
}

func startBackgroundMonitor(user string, session *UserSession) {
	ticker := time.NewTicker(session.Frequency)
	defer ticker.Stop()

	log.Printf("[+] Starting background tracker loop for user %s every %v", user, session.Frequency)

	triggerScrapeAndSend(user, session)

	for range ticker.C {
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
	log.Printf("[Scraper] Running Chromedp engine cycle for: %s | %s", session.Theater, session.Time)

	matrixStr := fetchCanvasMatrix(session.TargetURL)

	// Safe Unicode (Rune) Truncation to prevent invalid UTF-8 byte corruption
	runes := []rune(matrixStr)
	if len(runes) > 500 {
		matrixStr = string(runes[:500]) + "\n...[truncated]"
	}

	formattedMsg := fmt.Sprintf("🎬 *BMS Monitor Update: %s*\n📍 %s | ⏰ %s\n\n```\n%s\n```",
		session.Movie, session.Theater, session.Time, matrixStr)

	sendTwilioWhatsAppMessage(user, formattedMsg)
}

func fetchCanvasMatrix(targetURL string) string {
	ctx, cancelTimeout := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancelTimeout()

	// Connect context with Docker-compatible Chromium flags
	taskCtx, cancelChrome := createChromeContext(ctx)
	defer cancelChrome()

	var finalMatrix string

	err := chromedp.Run(taskCtx,
		chromedp.Navigate(targetURL),
		chromedp.Sleep(7*time.Second),

		chromedp.Evaluate(`(() => {
			if (!window.Konva || !window.Konva.stages || window.Konva.stages.length === 0) {
				return "ERROR: Canvas stage not initialized yet.";
			}

			let stage = window.Konva.stages[0];
			let rows = {};
			let tiers = [];

			let textNodes = stage.find('Text');
			textNodes.forEach(t => {
				let textStr = (t.attrs.text || "").trim();
				if (textStr && (textStr.includes('₹') || textStr.includes('Rs.') || textStr.length > 8)) {
					if (!textStr.includes(':') && !textStr.match(/^\d+$/) && !textStr.includes('Pay')) {
						tiers.push({ y: t.attrs.y, name: textStr.toUpperCase() });
					}
				}
			});

			tiers.sort((a, b) => a.y - b.y);

			let rectShapes = stage.find('Rect');
			rectShapes.forEach(shape => {
				let attrs = shape.attrs || {};
				if (attrs.width === 22 && attrs.height === 22) {
					let yKey = attrs.y; 
					let fill = String(attrs.fill || "").toUpperCase();
					
					if (!rows[yKey]) rows[yKey] = [];

					if (fill === '#E5E5E5' || fill === 'E5E5E5') {
						rows[yKey].push({ x: attrs.x, emoji: "🟥" });
					} else {
						rows[yKey].push({ x: attrs.x, emoji: "🟩" });
					}
				}
			});

			let sortedY = Object.keys(rows).sort((a, b) => Number(a) - Number(b));
			let output = "";
			let currentTierIndex = 0;
			let rowLabelChar = 65;

			sortedY.forEach(y => {
				let rowYNum = Number(y);

				while (currentTierIndex < tiers.length && rowYNum > tiers[currentTierIndex].y) {
					output += "\n✨ --- " + tiers[currentTierIndex].name + " --- ✨\n";
					currentTierIndex++;
				}

				let seatRow = rows[y];
				seatRow.sort((a, b) => a.x - b.x);
				
				let rowString = String.fromCharCode(rowLabelChar) + ": ";
				let prevX = null;

				seatRow.forEach(seat => {
					if (prevX !== null) {
						let diff = seat.x - prevX;
						if (diff > 35) {
							let emptySlots = Math.max(1, Math.round(diff / 26) - 1);
							for (let i = 0; i < emptySlots; i++) {
								rowString += "⬜ "; 
							}
						}
					}
					rowString += seat.emoji + " ";
					prevX = seat.x;
				});
				
				output += rowString + "\n";
				rowLabelChar++;
			});

			return output || "ERROR: Could not compile canvas layout elements.";
		})()`, &finalMatrix),
	)

	if err != nil {
		return fmt.Sprintf("Scraper Error: %v", err)
	}

	return finalMatrix
}

func sendTwilioWhatsAppMessage(toUser string, messageText string) {
	if TwilioAccountSID == "YOUR_TWILIO_ACCOUNT_SID" {
		log.Println("[Warning] Twilio credentials not configured. Outputting to console instead.")
		log.Printf("[OUTBOUND TO %s]:\n%s", toUser, messageText)
		return
	}

	formattedTo := toUser
	if !strings.HasPrefix(formattedTo, "whatsapp:") {
		formattedTo = "whatsapp:" + formattedTo
	}

	formattedFrom := TwilioFromNumber
	if !strings.HasPrefix(formattedFrom, "whatsapp:") {
		formattedFrom = "whatsapp:" + formattedFrom
	}

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", TwilioAccountSID)

	// Create JSON variables for the default Twilio WhatsApp Sandbox template
	// Template expects: {"1": "your_custom_text_here"}
	varsJSON, _ := json.Marshal(map[string]string{
		"1": messageText,
	})

	data := url.Values{}
	data.Set("From", formattedFrom)
	data.Set("To", formattedTo)
	
	// Twilio's standard free sandbox ContentSid for outbound templates:
	data.Set("ContentSid", "HXb5b62575e6e4ff6129ad7c8efe1f983e")
	data.Set("ContentVariables", string(varsJSON))

	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
	req.SetBasicAuth(TwilioAccountSID, TwilioAuthToken)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[-] Failed to deliver Twilio payload: %v", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("[+] Twilio Response (Status %s): %s", resp.Status, string(respBody))
}

func createChromeContext(parentCtx context.Context) (context.Context, context.CancelFunc) {
	execPath := os.Getenv("CHROME_PATH")
	if execPath == "" {
		execPath = "/usr/bin/chromium-browser"
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("single-process", true),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parentCtx, opts...)
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)

	cancel := func() {
		cancelTask()
		cancelAlloc()
	}

	return taskCtx, cancel
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}