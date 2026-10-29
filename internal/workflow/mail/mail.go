// Package mail delivers verification email for local user registration.
// It is the only mail-sending boundary in the engine: the service layer
// depends on the Sender interface, and main wires the SMTP or log
// implementation from configuration.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Sender delivers a verification link. Implementations must never log the
// recipient's secrets; the link itself is single-use and expiring, but the
// SMTP sender still keeps it out of logs.
type Sender interface {
	SendVerification(ctx context.Context, to, link string, ttl time.Duration) error
}

// logHost selects the dev/e2e sender that logs the link instead of sending
// it. It is matched exactly: any other host is an SMTP server.
const logHost = "log"

// sendTimeout bounds one SMTP delivery so a blackhole relay fails the send
// instead of hanging the request that triggered it.
const sendTimeout = 10 * time.Second

// SMTPSender delivers verification mail through an SMTP relay using only
// the standard library. With UseTLS it negotiates STARTTLS (or direct TLS
// on port 465); without it the connection stays plaintext, which suits a
// localhost relay and nothing else. An empty username sends without AUTH.
type SMTPSender struct {
	host      string
	port      int
	username  string
	password  string
	from      string
	useTLS    bool
	templates *verifyTemplates
}

// NewSMTPSender builds the SMTP sender. Validation mirrors the
// configuration rules (which cannot import this package), so keep the two
// in sync: a missing host, from, or port fails the boot here rather than
// the first registration.
func NewSMTPSender(host string, port int, username, password, from string, useTLS bool, templateDir string) (*SMTPSender, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("mail: smtp host is required")
	}
	if port <= 0 || port > 65535 {
		return nil, errors.New("mail: smtp port must be 1..65535")
	}
	if strings.TrimSpace(from) == "" {
		return nil, errors.New("mail: smtp from is required")
	}
	if !useTLS {
		switch strings.TrimSpace(host) {
		case "localhost", "127.0.0.1", "::1":
		default:
			slog.Warn("mail: smtp without TLS to a non-local relay sends verification links in cleartext", "host", strings.TrimSpace(host))
		}
	}
	templates, err := loadVerifyTemplates(templateDir)
	if err != nil {
		return nil, err
	}
	return &SMTPSender{
		host:      strings.TrimSpace(host),
		port:      port,
		username:  username,
		password:  password,
		from:      strings.TrimSpace(from),
		useTLS:    useTLS,
		templates: templates,
	}, nil
}

// SendVerification sends the verification link to one recipient.
func (s *SMTPSender) SendVerification(ctx context.Context, to, link string, ttl time.Duration) error {
	if s == nil {
		return errors.New("mail: sender is nil")
	}
	to = strings.TrimSpace(to)
	if to == "" || link == "" {
		return errors.New("mail: recipient and link are required")
	}
	// Defense in depth behind the service's address validation: these
	// values reach the SMTP envelope and headers, where a newline would
	// inject commands.
	for _, field := range []string{to, s.from, link} {
		if strings.ContainsAny(field, "\r\n") {
			return errors.New("mail: address or link contains a newline")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
	dialer := &net.Dialer{Timeout: sendTimeout}
	var conn net.Conn
	var err error
	if s.useTLS && s.port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", s.host, err)
	}
	defer func() { _ = conn.Close() }()
	// The dial timeout stops covering the connection once it is open; an
	// overall deadline keeps a stalled relay from hanging the request
	// that triggered the send.
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		return fmt.Errorf("mail: deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fmt.Errorf("mail: smtp handshake: %w", err)
	}
	defer func() { _ = client.Quit() }()
	if s.useTLS && s.port != 465 {
		// A relay that does not advertise STARTTLS cannot be used
		// securely, so the send fails rather than downgrading.
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("mail: relay %s does not advertise STARTTLS", s.host)
		}
		if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mail: starttls: %w", err)
		}
	}
	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return fmt.Errorf("mail: smtp auth: %w", err)
		}
	}
	if err := client.Mail(s.from); err != nil {
		return fmt.Errorf("mail: smtp from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("mail: smtp to: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: smtp data: %w", err)
	}
	defer func() { _ = writer.Close() }()
	html, text, err := s.templates.render(link, ttl)
	if err != nil {
		return err
	}
	var msg bytes.Buffer
	mw := multipart.NewWriter(&msg)
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: multipart/alternative; boundary=%s\r\n\r\n",
		s.from, to, subjectLine, mw.Boundary())
	// Text first, HTML second: that is the order of increasing fidelity
	// multipart/alternative promises, and quoted-printable keeps both
	// inside SMTP's line limits.
	for _, part := range []struct {
		contentType string
		body        string
	}{
		{"text/plain; charset=utf-8", text},
		{"text/html; charset=utf-8", html},
	} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return fmt.Errorf("mail: mime part: %w", err)
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(part.body)); err != nil {
			return fmt.Errorf("mail: mime encode: %w", err)
		}
		if err := qp.Close(); err != nil {
			return fmt.Errorf("mail: mime encode: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("mail: mime close: %w", err)
	}
	if _, err := writer.Write(msg.Bytes()); err != nil {
		return fmt.Errorf("mail: smtp write: %w", err)
	}
	return nil
}

// LogSender logs the verification link instead of sending it. It exists
// for development and the black-box harness, selected by smtp.host "log".
type LogSender struct{}

// SendVerification logs the link at WARN so it stands out of routine logs.
func (LogSender) SendVerification(_ context.Context, to, link string, _ time.Duration) error {
	slog.Warn("mail: verification link (log sender; not sent)", "to", to, "link", link)
	return nil
}

// NewSender builds the configured sender: the log sender for host "log",
// the SMTP sender otherwise.
func NewSender(host string, port int, username, password, from string, useTLS bool, templateDir string) (Sender, error) {
	if strings.TrimSpace(host) == logHost {
		return LogSender{}, nil
	}
	return NewSMTPSender(host, port, username, password, from, useTLS, templateDir)
}
