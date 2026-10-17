// Package amisim is a small simulated Asterisk Manager Interface server.
//
// It answers the handful of actions the exporter uses with the same packet
// layout a real Asterisk 20 produces (the formats were captured from one), so
// the exporter can be tested and demoed without a PBX. It is test tooling,
// not a general AMI implementation.
package amisim

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Channel is one entry of CoreShowChannels.
type Channel struct{ Name, State, Linkedid string }

// Endpoint is one PJSIP endpoint.
type Endpoint struct {
	Name, DeviceState string
	ActiveChannels    int
}

// Contact is one registered PJSIP contact.
type Contact struct {
	Endpoint, Status string
	RTTUsec          int
}

// Room is one ConfBridge conference.
type Room struct {
	Name    string
	Parties int
}

// Peer is one chan_sip peer.
type Peer struct{ Name, Status, IP string }

// State is everything the simulated PBX reports.
type State struct {
	AsteriskVersion string
	AMIVersion      string
	MaxCalls        int
	CoreChannels    int // reported by CoreStatus as CoreCurrentCalls
	StartTime       time.Time
	ReloadTime      time.Time

	Channels  []Channel
	Endpoints []Endpoint
	Contacts  []Contact
	Rooms     []Room
	Peers     []Peer

	// Modules that are "not loaded": their actions answer
	// "Invalid/unknown command", like Asterisk does.
	NoPJSIP, NoConfBridge, NoSIP bool
	// Actions refused with "Permission denied".
	Denied map[string]bool
}

// Server is the simulated AMI endpoint.
type Server struct {
	Username, Secret string
	// State is called on every request, so tests can change it and demos can
	// make it move over time.
	State func() State
	// Location is the zone startup/reload times are printed in (default UTC).
	Location *time.Location
	// Noise makes the server send an unrelated asynchronous event before each
	// response, as a real Asterisk does when events are enabled.
	Noise bool
	// Stall makes the server read but never answer the named action.
	Stall string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.conns == nil {
			s.conns = map[net.Conn]struct{}{}
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				_ = conn.Close()
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.handle(conn)
		}()
	}
}

// CloseConns drops every open client connection.
func (s *Server) CloseConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.Close()
	}
}

func (s *Server) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.UTC
}

func (s *Server) handle(conn net.Conn) {
	st := s.State()
	fmt.Fprintf(conn, "Asterisk Call Manager/%s\r\n", st.AMIVersion)
	r := bufio.NewReader(conn)
	loggedIn := false
	for {
		req, err := readRequest(r)
		if err != nil {
			return
		}
		action, id := req["action"], req["actionid"]
		if s.Noise {
			fmt.Fprint(conn, "Event: Newchannel\r\nPrivilege: call,all\r\nChannel: PJSIP/9999-00000001\r\n\r\n")
		}
		if action == s.Stall && action != "" {
			continue
		}
		switch {
		case strings.EqualFold(action, "Login"):
			if req["username"] == s.Username && req["secret"] == s.Secret {
				loggedIn = true
				writePacket(conn, id, "Response: Success", "Message: Authentication accepted")
				continue
			}
			writePacket(conn, id, "Response: Error", "Message: Authentication failed")
			return
		case strings.EqualFold(action, "Logoff"):
			writePacket(conn, id, "Response: Goodbye", "Message: Thanks for all the fish.")
			return
		case !loggedIn:
			writePacket(conn, id, "Response: Error", "Message: Permission denied")
			continue
		}
		s.answer(conn, s.State(), action, id)
	}
}

