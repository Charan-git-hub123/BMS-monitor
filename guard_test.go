package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// The exact scenarios from the incident, encoded so a regression fails the build.
func TestAllowInbound(t *testing.T) {
	const wa = "s.whatsapp.net"
	me := types.JID{User: "911111111111", Server: wa}
	myLID := types.JID{User: "777777777777", Server: "lid"}
	stranger := types.JID{User: "912222222222", Server: wa}
	group := types.JID{User: "912222222222-1600000000", Server: "g.us"}

	owner := []types.JID{me, myLID}
	start := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	live := start.Add(time.Minute)
	stale := start.Add(-time.Hour)

	info := func(chat, sender types.JID, ts time.Time, isGroup bool) types.MessageInfo {
		return types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: isGroup},
			Timestamp:     ts,
		}
	}

	cases := []struct {
		name  string
		info  types.MessageInfo
		owner []types.JID
		want  bool
	}{
		{"own self-chat, live", info(me, me, live, false), owner, true},
		{"own self-chat via LID", info(myLID, myLID, live, false), owner, true},
		{"own self-chat, device suffix", info(
			types.JID{User: me.User, Server: wa, Device: 3},
			types.JID{User: me.User, Server: wa, Device: 3}, live, false), owner, true},

		// Each of these sent a message during the incident.
		{"DM from a stranger", info(stranger, stranger, live, false), owner, false},
		{"stranger spoke in a group", info(group, stranger, live, true), owner, false},
		{"I spoke in a group", info(group, me, live, true), owner, false},
		{"replayed backlog from self", info(me, me, stale, false), owner, false},
		{"owner not yet known", info(me, me, live, false), nil, false},

		// Sender is me but chat is someone else: a message I sent to a contact
		// from my phone. Must not trigger a reply to that contact.
		{"my own outgoing DM to a contact", info(stranger, me, live, false), owner, false},
		{"broadcast list", info(types.JID{User: "status", Server: "broadcast"}, me, live, false), owner, false},
	}

	for _, c := range cases {
		got, reason := allowInbound(c.info, c.owner, start)
		if got != c.want {
			t.Errorf("%s: allowInbound = %v (reason %q), want %v", c.name, got, reason, c.want)
		}
	}
}

func TestAllowOutbound(t *testing.T) {
	const wa = "s.whatsapp.net"
	me := types.JID{User: "911111111111", Server: wa}
	owner := []types.JID{me}

	cases := []struct {
		name  string
		to    types.JID
		owner []types.JID
		want  bool
	}{
		{"to self", me, owner, true},
		{"to self with device suffix", types.JID{User: me.User, Server: wa, Device: 7}, owner, true},
		{"to a stranger", types.JID{User: "912222222222", Server: wa}, owner, false},
		{"to a group", types.JID{User: "912222222222-1600000000", Server: "g.us"}, owner, false},
		{"to status broadcast", types.JID{User: "status", Server: "broadcast"}, owner, false},
		{"owner not yet known", me, nil, false},
		{"empty JID", types.JID{}, owner, false},
	}

	for _, c := range cases {
		got, reason := allowOutbound(c.to, c.owner)
		if got != c.want {
			t.Errorf("%s: allowOutbound = %v (reason %q), want %v", c.name, got, reason, c.want)
		}
	}
}