package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
)

// Cursor is how far docket has read a folder.
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// Conn is a logged-in IMAP connection for one account.
type Conn struct {
	acct  Account
	c     *client.Client
	gmail bool
}

// maxBody is the largest message whose full body docket fetches; bigger
// ones (big attachments) get their headers and first text part only.
const maxBody = 15 << 20

// Dial connects and logs in. The context bounds the connect and login.
func (a Account) Dial(ctx context.Context) (*Conn, error) {
	pw, err := a.Password()
	if err != nil {
		return nil, err
	}
	host, port, _ := net.SplitHostPort(a.IMAP)
	d := &net.Dialer{Timeout: 30 * time.Second}
	if dl, ok := ctx.Deadline(); ok {
		d.Deadline = dl
	}
	var c *client.Client
	switch {
	case a.Security == "plain":
		c, err = client.DialWithDialer(d, a.IMAP)
	case port == "993":
		c, err = client.DialWithDialerTLS(d, a.IMAP, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	default:
		c, err = client.DialWithDialer(d, a.IMAP)
		if err == nil {
			if ok, _ := c.SupportStartTLS(); !ok {
				c.Logout()
				return nil, fmt.Errorf("%s: %s doesn't offer STARTTLS; refusing to log in in plain text", a.Name, a.IMAP)
			}
			err = c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: connect %s: %w", a.Name, a.IMAP, err)
	}
	// DavMail answers a first login only after Microsoft's sign-in, so the
	// login gets longer than ordinary commands.
	c.Timeout = 3 * time.Minute
	if err := c.Login(a.User, pw); err != nil {
		c.Logout()
		return nil, fmt.Errorf("%s: login: %w", a.Name, err)
	}
	c.Timeout = 2 * time.Minute
	gm, _ := c.Support("X-GM-EXT-1")
	return &Conn{acct: a, c: c, gmail: gm}, nil
}

// Close logs out.
func (c *Conn) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	return c.c.Logout()
}

// Folders finds the Sent and Drafts folders (the configured ones, else the
// ones the server marks \Sent and \Drafts, else common names).
func (c *Conn) Folders() (sent, drafts string, all []string, err error) {
	ch := make(chan *imap.MailboxInfo, 50)
	done := make(chan error, 1)
	go func() { done <- c.c.List("", "*", ch) }()
	byName := map[string]bool{}
	for mb := range ch {
		all = append(all, mb.Name)
		byName[strings.ToLower(mb.Name)] = true
		for _, at := range mb.Attributes {
			switch at {
			case imap.SentAttr:
				if sent == "" {
					sent = mb.Name
				}
			case imap.DraftsAttr:
				if drafts == "" {
					drafts = mb.Name
				}
			}
		}
	}
	if err = <-done; err != nil {
		return "", "", nil, fmt.Errorf("%s: list folders: %w", c.acct.Name, err)
	}
	pick := func(names ...string) string {
		for _, n := range names {
			if byName[strings.ToLower(n)] {
				for _, a := range all {
					if strings.EqualFold(a, n) {
						return a
					}
				}
			}
		}
		return ""
	}
	if sent == "" {
		sent = pick("[Gmail]/Sent Mail", "Sent", "Sent Items", "Sent Messages", "INBOX.Sent")
	}
	if drafts == "" {
		drafts = pick("[Gmail]/Drafts", "Drafts", "INBOX.Drafts")
	}
	if c.acct.SentFolder != "" {
		sent = c.acct.SentFolder
	}
	if c.acct.DraftsFolder != "" {
		drafts = c.acct.DraftsFolder
	}
	return sent, drafts, all, nil
}

// Fetched is what one folder read returned.
type Fetched struct {
	Folder   string
	Cursor   Cursor
	Messages []*Message
	Skipped  int // matched the account's skip search (Gmail tabs)
	Dropped  int // over the limit; the oldest were left out
}

// Fetch reads a folder's new messages: everything after the cursor, or,
// on a first read (or after the server renumbered the folder), the messages
// since `since`. It opens the folder read-only and peeks at bodies, so
// nothing is marked read. With skip, messages matching the account's Gmail
// skip search are left out. At most max messages are returned (the newest).
func (c *Conn) Fetch(folder string, cur Cursor, since time.Time, skip bool, max int) (Fetched, error) {
	out := Fetched{Folder: folder, Cursor: cur}
	st, err := c.c.Select(folder, true)
	if err != nil {
		return out, fmt.Errorf("%s: open %s: %w", c.acct.Name, folder, err)
	}
	fresh := cur.UIDValidity != st.UidValidity || cur.LastUID == 0
	crit := imap.NewSearchCriteria()
	var rawBase []interface{}
	if fresh {
		crit.Since = since
		rawBase = []interface{}{imap.RawString("SINCE"), since.Format("2-Jan-2006")}
	} else {
		set := new(imap.SeqSet)
		set.AddRange(cur.LastUID+1, 0)
		crit.Uid = set
		rawBase = []interface{}{imap.RawString("UID"), set}
	}
	uids, err := c.c.UidSearch(crit)
	if err != nil {
		return out, fmt.Errorf("%s: search %s: %w", c.acct.Name, folder, err)
	}
	out.Cursor = Cursor{UIDValidity: st.UidValidity, LastUID: cur.LastUID}
	if fresh {
		out.Cursor.LastUID = 0
	}
	var keep []uint32
	for _, u := range uids {
		if fresh || u > cur.LastUID { // "n:*" always matches the last message
			keep = append(keep, u)
		}
		if u > out.Cursor.LastUID {
			out.Cursor.LastUID = u
		}
	}
	if fresh && st.UidNext > 1 && out.Cursor.LastUID < st.UidNext-1 {
		// Nothing new since `since`: start after the newest message anyway.
		out.Cursor.LastUID = st.UidNext - 1
	}
	if q := c.acct.SkipQuery(); skip && q != "" && c.gmail && len(keep) > 0 {
		skipped, err := c.rawSearch(append(rawBase, imap.RawString("X-GM-RAW"), q))
		if err != nil {
			return out, fmt.Errorf("%s: skip search: %w", c.acct.Name, err)
		}
		drop := map[uint32]bool{}
		for _, u := range skipped {
			drop[u] = true
		}
		k := keep[:0]
		for _, u := range keep {
			if drop[u] {
				out.Skipped++
			} else {
				k = append(k, u)
			}
		}
		keep = k
	}
	if len(keep) == 0 {
		return out, nil
	}
	msgs, err := c.fetchUIDs(folder, st.UidValidity, keep)
	if err != nil {
		return out, err
	}
	// Exchange's UIDs aren't in date order: sort by date, keep the newest.
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Date.Before(msgs[j].Date) })
	if max > 0 && len(msgs) > max {
		out.Dropped = len(msgs) - max
		msgs = msgs[len(msgs)-max:]
	}
	out.Messages = msgs
	return out, nil
}

