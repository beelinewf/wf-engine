package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/mail"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/pkg/contextpath"
)

// maxSMTPRecipients caps the envelope of one workflow mail.
const maxSMTPRecipients = 100

// SMTPSender delivers one built workflow message.
type SMTPSender interface {
	Send(ctx context.Context, msg mail.Message) error
}

// SMTPObjectReader reads template objects for smtp file bodies.
type SMTPObjectReader interface {
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
}

// SMTPExecutor sends one templated email per execution through the operator
// relay. Every message field renders against the request template data; a
// file body loads under the node timeout and renders against its data map
// alone.
type SMTPExecutor struct {
	sender      SMTPSender
	objects     SMTPObjectReader
	from        string
	templateDir string
	maxBody     int
}

// NewSMTPExecutor builds the workflow-mail sender over the canonical form of
// the template root. A set but unusable root panics: bad relay config must
// fail fast at startup, never per-request. A nil sender or a blank root
// disables that path with a per-request error instead, so definitions still
// parse when the relay is not configured.
func NewSMTPExecutor(limits Limits, deps Dependencies) *SMTPExecutor {
	root := ""
	if strings.TrimSpace(limits.SMTPTemplateDir) != "" {
		canonical, err := canonicalizeTemplateDir(limits.SMTPTemplateDir)
		if err != nil {
			panic(fmt.Sprintf("executor: engine.smtp.template_dir %q is not a usable directory: %v", limits.SMTPTemplateDir, err))
		}
		root = canonical
	}
	return &SMTPExecutor{
		sender:      deps.SMTPSender,
		objects:     deps.SMTPObjects,
		from:        limits.SMTPFrom,
		templateDir: root,
		maxBody:     limits.MaxOutputBytes,
	}
}

// canonicalizeTemplateDir resolves dir to its canonical absolute path. It
// fails when the path cannot be resolved or names no directory.
func canonicalizeTemplateDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return canonical, nil
}

func (e *SMTPExecutor) Execute(ctx context.Context, req Request) (*Result, error) {
	if e.sender == nil {
		return nil, &NodeError{Node: req.Node, Reason: "smtp", Err: errors.New("smtp is not configured")}
	}
	sendCtx, cancel := context.WithTimeout(ctx, nodeTimeout(req.Node))
	defer cancel()
	msg, err := e.buildSMTPMessage(sendCtx, req)
	if err != nil {
		return nil, &NodeError{Node: req.Node, Reason: "smtp", Err: err}
	}
	if err := e.sender.Send(sendCtx, msg); err != nil {
		return nil, &NodeError{Node: req.Node, Reason: "smtp", Err: err}
	}
	return &Result{Output: &SMTPResult{
		To: msg.To, Cc: msg.Cc, Bcc: msg.Bcc, From: msg.From, Subject: msg.Subject,
	}}, nil
}

// buildSMTPMessage renders the node message against the request template
// data. All validation runs after rendering; any failure aborts before
// anything is sent.
func (e *SMTPExecutor) buildSMTPMessage(ctx context.Context, req Request) (mail.Message, error) {
	cfg := req.Node.SMTP
	data := req.TemplateData()
	var msg mail.Message
	var err error
	from := e.from
	if cfg.From != "" {
		if from, err = renderSMTPString("from", cfg.From, data); err != nil {
			return mail.Message{}, err
		}
	}
	msg.From = strings.TrimSpace(from)
	if msg.ReplyTo, err = renderSMTPString("reply_to", cfg.ReplyTo, data); err != nil {
		return mail.Message{}, err
	}
	msg.ReplyTo = strings.TrimSpace(msg.ReplyTo)
	if msg.Subject, err = renderSMTPString("subject", cfg.Subject, data); err != nil {
		return mail.Message{}, err
	}
	if msg.Text, err = renderSMTPString("text", cfg.Text, data); err != nil {
		return mail.Message{}, err
	}
	if msg.HTML, err = renderSMTPString("html", cfg.HTML, data); err != nil {
		return mail.Message{}, err
	}
	if msg.To, err = renderSMTPRecipients("to", cfg.To, data); err != nil {
		return mail.Message{}, err
	}
	if msg.Cc, err = renderSMTPRecipients("cc", cfg.Cc, data); err != nil {
		return mail.Message{}, err
	}
	if msg.Bcc, err = renderSMTPRecipients("bcc", cfg.Bcc, data); err != nil {
		return mail.Message{}, err
	}
	if n := len(msg.To) + len(msg.Cc) + len(msg.Bcc); n == 0 {
		return mail.Message{}, errors.New("smtp_config rendered to no recipients")
	} else if n > maxSMTPRecipients {
		return mail.Message{}, fmt.Errorf("smtp_config has %d recipients, at most %d are allowed", n, maxSMTPRecipients)
	}
	if cfg.File != nil {
		if msg.HTML, err = e.loadSMTPFileBody(ctx, cfg.File, data); err != nil {
			return mail.Message{}, err
		}
	}
	if e.maxBody > 0 {
		if len(msg.Text) > e.maxBody {
			return mail.Message{}, fmt.Errorf("smtp_config.text body exceeds the %d byte limit", e.maxBody)
		}
		if len(msg.HTML) > e.maxBody {
			return mail.Message{}, fmt.Errorf("smtp_config.html body exceeds the %d byte limit", e.maxBody)
		}
	}
	msg.Date = time.Now().UTC().Format(time.RFC1123Z)
	msg.MessageID = fmt.Sprintf("<%s.%s@beelinewf>", req.NodeInstanceID, req.InstanceID)
	return msg, nil
}

