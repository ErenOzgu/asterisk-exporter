package collector

import (
	"strings"
	"time"

	"github.com/ErenOzgu/asterisk-exporter/internal/ami"
	"github.com/ErenOzgu/asterisk-exporter/internal/expfmt"
)

// ---- core ------------------------------------------------------------------

func collectCore(c *ami.Client, cfg Config, out *expfmt.Set) error {
	st, err := c.Do("CoreStatus")
	if err != nil {
		return err
	}
	// Despite its name, AMI's CoreCurrentCalls is Asterisk's active *channel*
	// count (it includes internal channels such as ConfBridge announcers).
	if v, ok := num(st.Response.Get("CoreCurrentCalls")); ok {
		out.Add("asterisk_core_current_channels", "Channels currently allocated in Asterisk, internal ones included (AMI field CoreCurrentCalls).", expfmt.Gauge, v)
	}
	if t, ok := parseAMITime(st.Response.Get("CoreStartupDate"), st.Response.Get("CoreStartupTime"), cfg.Location); ok {
		out.Add("asterisk_core_start_time_seconds", "Start time of Asterisk since the Unix epoch, in seconds. Uptime is time() - this.", expfmt.Gauge, float64(t.Unix()))
	}
	if t, ok := parseAMITime(st.Response.Get("CoreReloadDate"), st.Response.Get("CoreReloadTime"), cfg.Location); ok {
		out.Add("asterisk_core_last_reload_time_seconds", "Time of the last Asterisk reload since the Unix epoch, in seconds.", expfmt.Gauge, float64(t.Unix()))
	}

	cs, err := c.Do("CoreSettings")
	if err != nil {
		return err
	}
	r := cs.Response
	out.Add("asterisk_info", "Asterisk version information; the value is always 1.", expfmt.Gauge, 1,
		"version", r.Get("AsteriskVersion"), "ami_version", r.Get("AMIversion"), "system_name", r.Get("SystemName"))
	if v, ok := num(r.Get("CoreMaxCalls")); ok {
		out.Add("asterisk_core_max_calls", "Configured maxcalls limit; 0 means unlimited.", expfmt.Gauge, v)
	}
	return nil
}

// parseAMITime parses the "2026-10-17" + "12:17:48" pair AMI uses, which is
// local wall-clock time of the Asterisk host.
func parseAMITime(date, clock string, loc *time.Location) (time.Time, bool) {
	if date == "" || clock == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, loc)
	return t, err == nil
}

// ---- channels --------------------------------------------------------------

var channelStates = []string{"down", "rsrvd", "offhook", "dialing", "ring", "ringing", "up", "busy", "dialing_offhook", "pre_ring", "unknown"}

func collectChannels(c *ami.Client, _ Config, out *expfmt.Set) error {
	events, err := list(c, "CoreShowChannels", "CoreShowChannel")
	if err != nil {
		return err
	}
	byState := map[string]float64{}
	for _, s := range channelStates {
		byState[s] = 0 // known states always present, so graphs do not have gaps
	}
	calls := map[string]struct{}{}
	for _, ev := range events {
		byState[normalize(ev.Get("ChannelStateDesc"))]++
		// All legs of one call (including Local channel pairs) share a Linkedid.
		if id := ev.Get("Linkedid"); id != "" {
			calls[id] = struct{}{}
		}
	}
	for state, n := range byState {
		out.Add("asterisk_channels", "Channels by state, as listed by CoreShowChannels (internal channels are not listed).", expfmt.Gauge, n, "state", state)
	}
	out.Add("asterisk_calls_active", "Active calls: distinct Linkedid values among listed channels. All legs of a call share one.", expfmt.Gauge, float64(len(calls)))
	return nil
}

// ---- PJSIP -----------------------------------------------------------------

var (
	endpointStates = []string{"unknown", "not_in_use", "in_use", "busy", "invalid", "unavailable", "ringing", "in_use_ringing", "on_hold"}
	contactStatus  = []string{"reachable", "unreachable", "nonqualified", "unknown", "removed"}
)

