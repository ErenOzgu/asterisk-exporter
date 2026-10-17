package amisim

import (
	"fmt"
	"math"
	"time"
)

var startedAt = time.Now()

// Demo returns a PBX state that moves with time: calls come and go, a couple
// of endpoints drop off the network now and then and a conference fills and
// empties. It is deterministic in now, which keeps screenshots reproducible.
func Demo(now time.Time) State {
	t := float64(now.Unix())
	st := State{
		AsteriskVersion: "20.6.0 (simulated)",
		AMIVersion:      "9.0.0",
		StartTime:       startedAt,
		ReloadTime:      startedAt,
	}

	const endpoints = 12
	for i := 0; i < endpoints; i++ {
		name := fmt.Sprintf("%d", 1001+i)
		// Each endpoint is unreachable for ~2 of every ~37 minutes, offset per endpoint.
		down := (int64(t)/60+int64(i)*7)%37 < 2
		state, status := "Not in use", "Reachable"
		if down {
			state, status = "Unavailable", "Unreachable"
		}
		st.Endpoints = append(st.Endpoints, Endpoint{Name: name, DeviceState: state})
		c := Contact{Endpoint: name, Status: status}
		if !down {
			// 1-3 ms with a slow wobble; one endpoint sits on a worse link.
			base := 1200 + 800*math.Sin(t/45+float64(i))
			if i == 3 {
				base += 28000
			}
			c.RTTUsec = int(base)
		}
		st.Contacts = append(st.Contacts, c)
	}

	calls := int(math.Round(2 + 2*math.Sin(t/90)))
	for i := 0; i < calls; i++ {
		id := fmt.Sprintf("%d.%d", int64(t)-int64(i)*13, i)
		a, b := 1001+(i*2)%endpoints, 1002+(i*2)%endpoints
		stateB := "Up"
		if i == calls-1 && int64(t)%20 < 6 {
			stateB = "Ringing"
		}
		st.Channels = append(st.Channels,
			Channel{Name: fmt.Sprintf("PJSIP/%d-%08x", a, i*2), State: "Up", Linkedid: id},
			Channel{Name: fmt.Sprintf("PJSIP/%d-%08x", b, i*2+1), State: stateB, Linkedid: id})
		st.Endpoints[a-1001].DeviceState, st.Endpoints[a-1001].ActiveChannels = "In use", 1
	}

	parties := int(math.Round(2 + 2*math.Sin(t/150)))
	if parties > 0 {
		st.Rooms = append(st.Rooms, Room{Name: "700", Parties: parties})
	}
	st.CoreChannels = len(st.Channels) + parties*2 // ConfBridge adds internal channels
	return st
}
