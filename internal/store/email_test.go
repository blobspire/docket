package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/mail"
)

func me(a string) bool { return a == "me@example.com" }

func TestEmailThreadsConvergeWhateverOrderMessagesArrive(t *testing.T) {
	s := open(t)
	now := time.Now()
	// The reply arrives before the original it answers.
	reply := &mail.Message{Folder: "INBOX", UID: 2, UIDValidity: 1, MessageID: "b@x", InReplyTo: []string{"a@x"}, References: []string{"a@x"},
		From: mail.Addr{Email: "nancy@x.org"}, Subject: "Re: Undo", Date: now}
	orig := &mail.Message{Folder: "Sent", UID: 7, UIDValidity: 1, MessageID: "a@x", From: mail.Addr{Email: "me@example.com"}, Subject: "Undo", Date: now.Add(-time.Hour)}
	third := &mail.Message{Folder: "INBOX", UID: 3, UIDValidity: 1, MessageID: "c@x", InReplyTo: []string{"b@x"}, References: []string{"b@x"},
		From: mail.Addr{Email: "nancy@x.org"}, Subject: "Re: Undo", Date: now.Add(time.Minute)}
	if _, err := s.AddEmail("gmail", []*mail.Message{reply}, me); err != nil {
		t.Fatal(err)
	}
	added, err := s.AddEmail("gmail", []*mail.Message{orig, third}, me)
	if err != nil || len(added) != 2 {
		t.Fatalf("added %d %v", len(added), err)
	}
	if added[0].Thread != "a@x" || added[1].Thread != "a@x" || !added[0].FromMe || added[1].FromMe {
		t.Fatalf("threads: %+v", added)
	}
	msgs, _ := s.ThreadMessages("gmail", "a@x")
	if len(msgs) != 3 || msgs[0].MessageID != "a@x" || msgs[2].MessageID != "c@x" {
		t.Fatalf("thread order: %+v", msgs)
	}
	// Storing the same message again adds nothing.
	again, _ := s.AddEmail("gmail", []*mail.Message{reply}, me)
	if len(again) != 0 {
		t.Fatalf("re-added %d", len(again))
	}
	// The same message in two folders is listed once.
	dup := *reply
	dup.Folder = "Important"
	s.AddEmail("gmail", []*mail.Message{&dup}, me)
	msgs, _ = s.ThreadMessages("gmail", "a@x")
	if len(msgs) != 3 {
		t.Fatalf("duplicate listed: %d", len(msgs))
	}
	// Accounts are separate.
	if m, _ := s.ThreadMessages("vt", "a@x"); len(m) != 0 {
		t.Fatal("threads leaked across accounts")
	}
}

func TestKnownSendersComeFromWhoYouWroteTo(t *testing.T) {
	s := open(t)
	s.AddContacts("vt", map[string]int64{"Advisor@VT.edu ": 100})
	if !s.Known("vt", "advisor@vt.edu") || s.Known("gmail", "advisor@vt.edu") || s.Known("vt", "stranger@x.org") {
		t.Fatal("known senders")
	}
}

