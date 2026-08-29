package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type ChatState string

const (
	StateStart             ChatState = "START"
	StateAwaitingCity      ChatState = "AWAITING_CITY"
	StateAwaitingMovie     ChatState = "AWAITING_MOVIE"
	StateAwaitingEventCode ChatState = "AWAITING_EVENT_CODE"
	StateAwaitingDate      ChatState = "AWAITING_DATE"
	StateAwaitingTheater   ChatState = "AWAITING_THEATER"
	StateAwaitingShowID    ChatState = "AWAITING_SHOW_ID"
	StateAwaitingFrequency ChatState = "AWAITING_FREQUENCY"
	StateMonitoring        ChatState = "MONITORING"
)

type UserSession struct {
	CurrentState  ChatState
	City          string
	Movie         string
	EventCode     string
	Date          string
	Theater       string
	ShowID        string
	Frequency     time.Duration
	TargetURL     string
	CancelMonitor context.CancelFunc
}

var (
	client       *whatsmeow.Client
	sessionStore = make(map[string]*UserSession)
	sessionMutex sync.Mutex
)

func main() {
	// ==========================================
	// CONFIG FOR LOCAL TESTING
	// ==========================================
	dbURL := "postgresql://postgres.lbcryawiflxbygamfdbn:Icelovesme@278727@aws-0-ap-southeast-2.pooler.supabase.com:5432/postgres"


	cleanPhone := "917989061601"

	ctx := context.Background()
	dbLog := waLog.Stdout("Database", "Error", true)
	container, err := sqlstore.New(ctx, "postgres", dbURL, dbLog)
	if err != nil {
		log.Fatalf("Failed to connect to Supabase: %v", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("Failed to get device store: %v", err)
	}

	clientLog := waLog.Stdout("Client", "Info", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(handleWhatsAppEvent)

	if client.Store.ID == nil {
		err = client.Connect()
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}

		pairingCode, err := client.PairPhone(ctx, cleanPhone, true, whatsmeow.PairClientChrome, "Chrome (Windows)")
		if err != nil {
			log.Fatalf("Failed to get pairing code: %v", err)
		}
		log.Printf("[+] WhatsApp Pairing Code: %s", pairingCode)
	} else {
		err = client.Connect()
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
		log.Println("[+] WhatsApp client reconnected successfully.")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server listening on :%s", port)
	select {}
}

func handleWhatsAppEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		log.Println("[+] WhatsApp WebSocket connection active.")

	case *events.Message:
		sender := v.Info.Sender.String()
		text := v.Message.GetConversation()
		if text == "" && v.Message.GetExtendedTextMessage() != nil {
			text = v.Message.GetExtendedTextMessage().GetText()
		}
		
		userJID := v.Info.Sender.ToNonAD().String()
		msgText := strings.TrimSpace(text)
		if msgText == "" {
			return
		}

		log.Printf("[INCOMING MESSAGE] From: %s | Text: '%s'", sender, msgText)

		sessionMutex.Lock()
		session, exists := sessionStore[userJID]
		if !exists || strings.ToLower(msgText) == "restart" {
			session = &UserSession{CurrentState: StateStart}
			sessionStore[userJID] = session
		}
		sessionMutex.Unlock()

		reply := processState(userJID, session, msgText, v.Info.Sender)
		if reply != "" {
			sendWhatsAppMessage(v.Info.Sender.ToNonAD(), reply)
		}
	}
}

