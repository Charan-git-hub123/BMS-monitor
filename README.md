# 🎬 BookMyShow WhatsApp Seat Monitor

A stateful, interactive WhatsApp bot built in Go that tracks real-time seat availability on BookMyShow. 

Instead of relying on rigid, hardcoded URLs, this bot acts as a live headless browser surrogate. It interactively scrapes BookMyShow based on user WhatsApp inputs, guides the user through the booking flow (City -> Movie -> Theater -> Time), and monitors the final seat layout canvas for updates.

## 🛠️ Tech Stack
* **Language:** Go
* **WhatsApp Integration:** [`whatsmeow`](https://github.com/tulir/whatsmeow) (with PostgreSQL/SQLite session storage)
* **Web Automation & Scraping:** [`chromedp`](https://github.com/chromedp/chromedp)
* **State Management:** In-memory struct mapping for multi-step conversational state handling

---

## 🎯 Project Requirements & Objectives

### Initial Approach (And Why It Failed)
The initial requirement was to build a bot where a user could manually input a movie's Event Code (e.g., `ET00510229`), Theater Code, Date, and Show ID to generate a target BookMyShow URL and monitor seat availability. 

**What is failing currently:** 
BookMyShow’s URL structure is entirely dynamic. Every movie release, theater location, and showtime gets assigned unique internal database IDs. Hardcoding or manually guessing these codes breaks the scraper because the IDs expire or mismatch, resulting in invalid URLs and failed canvas matrix extraction.

### Current Architecture (The Solution)
To eliminate manual ID entry and URL failures, the bot is being re-architected into a **Parallel Step-by-Step Interactive Bot**. Every WhatsApp reply triggers a live `chromedp` background navigation on BookMyShow to extract active UI elements and present them as numbered choices in the chat.

#### The Dynamic Conversational Flow:
1. **City Selection:** User selects a city (e.g., Hyderabad). The backend scrapes the active explore page and returns a list of currently showing movies.
2. **Movie Selection:** User selects a movie. The backend navigates to the movie page and checks for a multi-language prompt.
3. **Language & Theater Selection:** After optional language selection, the backend extracts all active theater cards and dates, presenting them in WhatsApp.
4. **Showtime Selection:** User selects a time. The backend clicks the exact showtime pill in the headless browser.
5. **URL Capture & Monitoring:** The backend reads the final, valid seat-layout URL directly from the active browser window and begins the scheduled polling loop to extract the seat matrix.

---

## 🚧 Current Status & Next Steps

The foundation for WhatsApp session management, database pairing, and the base `chromedp` canvas evaluation script is complete. The project is currently paused to implement the dynamic data extraction modules.

### What Needs to be Implemented Next:
- [ ] `fetchCityMovies(city)`: Navigate to the city URL and scrape active movie titles.
- [ ] `fetchMovieLanguagesAndTheaters(movieURL)`: Handle conditional language popups and extract available theaters/dates.
- [ ] `resolveSeatLayoutURL(theater, showtime)`: Click the final UI elements to capture the native active URL without manual construction.
- [ ] Integrate these extraction functions into the `whatsmeow` message event handler to drive the state machine.

---

## 🚀 How to Run Locally

1. Clone the repository.
2. Set up your PostgreSQL/Supabase database URL and target phone number in `main.go`.
3. Install dependencies:
   ```bash
   go mod init bms-monitor
   go get [github.com/chromedp/chromedp](https://github.com/chromedp/chromedp)
   go get go.mau.fi/whatsmeow
   go get [github.com/mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
   go get google.golang.org/protobuf/proto
   go mod tidy