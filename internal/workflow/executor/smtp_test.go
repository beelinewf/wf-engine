package executor_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/mail"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// smtpDelivery is one captured send: the envelope plus the DATA body.
type smtpDelivery struct {
	from string
	to   []string
	body string
}

// fakeRelay is a minimal relay recording the envelope and the DATA body.
type fakeRelay struct {
	listener   net.Listener
	deliveries chan smtpDelivery
}

func startFakeRelay(t *testing.T) (*fakeRelay, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRelay{listener: listener, deliveries: make(chan smtpDelivery, 4)}
	go f.serve()
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return f, "127.0.0.1", addr.Port
}

func (f *fakeRelay) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRelay) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	_, _ = fmt.Fprintf(conn, "220 fake\r\n")
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
				_, _ = fmt.Fprintf(conn, "250 ok\r\n")
				continue
			}
			data.WriteString(line)
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			_, _ = fmt.Fprintf(conn, "250-fake\r\n250 HELP\r\n")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			_, addr, _ := strings.Cut(trimmed, ":")
			from = strings.Trim(addr, " <>")
			_, _ = fmt.Fprintf(conn, "250 ok\r\n")
		case strings.HasPrefix(upper, "RCPT TO:"):
			_, addr, _ := strings.Cut(trimmed, ":")
			rcpts = append(rcpts, strings.Trim(addr, " <>"))
			_, _ = fmt.Fprintf(conn, "250 ok\r\n")
		case upper == "DATA":
			inData = true
			_, _ = fmt.Fprintf(conn, "354 end with .\r\n")
		case upper == "QUIT":
			_, _ = fmt.Fprintf(conn, "221 bye\r\n")
			return
		default:
			_, _ = fmt.Fprintf(conn, "502 unimplemented\r\n")
		}
	}
}

func receiveSMTP(t *testing.T, f *fakeRelay) smtpDelivery {
	t.Helper()
	select {
	case d := <-f.deliveries:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no DATA received")
		return smtpDelivery{}
	}
}

func assertNoSMTP(t *testing.T, f *fakeRelay) {
	t.Helper()
	select {
	case d := <-f.deliveries:
		t.Fatalf("unexpected DATA sent: %+v", d)
	default:
	}
}

func smtpNode(cfg *model.SMTPConfig) *model.NodeContent {
	return &model.NodeContent{
		Type:    model.NodeTypeExternalCall,
		Name:    "mail",
		SMTP:    cfg,
		Timeout: testTimeout,
	}
}

func smtpSender(t *testing.T, host string, port int, useTLS bool) *mail.SMTPSender {
	t.Helper()
	sender, err := mail.NewSMTPSender(host, port, "", "", "relay@example.test", useTLS, "")
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	return sender
}

func smtpExecutor(sender *mail.SMTPSender, objects executor.SMTPObjectReader, templateDir string, maxBody int) executor.Executor {
	return executor.NewExecutors(
		executor.Limits{SMTPFrom: "relay@example.test", SMTPTemplateDir: templateDir, MaxOutputBytes: maxBody},
		nil,
		executor.Dependencies{SMTPSender: sender, SMTPObjects: objects},
	)[model.NodeTypeExternalCall]
}

func smtpRequest(nc *model.NodeContent, ctx map[string]any) executor.Request {
	return executor.Request{
		Node: nc, Context: ctx,
		InstanceID: "instance-1", NodeInstanceID: "node-1",
	}
}

func assertSMTPReason(t *testing.T, err error) {
	t.Helper()
	var nerr *executor.NodeError
	if !errors.As(err, &nerr) || nerr.Reason != "smtp" {
		t.Fatalf("Execute() error = %v, want NodeError with reason smtp", err)
	}
}

// The highest-risk check: templated delivery proving model, templating, and
// mail layers compose.
func TestSMTPExecutorDeliversTemplated(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, "", 1<<20)
	res, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:      []string{"{{ user.email }}"},
		Subject: "Order {{ order.id }} confirmed",
		Text:    "Hi {{ user.name }}, total {{ order.total }}.",
		HTML:    "<p>Hi {{ user.name }}</p>",
	}), map[string]any{
		"user":  map[string]any{"email": "ada@example.test", "name": "Ada"},
		"order": map[string]any{"id": "123", "total": "42"},
	}))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	d := receiveSMTP(t, fake)
	if d.from != "relay@example.test" {
		t.Errorf("envelope from = %q, want relay default", d.from)
	}
	if len(d.to) != 1 || d.to[0] != "ada@example.test" {
		t.Errorf("envelope to = %v", d.to)
	}
	for _, want := range []string{
		"Subject: Order 123 confirmed",
		"Hi Ada, total 42.",
		"<p>Hi Ada</p>",
		"Message-ID: <node-1.instance-1@beelinewf>",
		"Date: ",
	} {
		if !strings.Contains(d.body, want) {
			t.Errorf("body lacks %q:\n%s", want, d.body)
		}
	}
	if strings.Contains(d.body, "{{") {
		t.Errorf("body has unrendered placeholders:\n%s", d.body)
	}
	out, ok := res.Output.(*executor.SMTPResult)
	if !ok {
		t.Fatalf("Output = %T, want *SMTPResult", res.Output)
	}
	if len(out.To) != 1 || out.To[0] != "ada@example.test" || out.From != "relay@example.test" || out.Subject != "Order 123 confirmed" {
		t.Errorf("SMTPResult = %+v", out)
	}
}

