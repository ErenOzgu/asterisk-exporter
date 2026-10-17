// Package ami is a small client for the Asterisk Manager Interface (AMI).
//
// It speaks just enough of the protocol to log in and run "status" and
// "list" actions: one request at a time, with event-list responses
// (EventList: start ... EventList: Complete) collected for the caller.
// It has no third-party dependencies.
package ami

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// maxLine bounds a single protocol line so a misbehaving peer cannot make us
// buffer unbounded data.
const maxLine = 64 << 10

// Header is one "Key: Value" line of an AMI packet.
type Header struct{ Key, Value string }

// Message is one AMI packet: a response or an event.
type Message struct {
	Headers []Header
	// Body holds lines that were not in "Key: Value" form.
	Body []string
}

// Get returns the value of the first header with the given key
// (case-insensitive), or "" if there is none.
func (m Message) Get(key string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Key, key) {
			return h.Value
		}
	}
	return ""
}

// Has reports whether the message carries a header with the given key.
func (m Message) Has(key string) bool {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Key, key) {
			return true
		}
	}
	return false
}

// Result is the outcome of a successful action.
type Result struct {
	// Response is the packet that answered the action.
	Response Message
	// Events are the list items of an event-list action, in arrival order.
	Events []Message
	// Complete is the terminating "...Complete" event of an event list.
	Complete Message
}

// ActionError is returned when Asterisk answers an action with "Response: Error".
type ActionError struct {
	Action  string
	Message string
}

func (e *ActionError) Error() string { return fmt.Sprintf("ami: %s: %s", e.Action, e.Message) }

// IsPermissionDenied reports whether err is Asterisk refusing an action
// because the AMI user lacks the required permission class.
func IsPermissionDenied(err error) bool {
	var ae *ActionError
	return errors.As(err, &ae) && strings.EqualFold(strings.TrimSpace(ae.Message), "Permission denied")
}

// IsEmptyList reports whether err is Asterisk's way of saying a list action
// matched nothing. Several list actions answer an empty result with an error
// instead of an empty event list, e.g. "No Contacts found" (PJSIP) or
// "No active conferences." (ConfBridge).
func IsEmptyList(err error) bool {
	var ae *ActionError
	if !errors.As(err, &ae) {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(ae.Message))
	return strings.HasPrefix(msg, "no ") && !strings.Contains(msg, "such")
}

// Client is a logged-in (or about to log in) AMI session. It is not safe for
// concurrent use.
type Client struct {
	conn   net.Conn
	r      *bufio.Reader
	banner string
	seq    int
	stop   func() bool
}

// Dial connects to an AMI endpoint and reads its banner. The connection
// closes when ctx ends; timeout bounds connecting and the whole session
// unless ctx has an earlier deadline.
func Dial(ctx context.Context, addr string, timeout time.Duration) (*Client, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	c := &Client{conn: conn, r: bufio.NewReader(conn)}
	c.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })

	line, err := c.readLine()
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ami: reading banner: %w", err)
	}
	if !strings.HasPrefix(line, "Asterisk Call Manager") {
		_ = c.Close()
		return nil, fmt.Errorf("ami: %s is not an AMI endpoint (banner %q)", addr, truncate(line, 40))
	}
	c.banner = strings.TrimSpace(line)
	return c, nil
}

// Banner returns the greeting line, e.g. "Asterisk Call Manager/9.0.0".
func (c *Client) Banner() string { return c.banner }

// Close ends the session (best-effort Logoff) and closes the connection.
func (c *Client) Close() error {
	if c.stop != nil {
		c.stop()
	}
	_, _ = io.WriteString(c.conn, "Action: Logoff\r\n\r\n")
	return c.conn.Close()
}

// Login authenticates. Asynchronous events are switched off for the session:
// the exporter only needs request/response traffic.
func (c *Client) Login(username, secret string) error {
	_, err := c.Do("Login", "Username", username, "Secret", secret, "Events", "off")
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

// Do sends an action with its parameters (alternating key, value) and waits
// for the answer. For event-list actions it also collects every list item.
func (c *Client) Do(action string, kv ...string) (*Result, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("ami: odd number of key/value arguments")
	}
	c.seq++
	id := strconv.Itoa(c.seq)

	var sb strings.Builder
	sb.WriteString("Action: " + action + "\r\n")
	sb.WriteString("ActionID: " + id + "\r\n")
	for i := 0; i < len(kv); i += 2 {
		if strings.ContainsAny(kv[i], "\r\n:") || strings.ContainsAny(kv[i+1], "\r\n") {
			return nil, errors.New("ami: header contains a forbidden character")
		}
		sb.WriteString(kv[i] + ": " + kv[i+1] + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := io.WriteString(c.conn, sb.String()); err != nil {
		return nil, fmt.Errorf("ami: sending %s: %w", action, err)
	}

	res := &Result{}
	for { // the response: skip anything that is not ours or not a Response
		m, err := c.readMessage()
		if err != nil {
			return nil, fmt.Errorf("ami: %s: %w", action, err)
		}
		if got := m.Get("ActionID"); got != "" && got != id {
			continue
		}
		if m.Has("Response") {
			res.Response = m
			break
		}
	}
	if strings.EqualFold(res.Response.Get("Response"), "Error") {
		return nil, &ActionError{Action: action, Message: res.Response.Get("Message")}
	}
	if !strings.EqualFold(res.Response.Get("EventList"), "start") {
		return res, nil
	}
	for {
		m, err := c.readMessage()
		if err != nil {
			return nil, fmt.Errorf("ami: %s: reading list: %w", action, err)
		}
		if got := m.Get("ActionID"); got != "" && got != id {
			continue
		}
		if strings.EqualFold(m.Get("EventList"), "Complete") {
			res.Complete = m
			return res, nil
		}
		if m.Has("Event") {
			res.Events = append(res.Events, m)
		}
	}
}

func (c *Client) readLine() (string, error) {
	var sb strings.Builder
	for {
		part, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return "", err
		}
		sb.Write(part)
		if sb.Len() > maxLine {
			return "", errors.New("line too long")
		}
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

// readMessage reads one packet: lines up to the next blank line.
func (c *Client) readMessage() (Message, error) {
	var m Message
	for {
		line, err := c.readLine()
		if err != nil {
			return m, err
		}
		if line == "" {
			if len(m.Headers) == 0 && len(m.Body) == 0 {
				continue // stray blank line between packets
			}
			return m, nil
		}
		if i := strings.Index(line, ":"); i > 0 {
			m.Headers = append(m.Headers, Header{Key: line[:i], Value: strings.TrimSpace(line[i+1:])})
		} else {
			m.Body = append(m.Body, line)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