// renderSMTPString renders one message field, requiring a string result.
func renderSMTPString(field, value string, data map[string]any) (string, error) {
	rendered, err := contextpath.RenderTemplate(value, data)
	if err != nil {
		return "", fmt.Errorf("render smtp_config.%s: %w", field, err)
	}
	s, ok := rendered.(string)
	if !ok {
		return "", fmt.Errorf("smtp_config.%s rendered to %T, want string", field, rendered)
	}
	return s, nil
}

// renderSMTPRecipients renders one recipient list, splicing a typed string
// list when an element is a lone placeholder and stringifying otherwise.
func renderSMTPRecipients(field string, addrs []string, data map[string]any) ([]string, error) {
	var out []string
	for i, addr := range addrs {
		rendered, err := contextpath.RenderTemplate(addr, data)
		if err != nil {
			return nil, fmt.Errorf("render smtp_config.%s[%d]: %w", field, i, err)
		}
		switch v := rendered.(type) {
		case string:
			out = append(out, strings.TrimSpace(v))
		case []string:
			for _, item := range v {
				out = append(out, strings.TrimSpace(item))
			}
		case []any:
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("smtp_config.%s[%d] expands to a non-string element", field, i)
				}
				out = append(out, strings.TrimSpace(s))
			}
		default:
			out = append(out, strings.TrimSpace(fmt.Sprintf("%v", rendered)))
		}
	}
	return out, nil
}

// loadSMTPFileBody resolves the file HTML body: it renders the path and the
// data map against the request template data, loads the raw template, then
// renders its contents against the rendered data map alone.
func (e *SMTPExecutor) loadSMTPFileBody(ctx context.Context, file *model.SMTPFileConfig, data map[string]any) (string, error) {
	renderedPath, err := contextpath.RenderTemplate(file.Path, data)
	if err != nil {
		return "", fmt.Errorf("render smtp_config.file.path: %w", err)
	}
	pathStr, ok := renderedPath.(string)
	if !ok {
		return "", fmt.Errorf("smtp_config.file.path rendered to %T, want string", renderedPath)
	}
	lower := strings.ToLower(pathStr)
	if !strings.HasSuffix(lower, ".html") && !strings.HasSuffix(lower, ".htm") {
		return "", fmt.Errorf("smtp_config.file.path %q must end in .html or .htm", pathStr)
	}
	fileData := map[string]any{}
	if len(file.Data) > 0 {
		renderedData, err := contextpath.RenderJSON(file.Data, data)
		if err != nil {
			return "", fmt.Errorf("render smtp_config.file.data: %w", err)
		}
		if err := json.Unmarshal(renderedData, &fileData); err != nil {
			return "", fmt.Errorf("render smtp_config.file.data: %w", err)
		}
		if fileData == nil {
			return "", errors.New("render smtp_config.file.data: rendered to null, want object")
		}
	}
	raw, err := e.loadSMTPTemplate(ctx, pathStr)
	if err != nil {
		return "", err
	}
	rendered, err := contextpath.RenderTemplate(string(raw), fileData)
	if err != nil {
		return "", fmt.Errorf("render smtp_config.file: %w", err)
	}
	html, ok := rendered.(string)
	if !ok {
		return "", fmt.Errorf("smtp_config.file rendered to %T, want string", rendered)
	}
	if e.maxBody > 0 && len(html) > e.maxBody {
		return "", fmt.Errorf("smtp_config.file rendered body exceeds the %d byte limit", e.maxBody)
	}
	return html, nil
}

// loadSMTPTemplate reads the raw template bytes for a rendered path:
// s3://bucket/key via the object reader, otherwise a relative path joined
// under the template root and rejected on escape after symlink resolution.
func (e *SMTPExecutor) loadSMTPTemplate(ctx context.Context, path string) ([]byte, error) {
	if strings.HasPrefix(path, "s3://") {
		if e.objects == nil {
			return nil, errors.New("load smtp_config.file: object storage is not configured")
		}
		rest := strings.TrimPrefix(path, "s3://")
		bucket, key, ok := strings.Cut(rest, "/")
		if !ok || bucket == "" || key == "" {
			return nil, fmt.Errorf("load smtp_config.file: path %q must be s3://bucket/key", path)
		}
		rc, err := e.objects.Get(ctx, bucket, key)
		if err != nil {
			return nil, fmt.Errorf("load smtp_config.file: %w", err)
		}
		defer func() { _ = rc.Close() }()
		return readSMTPFile(rc, e.maxBody, path)
	}
	if strings.Contains(path, "://") {
		return nil, fmt.Errorf("load smtp_config.file: scheme in path %q is not supported (want s3:// or a template_dir-relative path)", path)
	}
	if e.templateDir == "" {
		return nil, errors.New("load smtp_config.file: smtp template_dir is not configured")
	}
	if filepath.IsAbs(path) {
		return nil, fmt.Errorf("load smtp_config.file: path %q must be relative to the template dir", path)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Join(e.templateDir, path))
	if err != nil {
		return nil, fmt.Errorf("load smtp_config.file: %w", err)
	}
	if canonical != e.templateDir && !strings.HasPrefix(canonical, e.templateDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("load smtp_config.file: path %q escapes the template dir", path)
	}
	f, err := os.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("load smtp_config.file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readSMTPFile(f, e.maxBody, path)
}

// readSMTPFile reads r up to max bytes; longer content is rejected. A
// non-positive max is unlimited.
func readSMTPFile(r io.Reader, maxBytes int, path string) ([]byte, error) {
	if maxBytes <= 0 {
		out, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("load smtp_config.file: read %q: %w", path, err)
		}
		return out, nil
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("load smtp_config.file: read %q: %w", path, err)
	}
	if len(out) > maxBytes {
		return nil, fmt.Errorf("load smtp_config.file: %q exceeds the %d byte limit", path, maxBytes)
	}
	return out, nil
}