func processState(userJID string, session *UserSession, input string, sender types.JID) string {
	switch session.CurrentState {
	case StateStart:
		session.CurrentState = StateAwaitingCity
		return "🎬 *Welcome to BMS Seat Monitor Bot!*\n\nLet's set up your tracker.\n\nReply with your City (e.g., Hyderabad, Bangalore, Vizag):"

	case StateAwaitingCity:
		session.City = input
		session.CurrentState = StateAwaitingMovie
		return fmt.Sprintf("City set to: *%s* 📍\n\nNow enter the Movie Name you want to watch (e.g., DC):", session.City)

	case StateAwaitingMovie:
		session.Movie = input
		session.CurrentState = StateAwaitingEventCode
		return fmt.Sprintf("Movie: *%s* 🎥\n\nEnter the BookMyShow Movie Event Code (e.g., ET00511463):", session.Movie)

	case StateAwaitingEventCode:
		session.EventCode = input
		session.CurrentState = StateAwaitingDate
		return "Enter the Date in YYYYMMDD format (e.g., 20260816):"

	case StateAwaitingDate:
		session.Date = input
		session.CurrentState = StateAwaitingTheater
		return "Enter your Theater Code (e.g., AMBH):"

	case StateAwaitingTheater:
		session.Theater = input
		session.CurrentState = StateAwaitingShowID
		return "Enter the specific Show/Venue ID (e.g., 116579):"

	case StateAwaitingShowID:
		session.ShowID = input
		session.CurrentState = StateAwaitingFrequency
		
		session.TargetURL = fmt.Sprintf("https://in.bookmyshow.com/movies/hyd/seat-layout/%s/%s/%s/%s", 
			session.EventCode, session.Theater, session.ShowID, session.Date)

		return "Final step: How often do you want updates?\n\nReply with a number in minutes (e.g., type 5, 15, or 30):"

	case StateAwaitingFrequency:
		var mins int
		_, err := fmt.Sscanf(input, "%d", &mins)
		if err != nil || mins <= 0 {
			mins = 10
		}
		session.Frequency = time.Duration(mins) * time.Minute
		session.CurrentState = StateMonitoring

		ctx, cancel := context.WithCancel(context.Background())
		session.CancelMonitor = cancel
		go startMonitoring(sender, session, ctx)

		return fmt.Sprintf("🚀 *Monitoring Active!*\n\nTarget URL: %s\nChecking every %d minutes. Type *restart* anytime to reset.", session.TargetURL, mins)

	case StateMonitoring:
		if strings.ToLower(input) == "stop" {
			if session.CancelMonitor != nil {
				session.CancelMonitor()
			}
			session.CurrentState = StateStart
			return "⏹️ Monitoring stopped. Send *restart* to begin again."
		}
		return "Bot is currently monitoring seats. Type *stop* or *restart*."
	}

	return "Send *restart* to begin tracking."
}

func startMonitoring(targetJID types.JID, session *UserSession, ctx context.Context) {
	ticker := time.NewTicker(session.Frequency)
	defer ticker.Stop()

	runScraperAndNotify(targetJID, session)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runScraperAndNotify(targetJID, session)
		}
	}
}

func runScraperAndNotify(targetJID types.JID, session *UserSession) {
	log.Printf("[Scraper] Running Chromedp engine for URL: %s", session.TargetURL)
	matrix := fetchCanvasMatrix(session.TargetURL)

	msg := fmt.Sprintf("🎬 *BMS Monitor Update: %s*\n📍 Theater: %s\n\n```\n%s\n```", session.Movie, session.Theater, matrix)
	sendWhatsAppMessage(targetJID, msg)
}

func fetchCanvasMatrix(targetURL string) string {
	ctx, cancelTimeout := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancelTimeout()

	taskCtx, cancelChrome := createChromeContext(ctx)
	defer cancelChrome()

	var finalMatrix string

	err := chromedp.Run(taskCtx,
		chromedp.Navigate(targetURL),
		chromedp.Sleep(12*time.Second),
		chromedp.Evaluate(`(() => {
			if (!window.Konva || !window.Konva.stages || window.Konva.stages.length === 0) {
				return "ERROR: Konva stage object not found on page (Page structure may have changed or blocked).";
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

			return output || "ERROR: Canvas rect elements matched zero seats.";
		})()`, &finalMatrix),
	)

	if err != nil {
		return fmt.Sprintf("Scraper Real Error: %v", err)
	}

	return finalMatrix
}

func createChromeContext(parentCtx context.Context) (context.Context, context.CancelFunc) {
	execPath := os.Getenv("CHROME_PATH")
	if execPath == "" {
		if _, err := os.Stat(`C:\Program Files\Google\Chrome\Application\chrome.exe`); err == nil {
			execPath = `C:\Program Files\Google\Chrome\Application\chrome.exe`
		} else if _, err := os.Stat(`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`); err == nil {
			execPath = `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`
		} else {
			execPath = "/usr/bin/chromium-browser"
		}
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
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parentCtx, opts...)
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)

	cancel := func() {
		cancelTask()
		cancelAlloc()
	}

	return taskCtx, cancel
}

func sendWhatsAppMessage(recipient types.JID, message string) {
	if client == nil || !client.IsConnected() {
		log.Println("[-] WhatsApp client not connected, cannot send message.")
		return
	}
	
	_, err := client.SendMessage(context.Background(), recipient, &waE2E.Message{
		Conversation: proto.String(message),
	})
	if err != nil {
		log.Printf("[-] Failed to send WhatsApp message: %v", err)
	}
}