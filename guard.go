package main

// Safety gate. Context: an earlier build replied to *any* incoming message,
// which meant every contact who messaged -- plus every participant who spoke
// in a shared group, plus everyone in the offline backlog replayed at
// connect -- received an automated reply. These functions exist so that can
// never happen again.
//
// Two independent gates, both fail-closed:
//   allowInbound  - which messages the bot is permitted to react to
//   allowOutbound - who the bot is permitted to send to
// The outbound gate matters most: it is the last check before the network,
// so even a future code path that tries to message someone else is stopped.

import (
	"fmt"
	"os"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

var (
	// startedAt is used to discard the message backlog the server replays on
	// connect. Anything older than this process is not a live request.
	startedAt = time.Now()

	ownerMu    sync.RWMutex
	ownerIDs   []types.JID
	dryRunMode = os.Getenv("DRY_RUN") == "1"
)

// setOwnerIdentities records the identities of the linked device. The owner is
// discovered from the paired device itself, never from configuration, so there
// is no value to mistype. A device has both a phone JID and a LID, and WhatsApp
// may address the same chat by either.
func setOwnerIdentities(ids ...types.JID) {
	var kept []types.JID
	for _, id := range ids {
		if id.User != "" {
			kept = append(kept, id.ToNonAD())
		}
	}
	ownerMu.Lock()
	ownerIDs = kept
	ownerMu.Unlock()
}

func ownerIdentities() []types.JID {
	ownerMu.RLock()
	defer ownerMu.RUnlock()
	out := make([]types.JID, len(ownerIDs))
	copy(out, ownerIDs)
	return out
}

// jidMatches compares on user+server, ignoring the device suffix.
func jidMatches(j types.JID, set []types.JID) bool {
	if j.User == "" {
		return false
	}
	m := j.ToNonAD()
	for _, o := range set {
		if m.User == o.User && m.Server == o.Server {
			return true
		}
	}
	return false
}

// allowInbound decides whether a received message may drive the bot.
// Returns false plus a human-readable reason for the log.
//
// The load-bearing rule is that BOTH the chat and the sender must be the
// owner. In your own "message yourself" chat, Chat == Sender == you. In a
// group, Chat is the group. In a DM from someone else, Chat is that person.
// Requiring Chat to be the owner therefore excludes every other conversation
// by construction, not by blacklist.
func allowInbound(info types.MessageInfo, owner []types.JID, started time.Time) (bool, string) {
	if len(owner) == 0 {
		return false, "own identity not established yet"
	}
	if info.IsGroup {
		return false, "group or broadcast chat"
	}
	switch info.Chat.Server {
	case "g.us", "broadcast", "newsletter":
		return false, fmt.Sprintf("non-DM chat server %q", info.Chat.Server)
	}
	if !jidMatches(info.Chat, owner) {
		return false, fmt.Sprintf("chat %s is not the owner's self-chat", info.Chat.ToNonAD())
	}
	if !jidMatches(info.Sender, owner) {
		return false, fmt.Sprintf("sender %s is not the owner", info.Sender.ToNonAD())
	}
	if !started.IsZero() && info.Timestamp.Before(started) {
		return false, fmt.Sprintf("replayed backlog message from %s", info.Timestamp.Format(time.RFC3339))
	}
	return true, ""
}

// allowOutbound decides whether the bot may send to a JID. Only the owner.
func allowOutbound(to types.JID, owner []types.JID) (bool, string) {
	if len(owner) == 0 {
		return false, "own identity not established yet"
	}
	switch to.Server {
	case "g.us", "broadcast", "newsletter":
		return false, fmt.Sprintf("refusing to send to %q", to.Server)
	}
	if !jidMatches(to, owner) {
		return false, fmt.Sprintf("recipient %s is not the owner", to.ToNonAD())
	}
	return true, ""
}