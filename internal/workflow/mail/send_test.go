package mail_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/mail"
)

// smtpDelivery is one captured send: the envelope plus the DATA body.
type smtpDelivery struct {
	from string
	to   []string
	body string
}

// envelopeSMTP is a minimal relay recording the envelope and the DATA body.
type envelopeSMTP struct {
	listener   net.Listener
	deliveries chan smtpDelivery
}

func startEnvelopeSMTP(t *testing.T) (*envelopeSMTP, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &envelopeSMTP{listener: listener, deliveries: make(chan smtpDelivery, 4)}
	go f.serve()
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return f, "127.0.0.1", addr.Port
}

func (f *envelopeSMTP) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func smtpAddr(line string) string {
	_, addr, _ := strings.Cut(line, ":")
	return strings.Trim(addr, " <>")
}

func (f *envelopeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	reply(conn, "220 fake\r\n")
	var from string
	var rcpts []string
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
				f.deliveries <- smtpDelivery{from: from, to: rcpts, body: data.String()}
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
			from = smtpAddr(trimmed)
			reply(conn, "250 ok\r\n")
		case strings.HasPrefix(upper, "RCPT TO:"):
			rcpts = append(rcpts, smtpAddr(trimmed))
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

func receiveDelivery(t *testing.T, f *envelopeSMTP) smtpDelivery {
	t.Helper()
	select {
	case d := <-f.deliveries:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no DATA received")
		return smtpDelivery{}
	}
}

// Cc and Bcc join the envelope; only Bcc stays out of the headers.
func TestSMTPSenderGenericCCBCC(t *testing.T) {
	fake, host, port := startEnvelopeSMTP(t)
	sender, err := mail.NewSMTPSender(host, port, "", "", "sender@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	err = sender.Send(context.Background(), mail.Message{
		From:      "sender@example.test",
		To:        []string{"ada@example.test"},
		Cc:        []string{"grace@example.test"},
		Bcc:       []string{"hidden@example.test"},
		ReplyTo:   "reply@example.test",
		Subject:   "hello",
		Text:      "text body",
		HTML:      "<p>html body</p>",
		Date:      "Thu, 29 Oct 2026 12:00:00 +0000",
		MessageID: "<test-id@example.test>",
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	d := receiveDelivery(t, fake)
	if d.from != "sender@example.test" {
		t.Errorf("envelope from = %q", d.from)
	}
	if len(d.to) != 3 || d.to[0] != "ada@example.test" || d.to[1] != "grace@example.test" || d.to[2] != "hidden@example.test" {
		t.Errorf("envelope to = %v, want to+cc+bcc", d.to)
	}
	for _, want := range []string{
		"To: ada@example.test",
		"Cc: grace@example.test",
		"Reply-To: reply@example.test",
		"Subject: hello",
		"Date: Thu, 29 Oct 2026 12:00:00 +0000",
		"Message-ID: <test-id@example.test>",
	} {
		if !strings.Contains(d.body, want) {
			t.Errorf("body lacks %q:\n%s", want, d.body)
		}
	}
	if strings.Contains(d.body, "hidden@example.test") {
		t.Errorf("body leaks the bcc address:\n%s", d.body)
	}
	parts := decodeMultipart(t, d.body)
	if parts["text/plain"] != "text body" {
		t.Errorf("text part = %q", parts["text/plain"])
	}
	if parts["text/html"] != "<p>html body</p>" {
		t.Errorf("html part = %q", parts["text/html"])
	}
}

func TestSMTPSenderGenericTextOnly(t *testing.T) {
	fake, host, port := startEnvelopeSMTP(t)
	sender, err := mail.NewSMTPSender(host, port, "", "", "sender@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	if err := sender.Send(context.Background(), mail.Message{
		From:    "sender@example.test",
		To:      []string{"ada@example.test"},
		Subject: "text only",
		Text:    "just text",
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	d := receiveDelivery(t, fake)
	parts := decodeMultipart(t, d.body)
	if parts["text/plain"] != "just text" {
		t.Errorf("text part = %q", parts["text/plain"])
	}
	if _, present := parts["text/html"]; present {
		t.Errorf("html part present, want text only:\n%s", d.body)
	}
}

func TestSMTPSenderGenericHTMLOnly(t *testing.T) {
	fake, host, port := startEnvelopeSMTP(t)
	sender, err := mail.NewSMTPSender(host, port, "", "", "sender@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	if err := sender.Send(context.Background(), mail.Message{
		From:    "sender@example.test",
		To:      []string{"ada@example.test"},
		Subject: "html only",
		HTML:    "<p>just html</p>",
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	d := receiveDelivery(t, fake)
	parts := decodeMultipart(t, d.body)
	if parts["text/html"] != "<p>just html</p>" {
		t.Errorf("html part = %q", parts["text/html"])
	}
	if _, present := parts["text/plain"]; present {
		t.Errorf("text part present, want html only:\n%s", d.body)
	}
}

// Rendered addresses validate before dialing: the relay here is
// unreachable, so any send reaching it would fail differently.
func TestSMTPSenderGenericRejectsInvalid(t *testing.T) {
	sender, err := mail.NewSMTPSender("relay.example.test", 587, "", "", "sender@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	cases := map[string]struct {
		msg  mail.Message
		want string
	}{
		"empty from": {
			msg:  mail.Message{To: []string{"a@example.test"}, Subject: "s", Text: "t"},
			want: "mail: from is required",
		},
		"no recipients": {
			msg:  mail.Message{From: "s@example.test", Subject: "s", Text: "t"},
			want: "mail: at least one recipient is required",
		},
		"bad to": {
			msg:  mail.Message{From: "s@example.test", To: []string{"bad"}, Text: "t"},
			want: `mail: to[0] "bad" is not a valid email address`,
		},
		"crlf bcc": {
			msg:  mail.Message{From: "s@example.test", To: []string{"a@example.test"}, Bcc: []string{"b@example.test\r\nX: y"}, Text: "t"},
			want: "mail: bcc[0] contains a newline",
		},
		"crlf subject": {
			msg:  mail.Message{From: "s@example.test", To: []string{"a@example.test"}, Subject: "a\nb", Text: "t"},
			want: "mail: subject contains a newline",
		},
	}
	for name, c := range cases {
		if err := sender.Send(context.Background(), c.msg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Send() error = %v, want containing %q", name, err, c.want)
		}
	}
}

func TestLogSenderGenericSend(t *testing.T) {
	if err := (mail.LogSender{}).Send(context.Background(), mail.Message{
		From: "s@example.test", To: []string{"a@example.test"}, Subject: "s", Text: "secret body",
	}); err != nil {
		t.Errorf("log Send() error = %v", err)
	}
}

func TestNewMessageSender(t *testing.T) {
	log, err := mail.NewMessageSender("log", 587, "", "", "", true)
	if err != nil {
		t.Fatalf("NewMessageSender(log) error = %v", err)
	}
	if _, ok := log.(mail.LogSender); !ok {
		t.Errorf("NewMessageSender(log) = %T, want mail.LogSender", log)
	}
	smtp, err := mail.NewMessageSender("relay.example.test", 587, "", "", "s@example.test", true)
	if err != nil {
		t.Fatalf("NewMessageSender(relay) error = %v", err)
	}
	if _, ok := smtp.(*mail.SMTPSender); !ok {
		t.Errorf("NewMessageSender(relay) = %T, want *mail.SMTPSender", smtp)
	}
	if _, err := mail.NewMessageSender("relay.example.test", 0, "", "", "s@example.test", true); err == nil {
		t.Error("NewMessageSender(bad port) error = nil, want an error")
	}
}