func TestSMTPExecutorExpandsTypedRecipientList(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, "", 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"{{ team.emails }}"},
		Cc:   []string{"{{ team.leads }}"},
		Bcc:  []string{"support-{{ region }}@example.test"},
		From: "sender@example.test",
		Text: "hi",
	}), map[string]any{
		"team":   map[string]any{"emails": []any{"a@example.test", "b@example.test"}, "leads": []string{"c@example.test"}},
		"region": "eu",
	}))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	d := receiveSMTP(t, fake)
	want := []string{"a@example.test", "b@example.test", "c@example.test", "support-eu@example.test"}
	if len(d.to) != len(want) {
		t.Fatalf("envelope to = %v, want %v", d.to, want)
	}
	for i := range want {
		if d.to[i] != want[i] {
			t.Fatalf("envelope to = %v, want %v", d.to, want)
		}
	}
	if d.from != "sender@example.test" {
		t.Errorf("envelope from = %q, want node override", d.from)
	}
}

func TestSMTPExecutorRenderErrorSendsNothing(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, "", 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To: []string{"{{ missing.path }}"}, Text: "hi",
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "render smtp_config.to[0]") {
		t.Fatalf("Execute() error = %v, want render smtp_config.to[0]", err)
	}
	assertSMTPReason(t, err)
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorRejectsInjectedSubject(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, "", 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To: []string{"a@example.test"}, Subject: "{{ evil }}", Text: "hi",
	}), map[string]any{"evil": "x\nBcc: mallory@example.test"}))
	if err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("Execute() error = %v, want newline rejection", err)
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorRefusesPlaintextDowngrade(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, true), nil, "", 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To: []string{"a@example.test"}, Text: "hi",
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("Execute() error = %v, want STARTTLS refusal", err)
	}
	assertSMTPReason(t, err)
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorUnconfiguredRelay(t *testing.T) {
	ex := executor.NewExecutors(
		executor.Limits{MaxOutputBytes: 1 << 20}, nil, executor.Dependencies{},
	)[model.NodeTypeExternalCall]
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To: []string{"a@example.test"}, Text: "hi",
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "smtp is not configured") {
		t.Fatalf("Execute() error = %v, want not-configured", err)
	}
	assertSMTPReason(t, err)
}

func TestSMTPExecutorEnforcesTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				time.Sleep(5 * time.Second)
			}()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	sender, err := mail.NewSMTPSender("127.0.0.1", addr.Port, "", "", "relay@example.test", false, "")
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	ex := smtpExecutor(sender, nil, "", 1<<20)
	nc := smtpNode(&model.SMTPConfig{To: []string{"a@example.test"}, Text: "hi"})
	nc.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err = ex.Execute(context.Background(), smtpRequest(nc, map[string]any{}))
	if err == nil {
		t.Fatal("Execute() error = nil, want timeout")
	}
	assertSMTPReason(t, err)
	if !strings.Contains(strings.ToLower(err.Error()), "timeout") && !strings.Contains(strings.ToLower(err.Error()), "deadline") {
		t.Errorf("Execute() error = %v, want timeout mention", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

// fakeObjects serves s3 file bodies from memory.
type fakeObjects struct {
	files map[string]string
	err   error
	got   [][2]string
}

func (f *fakeObjects) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	f.got = append(f.got, [2]string{bucket, key})
	if f.err != nil {
		return nil, f.err
	}
	content, ok := f.files[bucket+"/"+key]
	if !ok {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func writeTemplate(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
}

func TestSMTPExecutorFileLocalBody(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "order.html", "<p>Hi {{ name }}, total {{ total }}</p>")
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, dir, 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "order.html", Data: json.RawMessage(`{"name": "{{ user.name }}", "total": "{{ order.total }}"}`)},
	}), map[string]any{
		"user":  map[string]any{"name": "Ada"},
		"order": map[string]any{"total": "42"},
	}))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	d := receiveSMTP(t, fake)
	if !strings.Contains(d.body, "<p>Hi Ada, total 42</p>") {
		t.Errorf("body lacks rendered file:\n%s", d.body)
	}
	if !strings.Contains(d.body, "text/html") {
		t.Errorf("body lacks html part:\n%s", d.body)
	}
	if strings.Contains(d.body, "text/plain") {
		t.Errorf("body has text part, want file-only:\n%s", d.body)
	}
}

