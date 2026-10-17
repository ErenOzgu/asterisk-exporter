package ami

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ErenOzgu/asterisk-exporter/internal/amisim"
)

func startSim(t *testing.T, mutate func(*amisim.Server)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &amisim.Server{
		Username: "u", Secret: "s",
		State: func() amisim.State {
			return amisim.State{
				AsteriskVersion: "20.6.0", AMIVersion: "9.0.0",
				Channels:  []amisim.Channel{{Name: "PJSIP/1-0", State: "Up", Linkedid: "L1"}, {Name: "PJSIP/2-1", State: "Up", Linkedid: "L1"}},
				Endpoints: []amisim.Endpoint{{Name: "1", DeviceState: "Not in use"}},
				Denied:    map[string]bool{"SIPpeers": true},
			}
		},
	}
	if mutate != nil {
		mutate(srv)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close(); srv.CloseConns() })
	return ln.Addr().String()
}

func dial(t *testing.T, addr string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, err := Dial(ctx, addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestLoginAndStatusAction(t *testing.T) {
	c := dial(t, startSim(t, nil))
	if got := c.Banner(); got != "Asterisk Call Manager/9.0.0" {
		t.Errorf("banner = %q", got)
	}
	if err := c.Login("u", "s"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Do("CoreSettings")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Response.Get("asteriskversion"); got != "20.6.0" { // keys are case-insensitive
		t.Errorf("AsteriskVersion = %q", got)
	}
}

func TestLoginFailure(t *testing.T) {
	c := dial(t, startSim(t, nil))
	err := c.Login("u", "wrong")
	var ae *ActionError
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "Authentication failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventListIsCollectedAndForeignEventsSkipped(t *testing.T) {
	c := dial(t, startSim(t, func(s *amisim.Server) { s.Noise = true })) // unrelated async events before every reply
	if err := c.Login("u", "s"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Do("CoreShowChannels")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(res.Events), res.Events)
	}
	if res.Events[0].Get("Channel") != "PJSIP/1-0" || res.Events[1].Get("Linkedid") != "L1" {
		t.Errorf("unexpected events: %+v", res.Events)
	}
	if res.Complete.Get("ListItems") != "2" {
		t.Errorf("ListItems = %q", res.Complete.Get("ListItems"))
	}
	// the session stays usable for the next action
	if _, err := c.Do("PJSIPShowEndpoints"); err != nil {
		t.Fatal(err)
	}
}

func TestErrorResponses(t *testing.T) {
	c := dial(t, startSim(t, nil))
	if err := c.Login("u", "s"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Do("SIPpeers")
	if !IsPermissionDenied(err) {
		t.Errorf("SIPpeers: want permission denied, got %v", err)
	}
	_, err = c.Do("PJSIPShowContacts")
	if !IsEmptyList(err) { // "No Contacts found"
		t.Errorf("PJSIPShowContacts: want empty-list error, got %v", err)
	}
	_, err = c.Do("ConfbridgeListRooms")
	if !IsEmptyList(err) { // "No active conferences."
		t.Errorf("ConfbridgeListRooms: want empty-list error, got %v", err)
	}
	_, err = c.Do("NoSuchAction")
	if err == nil || IsEmptyList(err) || IsPermissionDenied(err) {
		t.Errorf("unknown action must be a plain error, got %v", err)
	}
}

func TestErrorClassifiers(t *testing.T) {
	cases := []struct {
		msg           string
		empty, denied bool
	}{
		{"No Contacts found", true, false},
		{"No active conferences.", true, false},
		{"Permission denied", false, true},
		{"No such command 'x'", false, false},
		{"Invalid/unknown command: Foo.", false, false},
		{"Authentication failed", false, false},
	}
	for _, tc := range cases {
		err := error(&ActionError{Action: "A", Message: tc.msg})
		if IsEmptyList(err) != tc.empty || IsPermissionDenied(err) != tc.denied {
			t.Errorf("%q: empty=%v denied=%v", tc.msg, IsEmptyList(err), IsPermissionDenied(err))
		}
	}
	if IsEmptyList(errors.New("boom")) || IsPermissionDenied(nil) {
		t.Error("non-ActionError must not classify")
	}
}

func TestRejectsNonAMIEndpoint(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			conn.Close()
		}
	}()
	_, err := Dial(context.Background(), ln.Addr().String(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "not an AMI endpoint") {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectsHeaderInjection(t *testing.T) {
	c := dial(t, startSim(t, nil))
	if err := c.Login("u", "s"); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][]string{{"Key", "value\r\nAction: Hangup"}, {"Bad\nKey", "v"}, {"Odd"}} {
		if _, err := c.Do("CoreStatus", kv...); err == nil {
			t.Errorf("expected rejection for %q", kv)
		}
	}
}

func TestTimeoutWhenServerStalls(t *testing.T) {
	addr := startSim(t, func(s *amisim.Server) { s.Stall = "CoreStatus" })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c, err := Dial(ctx, addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("u", "s"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Do("CoreStatus"); err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stalled action took %v, should stop near the 300ms deadline", d)
	}
}

// A raw server for protocol corner cases the simulator does not produce.
func rawServer(t *testing.T, script func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}()
	return ln.Addr().String()
}

func readUntilBlank(c net.Conn) {
	buf := make([]byte, 1)
	var last4 []byte
	for {
		if _, err := c.Read(buf); err != nil {
			return
		}
		last4 = append(last4, buf[0])
		if len(last4) > 4 {
			last4 = last4[1:]
		}
		if string(last4) == "\r\n\r\n" {
			return
		}
	}
}

func TestToleratesLFOnlyStrayBlankLinesAndForeignActionIDs(t *testing.T) {
	addr := rawServer(t, func(c net.Conn) {
		c.Write([]byte("Asterisk Call Manager/2.10.0\r\n"))
		readUntilBlank(c)
		c.Write([]byte("\n\n" + // stray blank lines
			"Response: Success\nActionID: 999\nMessage: someone else's reply\n\n" + // foreign ActionID
			"Response: Success\nActionID: 1\nMessage: ok\n\n"))
	})
	c := dial(t, addr)
	res, err := c.Do("Ping")
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Get("Message") != "ok" {
		t.Errorf("got %+v", res.Response)
	}
}

func TestOverlongLineIsRejected(t *testing.T) {
	addr := rawServer(t, func(c net.Conn) {
		c.Write([]byte("Asterisk Call Manager/9.0.0\r\n"))
		readUntilBlank(c)
		c.Write([]byte("Response: Success\r\nMessage: " + strings.Repeat("x", 3*maxLine) + "\r\n\r\n"))
	})
	c := dial(t, addr)
	if _, err := c.Do("Ping"); err == nil || !strings.Contains(err.Error(), "line too long") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectionClosedMidList(t *testing.T) {
	addr := rawServer(t, func(c net.Conn) {
		c.Write([]byte("Asterisk Call Manager/9.0.0\r\n"))
		readUntilBlank(c)
		c.Write([]byte("Response: Success\r\nActionID: 1\r\nEventList: start\r\n\r\nEvent: X\r\nActionID: 1\r\n\r\n"))
		// ... and hang up before EventList: Complete
	})
	c := dial(t, addr)
	if _, err := c.Do("List"); err == nil {
		t.Fatal("expected an error for a truncated list")
	}
}
