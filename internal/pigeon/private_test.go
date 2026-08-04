package pigeon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withUserConfig points the machine config at a throwaway directory and writes
// one. Policy must never be read from, or written to, a real ~/.config.
func withUserConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "pigeon"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(UserConfigPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	resetUserConfigForTest()
	t.Cleanup(resetUserConfigForTest)
}

// insideClaude makes this process look like one Claude Code spawned, which is
// what the privacy rule keys on.
func insideClaude(t *testing.T, sid string) {
	t.Helper()
	t.Setenv(EnvSessionID, sid)
}

func ownTerminal(t *testing.T) {
	t.Helper()
	t.Setenv(EnvSessionID, "")
}

// --- policy comes from the machine, never from a checkout ------------------

func TestPrivacyIsDeclaredByTheMachineConfig(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)

	if !mustNS(t, "acme").IsPrivate() {
		t.Error("acme should be private")
	}
	if mustNS(t, "other").IsPrivate() {
		t.Error("other should not be private")
	}
}

// A project config travels with a `git clone`, so it may say which namespace a
// checkout's sessions belong to, and must not be able to say that the namespace
// is private -- that would let a cloned repository hide its own sessions from
// the person who cloned it.
func TestAProjectCannotDeclareItsNamespacePrivate(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{}`)
	dir := writeProjectConfig(t, `{"namespace": "acme", "private": true}`)

	cfg, _, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if cfg == nil || cfg.Namespace != "acme" {
		t.Fatalf("the project should still choose its namespace: %+v", cfg)
	}
	// `private` in a project config is the per-session setting, which withholds
	// that session's own cwd and description. It says nothing about namespaces.
	if mustNS(t, "acme").IsPrivate() {
		t.Error("a checkout made its own namespace private")
	}
}

func TestMalformedUserConfigIsNoPolicyRatherThanAnError(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}` /* truncated */)

	if mustNS(t, "acme").IsPrivate() {
		t.Error("a malformed config granted a policy")
	}
}

// --- invisible outside, normal inside --------------------------------------

func TestPrivateNamespaceIsHiddenFromInsideASession(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)
	acme := mustNS(t, "acme")
	liveEntryIn(t, acme, "aaaa1111", "secret", "/tmp/work")
	liveEntryIn(t, DefaultNamespace(), "bbbb2222", "beta", "/tmp/other")

	insideClaude(t, "bbbb2222")
	spaces, err := ListNamespaces()
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	for _, info := range spaces {
		if info.Name == "acme" {
			t.Error("a private namespace was listed inside a session")
		}
	}
	// And it cannot be reached by naming it either.
	if err := CheckNamespaceAccess(acme); err == nil {
		t.Error("a private namespace was addressable inside a session")
	}
}

// The escape hatch: your own terminal is not inside a session, so everything
// works there. Without this the feature would hide the namespace from you too.
func TestPrivateNamespaceIsVisibleFromYourOwnTerminal(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)
	acme := mustNS(t, "acme")
	liveEntryIn(t, acme, "aaaa1111", "secret", "/tmp/work")

	ownTerminal(t)
	if err := CheckNamespaceAccess(acme); err != nil {
		t.Errorf("the escape hatch is closed: %v", err)
	}
	spaces, err := ListNamespaces()
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	found := false
	for _, info := range spaces {
		if info.Name == "acme" {
			found = true
		}
	}
	if !found {
		t.Error("a private namespace was hidden from your own terminal")
	}
}

