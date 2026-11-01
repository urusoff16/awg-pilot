// Package health decides when a running tunnel is dead and what to try next. Windows and
// Android share it, so both recover the same way: ping, reconnect, reconnect again, a fresh
// config, the next profile, another server, and then the whole round again until it works.
package health

import "time"

const (
	// Tick is how often the watchdog looks at the tunnel (the ping runs every 5 seconds).
	Tick = 5 * time.Second
	// Grace lets a new tunnel or a recovery step finish its handshake before it is judged.
	Grace = 12 * time.Second
	// NoHandshakeAfter: a tunnel that has not completed a single handshake by then is dead.
	NoHandshakeAfter = 20 * time.Second
	// StaleAfter applies when the server never answers pings: WireGuard re-handshakes every
	// 2 minutes while traffic flows, so a much older handshake means a dead link.
	StaleAfter = 190 * time.Second
	// PingFails in a row (15 s) mean a dead link; FastPingFails right after a network switch.
	PingFails     = 3
	FastPingFails = 2
	FastWindow    = time.Minute
	// RoundPause separates full recovery rounds once every step has been tried.
	RoundPause = 30 * time.Second
)

// Input is what the watchdog knows about the running tunnel.
type Input struct {
	Now           time.Time
	Since         time.Time // tunnel start
	LastHandshake time.Time
	PingFails     int       // consecutive unanswered pings on this tunnel
	PingWorks     bool      // the server has answered pings on this profile before
	NetChangedAt  time.Time // last Wi-Fi/mobile/cable switch
	ActionAt      time.Time // last recovery step
}

// Dead reports whether the tunnel has stopped carrying traffic, with the reason for the log.
func Dead(in Input) (bool, string) {
	start := in.Since
	if in.ActionAt.After(start) {
		start = in.ActionAt
	}
	if in.Now.Sub(start) < Grace {
		return false, ""
	}
	if in.LastHandshake.IsZero() {
		if in.Now.Sub(in.Since) >= NoHandshakeAfter {
			return true, "сервер не ответил ни разу"
		}
		return false, ""
	}
	if in.PingWorks {
		limit := PingFails
		if !in.NetChangedAt.IsZero() && in.Now.Sub(in.NetChangedAt) < FastWindow {
			limit = FastPingFails
		}
		if in.PingFails >= limit {
			return true, "сервер не отвечает на пинг"
		}
		return false, ""
	}
	if in.Now.Sub(in.LastHandshake) > StaleAfter {
		return true, "нет обмена ключами с сервером больше 3 минут"
	}
	return false, ""
}

// Recovered reports that a tunnel under recovery works again: the server completed a handshake
// after the last step and, if it answers pings, the pings are back.
func Recovered(in Input) bool {
	if dead, _ := Dead(in); dead || in.Now.Sub(in.ActionAt) < Grace {
		return false
	}
	start := in.Since
	if in.ActionAt.After(start) {
		start = in.ActionAt
	}
	if !in.LastHandshake.After(start) {
		return false
	}
	return !in.PingWorks || in.PingFails == 0
}

// Step is the next recovery action.
type Step int

const (
	Reconnect Step = iota
	RefreshConfig
	NextProfile
	OtherServer
	Pause // all steps tried: wait RoundPause, then start the round again
)

// Stages: 0 healthy; each step moves to the next stage while the tunnel stays dead.
// Plan returns the step for the current stage. hasKey: the profile can talk to the provider;
// moreProfiles: another profile has not been tried in this round.
func Plan(stage int, hasKey, moreProfiles bool) Step {
	switch stage {
	case 0, 1:
		return Reconnect // reconnect, then once more
	case 2:
		if hasKey {
			return RefreshConfig
		}
		fallthrough
	case 3:
		if moreProfiles {
			return NextProfile
		}
		fallthrough
	case 4:
		if hasKey {
			return OtherServer
		}
	}
	return Pause
}
