package store

import (
	"fmt"
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
