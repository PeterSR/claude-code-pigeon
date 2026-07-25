package pigeon

import (
	"os"
	"strings"
)

// A plain shell can act as an ephemeral peer -- an inbox opened with
// `pigeon listen` -- so that its sends and publishes carry a real reply address
// instead of the anonymous shell:user@host stamp. The identity is spelled three
// symmetric ways, resolved here in one place:
//
//   - a --as <name> flag on the invocation (most explicit)
//   - the PIGEON_AS environment variable (per script or subshell)
//   - a standing `pigeon as <name>` preference in cli.json
//
// This is deliberately separate from CurrentSessionID. The monitor and the MCP
// server are always real Claude Code sessions, and an ambient shell default must
// never hijack one, so only the CLI consults this and a real session outranks
// every ambient layer below the flag.

// ephemeralIDPrefix marks a session id that belongs to a shell inbox rather than
// a Claude Code session. A real session id is a UUID, which never starts with
// this, so the prefix is an unambiguous test either way.
const ephemeralIDPrefix = "listen-"

// syntheticSessionID maps a shell's acting name to a session id. It always
// passes ValidSessionID: a valid name is [A-Za-z0-9][A-Za-z0-9._-]{0,31}, and
// the prefix only adds characters already in that class.
func syntheticSessionID(name string) string { return ephemeralIDPrefix + name }

// isEphemeralID reports whether a session id belongs to a shell inbox.
func isEphemeralID(sid string) bool { return strings.HasPrefix(sid, ephemeralIDPrefix) }

// ephemeralName recovers the bare name from a shell inbox's session id.
func ephemeralName(sid string) string { return strings.TrimPrefix(sid, ephemeralIDPrefix) }

// ActingIdentity resolves the session id a CLI invocation acts as, and where
// that came from. Precedence, highest first: an explicit --as flag, then a real
// Claude Code session, then PIGEON_AS, then the standing `pigeon as` preference,
// then nothing -- a plain shell.
//
// An unusable ambient value (a hand-edited env var or config that is not a name)
// is dropped rather than obeyed, but an unusable --as is reported: the caller
// typed it, so silently acting as someone else would be the wrong stamp on a
// real message.
func ActingIdentity(flagAs string) (sid, origin string) {
	if raw := strings.TrimSpace(flagAs); raw != "" {
		if ValidName(raw) == nil {
			return syntheticSessionID(raw), "--as"
		}
		return "", "--as is not a usable name, so it was ignored"
	}
	// A real session outranks the ambient layers: a standing default must not be
	// able to speak in a genuine session's name.
	if real := CurrentSessionID(); real != "" {
		return real, EnvSessionID
	}
	if raw := strings.TrimSpace(os.Getenv(EnvAs)); raw != "" {
		if ValidName(raw) == nil {
			return syntheticSessionID(raw), EnvAs
		}
		return "", EnvAs + " is not a usable name, so it was ignored"
	}
	if raw := readCLIConfig().As; raw != "" {
		// readCLIConfig already dropped a value that is not a valid name.
		return syntheticSessionID(raw), CLIConfigPath()
	}
	return "", "a plain shell"
}

// ActingName resolves the ephemeral name a shell acts as -- the identity it
// opens an inbox under with `pigeon listen`, and the standing preference
// `pigeon as` reports. It is the ephemeral-only view: a real Claude Code session
// is not an ephemeral inbox, so unlike ActingIdentity this does not consult one.
// Precedence: --as flag, then PIGEON_AS, then the `pigeon as` preference.
func ActingName(flagAs string) (name, origin string) {
	if raw := strings.TrimSpace(flagAs); raw != "" {
		if ValidName(raw) == nil {
			return raw, "--as"
		}
		return "", "--as is not a usable name, so it was ignored"
	}
	if raw := strings.TrimSpace(os.Getenv(EnvAs)); raw != "" {
		if ValidName(raw) == nil {
			return raw, EnvAs
		}
		return "", EnvAs + " is not a usable name, so it was ignored"
	}
	if raw := readCLIConfig().As; raw != "" {
		return raw, CLIConfigPath()
	}
	return "", "not set"
}

// ActingSender builds the `from` stamp for a CLI send or publish, honouring the
// acting identity.
//
// The reply address is the one field that must not lie. A shell claims an inbox
// identity only when that inbox is actually live -- holding its spool open,
// ready to be replied to. A standing `pigeon as` preference is therefore inert
// whenever nothing is listening: the message goes out as a plain shell with no
// reply handle, exactly as it would with no identity set at all.
func ActingSender(flagAs string) Sender {
	sid, _ := ActingIdentity(flagAs)
	// A plain shell, or a real Claude Code session: CurrentSender already
	// describes both -- it reads the real session id from the environment and
	// degrades to a shell stamp otherwise.
	if sid == "" || sid == CurrentSessionID() {
		return CurrentSender()
	}
	ns, e, err := locateSession(sid)
	if err != nil || e == nil || !e.Ephemeral || e.Status != StatusLive {
		return CurrentSender()
	}
	// The entry was written with its privacy policy already applied (WriteEntry
	// blanks a private session's cwd), so reflect it back verbatim.
	return Sender{
		Kind:      "session",
		SessionID: sid,
		Name:      e.Name,
		Cwd:       e.Cwd,
		Namespace: ns.String(),
	}
}