func (c *Conn) rawSearch(args []interface{}) ([]uint32, error) {
	cmd := &commands.Uid{Cmd: &imap.Command{Name: "SEARCH", Arguments: args}}
	res := &responses.Search{}
	status, err := c.c.Execute(cmd, res)
	if err != nil {
		return nil, err
	}
	if err := status.Err(); err != nil {
		return nil, err
	}
	return res.Ids, nil
}

// fetchUIDs fetches size first, then full bodies for ordinary messages and
// headers plus the first text part for very large ones.
func (c *Conn) fetchUIDs(folder string, validity uint32, uids []uint32) ([]*Message, error) {
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	sizes := map[uint32]uint32{}
	flags := map[uint32][]string{}
	if err := c.each(set, []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size, imap.FetchFlags}, func(m *imap.Message) {
		sizes[m.Uid] = m.Size
		flags[m.Uid] = m.Flags
	}); err != nil {
		return nil, fmt.Errorf("%s: fetch sizes in %s: %w", c.acct.Name, folder, err)
	}
	small, big := new(imap.SeqSet), new(imap.SeqSet)
	var nSmall, nBig int
	for u, sz := range sizes {
		if sz <= maxBody {
			small.AddNum(u)
			nSmall++
		} else {
			big.AddNum(u)
			nBig++
		}
	}
	var out []*Message
	add := func(m *imap.Message, sec *imap.BodySectionName) {
		lit := m.GetBody(sec)
		if lit == nil {
			return
		}
		raw, err := io.ReadAll(lit)
		if err != nil {
			return
		}
		msg, _ := Parse(raw)
		if msg == nil {
			return
		}
		msg.Folder, msg.UID, msg.UIDValidity = folder, m.Uid, validity
		msg.Size = int(sizes[m.Uid])
		for _, f := range flags[m.Uid] {
			if f == imap.SeenFlag {
				msg.Seen = true
			}
		}
		if msg.Date.IsZero() {
			msg.Date = m.InternalDate
		}
		out = append(out, msg)
	}
	if nSmall > 0 {
		sec := &imap.BodySectionName{Peek: true}
		if err := c.each(small, []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, sec.FetchItem()}, func(m *imap.Message) { add(m, sec) }); err != nil {
			return nil, fmt.Errorf("%s: fetch %s: %w", c.acct.Name, folder, err)
		}
	}
	if nBig > 0 {
		hdr := &imap.BodySectionName{Peek: true, BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier}}
		txt := &imap.BodySectionName{Peek: true, BodyPartName: imap.BodyPartName{Path: []int{1}}, Partial: []int{0, maxText}}
		if err := c.each(big, []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, hdr.FetchItem(), txt.FetchItem()}, func(m *imap.Message) {
			h, b := m.GetBody(hdr), m.GetBody(txt)
			if h == nil {
				return
			}
			hb, _ := io.ReadAll(h)
			var tb []byte
			if b != nil {
				tb, _ = io.ReadAll(b)
			}
			m.Body = map[*imap.BodySectionName]imap.Literal{{}: bytes.NewReader(append(append(stripCT(hb), "Content-Type: text/plain; charset=utf-8\r\n\r\n"...), tb...))}
			add(m, &imap.BodySectionName{})
		}); err != nil {
			return nil, fmt.Errorf("%s: fetch large messages in %s: %w", c.acct.Name, folder, err)
		}
	}
	return out, nil
}

