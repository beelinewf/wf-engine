package mail_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/mail"
)

// decodeMultipart parses a captured DATA body into media type to decoded
// text. It fails the test unless the body is multipart/alternative with
// quoted-printable parts.
func decodeMultipart(t *testing.T, body string) map[string]string {
	t.Helper()
	head, msg, ok := strings.Cut(body, "\r\n\r\n")
	if !ok {
		head, msg, ok = strings.Cut(body, "\n\n")
	}
	if !ok {
		t.Fatalf("no header/body split:\n%s", body)
	}
	var contentType string
	for line := range strings.Lines(head) {
		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-type:"); ok {
			contentType = strings.TrimSpace(v)
		}
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q, want multipart/alternative with a boundary", contentType)
	}
	parts := map[string]string{}
	reader := multipart.NewReader(strings.NewReader(msg), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		raw, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		decoded := raw
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			decoded, err = io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(raw))))
			if err != nil {
				t.Fatalf("decode part: %v", err)
			}
		}
		mt, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("part Content-Type: %v", err)
		}
		parts[mt] = string(decoded)
	}
}

func TestNewSenderSelectsLog(t *testing.T) {
	sender, err := mail.NewSender("log", 587, "", "", "", true, "")
	if err != nil {
		t.Fatalf("NewSender(log) error = %v", err)
	}
	// The log sender never fails: it only logs.
	if err := sender.SendVerification(context.Background(), "a@example.test", "https://app/verify-email?token=x", time.Hour); err != nil {
		t.Errorf("log SendVerification() error = %v", err)
	}
}

func TestNewSMTPSenderRejectsIncomplete(t *testing.T) {
	for name, triple := range map[string][3]any{
		"empty host": {"", 587, "n@example.test"},
		"bad port":   {"smtp", 0, "n@example.test"},
		"empty from": {"smtp", 587, ""},
	} {
		_, err := mail.NewSender(triple[0].(string), triple[1].(int), "", "", triple[2].(string), true, "")
		if err == nil {
			t.Errorf("%s: error = nil, want an error", name)
		}
	}
}

// fakeSMTP is a minimal in-test relay: EHLO, MAIL, RCPT, DATA, QUIT over
// plaintext, recording the DATA body. Anything beyond that contract fails
// the delivery under test.
type fakeSMTP struct {
	listener net.Listener
	body     chan string
}

func startFakeSMTP(t *testing.T) (*fakeSMTP, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{listener: listener, body: make(chan string, 1)}
	go f.serve()
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return f, "127.0.0.1", addr.Port
}

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

// reply writes one SMTP reply; a failed test connection fails the delivery
// under test, so the write error needs no other handling.
func reply(conn net.Conn, format string, args ...any) {
	_, _ = fmt.Fprintf(conn, format, args...)
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	reply(conn, "220 fake\r\n")
	var data strings.Builder
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if inData {
			if trimmed == "." {
				inData = false
				f.body <- data.String()
				reply(conn, "250 ok\r\n")
				continue
			}
			data.WriteString(line)
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			reply(conn, "250-fake\r\n250 HELP\r\n")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			reply(conn, "250 ok\r\n")
		case strings.HasPrefix(upper, "RCPT TO:"):
			reply(conn, "250 ok\r\n")
		case upper == "DATA":
			inData = true
			reply(conn, "354 end with .\r\n")
		case upper == "QUIT":
			reply(conn, "221 bye\r\n")
			return
		default:
			reply(conn, "502 unimplemented\r\n")
		}
	}
}