// Inside the namespace everything is ordinary: members see each other, and they
// can see the other namespaces around them.
func TestInsideAPrivateNamespaceEverythingIsNormal(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)
	acme := mustNS(t, "acme")
	liveEntryIn(t, acme, "aaaa1111", "one", "/tmp/a")
	liveEntryIn(t, acme, "bbbb2222", "two", "/tmp/b")
	liveEntryIn(t, DefaultNamespace(), "cccc3333", "outsider", "/tmp/c")

	insideClaude(t, "aaaa1111")
	t.Setenv(EnvNamespace, "acme")

	if err := CheckNamespaceAccess(acme); err != nil {
		t.Errorf("a member was refused its own namespace: %v", err)
	}
	peers, err := acme.ListSessions(false, false)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(peers) != 2 {
		t.Errorf("a member sees %d peers, want both", len(peers))
	}
	// The namespaces around it are not private, so they are still there.
	spaces, err := ListNamespaces()
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	var names []string
	for _, info := range spaces {
		names = append(names, info.Name)
	}
	if !contains(names, DefaultNamespaceName) || !contains(names, "acme") {
		t.Errorf("a member sees %v, want its own namespace and the public ones", names)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// --- sealed against machine-wide topics, both ways -------------------------

// A private namespace that can still broadcast to @all publishes exactly what
// it was made private to keep in.
func TestPrivateNamespaceCannotPublishToAGlobalTopic(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)
	acme := mustNS(t, "acme")

	if _, err := acme.Publish("@all", Draft{Text: "leaking"}, Sender{Kind: "shell", Name: "sh"}); err == nil {
		t.Error("a private namespace published to the machine-wide mailbox")
	}
	// Its own namespaced topics are unaffected.
	liveEntryIn(t, acme, "aaaa1111", "one", "/tmp/a")
	if _, err := acme.Publish("all", Draft{Text: "internal"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Errorf("a private namespace cannot use its own mailbox: %v", err)
	}
}

// The other direction: a broadcast from outside must not reach in.
func TestAGlobalTopicDoesNotReachIntoAPrivateNamespace(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)
	acme := mustNS(t, "acme")
	pub := DefaultNamespace()

	// Give the private session the subscription anyway, to prove the seal is
	// enforced rather than merely not offered.
	subscribed := func(ns Namespace, id, name string) {
		t.Helper()
		e := liveEntryIn(t, ns, id, name, "/tmp/"+name)
		e.Subscriptions = []string{PublicTopic, GlobalPublicTopic}
		if err := ns.WriteEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	subscribed(acme, "aaaa1111", "secret")
	subscribed(pub, "bbbb2222", "beta")

	if n := pub.SubscriberCount("@all", "bbbb2222"); n != 0 {
		t.Errorf("a publisher counts %d subscribers, want none: the private session is in the audience", n)
	}
}

// A session in a private namespace does not join @all in the first place.
func TestPrivateNamespaceDoesNotJoinTheMachineWideMailbox(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespaces": {"acme": {"private": true}}}`)

	got := strings.Join(defaultSubscriptions(mustNS(t, "acme")), ",")
	if strings.Contains(got, GlobalPublicTopic) {
		t.Errorf("default subscriptions %q include the machine-wide mailbox", got)
	}
	open := strings.Join(defaultSubscriptions(DefaultNamespace()), ",")
	if !strings.Contains(open, GlobalPublicTopic) {
		t.Errorf("a public namespace lost the machine-wide mailbox: %q", open)
	}
}

// --- writing policy --------------------------------------------------------

func TestSetNamespacePolicyPreservesTheRestOfTheFile(t *testing.T) {
	withHome(t)
	withUserConfig(t, `{"namespace": "work", "namespaces": {"acme": {"private": true}}}`)

	if err := SetNamespacePolicy(mustNS(t, "other"), NamespacePolicy{Private: true}); err != nil {
		t.Fatalf("SetNamespacePolicy: %v", err)
	}
	resetUserConfigForTest()
	c := LoadUserConfig()
	if c.Namespace != "work" {
		t.Errorf("the namespace preference was lost: %q", c.Namespace)
	}
	if !c.Namespaces["acme"].Private || !c.Namespaces["other"].Private {
		t.Errorf("policies did not survive: %+v", c.Namespaces)
	}
}
