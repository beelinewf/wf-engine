package mail

import (
	"bytes"
	_ "embed"
	"fmt"
	htmltemplate "html/template"
	"os"
	"path/filepath"
	"text/template"
	"time"
)

//go:embed templates/verify_email.html
var defaultVerifyHTML string

//go:embed templates/verify_email.txt
var defaultVerifyText string

// Template filenames live in one directory so a single template_dir knob
// overrides the pair. Both are required: mail without a text part reads
// as spam to several filters.
const (
	verifyHTMLName = "verify_email.html"
	verifyTextName = "verify_email.txt"
)

// templateData is the whole contract override templates program against:
// the absolute verification link and a humanized expiry ("24 hours").
type templateData struct {
	Link      string
	ExpiresIn string
}

// verifyTemplates renders the two verification-mail parts from one data.
type verifyTemplates struct {
	html *htmltemplate.Template
	text *template.Template
}

// loadVerifyTemplates parses the embedded defaults, or the override pair
// from dir when set. Every failure names the cause: construction feeds
// the boot, so a broken override must fail there, never at the first
// registration.
func loadVerifyTemplates(dir string) (*verifyTemplates, error) {
	htmlSrc, textSrc := defaultVerifyHTML, defaultVerifyText
	if dir != "" {
		htmlRaw, err := os.ReadFile(filepath.Join(dir, verifyHTMLName))
		if err != nil {
			return nil, fmt.Errorf("mail: read %s: %w", verifyHTMLName, err)
		}
		textRaw, err := os.ReadFile(filepath.Join(dir, verifyTextName))
		if err != nil {
			return nil, fmt.Errorf("mail: read %s: %w", verifyTextName, err)
		}
		htmlSrc, textSrc = string(htmlRaw), string(textRaw)
	}
	htmlT, err := htmltemplate.New(verifyHTMLName).Parse(htmlSrc)
	if err != nil {
		return nil, fmt.Errorf("mail: parse %s: %w", verifyHTMLName, err)
	}
	textT, err := template.New(verifyTextName).Parse(textSrc)
	if err != nil {
		return nil, fmt.Errorf("mail: parse %s: %w", verifyTextName, err)
	}
	tmpl := &verifyTemplates{html: htmlT, text: textT}
	// A well-formed template can still name a field the data lacks;
	// that fails at execution, so one trial render keeps the boot the
	// place an override breaks.
	if _, _, err := tmpl.render("https://example.test/verify-email?token=boot-check", time.Hour); err != nil {
		return nil, err
	}
	return tmpl, nil
}

// render executes both parts for one delivery.
func (t *verifyTemplates) render(link string, ttl time.Duration) (html, text string, err error) {
	data := templateData{Link: link, ExpiresIn: humanizeTTL(ttl)}
	var htmlBuf, textBuf bytes.Buffer
	if err := t.html.Execute(&htmlBuf, data); err != nil {
		return "", "", fmt.Errorf("mail: render %s: %w", verifyHTMLName, err)
	}
	if err := t.text.Execute(&textBuf, data); err != nil {
		return "", "", fmt.Errorf("mail: render %s: %w", verifyTextName, err)
	}
	return htmlBuf.String(), textBuf.String(), nil
}

// humanizeTTL renders an expiry the way a recipient reads it: whole hours
// past two of them, whole minutes below. Hours floor so the text never
// overstates the link's life.
func humanizeTTL(ttl time.Duration) string {
	minutes := int(ttl.Round(time.Minute).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	if minutes >= 120 {
		hours := minutes / 60
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// subjectLine is the one header templates do not own: it stays constant
// so filters and frontends can key on it.
const subjectLine = "Verify your email"
