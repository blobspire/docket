package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-message/charset"

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
	c.Timeout = 5 * time.Minute // per command; body fetches come in bounded batches
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
	// Choose before downloading: dates and sizes first, then bodies for the
	// newest max only. Exchange's UIDs aren't in date order, so "newest"
	// is by the server's arrival date.
	metas, err := c.metadata(folder, keep)
	if err != nil {
		return out, err
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].date.Before(metas[j].date) })
	if max > 0 && len(metas) > max {
		out.Dropped = len(metas) - max
		metas = metas[len(metas)-max:]
	}
	msgs, err := c.bodies(folder, st.UidValidity, metas)
	if err != nil {
		return out, err
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Date.Before(msgs[j].Date) })
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

type meta struct {
	uid  uint32
	date time.Time
	size uint32
	seen bool
}

// metadata reads arrival dates, sizes and flags, 500 messages per command.
func (c *Conn) metadata(folder string, uids []uint32) ([]meta, error) {
	var out []meta
	for i := 0; i < len(uids); i += 500 {
		j := i + 500
		if j > len(uids) {
			j = len(uids)
		}
		set := new(imap.SeqSet)
		set.AddNum(uids[i:j]...)
		if err := c.each(set, []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchFlags}, func(m *imap.Message) {
			mt := meta{uid: m.Uid, date: m.InternalDate, size: m.Size}
			for _, f := range m.Flags {
				if f == imap.SeenFlag {
					mt.seen = true
				}
			}
			out = append(out, mt)
		}); err != nil {
			return nil, fmt.Errorf("%s: read %s: %w", c.acct.Name, folder, err)
		}
	}
	return out, nil
}

// Body fetches come in batches of at most bodyBatch messages or
// bodyBatchBytes, each its own command, so every batch gets its own
// deadline and a big backlog can't run one command past it.
const (
	bodyBatch      = 25
	bodyBatchBytes = 20 << 20
)

// bodies fetches full messages for ordinary sizes and the text part plus
// the structure for very large ones. Parsing happens after a batch has
// arrived, never while the server is still sending.
func (c *Conn) bodies(folder string, validity uint32, metas []meta) ([]*Message, error) {
	var out []*Message
	var batch []meta
	var batchBytes uint32
	var big []meta
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		set := new(imap.SeqSet)
		by := map[uint32]meta{}
		for _, m := range batch {
			set.AddNum(m.uid)
			by[m.uid] = m
		}
		raws := map[uint32][]byte{}
		sec := &imap.BodySectionName{Peek: true}
		if err := c.each(set, []imap.FetchItem{imap.FetchUid, sec.FetchItem()}, func(m *imap.Message) {
			if lit := m.GetBody(sec); lit != nil {
				if b, err := io.ReadAll(lit); err == nil {
					raws[m.Uid] = b
				}
			}
		}); err != nil {
			return fmt.Errorf("%s: fetch %s: %w", c.acct.Name, folder, err)
		}
		for uid, raw := range raws {
			msg, _ := Parse(raw)
			if msg == nil {
				continue
			}
			out = append(out, finish(msg, folder, validity, by[uid]))
		}
		batch, batchBytes = nil, 0
		return nil
	}
	for _, m := range metas {
		if m.size > maxBody {
			big = append(big, m)
			continue
		}
		if len(batch) > 0 && (len(batch) >= bodyBatch || batchBytes+m.size > bodyBatchBytes) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		batch = append(batch, m)
		batchBytes += m.size
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for _, m := range big {
		msg, err := c.bigMessage(folder, m)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			out = append(out, finish(msg, folder, validity, m))
		}
	}
	return out, nil
}

func finish(msg *Message, folder string, validity uint32, m meta) *Message {
	msg.Folder, msg.UID, msg.UIDValidity = folder, m.uid, validity
	msg.Size, msg.Seen = int(m.size), m.seen
	if msg.Date.IsZero() {
		msg.Date = m.date
	}
	return msg
}

