package pigeon

// Package-level shims that address CurrentNamespace(), kept here so only tests
// can reach them.
//
// They were production code once, and after namespaces landed no production
// caller wanted them: every path that does real work already holds the
// Namespace it is operating on and passes it down. Leaving them exported in the
// package proper made them a standing invitation for a future change to resolve
// a namespace implicitly, halfway down a call chain that had already resolved
// one -- which is how a session ends up reading a registry it does not belong
// to. In a _test.go file that mistake will not compile.

func SessionsDir() string { return CurrentNamespace().SessionsDir() }

func InboxDir() string { return CurrentNamespace().InboxDir() }

func PayloadsDir() string { return CurrentNamespace().PayloadsDir() }

func LocksDir() string { return CurrentNamespace().LocksDir() }

func TopicsDir() string { return CurrentNamespace().TopicsDir() }

func CursorsDir() string { return CurrentNamespace().CursorsDir() }

func entryPath(sessionID string) string { return CurrentNamespace().entryPath(sessionID) }

func LockPath(sessionID string) string { return CurrentNamespace().LockPath(sessionID) }

func WriteEntry(e *Entry) error { return CurrentNamespace().WriteEntry(e) }

func monitorListening(sessionID string) bool {
	return CurrentNamespace().monitorListening(sessionID)
}

func cursorPath(sessionID string) string { return CurrentNamespace().cursorPath(sessionID) }

func readCursors(sessionID string) map[string]int64 {
	return CurrentNamespace().readCursors(sessionID)
}

func mutateCursors(sessionID string, fn func(map[string]int64)) error {
	return CurrentNamespace().mutateCursors(sessionID, fn)
}

func TopicPath(topic string) string { return CurrentNamespace().TopicPath(topic) }

func Render(m *Message) string { return CurrentNamespace().Render(m) }