// The default templates render as multipart/alternative: a text part
// mirroring the HTML part, the link in both, and a humanized expiry.
// The &-bearing link pins HTML auto-escaping (raw in text, escaped in
// HTML); quoted-printable decoding in the helper pins an encoding that
// survives SMTP's line limits.
func TestSMTPSenderDeliversMultipart(t *testing.T) {
	fake, host, port := startFakeSMTP(t)
	sender, err := mail.NewSender(host, port, "", "", "noreply@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	link := "https://app.example.test/verify-email?token=abc&x=1"
	if err := sender.SendVerification(context.Background(), "ada@example.test", link, 24*time.Hour); err != nil {
		t.Fatalf("SendVerification() error = %v", err)
	}
	select {
	case body := <-fake.body:
		if !strings.Contains(body, "ada@example.test") || !strings.Contains(body, "noreply@example.test") {
			t.Fatalf("body lacks to/from:\n%s", body)
		}
		parts := decodeMultipart(t, body)
		text, ok := parts["text/plain"]
		if !ok {
			t.Fatalf("no text/plain part:\n%s", body)
		}
		html, ok := parts["text/html"]
		if !ok {
			t.Fatalf("no text/html part:\n%s", body)
		}
		if !strings.Contains(text, link) {
			t.Errorf("text part lacks the raw link:\n%s", text)
		}
		if !strings.Contains(html, "token=abc&amp;x=1") {
			t.Errorf("html part lacks the escaped link:\n%s", html)
		}
		if !strings.Contains(text, "24 hours") || !strings.Contains(html, "24 hours") {
			t.Errorf("parts lack the humanized expiry:\ntext:\n%s\nhtml:\n%s", text, html)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no DATA received")
	}
}

// A relay that does not advertise STARTTLS cannot carry a TLS-required
// send: the delivery fails rather than downgrading to plaintext.
func TestSMTPSenderRefusesPlaintextDowngrade(t *testing.T) {
	_, host, port := startFakeSMTP(t)
	sender, err := mail.NewSender(host, port, "", "", "noreply@example.test", true, "")
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if err := sender.SendVerification(context.Background(), "ada@example.test", "https://x/y", time.Hour); err == nil {
		t.Error("SendVerification() error = nil, want a STARTTLS refusal")
	}
}

// A recipient or link smuggling CR/LF would inject SMTP headers or a
// second recipient: the send refuses before dialing.
func TestSMTPSenderRejectsHeaderInjection(t *testing.T) {
	sender, err := mail.NewSender("smtp.example.test", 587, "", "", "noreply@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	cases := map[string][2]string{
		"crlf recipient": {"ada@example.test\r\nBcc: mallory@example.test", "https://x/y"},
		"cr recipient":   {"ada@example.test\rmallory@example.test", "https://x/y"},
		"lf link":        {"ada@example.test", "https://x/y\nBcc: mallory@example.test"},
	}
	for name, c := range cases {
		if err := sender.SendVerification(context.Background(), c[0], c[1], time.Hour); err == nil {
			t.Errorf("%s: SendVerification() error = nil, want rejection", name)
		}
	}
}

// A template dir replaces both default parts: the override renders with
// the same .Link/.ExpiresIn data.
func TestSMTPSenderTemplateDirOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/verify_email.html", "<p>custom {{.Link}} {{.ExpiresIn}}</p>")
	writeFile(t, dir+"/verify_email.txt", "custom {{.Link}} {{.ExpiresIn}}")
	fake, host, port := startFakeSMTP(t)
	sender, err := mail.NewSender(host, port, "", "", "noreply@example.test", false, dir)
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	link := "https://app.example.test/verify-email?token=abc"
	if err := sender.SendVerification(context.Background(), "ada@example.test", link, 90*time.Minute); err != nil {
		t.Fatalf("SendVerification() error = %v", err)
	}
	select {
	case body := <-fake.body:
		parts := decodeMultipart(t, body)
		if !strings.Contains(parts["text/plain"], "custom "+link+" 90 minutes") {
			t.Errorf("text part lacks the override render:\n%s", parts["text/plain"])
		}
		if !strings.Contains(parts["text/html"], "custom "+link+" 90 minutes") {
			t.Errorf("html part lacks the override render:\n%s", parts["text/html"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no DATA received")
	}
}

// A template dir is all-or-nothing at construction: a missing file or a
// broken template fails the boot, never the first registration.
func TestNewSenderTemplateDirRejectsBad(t *testing.T) {
	empty := t.TempDir()

	half := t.TempDir()
	writeFile(t, half+"/verify_email.html", "<p>{{.Link}}</p>")

	broken := t.TempDir()
	writeFile(t, broken+"/verify_email.html", "<p>{{.Link}}</p>")
	writeFile(t, broken+"/verify_email.txt", "x {{.Bogus")

	unknown := t.TempDir()
	writeFile(t, unknown+"/verify_email.html", "<p>{{.Link}}</p>")
	writeFile(t, unknown+"/verify_email.txt", "x {{.Bogus}}")

	for name, dir := range map[string]string{
		"missing dir":   "/nonexistent-template-dir",
		"missing both":  empty,
		"missing txt":   half,
		"broken txt":    broken,
		"unknown field": unknown,
	} {
		if _, err := mail.NewSender("smtp.example.test", 587, "", "", "noreply@example.test", false, dir); err == nil {
			t.Errorf("%s: NewSender() error = nil, want an error", name)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSMTPSenderRejectsEmptyRecipientOrLink(t *testing.T) {
	sender, err := mail.NewSender("smtp.example.test", 587, "", "", "noreply@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSender() error = %v", err)
	}
	if err := sender.SendVerification(context.Background(), "", "https://x/y", time.Hour); err == nil {
		t.Error("empty recipient: error = nil")
	}
	if err := sender.SendVerification(context.Background(), "a@b.c", "", time.Hour); err == nil {
		t.Error("empty link: error = nil")
	}
}