// stripCT drops a header block's Content-* fields and its blank line, so a
// replacement body type can follow.
func stripCT(h []byte) []byte {
	lines := strings.Split(strings.TrimRight(string(h), "\r\n"), "\r\n")
	var out []string
	skipping := false
	for _, l := range lines {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') {
			if !skipping {
				out = append(out, l)
			}
			continue
		}
		skipping = strings.HasPrefix(strings.ToLower(l), "content-")
		if !skipping {
			out = append(out, l)
		}
	}
	return []byte(strings.Join(out, "\r\n") + "\r\n")
}

func (c *Conn) each(set *imap.SeqSet, items []imap.FetchItem, fn func(*imap.Message)) error {
	ch := make(chan *imap.Message, 20)
	done := make(chan error, 1)
	go func() { done <- c.c.UidFetch(set, items, ch) }()
	for m := range ch {
		fn(m)
	}
	return <-done
}

// Append saves a message to a folder (a draft, with \Draft and \Seen).
func (c *Conn) Append(folder string, raw []byte, draft bool) error {
	if folder == "" {
		return errors.New("no folder to save to")
	}
	var flags []string
	if draft {
		flags = []string{imap.DraftFlag, imap.SeenFlag}
	}
	if err := c.c.Append(folder, flags, time.Now(), bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("%s: save to %s: %w", c.acct.Name, folder, err)
	}
	return nil
}

// Recipients lists who you wrote to from a folder (your Sent folder) since
// a date, reading envelopes only, with the latest time for each address. It
// also returns the folder's cursor, so later reads start after it.
func (c *Conn) Recipients(folder string, since time.Time) (map[string]int64, Cursor, error) {
	st, err := c.c.Select(folder, true)
	if err != nil {
		return nil, Cursor{}, fmt.Errorf("%s: open %s: %w", c.acct.Name, folder, err)
	}
	cur := Cursor{UIDValidity: st.UidValidity}
	if st.UidNext > 0 {
		cur.LastUID = st.UidNext - 1
	}
	crit := imap.NewSearchCriteria()
	crit.Since = since
	uids, err := c.c.UidSearch(crit)
	if err != nil {
		return nil, cur, fmt.Errorf("%s: search %s: %w", c.acct.Name, folder, err)
	}
	out := map[string]int64{}
	var failed error
	for i := 0; i < len(uids); i += 500 {
		j := i + 500
		if j > len(uids) {
			j = len(uids)
		}
		// A server that sends something go-imap can't parse fails the whole
		// batch (DavMail did once, on its first session): retry in small
		// batches, and skip only the batches that still fail.
		if err := c.recipients(uids[i:j], out); err != nil {
			for k := i; k < j; k += 25 {
				l := k + 25
				if l > j {
					l = j
				}
				if err := c.recipients(uids[k:l], out); err != nil {
					failed = fmt.Errorf("%s: read %s: %w", c.acct.Name, folder, err)
				}
			}
		}
	}
	for _, u := range uids {
		if u > cur.LastUID {
			cur.LastUID = u
		}
	}
	return out, cur, failed
}

func (c *Conn) recipients(uids []uint32, out map[string]int64) error {
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	return c.each(set, []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate}, func(m *imap.Message) {
		if m.Envelope == nil {
			return
		}
		at := m.InternalDate.Unix()
		for _, list := range [][]*imap.Address{m.Envelope.To, m.Envelope.Cc, m.Envelope.Bcc} {
			for _, a := range list {
				addr := strings.ToLower(a.Address())
				if addr == "" || c.acct.Me(addr) {
					continue
				}
				if at > out[addr] {
					out[addr] = at
				}
			}
		}
	})
}