func TestPruneEmailKeepsHeadersForThreading(t *testing.T) {
	s := open(t)
	s.AddEmail("gmail", []*mail.Message{{Folder: "INBOX", UID: 1, UIDValidity: 1, MessageID: "a@x", Text: "body", HTML: "<p>body</p>", Subject: "s"}}, me)
	n, err := s.PruneEmail(time.Now().Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	msgs, _ := s.ThreadMessages("gmail", "a@x")
	if len(msgs) != 1 || msgs[0].Text != "" || msgs[0].HTML != "" || msgs[0].Subject != "s" || !msgs[0].Pruned {
		t.Fatalf("after prune: %+v", msgs)
	}
}

func TestEmailCursorsAndAccountState(t *testing.T) {
	s := open(t)
	s.SetEmailCursor("gmail", "INBOX", mail.Cursor{UIDValidity: 5, LastUID: 10})
	s.SetEmailCursor("gmail", "INBOX", mail.Cursor{UIDValidity: 5, LastUID: 12})
	if c := s.EmailCursor("gmail", "INBOX"); c.LastUID != 12 || c.UIDValidity != 5 {
		t.Fatalf("cursor %+v", c)
	}
	s.EmailAccountResult("gmail", errString("login failed"))
	first := s.EmailAccountState("gmail").ErrorAt
	s.EmailAccountResult("gmail", errString("login failed again"))
	st := s.EmailAccountState("gmail")
	if st.Error != "login failed again" || st.ErrorAt != first {
		t.Fatalf("an ongoing failure keeps its start: %+v", st)
	}
	s.EmailAccountResult("gmail", nil)
	if st := s.EmailAccountState("gmail"); st.Error != "" || st.OKAt == 0 {
		t.Fatalf("recovery: %+v", st)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// They answer your reply, and their client sends only In-Reply-To (or
// References with just your message): when your message is stored after
// theirs, their reply still joins the conversation.
func TestAReplyThatArrivesBeforeItsParentJoinsTheThread(t *testing.T) {
	s := open(t)
	now := time.Now()
	a := &mail.Message{Folder: "INBOX", UID: 1, UIDValidity: 1, MessageID: "A@x", From: mail.Addr{Email: "nancy@x.org"}, Date: now.Add(-3 * time.Hour)}
	b := &mail.Message{Folder: "Sent", UID: 1, UIDValidity: 1, MessageID: "B@x", InReplyTo: []string{"A@x"}, References: []string{"A@x"},
		From: mail.Addr{Email: "me@example.com"}, Date: now.Add(-2 * time.Hour)}
	c := &mail.Message{Folder: "INBOX", UID: 2, UIDValidity: 1, MessageID: "C@x", InReplyTo: []string{"B@x"},
		From: mail.Addr{Email: "nancy@x.org"}, Date: now.Add(-time.Hour)}
	s.AddEmail("gmail", []*mail.Message{a}, me)
	got, _ := s.AddEmail("gmail", []*mail.Message{c, b}, me)
	if len(got) != 2 || got[0].Thread != "A@x" || got[1].Thread != "A@x" {
		t.Fatalf("returned threads: %+v", got)
	}
	msgs, _ := s.ThreadMessages("gmail", "A@x")
	if len(msgs) != 3 {
		t.Fatalf("thread A holds %d messages, want 3", len(msgs))
	}
}

// A message with a huge References list (bypassing Parse's cap) still
// stores, with everything else in its batch.
func TestAHugeReferencesListNeverBlocksStoring(t *testing.T) {
	s := open(t)
	refs := make([]string, 40000)
	for i := range refs {
		refs[i] = fmt.Sprintf("r%d@x", i)
	}
	got, err := s.AddEmail("gmail", []*mail.Message{
		{Folder: "INBOX", UID: 1, UIDValidity: 1, MessageID: "spam@x", References: refs, Date: time.Now()},
		{Folder: "INBOX", UID: 2, UIDValidity: 1, MessageID: "real@x", Date: time.Now()},
	}, me)
	if err != nil || len(got) != 2 {
		t.Fatalf("stored %d: %v", len(got), err)
	}
}

// The cross-pass case: their reply was triaged (with a reply item and a
// draft) before your message linking it to the conversation was read. A
// thread holding state never moves: the reply item, its draft and anything
// on its way out stay exactly where they are.
func TestATriagedEarlyReplyStaysItsOwnThread(t *testing.T) {
	s := open(t)
	now := time.Now()
	inbound := func(f string) bool { return f == "INBOX" }
	a := &mail.Message{Folder: "INBOX", UID: 1, UIDValidity: 1, MessageID: "A@x", From: mail.Addr{Email: "nancy@x.org"}, Subject: "Undo?", Date: now.Add(-3 * time.Hour)}
	added, _ := s.AddEmail("gmail", []*mail.Message{a}, me)
	s.NoteMessages("gmail", added, inbound)
	c := &mail.Message{Folder: "INBOX", UID: 2, UIDValidity: 1, MessageID: "C@x", InReplyTo: []string{"B@x"}, From: mail.Addr{Email: "nancy@x.org"},
		Subject: "Re: Undo?", Date: now.Add(-time.Hour)}
	added, _ = s.AddEmail("gmail", []*mail.Message{c}, me)
	s.NoteMessages("gmail", added, inbound)
	early := ThreadID("gmail", "B@x")
	s.SetVerdict(early, "p2", Verdict{Verdict: "reply", Line: "Reply to Nancy"})
	s.SetThreadItem(early, "dk-item")
	s.AddDraft(Draft{ThreadID: early, HTML: "<p>draft</p>"})

	b := &mail.Message{Folder: "Sent", UID: 1, UIDValidity: 1, MessageID: "B@x", InReplyTo: []string{"A@x"}, References: []string{"A@x"},
		From: mail.Addr{Email: "me@example.com"}, Subject: "Re: Undo?", Date: now.Add(-2 * time.Hour)}
	added, _ = s.AddEmail("gmail", []*mail.Message{b}, me)
	s.NoteMessages("gmail", added, inbound)

	th, ok := s.Thread(early)
	if !ok || th.ItemID != "dk-item" {
		t.Fatalf("the triaged early thread must stay: %+v", th)
	}
	if d, ok := s.LatestDraft(early); !ok || d.HTML != "<p>draft</p>" {
		t.Fatal("its draft must stay with it")
	}
	if msgs, _ := s.ThreadMessages("gmail", "B@x"); len(msgs) != 1 || msgs[0].MessageID != "C@x" {
		t.Fatalf("its message must stay with it: %+v", msgs)
	}
}

// An early reply nobody has triaged yet does join its conversation, and
// its pending triage goes with it.
func TestAnUntriagedEarlyReplyJoinsItsConversation(t *testing.T) {
	s := open(t)
	now := time.Now()
	inbound := func(f string) bool { return f == "INBOX" }
	a := &mail.Message{Folder: "INBOX", UID: 1, UIDValidity: 1, MessageID: "A@x", From: mail.Addr{Email: "nancy@x.org"}, Subject: "Undo?", Date: now.Add(-3 * time.Hour)}
	added, _ := s.AddEmail("gmail", []*mail.Message{a}, me)
	s.NoteMessages("gmail", added, inbound)
	s.SetVerdict(ThreadID("gmail", "A@x"), "p1", Verdict{Verdict: "digest", Line: "Nancy asked"})
	c := &mail.Message{Folder: "INBOX", UID: 2, UIDValidity: 1, MessageID: "C@x", InReplyTo: []string{"B@x"}, From: mail.Addr{Email: "nancy@x.org"},
		Subject: "Re: Undo?", Date: now.Add(-time.Hour)}
	added, _ = s.AddEmail("gmail", []*mail.Message{c}, me)
	s.NoteMessages("gmail", added, inbound)
	b := &mail.Message{Folder: "Sent", UID: 1, UIDValidity: 1, MessageID: "B@x", InReplyTo: []string{"A@x"}, References: []string{"A@x"},
		From: mail.Addr{Email: "me@example.com"}, Subject: "Re: Undo?", Date: now.Add(-2 * time.Hour)}
	added, _ = s.AddEmail("gmail", []*mail.Message{b}, me)
	s.NoteMessages("gmail", added, inbound)
	if _, ok := s.Thread(ThreadID("gmail", "B@x")); ok {
		t.Fatal("the untriaged early row should merge away")
	}
	th, _ := s.Thread(ThreadID("gmail", "A@x"))
	if th.InboundAt != c.Date.Unix() || !th.NeedsTriage {
		t.Fatalf("merged row: %+v", th)
	}
	if msgs, _ := s.ThreadMessages("gmail", "A@x"); len(msgs) != 3 {
		t.Fatalf("messages: %d", len(msgs))
	}
}

// A database from before a column existed gets it on open.
func TestEmailColumnsAreMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE email_outbox (id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, item_id TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL,
		kind TEXT NOT NULL, hold_until INTEGER NOT NULL, status TEXT NOT NULL, message_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, sent_at INTEGER NOT NULL DEFAULT 0)`)
	old.Exec(`CREATE TABLE email_threads (id TEXT PRIMARY KEY, account TEXT NOT NULL, thread TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '',
		from_name TEXT NOT NULL DEFAULT '', from_addr TEXT NOT NULL DEFAULT '', last_inbound TEXT NOT NULL DEFAULT '', inbound_at INTEGER NOT NULL DEFAULT 0,
		replied_at INTEGER NOT NULL DEFAULT 0, verdict TEXT NOT NULL DEFAULT '', line TEXT NOT NULL DEFAULT '', why TEXT NOT NULL DEFAULT '',
		project TEXT NOT NULL DEFAULT '', work TEXT NOT NULL DEFAULT '', first_time INTEGER NOT NULL DEFAULT 0, needs_triage INTEGER NOT NULL DEFAULT 0,
		triaged_at INTEGER NOT NULL DEFAULT 0, pass_id TEXT NOT NULL DEFAULT '', item_id TEXT NOT NULL DEFAULT '', seen_at INTEGER NOT NULL DEFAULT 0,
		read_at INTEGER NOT NULL DEFAULT 0, until_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE (account, thread))`)
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.HoldSend(Outgoing{ID: "o1", ThreadID: "t", Version: 1, Kind: "reply", Answers: "m", HoldUntil: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ThreadsToTriage(10); err != nil {
		t.Fatal(err)
	}
}

// "They wrote again" counts new messages in the inbound folders only, by
// arrival: a copy of a message already seen doesn't count.
func TestWroteSinceIgnoresCopiesAndOtherFolders(t *testing.T) {
	s := open(t)
	inbound := []string{"INBOX", "Important"}
	s.AddEmail("gmail", []*mail.Message{{Folder: "INBOX", UID: 1, UIDValidity: 1, MessageID: "m1@x", From: mail.Addr{Email: "n@x.org"}, Date: time.Now()}}, me)
	basis := s.LastMessageRow()
	// The same message shows up in another watched folder: not new.
	s.AddEmail("gmail", []*mail.Message{{Folder: "Important", UID: 9, UIDValidity: 1, MessageID: "m1@x", From: mail.Addr{Email: "n@x.org"}, Date: time.Now()}}, me)
	if s.WroteSince("gmail", "m1@x", basis, inbound) {
		t.Fatal("a copy of a seen message counted as new")
	}
	// A message from an address that isn't you, in Sent: not to you.
	s.AddEmail("gmail", []*mail.Message{{Folder: "Sent", UID: 3, UIDValidity: 1, MessageID: "s1@x", InReplyTo: []string{"m1@x"}, From: mail.Addr{Email: "alias@x.org"}, Date: time.Now()}}, me)
	if s.WroteSince("gmail", "m1@x", basis, inbound) {
		t.Fatal("a Sent message counted as them writing again")
	}
	s.AddEmail("gmail", []*mail.Message{{Folder: "INBOX", UID: 2, UIDValidity: 1, MessageID: "m2@x", InReplyTo: []string{"m1@x"}, From: mail.Addr{Email: "n@x.org"}, Date: time.Now().Add(-48 * time.Hour)}}, me)
	if !s.WroteSince("gmail", "m1@x", basis, inbound) {
		t.Fatal("a new (backdated) message must count")
	}
	if s.WroteSince("gmail", "m1@x", s.LastMessageRow(), inbound) {
		t.Fatal("nothing after the newest")
	}
}