func (s *Server) answer(w net.Conn, st State, action, id string) {
	if st.Denied[action] {
		writePacket(w, id, "Response: Error", "Message: Permission denied")
		return
	}
	switch action {
	case "CoreStatus":
		start, reload := st.StartTime.In(s.loc()), st.ReloadTime.In(s.loc())
		writePacket(w, id, "Response: Success",
			"CoreStartupDate: "+start.Format("2006-01-02"), "CoreStartupTime: "+start.Format("15:04:05"),
			"CoreReloadDate: "+reload.Format("2006-01-02"), "CoreReloadTime: "+reload.Format("15:04:05"),
			"CoreCurrentCalls: "+strconv.Itoa(st.CoreChannels))
	case "CoreSettings":
		writePacket(w, id, "Response: Success", "AMIversion: "+st.AMIVersion, "AsteriskVersion: "+st.AsteriskVersion,
			"SystemName: ", "CoreMaxCalls: "+strconv.Itoa(st.MaxCalls), "CoreMaxLoadAvg: 0.000000", "CoreRunUser: ", "CoreRunGroup: ")
	case "CoreShowChannels":
		items := make([][]string, 0, len(st.Channels))
		for _, c := range st.Channels {
			items = append(items, []string{"Event: CoreShowChannel", "Channel: " + c.Name, "ChannelState: 6",
				"ChannelStateDesc: " + c.State, "Context: default", "Exten: s", "Priority: 1",
				"Uniqueid: " + c.Linkedid, "Linkedid: " + c.Linkedid, "Application: Wait", "Duration: 00:00:03", "BridgeId: "})
		}
		writeList(w, id, "Channels will follow", "CoreShowChannelsComplete", items)
	case "PJSIPShowEndpoints":
		if st.NoPJSIP {
			unknown(w, id, action)
			return
		}
		items := make([][]string, 0, len(st.Endpoints))
		for _, e := range st.Endpoints {
			active := ""
			if e.ActiveChannels > 0 {
				active = strconv.Itoa(e.ActiveChannels)
			}
			items = append(items, []string{"Event: EndpointList", "ObjectType: endpoint", "ObjectName: " + e.Name,
				"Transport: ", "Aor: " + e.Name, "Auths: ", "OutboundAuths: ", "Contacts: ", "DeviceState: " + e.DeviceState,
				"ActiveChannels: " + active})
		}
		writeList(w, id, "A listing of Endpoints follows, presented as EndpointList events", "EndpointListComplete", items)
	case "PJSIPShowContacts":
		if st.NoPJSIP {
			unknown(w, id, action)
			return
		}
		if len(st.Contacts) == 0 {
			writePacket(w, id, "Response: Error", "Message: No Contacts found")
			return
		}
		items := make([][]string, 0, len(st.Contacts))
		for _, c := range st.Contacts {
			rtt := "N/A"
			if c.RTTUsec > 0 {
				rtt = strconv.Itoa(c.RTTUsec)
			}
			items = append(items, []string{"Event: ContactList", "ObjectType: contact",
				"ObjectName: " + c.Endpoint + ";@0123456789abcdef", "Endpoint: " + c.Endpoint,
				"Uri: sip:" + c.Endpoint + "@192.0.2.10:5060", "Status: " + c.Status, "RoundtripUsec: " + rtt})
		}
		writeList(w, id, "A listing of Contacts follows, presented as ContactList events", "ContactListComplete", items)
	case "ConfbridgeListRooms":
		if st.NoConfBridge {
			unknown(w, id, action)
			return
		}
		if len(st.Rooms) == 0 {
			writePacket(w, id, "Response: Error", "Message: No active conferences.")
			return
		}
		items := make([][]string, 0, len(st.Rooms))
		for _, r := range st.Rooms {
			items = append(items, []string{"Event: ConfbridgeListRooms", "Conference: " + r.Name,
				"Parties: " + strconv.Itoa(r.Parties), "Marked: 0", "Locked: No", "Muted: No"})
		}
		writeList(w, id, "Confbridge conferences will follow", "ConfbridgeListRoomsComplete", items)
	case "SIPpeers":
		if st.NoSIP {
			unknown(w, id, action)
			return
		}
		items := make([][]string, 0, len(st.Peers))
		for _, p := range st.Peers {
			items = append(items, []string{"Event: PeerEntry", "Channeltype: SIP", "ObjectName: " + p.Name,
				"IPaddress: " + p.IP, "IPport: 5060", "Dynamic: yes", "Status: " + p.Status})
		}
		writeList(w, id, "Peer status list will follow", "PeerlistComplete", items)
	default:
		unknown(w, id, action)
	}
}

func unknown(w net.Conn, id, action string) {
	writePacket(w, id, "Response: Error",
		"Message: Invalid/unknown command: "+action+". Use Action: ListCommands to show available commands.")
}

// readRequest reads one request packet; keys are lower-cased.
func readRequest(r *bufio.Reader) (map[string]string, error) {
	req := map[string]string{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(req) == 0 {
				continue
			}
			return req, nil
		}
		if i := strings.Index(line, ":"); i > 0 {
			req[strings.ToLower(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
}

// writePacket writes a packet; the first line is the Response/Event header and
// the ActionID goes second, as Asterisk does.
func writePacket(w net.Conn, id string, lines ...string) {
	var sb strings.Builder
	sb.WriteString(lines[0] + "\r\n")
	if id != "" {
		sb.WriteString("ActionID: " + id + "\r\n")
	}
	for _, l := range lines[1:] {
		sb.WriteString(l + "\r\n")
	}
	sb.WriteString("\r\n")
	_, _ = w.Write([]byte(sb.String()))
}

func writeList(w net.Conn, id, message, completeEvent string, items [][]string) {
	writePacket(w, id, "Response: Success", "EventList: start", "Message: "+message)
	for _, it := range items {
		writePacket(w, id, it...)
	}
	writePacket(w, id, "Event: "+completeEvent, "EventList: Complete", "ListItems: "+strconv.Itoa(len(items)))
}