func TestSMTPExecutorFileS3Body(t *testing.T) {
	objects := &fakeObjects{files: map[string]string{"mail-templates/order.html": "<p>Hi {{ name }}</p>"}}
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), objects, "", 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		Text: "fallback",
		File: &model.SMTPFileConfig{Path: "s3://mail-templates/order.html", Data: json.RawMessage(`{"name": "Bo"}`)},
	}), map[string]any{}))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(objects.got) != 1 || objects.got[0] != [2]string{"mail-templates", "order.html"} {
		t.Fatalf("object reads = %v, want one s3 read", objects.got)
	}
	d := receiveSMTP(t, fake)
	if !strings.Contains(d.body, "<p>Hi Bo</p>") {
		t.Errorf("body lacks rendered s3 file:\n%s", d.body)
	}
	if !strings.Contains(d.body, "fallback") || !strings.Contains(d.body, "text/plain") {
		t.Errorf("body lacks inline text part:\n%s", d.body)
	}
}

// A file placeholder naming a context path directly fails: files render
// against data alone.
func TestSMTPExecutorFileRejectsContextPath(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "leak.html", "<p>{{ user.name }}</p>")
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, dir, 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "leak.html", Data: json.RawMessage(`{}`)},
	}), map[string]any{"user": map[string]any{"name": "Ada"}}))
	if err == nil || !strings.Contains(err.Error(), "render smtp_config.file") {
		t.Fatalf("Execute() error = %v, want render smtp_config.file", err)
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorFileRejectsGoTemplateSyntax(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "go.html", "<p>{{ .Name }}</p>")
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, dir, 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "go.html"},
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "render smtp_config.file") {
		t.Fatalf("Execute() error = %v, want render smtp_config.file", err)
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorFileRejectsEscapeAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "ok.html", "<p>ok</p>")
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, dir, 1<<20)
	for name, path := range map[string]string{
		"traversal": "../outside.html",
		"absolute":  filepath.Join(dir, "ok.html"),
	} {
		_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
			To:   []string{"a@example.test"},
			File: &model.SMTPFileConfig{Path: path},
		}), map[string]any{}))
		if err == nil || !strings.Contains(err.Error(), "load smtp_config.file") {
			t.Errorf("%s: Execute() error = %v, want load failure", name, err)
		}
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorFileRejectsBadRenderedExtension(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, t.TempDir(), 1<<20)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "{{ p }}"},
	}), map[string]any{"p": "note.txt"}))
	if err == nil || !strings.Contains(err.Error(), "must end in .html") {
		t.Fatalf("Execute() error = %v, want extension rejection", err)
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorFileRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "big.html", "<p>"+strings.Repeat("x", 4096)+"</p>")
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), nil, dir, 64)
	_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "big.html"},
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "exceeds the 64 byte limit") {
		t.Fatalf("Execute() error = %v, want size rejection", err)
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorFileMissingLoads(t *testing.T) {
	dir := t.TempDir()
	fake, host, port := startFakeRelay(t)
	ex := smtpExecutor(smtpSender(t, host, port, false), &fakeObjects{}, dir, 1<<20)
	for name, path := range map[string]string{
		"local": "nope.html",
		"s3":    "s3://mail-templates/nope.html",
	} {
		_, err := ex.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
			To:   []string{"a@example.test"},
			File: &model.SMTPFileConfig{Path: path},
		}), map[string]any{}))
		if err == nil || !strings.Contains(err.Error(), "load smtp_config.file") {
			t.Errorf("%s: Execute() error = %v, want load failure", name, err)
		}
	}
	assertNoSMTP(t, fake)
}

func TestSMTPExecutorFileUnconfiguredPaths(t *testing.T) {
	fake, host, port := startFakeRelay(t)
	sender := smtpSender(t, host, port, false)
	local := smtpExecutor(sender, nil, "", 1<<20)
	_, err := local.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "order.html"},
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "template_dir is not configured") {
		t.Errorf("local Execute() error = %v, want template_dir complaint", err)
	}
	s3 := smtpExecutor(sender, nil, t.TempDir(), 1<<20)
	_, err = s3.Execute(context.Background(), smtpRequest(smtpNode(&model.SMTPConfig{
		To:   []string{"a@example.test"},
		File: &model.SMTPFileConfig{Path: "s3://b/order.html"},
	}), map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "object storage is not configured") {
		t.Errorf("s3 Execute() error = %v, want storage complaint", err)
	}
	assertNoSMTP(t, fake)
}

func TestNewExecutorsPanicsOnBadSMTPTemplateDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file.html")
	if err := os.WriteFile(file, []byte("<p>x</p>"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, dir := range []string{missing, file} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("NewExecutors(%q) did not panic, want fail-fast", dir)
					return
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, "engine.smtp.template_dir") {
					t.Errorf("panic = %q, want engine.smtp.template_dir mention", msg)
				}
			}()
			executor.NewExecutors(executor.Limits{SMTPTemplateDir: dir}, nil, executor.Dependencies{})
		}()
	}
}
