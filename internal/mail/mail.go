// Package mail reads and sends email for docket's email triage. Only the
// docket server uses it: no agent ever gets mail access. Reading never
// changes the mailbox (folders are opened read-only, bodies fetched with
// BODY.PEEK), and the only writes are sending a reply the human approved and
// saving a draft they asked for.
package mail

import (
	"fmt"
	"net"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
)

// Account is one [[email.account]] in the server config.
type Account struct {
	Name         string   `toml:"name"`          // short id: "gmail", "vt"
	Label        string   `toml:"label"`         // tag shown on rows (default: name)
	Address      string   `toml:"address"`       // your address on this account
	Aliases      []string `toml:"aliases"`       // other addresses that are you
	DisplayName  string   `toml:"display_name"`  // the name on mail you send
	IMAP         string   `toml:"imap"`          // host:port
	SMTP         string   `toml:"smtp"`          // host:port
	User         string   `toml:"user"`          // login (default: address)
	PasswordFile string   `toml:"password_file"` // mode 0600; the password or app password
	// Security is "tls" (default: implicit TLS on 993/465, STARTTLS
	// otherwise, and never plain text) or "plain", allowed only for a
	// gateway on this machine's loopback (DavMail).
	Security     string   `toml:"security"`
	Folders      []string `toml:"folders"`       // read for triage (default INBOX)
	SentFolder   string   `toml:"sent_folder"`   // default: the folder marked \Sent
	DraftsFolder string   `toml:"drafts_folder"` // default: the folder marked \Drafts
	// Skip is a Gmail search whose matches are never triaged (default on
	// Gmail: Promotions and Social). Ignored by servers without X-GM-EXT-1.
	Skip *string `toml:"skip"`
	// SignatureHTML is an HTML signature file; images it references by a
	// relative path are sent inline. SignatureText is its plain-text twin.
	SignatureHTML string `toml:"signature_html"`
	SignatureText string `toml:"signature_text"`
	// Web is how "Open in …" links are built: "gmail", "outlook" or "".
	Web string `toml:"web"`
}

// DefaultGmailSkip keeps Gmail's Promotions and Social tabs out of triage.
const DefaultGmailSkip = "category:promotions OR category:social"

// Check validates an account and fills in defaults.
func (a *Account) Check() error {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" || strings.ContainsAny(a.Name, " :/") {
		return fmt.Errorf("every [[email.account]] needs a short name (no spaces, : or /)")
	}
	if a.Label == "" {
		a.Label = a.Name
	}
	addr, err := netmail.ParseAddress(a.Address)
	if err != nil {
		return fmt.Errorf("email account %s: address: %w", a.Name, err)
	}
	a.Address = strings.ToLower(addr.Address)
	if a.User == "" {
		a.User = a.Address
	}
	if a.IMAP == "" || a.SMTP == "" {
		return fmt.Errorf("email account %s needs imap and smtp (host:port)", a.Name)
	}
	for _, hp := range []string{a.IMAP, a.SMTP} {
		if _, _, err := net.SplitHostPort(hp); err != nil {
			return fmt.Errorf("email account %s: %q is not host:port", a.Name, hp)
		}
	}
	switch a.Security {
	case "", "tls":
		a.Security = "tls"
	case "plain":
		for _, hp := range []string{a.IMAP, a.SMTP} {
			if !Loopback(hp) {
				return fmt.Errorf("email account %s: security = \"plain\" is only allowed for a gateway on this machine (127.0.0.1), not %s", a.Name, hp)
			}
		}
	default:
		return fmt.Errorf("email account %s: security must be \"tls\" or \"plain\"", a.Name)
	}
	if a.PasswordFile == "" {
		return fmt.Errorf("email account %s needs password_file", a.Name)
	}
	a.PasswordFile = expand(a.PasswordFile)
	a.SignatureHTML = expand(a.SignatureHTML)
	a.SignatureText = expand(a.SignatureText)
	if len(a.Folders) == 0 {
		a.Folders = []string{"INBOX"}
	}
	switch a.Web {
	case "":
		if a.Gmail() {
			a.Web = "gmail"
		}
	case "gmail", "outlook":
	default:
		return fmt.Errorf("email account %s: web must be \"gmail\" or \"outlook\"", a.Name)
	}
	return nil
}

// Gmail reports whether the account is on Google's servers.
func (a Account) Gmail() bool {
	h, _, _ := net.SplitHostPort(a.IMAP)
	h = strings.ToLower(h)
	return h == "imap.gmail.com" || h == "imap.googlemail.com"
}

// SkipQuery is the Gmail search for mail never triaged ("" for none).
func (a Account) SkipQuery() string {
	if a.Skip != nil {
		return strings.TrimSpace(*a.Skip)
	}
	if a.Gmail() {
		return DefaultGmailSkip
	}
	return ""
}

// Password reads the password file. It must not be readable by others.
func (a Account) Password() (string, error) {
	fi, err := os.Stat(a.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("email account %s: password_file: %w", a.Name, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("email account %s: %s is readable by other users; run chmod 600 on it", a.Name, a.PasswordFile)
	}
	b, err := os.ReadFile(a.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("email account %s: password_file: %w", a.Name, err)
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return "", fmt.Errorf("email account %s: %s is empty", a.Name, a.PasswordFile)
	}
	return p, nil
}

// Me reports whether addr is one of your addresses on this account.
func (a Account) Me(addr string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return false
	}
	if addr == a.Address {
		return true
	}
	for _, x := range a.Aliases {
		if strings.EqualFold(strings.TrimSpace(x), addr) {
			return true
		}
	}
	return false
}

// From is the address mail from this account is sent as.
func (a Account) From() Addr { return Addr{Name: a.DisplayName, Email: a.Address} }

// Loopback reports whether host:port is on this machine.
func Loopback(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Addr is one mailbox: a display name and an address.
type Addr struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// String formats the address for a header, encoding the name if needed.
func (a Addr) String() string {
	return (&netmail.Address{Name: a.Name, Address: a.Email}).String()
}

// Display is the name if there is one, else the address.
func (a Addr) Display() string {
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	return a.Email
}

// ParseAddrs parses a comma-separated address list, skipping bad entries.
func ParseAddrs(s string) []Addr {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if list, err := netmail.ParseAddressList(s); err == nil {
		out := make([]Addr, 0, len(list))
		for _, a := range list {
			out = append(out, Addr{Name: a.Name, Email: strings.ToLower(a.Address)})
		}
		return out
	}
	var out []Addr
	for _, part := range strings.Split(s, ",") {
		if a, err := netmail.ParseAddress(strings.TrimSpace(part)); err == nil {
			out = append(out, Addr{Name: a.Name, Email: strings.ToLower(a.Address)})
		}
	}
	return out
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
