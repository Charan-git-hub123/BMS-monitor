package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/chromedp/chromedp"
)

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
	waClient     *whatsmeow.Client
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// 1. Initialize Supabase SQL Store for session persistence
	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New("postgres", dbURL, dbLog)
	if err != nil {
		log.Fatalf("Failed to connect to Supabase: %v", err)
	}

	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		log.Fatalf("Failed to fetch device store: %v", err)
	}

	clientLog := waLog.Stdout("WhatsApp", "INFO", true)
	waClient = whatsmeow.NewClient(deviceStore, clientLog)
	waClient.AddEventHandler(handleWhatsAppEvent)

	// 2. Pair with QR Code if not already logged in
	if waClient.Store.ID == nil {
		qrChan, _ := waClient.GetQRChannel(context.Background())
		err = waClient.Connect()
		if err != nil {
			log.Fatalf("Failed to connect to WhatsApp: %v", err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				log.Println("Scan the QR code below from WhatsApp (Linked Devices):")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				log.Println("QR Channel Event:", evt.Event)
			}
		}
	} else {
		err = waClient.Connect()
		if err != nil {
			log.Fatalf("Failed to reconnect WhatsApp: %v", err)
		}
		log.Println("WhatsApp client reconnected using existing Supabase session.")
	}

	// 3. Render HTTP Health Check server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "BMS Monitor WhatsApp Engine is running.")
	})
	go func() {
		log.Printf("Health server listening on :%s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("HTTP server terminated: %v", err)
		}
	}()

	// Graceful shutdown handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	waClient.Disconnect()
	log.Println("Shutting down gracefully...")
}

func handleWhatsAppEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if v.Info.IsFromMe {
			return
		}

		userJID := v.Info.Sender.ToNonAD().String()
		msgText := strings.TrimSpace(v.Message.GetConversation())
		if msgText == "" && v.Message.GetExtendedTextMessage() != nil {
			msgText = strings.TrimSpace(v.Message.GetExtendedTextMessage().GetText())
		}
		if msgText == "" {
			return
		}

		sessionMutex.Lock()
		session, exists := sessionStore[userJID]
		if !exists || strings.ToLower(msgText) == "hi" || strings.ToLower(msgText) == "restart" {
			session = &UserSession{CurrentState: StateStart}
			sessionStore[userJID] = session
		}
		sessionMutex.Unlock()

		reply := processState(userJID, session, msgText)
		sendWhatsAppMessage(v.Info.Sender.ToNonAD(), reply)
	}
}

func processState(userJID string, session *UserSession, input string) string {
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
		return "Perfect. Final step: how often do you want updates sent to your phone?\n\nReply with a number in minutes (e.g., type 5, 15, or 30):"

	case StateAwaitingFrequency:
		mins, err := strconv.Atoi(input)
		if err != nil || mins <= 0 {
			return "Please enter a valid positive number for the tracking frequency in minutes (e.g., 15):"
		}

		session.Frequency = time.Duration(mins) * time.Minute
		session.CurrentState = StateMonitoring

		targetJID, _ := types.ParseJID(userJID)
		go startBackgroundMonitor(targetJID, userJID, session)

		return fmt.Sprintf("✅ Configuration Complete!\n\nBMS Monitor is actively tracking seat availability for %s with randomized anti-detection intervals.", session.Movie)

	case StateMonitoring:
		return "I am currently monitoring your target showtime! Type 'restart' anytime to start over."
	}

	return "System Error. Type 'restart' to return to step one."
}

func startBackgroundMonitor(targetJID types.JID, userKey string, session *UserSession) {
	log.Printf("[+] Starting background tracker loop for user %s with base frequency %v", userKey, session.Frequency)

	// Immediate first run
	triggerScrapeAndSend(targetJID, session)

	for {
		// Randomized Jitter: Base interval + 60 to 120 seconds random sleep
		jitterSeconds := rand.Intn(61) + 60
		sleepDuration := session.Frequency + (time.Duration(jitterSeconds) * time.Second)

		log.Printf("[Monitor] Next check for %s scheduled in %v (including %ds jitter)", userKey, sleepDuration, jitterSeconds)
		time.Sleep(sleepDuration)

		sessionMutex.Lock()
		currentSession, exists := sessionStore[userKey]
		if !exists || currentSession.CurrentState != StateMonitoring {
			sessionMutex.Unlock()
			log.Printf("[-] Stopping monitor routine for user %s.", userKey)
			return
		}
		sessionMutex.Unlock()

		triggerScrapeAndSend(targetJID, session)
	}
}

func triggerScrapeAndSend(targetJID types.JID, session *UserSession) {
	log.Printf("[Scraper] Running Chromedp engine cycle for: %s | %s", session.Theater, session.Time)

	matrixStr := fetchCanvasMatrix(session.TargetURL)

	runes := []rune(matrixStr)
	if len(runes) > 1200 {
		matrixStr = string(runes[:1200]) + "\n...[truncated]"
	}

	formattedMsg := fmt.Sprintf("🎬 *BMS Monitor Update: %s*\n📍 %s | ⏰ %s\n\n```\n%s\n```",
		session.Movie, session.Theater, session.Time, matrixStr)

	// Human-like typing presence before sending
	_ = waClient.SendChatPresence(targetJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	time.Sleep(time.Duration(rand.Intn(3)+3) * time.Second)
	_ = waClient.SendChatPresence(targetJID, types.ChatPresencePaused, types.ChatPresenceMediaText)

	sendWhatsAppMessage(targetJID, formattedMsg)
}

func sendWhatsAppMessage(targetJID types.JID, messageText string) {
	msg := &waE2E.Message{
		Conversation: proto.String(messageText),
	}
	_, err := waClient.SendMessage(context.Background(), targetJID, msg)
	if err != nil {
		log.Printf("[-] Failed to deliver WhatsApp message to %s: %v", targetJID.String(), err)
		return
	}
	log.Printf("[+] Delivered WhatsApp message to %s", targetJID.String())
}

func fetchCanvasMatrix(targetURL string) string {
	ctx, cancelTimeout := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancelTimeout()

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