func collectPJSIP(c *ami.Client, cfg Config, out *expfmt.Set) error {
	endpoints, err := list(c, "PJSIPShowEndpoints", "EndpointList")
	if err != nil {
		return err
	}
	byState := map[string]float64{}
	for _, s := range endpointStates {
		byState[s] = 0
	}
	for _, ev := range endpoints {
		state := normalize(ev.Get("DeviceState"))
		byState[state]++
		if cfg.PerEndpoint {
			name := ev.Get("ObjectName")
			out.Add("asterisk_pjsip_endpoint_up", "Whether the PJSIP endpoint is available (1) or unavailable/invalid (0).", expfmt.Gauge,
				boolf(state != "unavailable" && state != "invalid"), "endpoint", name)
			ch, _ := num(ev.Get("ActiveChannels")) // empty when idle
			out.Add("asterisk_pjsip_endpoint_active_channels", "Channels currently active on the PJSIP endpoint.", expfmt.Gauge, ch, "endpoint", name)
		}
	}
	for state, n := range byState {
		out.Add("asterisk_pjsip_endpoints", "PJSIP endpoints by device state.", expfmt.Gauge, n, "state", state)
	}

	contacts, err := list(c, "PJSIPShowContacts", "ContactList")
	if err != nil {
		return err
	}
	byStatus := map[string]float64{}
	for _, s := range contactStatus {
		byStatus[s] = 0
	}
	worstRTT := map[string]float64{}
	for _, ev := range contacts {
		status := normalize(ev.Get("Status"))
		byStatus[status]++
		if !cfg.PerEndpoint || status != "reachable" {
			continue
		}
		usec, ok := num(ev.Get("RoundtripUsec"))
		if !ok || usec <= 0 {
			continue
		}
		name := firstNonEmpty(ev.Get("EndpointName"), ev.Get("Endpoint"))
		if name == "" {
			continue
		}
		if sec := usec / 1e6; sec > worstRTT[name] {
			worstRTT[name] = sec // an endpoint with several contacts reports its slowest
		}
	}
	for status, n := range byStatus {
		out.Add("asterisk_pjsip_contacts", "PJSIP contacts by qualify status.", expfmt.Gauge, n, "status", status)
	}
	for name, sec := range worstRTT {
		out.Add("asterisk_pjsip_contact_rtt_seconds", "Qualify round-trip time of the endpoint's reachable contacts (slowest if several).", expfmt.Gauge, sec, "endpoint", name)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- chan_sip --------------------------------------------------------------

var sipStatuses = []string{"ok", "lagged", "unreachable", "unknown", "unmonitored", "other"}

func collectSIP(c *ami.Client, cfg Config, out *expfmt.Set) error {
	peers, err := list(c, "SIPpeers", "PeerEntry")
	if err != nil {
		return err
	}
	byStatus := map[string]float64{}
	for _, s := range sipStatuses {
		byStatus[s] = 0
	}
	for _, ev := range peers {
		status := sipStatus(ev.Get("Status"))
		byStatus[status]++
		if cfg.PerEndpoint {
			up := status == "ok" || status == "lagged" ||
				(status == "unmonitored" && ev.Get("IPaddress") != "" && ev.Get("IPaddress") != "-none-")
			out.Add("asterisk_sip_peer_up", "Whether the chan_sip peer is reachable (1) or not (0).", expfmt.Gauge, boolf(up), "peer", ev.Get("ObjectName"))
		}
	}
	for status, n := range byStatus {
		out.Add("asterisk_sip_peers", "chan_sip peers by qualify status.", expfmt.Gauge, n, "status", status)
	}
	return nil
}

// sipStatus maps chan_sip's "OK (12 ms)" / "LAGGED (350 ms)" / "UNREACHABLE" ... to a label.
func sipStatus(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	for _, known := range sipStatuses[:5] {
		if strings.HasPrefix(l, known) {
			return known
		}
	}
	return "other"
}

// ---- ConfBridge ------------------------------------------------------------

func collectConfBridge(c *ami.Client, _ Config, out *expfmt.Set) error {
	rooms, err := list(c, "ConfbridgeListRooms", "ConfbridgeListRooms")
	if err != nil {
		return err
	}
	out.Add("asterisk_confbridge_rooms", "Active ConfBridge conferences.", expfmt.Gauge, float64(len(rooms)))
	for _, ev := range rooms {
		parties, _ := num(ev.Get("Parties"))
		out.Add("asterisk_confbridge_parties", "Participants in the ConfBridge conference.", expfmt.Gauge, parties, "conference", ev.Get("Conference"))
	}
	return nil
}