// bigMessage reads a message too large to download whole: its headers, its
// structure (for the attachments), and the start of its first text part,
// decoded with that part's own encoding and charset.
func (c *Conn) bigMessage(folder string, m meta) (*Message, error) {
	set := new(imap.SeqSet)
	set.AddNum(m.uid)
	hdr := &imap.BodySectionName{Peek: true, BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier}}
	var header []byte
	var bs *imap.BodyStructure
	if err := c.each(set, []imap.FetchItem{imap.FetchUid, imap.FetchBodyStructure, hdr.FetchItem()}, func(im *imap.Message) {
		if lit := im.GetBody(hdr); lit != nil {
			header, _ = io.ReadAll(lit)
		}
		bs = im.BodyStructure
	}); err != nil {
		return nil, fmt.Errorf("%s: fetch a large message in %s: %w", c.acct.Name, folder, err)
	}
	if header == nil {
		return nil, nil
	}
	msg, _ := Parse(append(stripCT(header), "Content-Type: text/plain; charset=utf-8\r\n\r\n"...))
	if msg == nil {
		return nil, nil
	}
	msg.Text, msg.HTML, msg.Attachments = "", "", nil
	if bs == nil {
		msg.Text = "(This message is too large for docket to show; open it in your mail.)"
		return msg, nil
	}
	var textPath, htmlPath []int
	var textPart, htmlPart *imap.BodyStructure
	bs.Walk(func(path []int, part *imap.BodyStructure) bool {
		if len(part.Parts) > 0 {
			return true
		}
		mt := strings.ToLower(part.MIMEType + "/" + part.MIMESubType)
		attached := strings.EqualFold(part.Disposition, "attachment")
		name := part.DispositionParams["filename"]
		if name == "" {
			name = part.Params["name"]
		}
		switch {
		case !attached && mt == "text/plain" && textPart == nil:
			textPath, textPart = append([]int{}, path...), part
		case !attached && mt == "text/html" && htmlPart == nil:
			htmlPath, htmlPart = append([]int{}, path...), part
		case attached || name != "" || !strings.EqualFold(part.MIMEType, "text"):
			if name == "" {
				name = "attachment"
			}
			msg.Attachments = append(msg.Attachments, Attachment{Name: name, Type: mt, Size: int(part.Size)})
		}
		return true
	})
	path, part := textPath, textPart
	if part == nil {
		path, part = htmlPath, htmlPart
	}
	if part == nil {
		msg.Text = "(This message is too large for docket to show; open it in your mail.)"
		return msg, nil
	}
	sec := &imap.BodySectionName{Peek: true, BodyPartName: imap.BodyPartName{Path: path}, Partial: []int{0, 2 * maxText}}
	var raw []byte
	if err := c.each(set, []imap.FetchItem{imap.FetchUid, sec.FetchItem()}, func(im *imap.Message) {
		if lit := im.GetBody(sec); lit != nil {
			raw, _ = io.ReadAll(lit)
		}
	}); err != nil {
		return nil, fmt.Errorf("%s: fetch a large message in %s: %w", c.acct.Name, folder, err)
	}
	text := decodePart(raw, part.Encoding, part.Params["charset"])
	if strings.EqualFold(part.MIMESubType, "html") {
		msg.HTML = clip(text, maxHTML)
		text = HTMLText(text)
	}
	msg.Text, _ = StripInvisible(clip(strings.TrimSpace(text), maxText))
	return msg, nil
}

// decodePart decodes the start of a part body (possibly cut mid-way).
func decodePart(raw []byte, encoding, cs string) string {
	var r io.Reader = bytes.NewReader(raw)
	switch strings.ToLower(encoding) {
	case "base64":
		clean := make([]byte, 0, len(raw))
		for _, b := range raw {
			if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
				clean = append(clean, b)
			}
		}
		clean = clean[:len(clean)/4*4] // a cut-off fetch ends mid-quantum
		dec := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, _ := base64.StdEncoding.Decode(dec, clean)
		r = bytes.NewReader(dec[:n])
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}
	if cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		if cr, err := charset.Reader(cs, r); err == nil {
			r = cr
		}
	}
	b, _ := io.ReadAll(io.LimitReader(r, 4*maxText))
	return strings.ToValidUTF8(string(b), "")
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
